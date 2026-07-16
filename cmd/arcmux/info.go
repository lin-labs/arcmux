package main

// info.go implements `arcmux info` — daemon-process introspection. Unlike
// `arcmux-cli status [<session_id>]`, which is per-session and requires the
// daemon to be reachable, `arcmux info` answers operational meta-questions:
//   - where is the socket / bolt / log?
//   - is the daemon running, and what PID?
//   - how long has it been up?
//   - how many sessions does it think it owns?
//
// All fields degrade gracefully: an unreachable daemon prints "n/a" for the
// runtime fields and still emits the resolved filesystem paths, because the
// paths come from the config layer (no daemon round-trip needed).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/mesh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// daemonInfo is the JSON shape `arcmux info --json` prints. Field order in
// the struct also drives the human-readable layout (info.go renders one
// "key: value" line per field, in declaration order, when --json is off).
type daemonInfo struct {
	DeviceID     string `json:"device_id"`
	SocketPath   string `json:"socket_path"`
	HTTPAddr     string `json:"http_addr"`
	DataRoot     string `json:"data_root"`
	BoltPath     string `json:"bolt_path"`
	LogDir       string `json:"log_dir"`
	TmuxSocket   string `json:"tmux_socket"`
	HookOutput   string `json:"hook_output_dir"`
	DaemonPID    int    `json:"daemon_pid"`
	Running      bool   `json:"running"`
	UptimeSec    int64  `json:"uptime_sec,omitempty"`
	SessionCount int    `json:"session_count"`
	SessionError string `json:"session_error,omitempty"`
	Version      string `json:"version"`
}

// cmdInfo is wired from main's switch; --config overrides the default config
// path the daemon would read. --json emits machine-readable output for
// scripts; bare invocation prints the human-friendly form.
func cmdInfo(args []string, stdout io.Writer) error {
	configPath := ""
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case "--json":
			asJSON = true
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	info := daemonInfo{
		DeviceID:   configuredMeshDeviceID(cfg),
		SocketPath: cfg.Daemon.Socket,
		HTTPAddr:   cfg.Daemon.HTTPAddr,
		DataRoot:   cfg.Pulse.DataRoot,
		BoltPath:   filepath.Join(cfg.Pulse.DataRoot, "arcmux", "_daemon", "state.bolt"),
		LogDir:     cfg.Daemon.LogDir,
		TmuxSocket: cfg.Tmux.SocketName,
		HookOutput: cfg.Hooks.HookOutputDir,
		Version:    version,
	}

	// PID + uptime via `pgrep -x arcmux`. -x for exact match so a child
	// `arcmux-cli` process doesn't get reported as the daemon.
	if pid := pgrepArcmux(); pid > 0 {
		info.DaemonPID = pid
		info.Running = true
		if u := processUptimeSeconds(pid); u > 0 {
			info.UptimeSec = u
		}
	}

	// Session count via gRPC ListSessions — best-effort. If the daemon
	// isn't running (or the socket is stale), record the error and keep
	// going; the path/PID fields are still useful on their own.
	if info.Running {
		count, err := listSessionCount(cfg.Daemon.Socket)
		if err != nil {
			info.SessionError = err.Error()
		} else {
			info.SessionCount = count
		}
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	return renderInfo(stdout, info)
}

func renderInfo(w io.Writer, info daemonInfo) error {
	state := "not running"
	if info.Running {
		state = fmt.Sprintf("running (pid %d", info.DaemonPID)
		if info.UptimeSec > 0 {
			state += ", uptime " + humanizeDuration(time.Duration(info.UptimeSec)*time.Second)
		}
		state += ")"
	}
	sess := strconv.Itoa(info.SessionCount)
	if info.SessionError != "" {
		sess = "n/a (" + info.SessionError + ")"
	}
	lines := []struct {
		key string
		val string
	}{
		{"version", info.Version},
		{"device id", info.DeviceID},
		{"state", state},
		{"socket", info.SocketPath},
		{"http addr", info.HTTPAddr},
		{"data root", info.DataRoot},
		{"bolt path", info.BoltPath},
		{"log dir", info.LogDir},
		{"tmux socket", info.TmuxSocket},
		{"hook output", info.HookOutput},
		{"sessions", sess},
	}
	width := 0
	for _, l := range lines {
		if n := len(l.key); n > width {
			width = n
		}
	}
	for _, l := range lines {
		fmt.Fprintf(w, "%-*s  %s\n", width, l.key, l.val)
	}
	return nil
}

// configuredMeshDeviceID reads the owner-only machine registry that is also
// authoritative for daemon mesh reloads. It intentionally does not infer an
// identity from a surface binding or peer: first-ever supervised dispatch must
// work before either exists. Info remains best-effort if mesh is unconfigured
// or its local registry needs repair.
func configuredMeshDeviceID(cfg *config.Config) string {
	parsed, err := cfg.Mesh.Parse()
	if err != nil {
		return ""
	}
	registry, err := mesh.LoadRegistry(parsed.RegistryPath)
	if err != nil {
		return ""
	}
	return registry.DeviceID
}

// pgrepArcmux returns the PID of a running `arcmux` daemon, or 0 if none.
// We rely on `pgrep -x arcmux` because (a) it's cross-Unix portable enough
// for the targets we care about (macOS + Linux) and (b) it lets us avoid
// parsing `ps` output by hand. The daemon's own /proc would be more
// authoritative on Linux, but we want this to work on macOS too.
func pgrepArcmux() int {
	out, err := exec.Command("pgrep", "-x", "arcmux").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// pgrep can return multiple PIDs (rare for us — the daemon
		// holds a Unix socket; only one can listen at a time). Take
		// the first one; the rest are stragglers and `info` is
		// advisory anyway.
		if pid, err := strconv.Atoi(line); err == nil && pid != os.Getpid() {
			return pid
		}
	}
	return 0
}

// processUptimeSeconds returns how long the given PID has been alive, in
// seconds. Best-effort across macOS and Linux:
//   - macOS: `ps -o lstart= -p <pid>` → "Sat May 24 09:12:33 2026", parse + diff.
//   - Linux: `ps -o etimes= -p <pid>` → integer seconds. The `etimes` column
//     is on most modern coreutils ps; if it's missing we just return 0.
//
// Returns 0 if the process is gone, ps doesn't speak the column, or
// parsing fails. Callers must treat 0 as "unknown".
func processUptimeSeconds(pid int) int64 {
	// Try Linux-style etimes first — fastest, integer answer.
	if out, err := exec.Command("ps", "-o", "etimes=", "-p", strconv.Itoa(pid)).Output(); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	// Fall back to lstart (macOS path).
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return int64(time.Since(t).Seconds())
}

// listSessionCount dials the daemon over its Unix socket and returns the
// number of in-memory sessions ListSessions reports. Uses a short timeout
// because `arcmux info` should never block.
func listSessionCount(socketPath string) (int, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	c := arcmuxv1.NewAgentRuntimeClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := c.ListSessions(ctx, &arcmuxv1.ListSessionsRequest{})
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}
	return len(r.Sessions), nil
}

// humanizeDuration prints a duration as e.g. "3h12m" or "47s". Keeps the
// `arcmux info` output compact instead of dumping "3h12m4.521s".
func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
