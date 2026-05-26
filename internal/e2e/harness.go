package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Scenario is the unit of eval work. Name() identifies it in reports and
// --scenario filtering. Run() owns SETUP → DISPATCH → VALIDATE; it must
// not depend on global state and must tolerate being cancelled via ctx.
type Scenario interface {
	Name() string
	Run(ctx context.Context, env *Env, log io.Writer) (*Outcome, error)
}

// Outcome is what a Scenario returns when it has completed (pass OR fail).
// A nil Outcome with a non-nil error indicates the scenario crashed before
// it could record a structured result (env setup failure, panic, etc.) —
// the runner treats that as a fail with the error message as Detail.
type Outcome struct {
	Status         string         `json:"status"` // pass | fail
	Detail         string         `json:"detail,omitempty"`
	Mode           string         `json:"mode"`               // "direct" (v0) | "chain" (future)
	AgentWallTime  time.Duration  `json:"agent_wall_time_ns"` // total time spent inside claude(s)
	ValidateOutput string         `json:"validate_output,omitempty"`
	Extras         map[string]any `json:"extras,omitempty"`
}

// Runner executes a set of scenarios and writes a JSON report.
type Runner struct {
	Scenarios       []Scenario
	ScenariosRoot   string // testdata/e2e-scenarios/
	ReportDir       string
	Verbose         bool
	ScenarioTimeout time.Duration // wall-clock cap per scenario; 0 = no override
	StopOnFail      bool
	KeepArtifs      bool
	ClaudeBin       string
	RepoRoot        string
	BaseEnv         []string
	NowFn           func() time.Time
	// Mode selects the dispatch path: "direct" (plain claude -p, v0) or
	// "elonco" (full-stack: arcmux daemon + elonco service + arcmux-spawned
	// claude session). Defaults to "elonco" at the CLI layer.
	Mode string
	// ArcmuxBin is the path to the arcmux daemon binary (required for
	// "elonco" mode; ignored in "direct" mode).
	ArcmuxBin string
	// ElonkoPython is the python interpreter to run `python -m elonco serve`
	// with. Required for "elonco" mode.
	ElonkoPython string
}

// StepReport mirrors validate.sh / e2e shape: per-scenario row in the report.
type StepReport struct {
	Name           string        `json:"name"`
	Status         string        `json:"status"`
	Mode           string        `json:"mode,omitempty"`
	DurationS      int           `json:"duration_s"`
	AgentWallTimeS int           `json:"agent_wall_time_s,omitempty"`
	StartedAt      string        `json:"started_at"`
	FinishedAt     string        `json:"finished_at"`
	Detail         string        `json:"detail,omitempty"`
	Artifacts      ArtifactPaths `json:"artifacts,omitempty"`
}

// ArtifactPaths records where the scenario left files for post-mortem.
type ArtifactPaths struct {
	TempRoot string `json:"temp_root,omitempty"`
	WorkRepo string `json:"workrepo,omitempty"`
	Trace    string `json:"trace,omitempty"`
}

// FinalReport is what gets written under $ARCMUX_EPHEMERAL/validate-reports/.
type FinalReport struct {
	Stamp     string       `json:"stamp"`
	Timezone  string       `json:"timezone"`
	RepoRoot  string       `json:"repo_root"`
	ReportDir string       `json:"report_dir"`
	ClaudeBin string       `json:"claude_bin"`
	Overall   string       `json:"overall"`
	Steps     []StepReport `json:"steps"`
}

// Run executes the configured scenarios and writes a report. Returns a
// non-nil error if any scenario failed (the report is still written).
func (r *Runner) Run(ctx context.Context, stdout io.Writer) error {
	now := r.NowFn
	if now == nil {
		now = time.Now
	}
	stamp := timeStamp(now())
	if err := os.MkdirAll(r.ReportDir, 0o755); err != nil {
		return fmt.Errorf("mkdir report dir: %w", err)
	}
	reportPath := filepath.Join(r.ReportDir, "e2e-"+stamp+".json")

	fmt.Fprintf(stdout, "arcmux e2e — %s PT\n", stamp)
	fmt.Fprintf(stdout, "  report: %s\n", reportPath)
	fmt.Fprintf(stdout, "  claude: %s\n\n", r.ClaudeBin)

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
		ClaudeBin: r.ClaudeBin,
		Overall:   overall,
		Steps:     steps,
	}
	if err := writeReport(reportPath, rep); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	fmt.Fprintf(stdout, "\noverall: %s\nreport:  %s\n", overall, reportPath)
	if overall != "pass" {
		return fmt.Errorf("e2e: one or more scenarios failed (see %s)", reportPath)
	}
	return nil
}

func (r *Runner) runOne(ctx context.Context, s Scenario, stdout io.Writer) StepReport {
	started := time.Now()
	report := StepReport{
		Name:      s.Name(),
		StartedAt: started.Format(time.RFC3339),
	}

	scenarioDir := filepath.Join(r.ScenariosRoot, s.Name())
	env, err := NewEnv(s.Name(), scenarioDir, r.ClaudeBin, r.BaseEnv)
	if err != nil {
		report.Status = "fail"
		report.Detail = "env setup failed: " + err.Error()
		report.DurationS = int(time.Since(started).Seconds())
		report.FinishedAt = time.Now().Format(time.RFC3339)
		fmt.Fprintf(stdout, "  [fail] %-26s %ds  (env setup)\n", s.Name(), report.DurationS)
		return report
	}
	env.Mode = r.Mode
	env.ArcmuxBin = r.ArcmuxBin
	env.ElonkoPython = r.ElonkoPython
	report.Artifacts = ArtifactPaths{
		TempRoot: env.TempRoot,
		WorkRepo: env.WorkRepo,
		Trace:    env.TracePath,
	}

	scenarioCtx := ctx
	var cancel context.CancelFunc
	if r.ScenarioTimeout > 0 {
		scenarioCtx, cancel = context.WithTimeout(ctx, r.ScenarioTimeout)
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	outcome, runErr := s.Run(scenarioCtx, env, env.TraceWriter())

	// Always teardown; keep artifacts when:
	//   - the user passed --keep, OR
	//   - the scenario failed (debugging needs trace + workrepo)
	keep := r.KeepArtifs || runErr != nil || (outcome != nil && outcome.Status != "pass")
	env.Teardown(stdout, keep)

	report.DurationS = int(time.Since(started).Seconds())
	report.FinishedAt = time.Now().Format(time.RFC3339)

	switch {
	case runErr != nil:
		report.Status = "fail"
		report.Detail = truncate(runErr.Error(), 600)
		fmt.Fprintf(stdout, "  [fail] %-26s %ds  trace=%s\n", s.Name(), report.DurationS, env.TracePath)
	case outcome == nil:
		report.Status = "fail"
		report.Detail = "scenario returned nil outcome and nil error"
		fmt.Fprintf(stdout, "  [fail] %-26s %ds  (nil outcome)\n", s.Name(), report.DurationS)
	default:
		report.Status = outcome.Status
		report.Mode = outcome.Mode
		report.AgentWallTimeS = int(outcome.AgentWallTime.Seconds())
		if outcome.Detail != "" {
			report.Detail = truncate(outcome.Detail, 600)
		}
		marker := "[pass]"
		if outcome.Status != "pass" {
			marker = "[fail]"
		}
		fmt.Fprintf(stdout, "  %s %-26s %ds  agent=%ds  mode=%s\n",
			marker, s.Name(), report.DurationS, report.AgentWallTimeS, report.Mode)
	}
	return report
}

// DispatchDirect spawns one claude -p invocation against env.WorkRepo with
// the given mission and a wall-time cap. Returns the agent's wall-time and
// any error (non-zero exit, timeout, etc.). The agent's stdout/stderr is
// captured into <env.LogsDir>/agent-direct.log and traced.
//
// The mission is passed as the user message via -p; no role file is appended
// (direct dispatch is plain claude, no arcmux identity). The agent's cwd is
// env.WorkRepo so any file-create tool calls land in the sandbox.
//
// Extra env entries (e.g. "FOO=BAR") are layered on top of SpawnedEnv.
func DispatchDirect(ctx context.Context, env *Env, mission string, wallCap time.Duration, extraEnv ...string) (time.Duration, error) {
	if wallCap <= 0 {
		wallCap = 5 * time.Minute
	}
	agentCtx, cancel := context.WithTimeout(ctx, wallCap)
	defer cancel()

	logPath := filepath.Join(env.LogsDir, "agent-direct.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("create agent log: %w", err)
	}
	defer logFile.Close()

	// --dangerously-skip-permissions is required for unattended dispatch;
	// the same flag the manager-mode bootstrap uses for in-pane launches.
	// Mission goes as a single -p arg; claude prints final assistant text +
	// usage to stdout and exits.
	args := []string{
		"-p", mission,
		"--dangerously-skip-permissions",
	}
	cmd := exec.CommandContext(agentCtx, env.ClaudeBin, args...)
	cmd.Dir = env.WorkRepo
	cmd.Env = env.SpawnedEnv(extraEnv...)
	// New process group so a timeout SIGKILL takes the whole agent tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Tee stdout+stderr into both the log file and a small in-memory buffer
	// for trace breadcrumbs.
	var head bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &headWriter{cap: 4096, b: &head})
	cmd.Stderr = io.MultiWriter(logFile, &headWriter{cap: 4096, b: &head})

	env.Tracef("$ (cwd=%s) claude -p <mission %d bytes> --dangerously-skip-permissions [cap=%s]",
		env.WorkRepo, len(mission), wallCap)
	started := time.Now()
	err = cmd.Run()
	elapsed := time.Since(started)
	env.Tracef("  claude exit after %s err=%v", elapsed, err)
	if head.Len() > 0 {
		env.Tracef("  claude head/tail: %s", truncate(head.String(), 800))
	}

	if err != nil {
		if agentCtx.Err() == context.DeadlineExceeded {
			return elapsed, fmt.Errorf("agent wall-time cap %s exceeded (log: %s)", wallCap, logPath)
		}
		return elapsed, fmt.Errorf("claude exited with error: %w (log: %s)", err, logPath)
	}
	return elapsed, nil
}

// RunValidateScript runs the scenario's validate.sh against env.WorkRepo
// and returns its combined output + pass/fail. Pass = exit 0.
func RunValidateScript(ctx context.Context, env *Env, scriptName string) (string, error) {
	scriptPath := env.ScenarioPath(scriptName)
	if _, err := os.Stat(scriptPath); err != nil {
		return "", fmt.Errorf("validate script %q: %w", scriptPath, err)
	}
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		// Non-fatal; bash <path> would still work, but try to chmod.
		env.Tracef("chmod %s: %v", scriptPath, err)
	}

	cmd := exec.CommandContext(ctx, "bash", scriptPath, env.WorkRepo)
	cmd.Env = env.SpawnedEnv()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	env.Tracef("$ bash %s %s", scriptPath, env.WorkRepo)
	err := cmd.Run()
	out := buf.String()
	env.Tracef("  validate exit err=%v output=%s", err, truncate(out, 800))
	if err != nil {
		return out, fmt.Errorf("validate.sh exit non-zero: %w", err)
	}
	return out, nil
}

// headWriter is a tiny io.Writer that captures only the first cap bytes.
// Used for trace breadcrumbs from very chatty agent runs.
type headWriter struct {
	cap int
	b   *bytes.Buffer
}

func (h *headWriter) Write(p []byte) (int, error) {
	if h.b.Len() >= h.cap {
		return len(p), nil
	}
	remaining := h.cap - h.b.Len()
	if len(p) <= remaining {
		h.b.Write(p)
		return len(p), nil
	}
	h.b.Write(p[:remaining])
	return len(p), nil
}

func writeReport(path string, rep FinalReport) error {
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func timeStamp(t time.Time) string {
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
