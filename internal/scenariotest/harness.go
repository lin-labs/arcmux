// Package scenariotest implements arcmux's substrate scenario test
// harness — fast, free, behavioral coverage that complements the
// structural checks in scripts/validate.sh. Where validate.sh proves
// the binaries COMPILE and the wire-edges respond, scenariotest proves
// the substrate actually DOES the right thing when a human-style
// sequence of commands is issued via arcmux-cli.
//
// (No real agent is invoked here — that's cmd/arcmux-e2e's job. See
// internal/e2e for the agent-behavioral harness.)
//
// Each scenario follows the SETUP → ACT → ASSERT → TEARDOWN shape (see
// scenario.go). Scenarios are independent — they run in fully isolated
// per-run temp dirs (unique data_root, vault_root, daemon socket, tmux
// socket name, cmux workspace prefix), so a failure in one cannot cascade
// to another. Teardown always runs, even on failure, so leftover state
// from a crashed scenario is best-effort cleaned.
//
// The harness writes a structured JSON report mirroring validate.sh's
// shape (overall pass/fail + per-step status/duration/detail), under
// $ARCMUX_EPHEMERAL/validate-reports/test-YYYY-MM-DD-HH-MM.json (or
// ./.validate-reports/ when $ARCMUX_EPHEMERAL is unset). The report is
// the durable evidence the harness was green before commit.
package scenariotest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scenario is the unit of scenariotest work. Name() identifies it in reports +
// --scenario filtering. Run() is the whole SETUP→ACT→ASSERT→TEARDOWN
// flow; it MUST tear down (even on panic / assertion failure) before
// returning.
type Scenario interface {
	Name() string
	Run(ctx context.Context, env *Env, log io.Writer) error
}

// Runner executes a set of scenarios and writes a JSON report.
type Runner struct {
	Scenarios   []Scenario
	ReportDir   string        // directory to write the JSON report into
	Verbose     bool          // mirror scenario logs to stdout in real time
	Timeout     time.Duration // per-scenario timeout (0 = no override)
	StopOnFail  bool          // stop after first failing scenario (default: false)
	KeepArtifs  bool          // keep per-scenario temp dirs after pass (default: false)
	NowFn       func() time.Time
	ArcmuxBin   string   // path to arcmux binary (for daemon spawn). Required.
	CallBin     string   // path to arcmux-cli binary. Required.
	RepoRoot    string   // arcmux repo root (for log breadcrumbs). Optional.
	BootEnvBase []string // OS env to inherit into spawned daemons; nil = os.Environ()
}

// StepReport captures one scenario's outcome. Fields mirror validate.sh.
type StepReport struct {
	Name       string        `json:"name"`
	Status     string        `json:"status"` // pass | fail | skip
	DurationS  int           `json:"duration_s"`
	StartedAt  string        `json:"started_at"`
	FinishedAt string        `json:"finished_at"`
	Detail     string        `json:"detail,omitempty"`
	Artifacts  ArtifactPaths `json:"artifacts,omitempty"`
}

// ArtifactPaths records where a scenario left durable artifacts so a
// failed run is debuggable post-mortem.
type ArtifactPaths struct {
	TempRoot  string `json:"temp_root,omitempty"`
	DaemonLog string `json:"daemon_log,omitempty"`
	Trace     string `json:"trace,omitempty"`
}

// FinalReport is what gets written to disk.
type FinalReport struct {
	Stamp     string       `json:"stamp"`
	Timezone  string       `json:"timezone"`
	RepoRoot  string       `json:"repo_root"`
	ReportDir string       `json:"report_dir"`
	Overall   string       `json:"overall"`
	Steps     []StepReport `json:"steps"`
}

// Run executes the configured scenarios and writes a report. Returns an
// error if any scenario failed (the report is still written).
func (r *Runner) Run(ctx context.Context, stdout io.Writer) error {
	now := r.NowFn
	if now == nil {
		now = time.Now
	}
	stamp := timeStamp(now())
	if err := os.MkdirAll(r.ReportDir, 0o755); err != nil {
		return fmt.Errorf("mkdir report dir: %w", err)
	}
	reportPath := filepath.Join(r.ReportDir, "test-"+stamp+".json")

	fmt.Fprintf(stdout, "arcmux test — %s PT\n", stamp)
	fmt.Fprintf(stdout, "  report: %s\n", reportPath)
	fmt.Fprintf(stdout, "  bin:    %s\n\n", r.ArcmuxBin)

	steps := make([]StepReport, 0, len(r.Scenarios))
	overall := "pass"

	for _, s := range r.Scenarios {
		step := r.runOne(ctx, s, stdout)
		steps = append(steps, step)
		if step.Status == "fail" {
			overall = "fail"
			if r.StopOnFail {
				break
			}
		}
	}

	rep := FinalReport{
		Stamp:     stamp,
		Timezone:  "America/Los_Angeles",
		RepoRoot:  r.RepoRoot,
		ReportDir: r.ReportDir,
		Overall:   overall,
		Steps:     steps,
	}
	if err := writeReport(reportPath, rep); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	fmt.Fprintf(stdout, "\noverall: %s\nreport:  %s\n", overall, reportPath)
	if overall != "pass" {
		return fmt.Errorf("test: one or more scenarios failed (see %s)", reportPath)
	}
	return nil
}

func (r *Runner) runOne(ctx context.Context, s Scenario, stdout io.Writer) StepReport {
	startedAt := time.Now()
	report := StepReport{
		Name:      s.Name(),
		StartedAt: startedAt.Format(time.RFC3339),
	}

	env, err := NewEnv(s.Name(), r.ArcmuxBin, r.CallBin, r.BootEnvBase)
	if err != nil {
		report.Status = "fail"
		report.Detail = "env setup failed: " + err.Error()
		report.DurationS = int(time.Since(startedAt).Seconds())
		report.FinishedAt = time.Now().Format(time.RFC3339)
		fmt.Fprintf(stdout, "  [fail] %-26s %ds  (env setup)\n", s.Name(), report.DurationS)
		return report
	}
	report.Artifacts.TempRoot = env.TempRoot
	report.Artifacts.DaemonLog = env.DaemonLogPath
	report.Artifacts.Trace = env.TracePath

	scenarioCtx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		scenarioCtx, cancel = context.WithTimeout(ctx, r.Timeout)
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	runErr := s.Run(scenarioCtx, env, env.TraceWriter())
	// Always teardown, even on failure. Keep temp artifacts when:
	//   - the user passed --keep, OR
	//   - the scenario failed (debugging needs the trace + daemon log).
	keep := r.KeepArtifs || runErr != nil
	env.Teardown(stdout, keep)

	report.DurationS = int(time.Since(startedAt).Seconds())
	report.FinishedAt = time.Now().Format(time.RFC3339)

	if runErr != nil {
		report.Status = "fail"
		report.Detail = truncate(runErr.Error(), 600)
		fmt.Fprintf(stdout, "  [fail] %-26s %ds  trace=%s\n", s.Name(), report.DurationS, env.TracePath)
	} else {
		report.Status = "pass"
		fmt.Fprintf(stdout, "  [pass] %-26s %ds\n", s.Name(), report.DurationS)
	}
	return report
}

func writeReport(path string, rep FinalReport) error {
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func timeStamp(t time.Time) string {
	// match validate.sh's TZ=America/Los_Angeles +%Y-%m-%d-%H-%M
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err == nil {
		t = t.In(loc)
	}
	return t.Format("2006-01-02-15-04")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
