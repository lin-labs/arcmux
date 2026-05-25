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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Env is one scenario's isolated test environment. It owns:
//   - a per-run temp dir under $TMPDIR/arcmux-e2e-<ts>-<scenario>/
//   - a data_root and vault_root under that temp dir
//   - a unique project slug ("test-e2e-<ts>-<scenario>") and cmux workspace
//     prefix ("e2e-<ts>-<scenario>-") so any leftover cmux workspaces are
//     trivially identifiable and reapable
//   - optional spawned arcmux daemon (unique socket, log dir, tmux socket
//     name, http addr=127.0.0.1:0 — never collides with a real daemon)
//   - a trace log capturing every command + assertion the scenario ran
//
// Scenarios use Env.Run* helpers to invoke arcmux/arcmux-call subprocesses
// inheriting the right ARCMUX_* env. Teardown is best-effort and idempotent.
type Env struct {
	Scenario        string
	TempRoot        string
	DataRoot        string
	VaultRoot       string
	ConfigPath      string
	SocketPath      string
	HTTPAddr        string
	TmuxSocket      string
	DaemonLogPath   string
	TracePath       string
	WorkspacePrefix string // "e2e-<ts>-<scenario>-"
	ProjectSlug     string // "test-e2e-<ts>-<scenario>"
	ArcmuxBin       string
	CallBin         string
	BaseEnv         []string

	trace    *os.File
	daemon   *exec.Cmd
	daemonOK bool
}

// NewEnv creates a fresh isolated Env. Caller must call Teardown.
func NewEnv(scenario, arcmuxBin, callBin string, baseEnv []string) (*Env, error) {
	if arcmuxBin == "" || callBin == "" {
		return nil, fmt.Errorf("NewEnv: arcmux/arcmux-call bin paths required")
	}
	if _, err := os.Stat(arcmuxBin); err != nil {
		return nil, fmt.Errorf("arcmux bin %q: %w", arcmuxBin, err)
	}
	if _, err := os.Stat(callBin); err != nil {
		return nil, fmt.Errorf("arcmux-call bin %q: %w", callBin, err)
	}

	ts := time.Now().UnixNano()
	tag := fmt.Sprintf("%d-%s", ts, sanitizeForSlug(scenario))
	tempBase := os.TempDir()
	tempRoot := filepath.Join(tempBase, "arcmux-e2e-"+tag)
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir temp root: %w", err)
	}

	dataRoot := filepath.Join(tempRoot, "data")
	vaultRoot := filepath.Join(tempRoot, "vault")
	logDir := filepath.Join(tempRoot, "logs")
	for _, d := range []string{dataRoot, vaultRoot, logDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// Project slug must satisfy paths.Validate regex (alnum start, then
	// [A-Za-z0-9_.-]). The "test-e2e-" prefix makes it obvious in logs;
	// the timestamp guarantees uniqueness across runs.
	projectSlug := fmt.Sprintf("test-e2e-%d-%s", ts, sanitizeForSlug(scenario))
	// Slug must be <=64 chars. Truncate if needed but keep ts for uniqueness.
	if len(projectSlug) > 64 {
		projectSlug = projectSlug[:64]
	}

	wsPrefix := "e2e-" + tag + "-"

	socketPath := filepath.Join(tempRoot, "arcmux.sock")
	tmuxSocket := "arcmux-e2e-" + sanitizeForTmux(tag)
	configPath := filepath.Join(tempRoot, "config.toml")
	daemonLog := filepath.Join(logDir, "daemon.log")
	tracePath := filepath.Join(tempRoot, "trace.log")

	traceFile, err := os.Create(tracePath)
	if err != nil {
		return nil, fmt.Errorf("create trace: %w", err)
	}

	env := &Env{
		Scenario:        scenario,
		TempRoot:        tempRoot,
		DataRoot:        dataRoot,
		VaultRoot:       vaultRoot,
		ConfigPath:      configPath,
		SocketPath:      socketPath,
		HTTPAddr:        "127.0.0.1:0",
		TmuxSocket:      tmuxSocket,
		DaemonLogPath:   daemonLog,
		TracePath:       tracePath,
		WorkspacePrefix: wsPrefix,
		ProjectSlug:     projectSlug,
		ArcmuxBin:       arcmuxBin,
		CallBin:         callBin,
		BaseEnv:         baseEnv,
		trace:           traceFile,
	}
	env.tracef("=== e2e scenario=%s temp=%s ===", scenario, tempRoot)
	env.tracef("project_slug=%s workspace_prefix=%s tmux_socket=%s",
		projectSlug, wsPrefix, tmuxSocket)
	return env, nil
}

// TraceWriter returns the trace log writer for callers that want to stream
// command output into it directly.
func (e *Env) TraceWriter() io.Writer { return e.trace }

func (e *Env) tracef(f string, args ...any) {
	if e.trace == nil {
		return
	}
	fmt.Fprintf(e.trace, "["+time.Now().Format("15:04:05.000")+"] "+f+"\n", args...)
}

// WriteDaemonConfig materializes a minimal TOML pointing the daemon at our
// isolated socket/tmux/data_root and turning the pulse cadences down so
// wakes fire in seconds rather than tens of seconds.
func (e *Env) WriteDaemonConfig() error {
	body := fmt.Sprintf(`[daemon]
socket = %q
log_dir = %q
http_addr = %q

[tmux]
socket_name = %q
default_session = "agents"

[health]
capture_interval = "5s"
idle_timeout_default = "60s"
stuck_timeout_default = "5m"

[hooks]
auto_install = false

[pulse]
enabled = true
data_root = %q
interval = "1s"
discovery_interval = "2s"

[pulse.cadence]
elon = "1s"
manager = "1s"
ic = "1s"
`, e.SocketPath, filepath.Join(e.TempRoot, "logs"), e.HTTPAddr, e.TmuxSocket, e.DataRoot)
	if err := os.WriteFile(e.ConfigPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write daemon config: %w", err)
	}
	e.tracef("wrote daemon config %s", e.ConfigPath)
	return nil
}

// StartDaemon spawns `arcmux start --config <config>` and waits for the
// socket to appear (proxy for "daemon is up"). Returns once the socket is
// listening or ctx deadline elapses.
func (e *Env) StartDaemon(ctx context.Context, ready time.Duration) error {
	if err := e.WriteDaemonConfig(); err != nil {
		return err
	}
	logFile, err := os.Create(e.DaemonLogPath)
	if err != nil {
		return fmt.Errorf("create daemon log: %w", err)
	}
	cmd := exec.CommandContext(ctx, e.ArcmuxBin, "start", "--config", e.ConfigPath)
	cmd.Env = append(e.envBase(), "ARCMUX_E2E=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// New process group so SIGTERM goes to the daemon only, not us.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	e.daemon = cmd
	e.tracef("daemon started pid=%d log=%s", cmd.Process.Pid, e.DaemonLogPath)

	// Wait for the socket file to appear, or until ready elapses.
	deadline := time.Now().Add(ready)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(e.SocketPath); err == nil {
			e.daemonOK = true
			e.tracef("daemon socket ready after %s", time.Since(deadline.Add(-ready)))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon socket %s did not appear within %s (see %s)",
		e.SocketPath, ready, e.DaemonLogPath)
}

// StopDaemon sends SIGTERM and waits up to timeout for the process to exit.
// SIGKILL if it doesn't. Idempotent.
func (e *Env) StopDaemon(timeout time.Duration) {
	if e.daemon == nil || e.daemon.Process == nil {
		return
	}
	pid := e.daemon.Process.Pid
	_ = e.daemon.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- e.daemon.Wait() }()
	select {
	case err := <-done:
		e.tracef("daemon pid=%d exited: %v", pid, err)
	case <-time.After(timeout):
		_ = e.daemon.Process.Kill()
		<-done
		e.tracef("daemon pid=%d SIGKILL after %s timeout", pid, timeout)
	}
	e.daemon = nil
	e.daemonOK = false
}

// Teardown closes the daemon, reaps cmux workspaces matching our prefix,
// kills our private tmux server, and (optionally) removes the temp dir.
// Always safe to call; errors are written to the trace but never returned.
func (e *Env) Teardown(stdout io.Writer, keepArtifacts bool) {
	e.tracef("=== teardown ===")
	e.StopDaemon(5 * time.Second)
	if err := e.cleanupCmuxWorkspaces(); err != nil {
		e.tracef("cmux cleanup: %v", err)
	}
	e.killPrivateTmux()
	if e.trace != nil {
		_ = e.trace.Close()
		e.trace = nil
	}
	if keepArtifacts {
		fmt.Fprintf(stdout, "    artifacts kept: %s\n", e.TempRoot)
		return
	}
	if err := os.RemoveAll(e.TempRoot); err != nil {
		fmt.Fprintf(stdout, "    teardown: rm %s: %v\n", e.TempRoot, err)
	}
}

// cleanupCmuxWorkspaces finds workspaces whose names start with our
// prefix and closes them via `cmux close-workspace --workspace <ref>`.
// Best-effort: failures are logged, not returned.
func (e *Env) cleanupCmuxWorkspaces() error {
	out, err := exec.Command("cmux", "list-workspaces").Output()
	if err != nil {
		// cmux not installed / not running — nothing to clean.
		return nil
	}
	var closed []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Line shape: "workspace:N  <name>"
		// We match on either "elon: <project>" / "team: <slug>" carrying our
		// project slug, OR any workspace whose name contains the prefix.
		if !strings.Contains(line, e.ProjectSlug) && !strings.Contains(line, e.WorkspacePrefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ref := fields[0]
		if cerr := exec.Command("cmux", "close-workspace", "--workspace", ref).Run(); cerr != nil {
			e.tracef("close-workspace %s: %v", ref, cerr)
			continue
		}
		closed = append(closed, ref)
	}
	if len(closed) > 0 {
		e.tracef("closed cmux workspaces: %v", closed)
	}
	return nil
}

// killPrivateTmux best-effort kills the per-run tmux server (-L <socket>).
// Failures (server already gone) are silent.
func (e *Env) killPrivateTmux() {
	_ = exec.Command("tmux", "-L", e.TmuxSocket, "kill-server").Run()
}

// envBase returns the OS env for spawned subprocesses, with the
// ARCMUX_* vars set so child commands automatically target our temp
// data_root / vault_root.
func (e *Env) envBase() []string {
	base := e.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	// Strip ARCMUX_* vars from the inherited env so the parent's values
	// don't leak in.
	filtered := make([]string, 0, len(base)+8)
	for _, kv := range base {
		if strings.HasPrefix(kv, "ARCMUX_") {
			continue
		}
		filtered = append(filtered, kv)
	}
	filtered = append(filtered,
		"ARCMUX_PROJECT="+e.ProjectSlug,
		"ARCMUX_DATA="+e.DataRoot,
		"ARCMUX_VAULT="+e.VaultRoot,
		"OBS_AGENTS="+e.VaultRoot,
		"ARCMUX_AGENT=claude",
	)
	return filtered
}

// RunCall invokes `arcmux-call <args...>` with our isolated env. Returns
// stdout, exit error (nil on success).
func (e *Env) RunCall(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.CallBin, args...)
	cmd.Env = e.envBase()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	e.tracef("$ arcmux-call %s", strings.Join(args, " "))
	err := cmd.Run()
	if err != nil {
		e.tracef("  err: %v stderr=%q", err, stderr.String())
		return stdout.Bytes(), fmt.Errorf("arcmux-call %v: %w (stderr=%s)", args, err, strings.TrimSpace(stderr.String()))
	}
	e.tracef("  ok: %d bytes stdout", stdout.Len())
	return stdout.Bytes(), nil
}

// RunCallJSON unmarshals arcmux-call stdout into v.
func (e *Env) RunCallJSON(ctx context.Context, v any, args ...string) error {
	out, err := e.RunCall(ctx, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode arcmux-call %v output: %w", args, err)
	}
	return nil
}

// RunArcmux invokes `arcmux <args...>` (the daemon binary in non-start
// subcommand modes — e.g. `arcmux manager`). Returns stdout + exit error.
func (e *Env) RunArcmux(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.ArcmuxBin, args...)
	cmd.Env = e.envBase()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	e.tracef("$ arcmux %s", strings.Join(args, " "))
	err := cmd.Run()
	if err != nil {
		e.tracef("  err: %v stderr=%q", err, strings.TrimSpace(stderr.String()))
		return stdout.Bytes(), fmt.Errorf("arcmux %v: %w (stderr=%s)", args, err, strings.TrimSpace(stderr.String()))
	}
	e.tracef("  ok: %d bytes stdout", stdout.Len())
	return stdout.Bytes(), nil
}

// AwaitFn polls fn every step until it returns nil or deadline elapses.
// The last error is returned on timeout. Useful for "wait for daemon to
// write an audit row".
func AwaitFn(ctx context.Context, total, step time.Duration, fn func() error) error {
	deadline := time.Now().Add(total)
	var lastErr error
	for {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("timeout after %s", total)
			}
			return fmt.Errorf("await: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(step):
		}
	}
}

// sanitizeForSlug keeps alnum + dashes for project slug compatibility.
func sanitizeForSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "scenario"
	}
	return out
}

// sanitizeForTmux is just sanitizeForSlug with no leading-dash worry —
// tmux socket names can have any printable ASCII, but we stick to slug-
// safe to keep grep simple.
func sanitizeForTmux(s string) string { return sanitizeForSlug(s) }

// FormatPaneRef returns a placeholder pane ref like "pane:99000+<n>" that
// the pulse loop will treat as a valid (but missing) cmux target —
// triggering pulse.wake.error rather than a panic. Numbers high enough
// that they won't collide with real cmux panes in normal use.
func FormatPaneRef(n int) string { return "pane:" + strconv.Itoa(99000+n) }
