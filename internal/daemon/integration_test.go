package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestIntegration_DaemonLifecycle(t *testing.T) {
	if os.Getenv("ARCMUX_INTEGRATION") == "" {
		t.Skip("set ARCMUX_INTEGRATION=1 to run integration tests")
	}

	tmpDir := t.TempDir()
	// Use /tmp for socket to avoid macOS 104-byte path limit
	socketPath := "/tmp/arcmux-test.sock"
	os.Remove(socketPath)
	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: socketPath,
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Tmux: config.TmuxConfig{
			SocketName:     "arcmux-test-int",
			DefaultSession: "test-agents",
		},
		Health: config.HealthConfig{
			CaptureInterval: "2s",
			IdleTimeout:     "30s",
			StuckTimeout:    "1m",
		},
		Hooks: config.HooksConfig{
			HookOutputDir: filepath.Join(tmpDir, "hooks"),
			AutoInstall:   false,
		},
		Agents: config.DefaultAgentProfiles(),
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := New(cfg, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start daemon
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer d.Stop()

	// Create a session against a supported built-in profile.
	sess, err := d.CreateSession(ctx, CreateSessionRequest{
		Agent: "codex",
		CWD:   tmpDir,
		Name:  "test-echo",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if sess.Snapshot().ID == "" {
		t.Fatal("session ID should not be empty")
	}

	// Wait for handshake to settle
	time.Sleep(3 * time.Second)

	// Check session state
	snap := sess.Snapshot()
	t.Logf("session state: %s, tmux_target: %s", snap.State, snap.TmuxTarget)

	// List sessions
	sessions := d.ListSessions()
	if len(sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions))
	}

	// Capture output
	output, err := d.Capture(ctx, snap.ID, false)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	t.Logf("capture output: %q", output)

	// Kill session
	if err := d.Kill(ctx, snap.ID, false, 5*time.Second); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Verify state
	killedSnap := sess.Snapshot()
	if killedSnap.State != session.StateExited {
		t.Errorf("state after kill = %q, want %q", killedSnap.State, session.StateExited)
	}

	// Cleanup tmux
	d.tmux.KillPane(ctx, snap.TmuxTarget)
}

// TestIntegration_TmuxMultiSessionPromptRoutingByPaneID is the daemon-level
// regression for Bug 1: rapid CreateSession calls each pasting an
// initial prompt MUST route each prompt into its own pane, not into
// some other pane that happens to share the window name.
//
// We use a profile-less test daemon and a fake "claude" / "codex"
// equivalent: the daemon's tmux transport sends keys directly to the
// pane_id-backed TmuxTarget. We bypass the full handshake by calling
// setupTmuxPane + SendKeys directly through the daemon's tmux client,
// then assert each pane contains its own marker and not the others.
//
// This pins the contract that sess.TmuxTarget is a pane_id (starts with
// %) and that SendKeys against that target hits exactly one pane.
func TestIntegration_TmuxMultiSessionPromptRoutingByPaneID(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "arcmux.sock")
	tmuxSocketName := fmt.Sprintf("arcmux-routing-%d", time.Now().UnixNano())
	defaultSession := fmt.Sprintf("agents-routing-%d", time.Now().UnixNano())

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: socketPath,
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Tmux: config.TmuxConfig{
			SocketName:     tmuxSocketName,
			DefaultSession: defaultSession,
		},
		Hooks: config.HooksConfig{
			HookOutputDir: filepath.Join(tmpDir, "hooks"),
			AutoInstall:   false,
		},
		Agents: config.DefaultAgentProfiles(),
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	d := New(cfg, logger)
	d.ctx = context.Background()

	if err := d.tmux.EnsureServer(d.ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	t.Cleanup(func() {
		_ = d.tmux.KillSession(context.Background(), defaultSession)
	})

	// Create three "windows", all sharing the same window name. This is
	// the exact configuration that broke elonco: rapid spawns where
	// every window got the same name and prompts routed by name
	// resolved to whichever pane tmux's index-resolver picked.
	const sharedName = "elonco-spawn"
	targets := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		target, err := d.setupTmuxPane(d.ctx, defaultSession, sharedName, tmpDir, nil)
		if err != nil {
			t.Fatalf("setupTmuxPane %d: %v", i, err)
		}
		if !strings.HasPrefix(target, "%") {
			t.Fatalf("setupTmuxPane %d returned %q; want pane_id starting with %%", i, target)
		}
		targets = append(targets, target)
	}

	// All three targets must be distinct.
	seen := map[string]bool{}
	for _, tt := range targets {
		if seen[tt] {
			t.Fatalf("duplicate target across spawns: %q in %v", tt, targets)
		}
		seen[tt] = true
	}

	// Send a unique prompt into each target. Each pane must contain
	// EXACTLY its own marker and NONE of the other markers.
	markers := map[string]string{
		targets[0]: "ROUTE_A_MARK",
		targets[1]: "ROUTE_B_MARK",
		targets[2]: "ROUTE_C_MARK",
	}
	for tgt, m := range markers {
		if err := d.tmux.SendKeys(d.ctx, tgt, "echo "+m, "Enter"); err != nil {
			t.Fatalf("SendKeys %s: %v", tgt, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for tgt, want := range markers {
		var captured string
		for time.Now().Before(deadline) {
			out, err := d.tmux.CapturePaneHistory(d.ctx, tgt)
			if err != nil {
				t.Fatalf("CapturePaneHistory %s: %v", tgt, err)
			}
			if strings.Contains(out, want) {
				captured = out
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if captured == "" {
			final, _ := d.tmux.CapturePaneHistory(d.ctx, tgt)
			t.Fatalf("pane %s never showed %q; final capture:\n%s", tgt, want, final)
		}
		for otherTgt, otherMark := range markers {
			if otherTgt == tgt {
				continue
			}
			if strings.Contains(captured, otherMark) {
				t.Errorf("pane %s contains foreign marker %q (cross-routing leak):\n%s",
					tgt, otherMark, captured)
			}
		}
	}

	// Touch profile so the import isn't unused under -tags noints.
	_ = profile.TransportTmux
}
