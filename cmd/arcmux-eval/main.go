// arcmux-eval is the agent-behavioral eval harness.
//
// Where cmd/arcmux-e2e proves the SUBSTRATE behaves (cmux + bbolt + audit
// rows show up where they should), arcmux-eval proves the AGENT STACK
// behaves: given a mission and a fresh sandbox workrepo, can claude
// produce an artifact that passes a per-scenario assertion script?
//
// v0 scope: direct-dispatch only. One `claude -p MISSION` invocation per
// scenario, no arcmux team chain. Chain-mode scenarios (Elon→Manager→IC
// dispatched via the arcmux substrate) are tracked in §F15 forward-plan
// and will land as additional Scenario implementations.
//
// Cost: every scenario spends real Claude tokens. Run intentionally.
//
// Usage:
//
//	bin/arcmux-eval                              # all scenarios
//	bin/arcmux-eval --scenario hello-server       # one scenario by name
//	bin/arcmux-eval --scenario a,b                # subset
//	bin/arcmux-eval --list                        # print scenario names
//	bin/arcmux-eval --keep                        # keep workrepo dirs after pass
//	bin/arcmux-eval --report-dir <path>           # override report dir
//	bin/arcmux-eval --claude <path>               # override claude binary
//	bin/arcmux-eval --timeout 10m                 # per-scenario wall cap
//
// Exit 0 only if every selected scenario passed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lin-labs/arcmux/internal/eval"
	"github.com/lin-labs/arcmux/internal/eval/scenarios"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "arcmux-eval: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("arcmux-eval", flag.ContinueOnError)
	scenarioFilter := fs.String("scenario", "", "comma-separated scenario names (default: all)")
	reportDir := fs.String("report-dir", "", "override report directory")
	claudeBin := fs.String("claude", "", "path to claude binary (default: search PATH)")
	scenariosRoot := fs.String("scenarios-root", "", "path to testdata/eval-scenarios/ (default: <repo>/testdata/eval-scenarios)")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-scenario wall-time cap (includes agent + validate)")
	listOnly := fs.Bool("list", false, "list available scenarios and exit")
	keep := fs.Bool("keep", false, "keep per-scenario sandbox dirs even on pass")
	stopOnFail := fs.Bool("stop", false, "stop after first failure")
	verbose := fs.Bool("v", false, "verbose progress to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	all := []eval.Scenario{
		scenarios.HelloServer{},
	}

	if *listOnly {
		for _, s := range all {
			fmt.Println(s.Name())
		}
		return nil
	}

	selected := all
	if f := strings.TrimSpace(*scenarioFilter); f != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(f, ",") {
			want[strings.TrimSpace(n)] = true
		}
		selected = nil
		for _, s := range all {
			if want[s.Name()] {
				selected = append(selected, s)
				delete(want, s.Name())
			}
		}
		if len(want) > 0 {
			unknown := make([]string, 0, len(want))
			for k := range want {
				unknown = append(unknown, k)
			}
			return fmt.Errorf("unknown scenarios: %s", strings.Join(unknown, ", "))
		}
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	if *scenariosRoot == "" {
		*scenariosRoot = filepath.Join(repoRoot, "testdata", "eval-scenarios")
	}
	if *reportDir == "" {
		*reportDir = resolveReportDir(repoRoot)
	}

	resolvedClaude := *claudeBin
	if resolvedClaude == "" {
		p, lookErr := exec.LookPath("claude")
		if lookErr != nil {
			return fmt.Errorf("claude not found on PATH (--claude <path> to override): %w", lookErr)
		}
		resolvedClaude = p
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := &eval.Runner{
		Scenarios:       selected,
		ScenariosRoot:   *scenariosRoot,
		ReportDir:       *reportDir,
		Verbose:         *verbose,
		ScenarioTimeout: *timeout,
		StopOnFail:      *stopOnFail,
		KeepArtifs:      *keep,
		ClaudeBin:       resolvedClaude,
		RepoRoot:        repoRoot,
	}
	return r.Run(ctx, os.Stdout)
}

// resolveReportDir mirrors arcmux-e2e: prefer $ARCMUX_EPHEMERAL when set,
// fall back to <repo>/.validate-reports otherwise. Reports land in a
// single directory alongside structural + e2e reports for one-stop audit.
func resolveReportDir(repoRoot string) string {
	if e := os.Getenv("ARCMUX_EPHEMERAL"); e != "" {
		return filepath.Join(e, "validate-reports")
	}
	return filepath.Join(repoRoot, ".validate-reports")
}
