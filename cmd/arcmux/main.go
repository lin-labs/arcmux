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
	"github.com/lin-labs/arcmux/internal/manager"
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
		return manager.CmdManager(context.Background(), args[1:], os.Stdout)
	case "pulse":
		return cmdPulse(args[1:])
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
	fmt.Print(`arcmux — Agent Tmux Runtime Service

Usage:
  arcmux start [--config path]                            Start the daemon (default command)
  arcmux manager <agent> <project> [--mission "..."]      Launch three-tier manager mode (Elon+Manager+ICs in cmux)
  arcmux pulse --project <slug> [--interval 30s] [--once] Drive the per-project wake loop
  arcmux version                                          Print version
  arcmux help                                             Show this help

The daemon listens on a Unix socket for gRPC requests.
Orchestrators connect to manage coding agent sessions.

Configuration: ~/.config/arcmux/config.toml
Socket: ~/.config/arcmux/arcmux.sock (configurable)
tmux server: tmux -L arcmux (isolated)

Observe agent panes:
  tmux -L arcmux attach

`)
}
