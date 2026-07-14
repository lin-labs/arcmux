package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	case "profiles":
		return cmdProfiles(args[1:], os.Stdout)
	case "mesh":
		return cmdMesh(args[1:], os.Stdin, os.Stdout)
	case "artifact":
		return cmdArtifact(args[1:], os.Stdout)
	case "surface":
		return cmdSurface(args[1:], os.Stdout)
	case "hook-env":
		return cmdHookEnv(args[1:], os.Stdout)
	case "hook":
		return cmdHook(args[1:])
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
	socketPath := ""
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--config" || args[i] == "-c" {
			configPath = args[i+1]
		}
		if args[i] == "--socket-path" {
			socketPath = args[i+1]
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if socketPath != "" {
		cfg.Daemon.Socket = socketPath
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
  arcmux start [--config path] [--socket-path path]        Start the daemon (default command — also runs the pulse supervisor)
  arcmux profiles list                                    List profile registry entries
  arcmux profiles create <name>                           Register a profile and ask the daemon to listen on its socket
  arcmux profiles remove <name> [--purge]                 Stop listening and remove the profile registry entry
  arcmux profiles purge <name>                            Stop listening, remove registry entry, and delete profile state
  arcmux info [--config path] [--json]                    Print daemon-process introspection (PID, socket, bolt path, session count, uptime)
  arcmux mesh status [--json]                             Show peer connectivity without exposing credentials
  arcmux mesh ping <peer>                                 Check a connected peer
  arcmux mesh serve <peer> --url <url> [--output file]    Create a one-time pairing invite
  arcmux mesh join <invite-file|->                        Join from a 0600 file or stdin
  arcmux mesh grant <peer> [scopes...]                    Allow explicit read-only application access
  arcmux mesh revoke <peer>                               Return a peer to transport-only access
  arcmux mesh sessions <peer> [--profile scope]           Synchronize and list safe remote sessions
  arcmux mesh session <peer> <scope> <session-id>         Show one safe remote session projection
  arcmux mesh artifacts <peer> [--kind kind]              Synchronize and list remote artifact references
  arcmux mesh artifact <peer> <kind> <source-id>          Fetch one live remote artifact reference
  arcmux mesh subscribe <peer> [topics...]                Subscribe to typed session/artifact updates
  arcmux artifact record --kind K --id ID [metadata]      Record a local artifact reference
  arcmux artifact list [--kind kind]                      List local and synchronized artifact references
  arcmux surface bind <device> <scope> <session-id>       Bind this cmux surface to an exact remote session
  arcmux surface show|list|unbind                         Inspect or remove durable cmux bindings
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

func cmdProfiles(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: arcmux profiles list|create|remove|purge")
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return profileHTTPJSON(cfg, "/profiles", stdout, true)
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("profiles create <name>")
		}
		return profileHTTPJSON(cfg, "/profiles/create?name="+url.QueryEscape(args[1]), stdout, false)
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("profiles remove <name> [--purge]")
		}
		purge := ""
		if len(args) > 2 && args[2] == "--purge" {
			purge = "&purge=1"
		}
		return profileHTTPJSON(cfg, "/profiles/remove?name="+url.QueryEscape(args[1])+purge, stdout, false)
	case "purge":
		if len(args) < 2 {
			return fmt.Errorf("profiles purge <name>")
		}
		return profileHTTPJSON(cfg, "/profiles/remove?name="+url.QueryEscape(args[1])+"&purge=1", stdout, false)
	default:
		return fmt.Errorf("unknown profiles subcommand %q", args[0])
	}
}

func profileHTTPJSON(cfg *config.Config, path string, stdout io.Writer, allowOfflineList bool) error {
	if cfg.Daemon.HTTPAddr == "" {
		return fmt.Errorf("daemon http_addr is disabled")
	}
	var resp *http.Response
	var err error
	if path == "/profiles" {
		resp, err = http.Get("http://" + cfg.Daemon.HTTPAddr + path)
	} else {
		resp, err = http.Post("http://"+cfg.Daemon.HTTPAddr+path, "application/json", nil)
	}
	if err != nil {
		if allowOfflineList {
			return printProfileRegistryFile(stdout)
		}
		return fmt.Errorf("daemon profile API unavailable at %s: %w", cfg.Daemon.HTTPAddr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("profile API %s: %s", resp.Status, string(body))
	}
	_, err = stdout.Write(body)
	return err
}

func printProfileRegistryFile(stdout io.Writer) error {
	path, err := daemon.DefaultProfileRegistryPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte(`{"profiles":{}}` + "\n")
		} else {
			return err
		}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(v)
}
