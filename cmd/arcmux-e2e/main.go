// arcmux-e2e is the agent-behavioral end-to-end harness.
//
// Where cmd/arcmux-test proves the SUBSTRATE behaves (cmux + bbolt +
// audit rows show up where they should), arcmux-e2e proves the AGENT
// STACK behaves: given a mission and a fresh sandbox workrepo, can
// claude produce an artifact that passes a per-scenario assertion
// script?
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
//	bin/arcmux-e2e                              # all scenarios
//	bin/arcmux-e2e --scenario hello-server       # one scenario by name
//	bin/arcmux-e2e --scenario a,b                # subset
//	bin/arcmux-e2e --list                        # print scenario names
//	bin/arcmux-e2e --keep                        # keep workrepo dirs after pass
//	bin/arcmux-e2e --report-dir <path>           # override report dir
//	bin/arcmux-e2e --claude <path>               # override claude binary
//	bin/arcmux-e2e --timeout 10m                 # per-scenario wall cap
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

	"github.com/lin-labs/arcmux/internal/e2e"
	"github.com/lin-labs/arcmux/internal/e2e/scenarios"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "arcmux-e2e: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("arcmux-e2e", flag.ContinueOnError)
	scenarioFilter := fs.String("scenario", "", "comma-separated scenario names (default: all)")
	reportDir := fs.String("report-dir", "", "override report directory")
	claudeBin := fs.String("claude", "", "path to claude binary (default: search PATH)")
	scenariosRoot := fs.String("scenarios-root", "", "path to testdata/e2e-scenarios/ (default: <repo>/testdata/e2e-scenarios)")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-scenario wall-time cap (includes agent + validate)")
	listOnly := fs.Bool("list", false, "list available scenarios and exit")
	keep := fs.Bool("keep", false, "keep per-scenario sandbox dirs even on pass")
	stopOnFail := fs.Bool("stop", false, "stop after first failure")
	verbose := fs.Bool("v", false, "verbose progress to stdout")
	mode := fs.String("mode", "elonco",
		"dispatcher: 'elonco' (full-stack: arcmux daemon + elonco service + arcmux-spawned agent) or 'direct' (plain claude -p)")
	arcmuxBin := fs.String("arcmux", "", "path to arcmux daemon binary (default: <repo>/bin/arcmux; required for --mode=elonco)")
	elonkoPython := fs.String("elonco-python", "",
		"python interpreter to run elonco with (default: /Users/blin/Projects/elonco/.venv/bin/python, else python3)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	all := []e2e.Scenario{
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
		*scenariosRoot = filepath.Join(repoRoot, "testdata", "e2e-scenarios")
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

	resolvedMode := strings.TrimSpace(*mode)
	if resolvedMode == "" {
		resolvedMode = "elonco"
	}
	if resolvedMode != "direct" && resolvedMode != "elonco" {
		return fmt.Errorf("--mode must be 'direct' or 'elonco' (got %q)", resolvedMode)
	}

	resolvedArcmux := *arcmuxBin
	resolvedElonko := *elonkoPython
	if resolvedMode == "elonco" {
		if resolvedArcmux == "" {
			candidate := filepath.Join(repoRoot, "bin", "arcmux")
			if _, statErr := os.Stat(candidate); statErr != nil {
				if p, lookErr := exec.LookPath("arcmux"); lookErr == nil {
					candidate = p
				} else {
					return fmt.Errorf("arcmux binary not found at %s and not on PATH (--arcmux <path> to override, or `make build`)", candidate)
				}
			}
			resolvedArcmux = candidate
		}
		if resolvedElonko == "" {
			candidates := []string{
				"/Users/blin/Projects/elonco/.venv/bin/python",
				"/Users/blin/Projects/elonco/.venv/bin/python3",
			}
			for _, c := range candidates {
				if _, statErr := os.Stat(c); statErr == nil {
					resolvedElonko = c
					break
				}
			}
			if resolvedElonko == "" {
				if p, lookErr := exec.LookPath("python3"); lookErr == nil {
					resolvedElonko = p
				} else {
					return fmt.Errorf("python3 not found on PATH (--elonco-python <path> to override)")
				}
			}
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := &e2e.Runner{
		Scenarios:       selected,
		ScenariosRoot:   *scenariosRoot,
		ReportDir:       *reportDir,
		Verbose:         *verbose,
		ScenarioTimeout: *timeout,
		StopOnFail:      *stopOnFail,
		KeepArtifs:      *keep,
		ClaudeBin:       resolvedClaude,
		RepoRoot:        repoRoot,
		Mode:            resolvedMode,
		ArcmuxBin:       resolvedArcmux,
		ElonkoPython:    resolvedElonko,
	}
	return r.Run(ctx, os.Stdout)
}

// resolveReportDir mirrors arcmux-test: prefer $ARCMUX_EPHEMERAL when set,
// fall back to <repo>/.validate-reports otherwise. Reports land in a
// single directory alongside structural + substrate reports for one-stop
// audit.
func resolveReportDir(repoRoot string) string {
	if e := os.Getenv("ARCMUX_EPHEMERAL"); e != "" {
		return filepath.Join(e, "validate-reports")
	}
	return filepath.Join(repoRoot, ".validate-reports")
}
