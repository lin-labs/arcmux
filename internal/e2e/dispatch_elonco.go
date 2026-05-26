package e2e

// dispatch_elonco.go — full-stack dispatcher.
//
// Where DispatchDirect spawns `claude -p` in-process and waits for it to
// exit, this dispatcher proves the whole architecture works end-to-end:
//
//   1. Spawn an isolated arcmux daemon (Unix socket under TempRoot).
//   2. Spawn `python -m elonco serve` against a free TCP port.
//   3. POST {mission, cwd=WorkRepo} to elonco's /v1/spawn — elonco
//      asks arcmux to register a claude agent against the workrepo.
//   4. Poll elonco's GET /v1/session/<name>/ready until the agent
//      goes idle (== finished its task) for a stable window.
//   5. POST /v1/session/<session-id>/close to tear the agent down.
//   6. Teardown: SIGTERM elonco, SIGTERM daemon, kill tmux server,
//      remove temp root (unless --keep).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ElonkoDispatch holds the runtime state of one full-stack dispatch: the
// arcmux daemon subprocess + the elonco subprocess + the session handle
// elonco gave back. Used inside a Scenario.Run to orchestrate the flow and
// to ensure both subprocesses are torn down on exit.
type ElonkoDispatch struct {
	env *Env

	socketPath string // arcmux unix socket (also: gRPC target)
	tmuxSocket string // unique tmux -L value
	daemonLog  string
	daemonCfg  string
	dataRoot   string
	vaultRoot  string
	elonkoPort int
	elonkoLog  string
	elonkoID   string
	arcmuxAddr string // "unix:///<socketPath>" — passed to elonco for gRPC dial

	daemonProc *exec.Cmd
	elonkoProc *exec.Cmd

	SessionName string
	SessionID   string
	TmuxTarget  string
}

// NewElonkoDispatch sets up filesystem paths and picks a free port.
// It does NOT start anything yet. Caller must call Start.
func NewElonkoDispatch(env *Env) (*ElonkoDispatch, error) {
	if env == nil {
		return nil, fmt.Errorf("ElonkoDispatch: env required")
	}
	if env.ArcmuxBin == "" {
		return nil, fmt.Errorf("ElonkoDispatch: ArcmuxBin not set on env (elonco mode requires arcmux binary)")
	}
	if env.ElonkoPython == "" {
		return nil, fmt.Errorf("ElonkoDispatch: ElonkoPython not set on env")
	}

	// macOS caps AF_UNIX paths at ~104 bytes. $TMPDIR-under-/var/folders
	// blows past that, so keep the socket under /tmp.
	socketDir, err := os.MkdirTemp("/tmp", "arcmux-e2e-sock-")
	if err != nil {
		return nil, fmt.Errorf("mkdir socket dir: %w", err)
	}
	socketPath := filepath.Join(socketDir, "arcmux.sock")

	dataRoot := filepath.Join(env.TempRoot, "arcmux-data")
	vaultRoot := filepath.Join(env.TempRoot, "vault")
	daemonLog := filepath.Join(env.LogsDir, "daemon.log")
	daemonCfg := filepath.Join(env.TempRoot, "arcmux.toml")
	elonkoLog := filepath.Join(env.LogsDir, "elonco.log")
	for _, d := range []string{dataRoot, vaultRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick elonco port: %w", err)
	}

	// Tmux socket name + elon-id must be filesystem/tmux safe.
	suffix := sanitize(filepath.Base(env.TempRoot))
	tmuxSocket := "arcmux-e2e-" + suffix
	if len(tmuxSocket) > 60 {
		tmuxSocket = tmuxSocket[:60]
	}
	elonkoID := "e2e-" + suffix
	if len(elonkoID) > 60 {
		elonkoID = elonkoID[:60]
	}

	return &ElonkoDispatch{
		env:        env,
		socketPath: socketPath,
		tmuxSocket: tmuxSocket,
		daemonLog:  daemonLog,
		daemonCfg:  daemonCfg,
		dataRoot:   dataRoot,
		vaultRoot:  vaultRoot,
		elonkoPort: port,
		elonkoLog:  elonkoLog,
		elonkoID:   elonkoID,
		arcmuxAddr: "unix://" + socketPath,
	}, nil
}

// Start brings up both arcmux and elonco subprocesses. On error, partial
// state is torn down via Teardown (safe to call regardless).
//
// IMPORTANT: “procCtx“ is the long-lived context that owns the lifetime
// of the spawned subprocesses (their stdin/SIGKILL is wired to it via
// “exec.CommandContext“). “waitCtx“ is the short-lived deadline used
// only for polling readiness/health and may be cancelled the moment Start
// returns — it must NOT also gate the subprocesses, or cancelling it will
// kill arcmux/elonco immediately after startup. This split was a real
// foot-gun the first time around (the harness silently SIGKILLed both
// children right after /health came up, manifesting as a "connection
// reset by peer" on the very next request).
func (d *ElonkoDispatch) Start(procCtx, waitCtx context.Context) error {
	if err := d.writeArcmuxConfig(); err != nil {
		return err
	}
	if err := d.startArcmux(procCtx); err != nil {
		return fmt.Errorf("start arcmux: %w", err)
	}
	if err := d.waitForArcmuxSocket(waitCtx, 10*time.Second); err != nil {
		return fmt.Errorf("arcmux socket: %w", err)
	}
	if err := d.startElonko(procCtx); err != nil {
		return fmt.Errorf("start elonco: %w", err)
	}
	if err := d.waitForElonkoHealth(waitCtx, 15*time.Second); err != nil {
		return fmt.Errorf("elonco health: %w", err)
	}
	return nil
}

func (d *ElonkoDispatch) writeArcmuxConfig() error {
	body := fmt.Sprintf(`[daemon]
socket = %q
log_dir = %q
http_addr = ""

[mux]
backend = "tmux"

[tmux]
socket_name = %q
default_session = "agents"

[health]
capture_interval = "2s"
idle_timeout_default = "30s"
stuck_timeout_default = "5m"

[hooks]
auto_install = false

[pulse]
enabled = false
data_root = %q
interval = "10s"
discovery_interval = "60s"

[pulse.cadence]
interval = "30s"
`, d.socketPath, d.env.LogsDir, d.tmuxSocket, d.dataRoot)
	if err := os.WriteFile(d.daemonCfg, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write daemon config: %w", err)
	}
	d.env.Tracef("wrote arcmux config %s", d.daemonCfg)
	return nil
}

func (d *ElonkoDispatch) startArcmux(ctx context.Context) error {
	logFile, err := os.Create(d.daemonLog)
	if err != nil {
		return fmt.Errorf("create daemon log: %w", err)
	}
	cmd := exec.CommandContext(ctx, d.env.ArcmuxBin, "start", "--config", d.daemonCfg)
	// Daemon env: scrub ARCMUX_* and OAuth-conflicting vars; pass through PATH.
	cmd.Env = d.env.SpawnedEnv("ARCMUX_E2E=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("exec arcmux start: %w", err)
	}
	d.daemonProc = cmd
	d.env.Tracef("arcmux daemon started pid=%d socket=%s log=%s",
		cmd.Process.Pid, d.socketPath, d.daemonLog)
	return nil
}

func (d *ElonkoDispatch) waitForArcmuxSocket(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(d.socketPath); err == nil {
			c, err := net.DialTimeout("unix", d.socketPath, 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				d.env.Tracef("arcmux socket ready")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf("socket %s never became ready (see %s)", d.socketPath, d.daemonLog)
}

func (d *ElonkoDispatch) startElonko(ctx context.Context) error {
	logFile, err := os.Create(d.elonkoLog)
	if err != nil {
		return fmt.Errorf("create elonco log: %w", err)
	}
	// Use the configured python with `-m elonco serve`. Bind to 127.0.0.1
	// only. Use a per-run elon-id so registration.json lands in its own
	// dir under ~/data/elonco/<elon-id>/. log-level=warning keeps the log
	// readable; debug if the harness is set verbose.
	args := []string{
		"-u", // unbuffered stdout/stderr so the log file isn't empty on crash
		"-m", "elonco", "serve",
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", d.elonkoPort),
		"--elon-id", d.elonkoID,
		"--log-level", "info",
	}
	cmd := exec.CommandContext(ctx, d.env.ElonkoPython, args...)
	cmd.Env = d.env.SpawnedEnv(
		"OBS_AGENTS="+d.vaultRoot,
		"ELONCO_DATA_ROOT="+filepath.Join(d.env.TempRoot, "elonco-data"),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("exec elonco: %w", err)
	}
	d.elonkoProc = cmd
	d.env.Tracef("elonco started pid=%d port=%d log=%s",
		cmd.Process.Pid, d.elonkoPort, d.elonkoLog)
	return nil
}

func (d *ElonkoDispatch) waitForElonkoHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := d.url("/health")
	client := &http.Client{Timeout: 1 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				d.env.Tracef("elonco /health OK")
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("elonco /health not ready in %s (last: %v, log: %s)",
		timeout, lastErr, d.elonkoLog)
}

func (d *ElonkoDispatch) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", d.elonkoPort, path)
}

// Spawn calls elonco's POST /v1/spawn with the mission text and cwd =
// env.WorkRepo, pointing it at our isolated arcmux unix socket. On
// success populates d.SessionName/SessionID/TmuxTarget.
func (d *ElonkoDispatch) Spawn(ctx context.Context, mission string) error {
	// Use the "claude_exec" arcmux profile: claude runs as a one-shot
	// `claude -p ... --json` subprocess and streams events back. The
	// session transitions to StateIdle the moment the subprocess exits,
	// which gives us a clean WaitIdle signal without needing a real PTY
	// or the --remote-control handshake (which doesn't reliably init in
	// a headless background tmux pane on macOS).
	body := map[string]any{
		"mission":        mission,
		"cwd":            d.env.WorkRepo,
		"agent":          "claude_exec",
		"arcmux_address": d.arcmuxAddr,
		"auto_close":     false,
	}
	payload, _ := json.Marshal(body)
	d.env.Tracef("POST %s (mission=%dB cwd=%s arcmux=%s)",
		d.url("/v1/spawn"), len(mission), d.env.WorkRepo, d.arcmuxAddr)
	req, err := http.NewRequestWithContext(ctx, "POST", d.url("/v1/spawn"),
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build spawn request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST /v1/spawn: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		out, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("/v1/spawn status %d: %s", resp.StatusCode, truncate(string(out), 400))
	}
	var out struct {
		OwnerID     string `json:"owner_id"`
		SessionID   string `json:"session_id"`
		SessionName string `json:"session_name"`
		TmuxTarget  string `json:"tmux_target"`
		PID         int    `json:"pid"`
		State       string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode spawn response: %w", err)
	}
	d.SessionName = out.SessionName
	d.SessionID = out.SessionID
	d.TmuxTarget = out.TmuxTarget
	d.env.Tracef("spawn OK: session_name=%s session_id=%s tmux=%s pid=%d state=%s",
		out.SessionName, out.SessionID, out.TmuxTarget, out.PID, out.State)
	return nil
}

// WaitIdle polls GET /v1/session/<name>/ready until the agent has:
//
//  1. Been observed in a busy state (working/starting/handshaking) at
//     least once, AND
//  2. Then settled back to "idle" for `stableWindow` consecutive samples.
//
// The two-phase requirement matters for the exec transport, which
// initializes the session in StateIdle and only flips to Working once
// SendPrompt's background goroutine actually kicks off claude. Without
// the "must have been busy" gate, WaitIdle would return immediately on
// that initial idle blip and the harness would race the agent.
//
// Returns when stable-idle or the hard cap `maxWait` elapses.
func (d *ElonkoDispatch) WaitIdle(ctx context.Context, maxWait, stableWindow time.Duration) (time.Duration, error) {
	if d.SessionName == "" {
		return 0, fmt.Errorf("WaitIdle: no session spawned yet")
	}
	urlPath := fmt.Sprintf("/v1/session/%s/ready?arcmux_address=%s",
		d.SessionName, d.arcmuxAddr)
	client := &http.Client{Timeout: 3 * time.Second}
	started := time.Now()
	deadline := started.Add(maxWait)
	pollInterval := 2 * time.Second
	var firstIdleAt time.Time
	sawBusy := false
	lastState := ""
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", d.url(urlPath), nil)
		resp, err := client.Do(req)
		if err != nil {
			d.env.Tracef("ready poll err: %v", err)
		} else {
			var rs struct {
				Ready        bool   `json:"ready"`
				Reason       string `json:"reason"`
				LastSignalAt string `json:"last_signal_at"`
				State        string `json:"state"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
				d.env.Tracef("ready decode err: %v", err)
			} else {
				if rs.State != lastState {
					d.env.Tracef("ready: state=%s", rs.State)
					lastState = rs.State
				}
				switch rs.State {
				case "working", "starting", "handshaking":
					sawBusy = true
					firstIdleAt = time.Time{}
				case "idle":
					if !sawBusy {
						// initial idle blip before SendPrompt kicked in;
						// ignore — wait for the busy → idle round-trip.
					} else if firstIdleAt.IsZero() {
						firstIdleAt = time.Now()
					} else if time.Since(firstIdleAt) >= stableWindow {
						resp.Body.Close()
						elapsed := time.Since(started)
						d.env.Tracef("ready: idle stable for %s after total %s",
							stableWindow, elapsed)
						return elapsed, nil
					}
				case "exited":
					// Exec transport sets StateExited when the subprocess
					// finishes cleanly. Treat that as a strong "done"
					// signal — no need to wait for a stable window.
					resp.Body.Close()
					elapsed := time.Since(started)
					d.env.Tracef("ready: state=exited after total %s", elapsed)
					return elapsed, nil
				case "failed", "stuck":
					resp.Body.Close()
					return time.Since(started), fmt.Errorf("agent reached terminal state %q (reason=%q)",
						rs.State, rs.Reason)
				default:
					// Unknown / not-yet-classified — keep polling.
				}
			}
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return time.Since(started), ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return time.Since(started), fmt.Errorf("agent did not reach stable idle in %s (last state %q, log: %s)",
		maxWait, lastState, d.elonkoLog)
}

// CloseSession asks elonco to Kill the arcmux session. Best-effort.
func (d *ElonkoDispatch) CloseSession(ctx context.Context) {
	if d.SessionID == "" {
		return
	}
	urlPath := fmt.Sprintf("/v1/session/%s/close?arcmux_address=%s",
		d.SessionID, d.arcmuxAddr)
	req, _ := http.NewRequestWithContext(ctx, "POST", d.url(urlPath), nil)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		d.env.Tracef("close session err: %v", err)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	d.env.Tracef("close session status=%d body=%s", resp.StatusCode,
		truncate(string(out), 200))
}

// Teardown stops elonco and arcmux subprocesses, kills the per-run tmux
// server (which closes any cmux panes our session opened), and removes
// the short /tmp socket dir. Idempotent and safe to call multiple times.
func (d *ElonkoDispatch) Teardown(stdout io.Writer) {
	// Close session first so arcmux gets a clean Kill RPC instead of a
	// dangling pane after the daemon disappears.
	if d.SessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		d.CloseSession(ctx)
		cancel()
	}

	if d.elonkoProc != nil && d.elonkoProc.Process != nil {
		pid := d.elonkoProc.Process.Pid
		_ = d.elonkoProc.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- d.elonkoProc.Wait() }()
		select {
		case <-done:
			d.env.Tracef("elonco pid=%d exited", pid)
		case <-time.After(5 * time.Second):
			_ = d.elonkoProc.Process.Kill()
			<-done
			d.env.Tracef("elonco pid=%d SIGKILL", pid)
		}
		d.elonkoProc = nil
	}

	if d.daemonProc != nil && d.daemonProc.Process != nil {
		pid := d.daemonProc.Process.Pid
		_ = d.daemonProc.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- d.daemonProc.Wait() }()
		select {
		case <-done:
			d.env.Tracef("arcmux pid=%d exited", pid)
		case <-time.After(5 * time.Second):
			_ = d.daemonProc.Process.Kill()
			<-done
			d.env.Tracef("arcmux pid=%d SIGKILL", pid)
		}
		d.daemonProc = nil
	}

	// Kill our private tmux server (closes any panes the agent opened).
	_ = exec.Command("tmux", "-L", d.tmuxSocket, "kill-server").Run()

	// Clean up the short /tmp socket dir we made.
	if d.socketPath != "" {
		_ = os.RemoveAll(filepath.Dir(d.socketPath))
	}
}

// DispatchElonko is the high-level entry point a Scenario.Run can call when
// env.Mode == "elonco". It starts arcmux + elonco, spawns the agent, polls
// until stable-idle, closes the session, and tears down. Returns the agent
// wall-time and any error.
//
// `wallCap` caps how long we'll wait for the agent to finish (idle stable
// for `stableWindow`). On timeout, the function returns an error but the
// scenario can still run validate.sh against whatever the agent produced.
func DispatchElonko(ctx context.Context, env *Env, mission string, wallCap, stableWindow time.Duration) (time.Duration, error) {
	if wallCap <= 0 {
		wallCap = 8 * time.Minute
	}
	if stableWindow <= 0 {
		stableWindow = 6 * time.Second
	}
	d, err := NewElonkoDispatch(env)
	if err != nil {
		return 0, err
	}
	// Always teardown, even if Start fails halfway.
	defer d.Teardown(env.TraceWriter())

	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := d.Start(ctx, startupCtx); err != nil {
		cancel()
		return 0, fmt.Errorf("dispatch startup: %w", err)
	}
	cancel()

	spawnCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := d.Spawn(spawnCtx, mission); err != nil {
		cancel()
		return 0, fmt.Errorf("spawn: %w", err)
	}
	cancel()

	elapsed, err := d.WaitIdle(ctx, wallCap, stableWindow)
	if err != nil {
		return elapsed, fmt.Errorf("wait-idle: %w", err)
	}
	return elapsed, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	return addr.Port, nil
}
