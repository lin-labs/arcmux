package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
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

	// Create a session that runs a simple echo command (not a real agent)
	sess, err := d.CreateSession(ctx, CreateSessionRequest{
		Agent: "grok", // grok profile is simplest (screen_only)
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
