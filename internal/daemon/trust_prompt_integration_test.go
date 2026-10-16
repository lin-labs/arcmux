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
)

// fakeTrustAgent renders Claude Code 2.1.301's folder-trust menu (cursor on
// "No, exit"), moves with arrow keys, and exits 1 when "No" is confirmed.
const fakeTrustAgent = `import sys, termios, tty
opts = ["No, exit", "Yes, I trust this folder"]
sel = 0
def draw():
    sys.stdout.write("\x1b[2J\x1b[H Accessing workspace:\r\n Is this a project you created or one you trust?\r\n")
    for i, o in enumerate(opts):
        sys.stdout.write((" ❯ " if i == sel else "   ") + o + "\r\n")
    sys.stdout.write(" Enter to confirm · Esc to cancel\r\n"); sys.stdout.flush()
fd = sys.stdin.fileno(); tty.setraw(fd); draw()
while True:
    ch = sys.stdin.read(1)
    if ch == "\x1b":
        seq = sys.stdin.read(2)
        sel = min(sel + 1, 1) if seq == "[B" else max(sel - 1, 0) if seq == "[A" else sel
        draw()
    elif ch in "\r\n":
        if sel == 0: sys.exit(1)
        termios.tcsetattr(fd, termios.TCSADRAIN, termios.tcgetattr(fd))
        sys.stdout.write("\x1b[2J\x1b[H remote-control is active\r\n"); sys.stdout.flush()
        import time; time.sleep(60)
`

func TestIntegration_TrustPromptSelectsAffirmativeOption(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "agent.py")
	if err := os.WriteFile(script, []byte(fakeTrustAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Daemon: config.DaemonConfig{Socket: filepath.Join(tmpDir, "arcmux.sock"), LogDir: filepath.Join(tmpDir, "logs")},
		Tmux:   config.TmuxConfig{SocketName: fmt.Sprintf("arcmux-trust-%d", time.Now().UnixNano()), DefaultSession: "trust"},
		Hooks:  config.HooksConfig{HookOutputDir: filepath.Join(tmpDir, "hooks")},
		Agents: config.DefaultAgentProfiles(),
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx := context.Background()
	if err := d.tmux.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	t.Cleanup(func() { _ = d.tmux.KillServer(context.Background()) })
	pane, err := d.tmux.NewSessionWithEnvPaneID(ctx, "trust-agent", "agent", tmpDir, nil, "python3 "+script)
	if err != nil {
		t.Fatalf("start fake agent: %v", err)
	}
	prof := profile.Profile{Name: "claude", TrustPromptPattern: "trust this folder", TrustResponse: "Enter"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		screen, _ := d.tmux.CapturePaneVisible(ctx, pane)
		if strings.Contains(screen, "trust this folder") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake agent never rendered its trust menu:\n%s", screen)
		}
		time.Sleep(100 * time.Millisecond)
	}

	handled, err := d.handleTrustPrompt(ctx, pane, prof)
	if !handled || err != nil {
		t.Fatalf("handleTrustPrompt = %v, %v; want handled with the agent still running", handled, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		screen, err := d.tmux.CapturePaneVisible(ctx, pane)
		if err == nil && strings.Contains(screen, "remote-control is active") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not reach ready after trust answer (err=%v):\n%s", err, screen)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
