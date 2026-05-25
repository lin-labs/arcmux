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
	"github.com/lin-labs/arcmux/internal/manager/pulse"
)

// cmdPulse runs the per-project wake loop. One process per project so the
// bbolt write lock (held by store.Open) stays project-local.
//
// Modes:
//
//	--once   run one Tick and exit (smoke + cron-style driver)
//	default  run forever, ticking every --interval, until SIGINT/SIGTERM
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

	pp := pulse.New(*project, p.DB, p.Opts.Cmux)
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

	fmt.Fprintf(os.Stderr, "arcmux pulse: project=%s interval=%s elon=%s manager=%s ic=%s\n",
		*project, *interval, pp.Cadence.Elon, pp.Cadence.Manager, pp.Cadence.IC)

	if err := pp.Run(ctx, *interval); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Fprintln(os.Stderr, "arcmux pulse: shutdown clean")
	return nil
}
