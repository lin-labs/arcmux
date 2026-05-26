// arcmux-e2e is the behavioral end-to-end test harness for arcmux.
//
// Where scripts/validate.sh proves the binaries COMPILE and the wire
// edges respond, this binary proves the substrate actually DOES the
// right thing when a human-style sequence of commands is issued.
//
// Each scenario runs in a fully isolated temp dir (unique data_root,
// vault_root, daemon socket, tmux socket, cmux workspace prefix) so a
// failure in one cannot cascade. Teardown is guaranteed even on
// assertion failure. A JSON report is written under
// $ARCMUX_EPHEMERAL/validate-reports/ mirroring validate.sh's shape.
//
// Usage:
//
//	bin/arcmux-e2e                                # all scenarios
//	bin/arcmux-e2e --scenario bootstrap            # one scenario by name
//	bin/arcmux-e2e --scenario bootstrap,pulse-wake # subset
//	bin/arcmux-e2e --list                          # print scenario names
//	bin/arcmux-e2e --keep                          # keep temp dirs after pass
//	bin/arcmux-e2e --report-dir <path>             # override report dir
//	bin/arcmux-e2e --bin <path> --call <path>      # override binary paths
//
// Exit 0 only if every selected scenario passed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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
	bin := fs.String("bin", "", "path to arcmux binary (default: ./bin/arcmux)")
	callBin := fs.String("call", "", "path to arcmux-cli binary (default: ./bin/arcmux-cli)")
	timeout := fs.Duration("timeout", 90*time.Second, "per-scenario timeout")
	listOnly := fs.Bool("list", false, "list available scenarios and exit")
	keep := fs.Bool("keep", false, "keep per-scenario temp dirs even on pass")
	stopOnFail := fs.Bool("stop", false, "stop after first failure")
	verbose := fs.Bool("v", false, "verbose progress to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	all := []e2e.Scenario{
		scenarios.Bootstrap{},
		scenarios.PulseWake{},
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
	if *bin == "" {
		*bin = filepath.Join(repoRoot, "bin", "arcmux")
	}
	if *callBin == "" {
		*callBin = filepath.Join(repoRoot, "bin", "arcmux-cli")
	}
	if *reportDir == "" {
		*reportDir = resolveReportDir(repoRoot)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := &e2e.Runner{
		Scenarios:  selected,
		ReportDir:  *reportDir,
		Verbose:    *verbose,
		Timeout:    *timeout,
		StopOnFail: *stopOnFail,
		KeepArtifs: *keep,
		ArcmuxBin:  *bin,
		CallBin:    *callBin,
		RepoRoot:   repoRoot,
	}
	return r.Run(ctx, os.Stdout)
}

// resolveReportDir mirrors validate.sh's logic: prefer $ARCMUX_EPHEMERAL
// when set, fall back to <repo>/.validate-reports otherwise. Reports
// land in validate-reports/ so a single dir holds both structural and
// e2e evidence.
func resolveReportDir(repoRoot string) string {
	if e := os.Getenv("ARCMUX_EPHEMERAL"); e != "" {
		return filepath.Join(e, "validate-reports")
	}
	return filepath.Join(repoRoot, ".validate-reports")
}
