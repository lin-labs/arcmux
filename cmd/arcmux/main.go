package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/daemon"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "arcmux: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		args = []string{"start"}
	}

	switch args[0] {
	case "start":
		return cmdStart(args[1:])
	case "manager":
		// C4 removed the `arcmux manager` launcher: arcmux is now a pure
		// substrate librarian and no longer owns the agent-class roles
		// (Elon / Manager / IC) that the subcommand booted. Project
		// registration moved to elonco's launcher, which calls
		// `manager.RegisterSession` directly.
		return fmt.Errorf("'arcmux manager' was removed in the pure-substrate refactor; use elonco's launcher (it calls arcmux's RegisterSession directly)")
	case "pulse":
		return cmdPulse(args[1:])
	case "info", "status":
		return cmdInfo(args[1:], os.Stdout)
	case "version":
		fmt.Printf("arcmux %s\n", version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s (try 'arcmux help')", args[0])
	}
}

func cmdStart(args []string) error {
	configPath := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--config" || args[i] == "-c" {
			configPath = args[i+1]
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Set up structured logging
	logLevel := slog.LevelInfo
	logHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	logger := slog.New(logHandler)

	d := daemon.New(cfg, logger)

	// Ignore SIGHUP so backgrounded daemon doesn't die when shell exits
	signal.Ignore(syscall.SIGHUP)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "arcmux v%s listening on %s\n", version, cfg.Daemon.Socket)
	fmt.Fprintf(os.Stderr, "tmux socket: %s (use 'tmux -L %s attach' to observe)\n",
		cfg.Tmux.SocketName, cfg.Tmux.SocketName)

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\nshutting down...")
	d.Stop()
	return nil
}

func printUsage() {
	fmt.Print(`arcmux — Agent Tmux Runtime Service (pure substrate)

Usage:
  arcmux start [--config path]                            Start the daemon (default command — also runs the pulse supervisor)
  arcmux info [--config path] [--json]                    Print daemon-process introspection (PID, socket, bolt path, session count, uptime)
  arcmux pulse --project <slug> [--interval 10s] [--once] Debug-only: pulse one project (the daemon does this for all projects)
  arcmux version                                          Print version
  arcmux help                                             Show this help

The daemon listens on a Unix socket for gRPC requests. Orchestrators
(elonco, etc.) connect to manage coding agent sessions and to register
new projects via the in-process RegisterSession API. Project launch is
NOT an arcmux concern post-C4 — the role-aware 'arcmux manager' launcher
was removed.

Pulse runtime: the daemon auto-discovers every project under
<pulse.data_root>/arcmux/*/state.bolt and runs one pulser per project.
Cadence is one uniform per-target interval (configurable via
~/.config/arcmux/config.toml under [pulse.cadence]).

Configuration: ~/.config/arcmux/config.toml
Socket: ~/.config/arcmux/arcmux.sock (configurable)
tmux server: tmux -L arcmux (isolated)

Observe agent panes:
  tmux -L arcmux attach

`)
}
