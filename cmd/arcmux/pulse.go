package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lin-labs/arcmux/internal/manager"
	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/pulse"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
)

// cmdPulse is a DEBUG SHIM. The canonical pulse runtime now lives inside
// the arcmux daemon (`arcmux start` → internal/daemon.PulseSupervisor)
// which auto-discovers every project under `<data_root>/arcmux/*/state.bolt`
// and runs one Pulser per project with cadences from
// `[pulse]` in `~/.config/arcmux/config.toml`.
//
// This subcommand is kept for two narrow uses:
//
//	--once   run one Tick against a specific project and print the report
//	         (great for cron-style drivers, smoke tests, debugging).
//	default  run a per-project loop in the foreground; useful when the
//	         daemon is intentionally stopped and you want a one-off
//	         pulse against a single project (e.g. local debugging).
//
// IMPORTANT: while the arcmux daemon is running, it already holds the
// state.bolt lock for this project. Invoking `arcmux pulse --project <slug>`
// at the same time will block on the file lock. Stop the daemon first
// (or use `--once` and rely on the daemon's own audit log) when debugging.
func cmdPulse(args []string) error {
	fs := flag.NewFlagSet("pulse", flag.ContinueOnError)
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (defaults to $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", os.Getenv("ARCMUX_DATA"), "data root (default $ARCMUX_DATA)")
	vaultRoot := fs.String("vault-root", os.Getenv("ARCMUX_VAULT"), "vault root (default $ARCMUX_VAULT or $OBS_AGENTS)")
	interval := fs.Duration("interval", 30*time.Second, "tick interval")
	once := fs.Bool("once", false, "run one tick and exit")
	verbose := fs.Bool("v", false, "verbose decision logging")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return errors.New("pulse: --project required (or set $ARCMUX_PROJECT)")
	}
	if *vaultRoot == "" {
		*vaultRoot = os.Getenv("OBS_AGENTS")
	}
	if *vaultRoot == "" {
		return errors.New("pulse: --vault-root required (or set $ARCMUX_VAULT or $OBS_AGENTS)")
	}

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	p, err := manager.Open(ctx, manager.OpenOptions{
		Project:   *project,
		DataRoot:  *dataRoot,
		VaultRoot: *vaultRoot,
	})
	if err != nil {
		return fmt.Errorf("open project %q: %w", *project, err)
	}
	defer p.Close()

	// Debug shim: default to the cmux backend. Production callers use the
	// daemon's PulseSupervisor, which honors [mux] backend config.
	backend := cmuxbackend.New(cmuxcli.New())
	pp := pulse.New(*project, p.DB, backend)
	pp.Log = logger

	if *once {
		rep, err := pp.Tick(ctx)
		if err != nil {
			return fmt.Errorf("tick: %w", err)
		}
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(out))
		return nil
	}

	fmt.Fprintf(os.Stderr, "arcmux pulse: project=%s interval=%s cadence=%s\n",
		*project, *interval, pp.Cadence.Interval)

	if err := pp.Run(ctx, *interval); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Fprintln(os.Stderr, "arcmux pulse: shutdown clean")
	return nil
}
