package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func TestNewClient(t *testing.T) {
	c := NewClient("test-arcmux")
	if c.socket != "test-arcmux" {
		t.Errorf("socket = %q, want %q", c.socket, "test-arcmux")
	}
}

func TestNewClient_DefaultSocket(t *testing.T) {
	c := NewClient("")
	if c.socket != defaultSocket {
		t.Errorf("socket = %q, want %q", c.socket, defaultSocket)
	}
}

func TestEnvFlags_StableOrdering(t *testing.T) {
	got := envFlags(map[string]string{
		"FOO": "1",
		"BAR": "two",
		"BAZ": "",
	})
	want := []string{"-e", "BAR=two", "-e", "BAZ=", "-e", "FOO=1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envFlags = %v, want %v", got, want)
	}
}

func TestEnvFlags_Empty(t *testing.T) {
	if got := envFlags(nil); got != nil {
		t.Errorf("envFlags(nil) = %v, want nil", got)
	}
	if got := envFlags(map[string]string{}); got != nil {
		t.Errorf("envFlags(empty) = %v, want nil", got)
	}
}

// TestIntegration_NewSessionWithEnv proves that caller-supplied env vars
// reach the spawned pane's shell. Regression for the silent-drop bug
// where CreateSession.Env was wired through every layer except tmux
// new-session, so role files (ARCMUX_PROJECT, ARCMUX_ROLE_FILE, …) saw
// empty values.
func TestIntegration_NewSessionWithEnv(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}

	ctx := context.Background()
	socket := fmt.Sprintf("arcmux-env-%d", time.Now().UnixNano())
	c := NewClient(socket)

	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	sessionName := fmt.Sprintf("arcmux-env-session-%d", time.Now().UnixNano())
	env := map[string]string{
		"ARCMUX_FOO": "bar-value",
		"ARCMUX_BAZ": "qux",
	}
	if err := c.NewSessionWithEnv(ctx, sessionName, "win", "", env); err != nil {
		t.Fatalf("NewSessionWithEnv: %v", err)
	}
	t.Cleanup(func() {
		_ = c.KillSession(context.Background(), sessionName)
	})

	target := sessionName + ":win"
	// Print both vars on a single line so we can grep for them.
	if err := c.SendKeys(ctx, target, "echo MARK1=$ARCMUX_FOO MARK2=$ARCMUX_BAZ", "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var output string
	for time.Now().Before(deadline) {
		out, err := c.CapturePaneHistory(ctx, target)
		if err != nil {
			t.Fatalf("CapturePaneHistory: %v", err)
		}
		if strings.Contains(out, "MARK1=bar-value") && strings.Contains(out, "MARK2=qux") {
			output = out
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if output == "" {
		final, _ := c.CapturePaneHistory(ctx, target)
		t.Fatalf("env vars never appeared in pane output; last capture:\n%s", final)
	}
}

// TestIntegration_NewWindowCanonical_TargetShape proves the canonical
// target form is `<session>:<window-name>` regardless of how tmux returns
// it internally. Regression for SessionSummary.TmuxTarget shape drift.
func TestIntegration_NewWindowCanonical_TargetShape(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	ctx := context.Background()
	socket := fmt.Sprintf("arcmux-canon-%d", time.Now().UnixNano())
	c := NewClient(socket)
	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	sessionName := fmt.Sprintf("arcmux-canon-session-%d", time.Now().UnixNano())
	if err := c.NewSessionWithEnv(ctx, sessionName, "first", "", nil); err != nil {
		t.Fatalf("NewSessionWithEnv: %v", err)
	}
	t.Cleanup(func() {
		_ = c.KillSession(context.Background(), sessionName)
	})

	target, err := c.NewWindowCanonical(ctx, sessionName, "secondwin", "", nil)
	if err != nil {
		t.Fatalf("NewWindowCanonical: %v", err)
	}
	want := sessionName + ":secondwin"
	if target != want {
		t.Errorf("target = %q, want %q", target, want)
	}
	if !c.PaneExists(ctx, target) {
		t.Errorf("pane %q should exist", target)
	}
}

func TestIntegration_SessionLifecycle(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}

	ctx := context.Background()
	c := NewClient("arcmux-test")

	// Ensure server
	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	// Create session
	sessionName := "arcmux-test-session"
	if err := c.NewSession(ctx, sessionName, "test-win", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() {
		// Cleanup: kill the test session
		c.run(ctx, "kill-session", "-t", sessionName)
	}()

	target := sessionName + ":test-win"

	// Send keys
	if err := c.SendKeys(ctx, target, "echo hello", "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	// Wait for output
	time.Sleep(500 * time.Millisecond)

	// Capture
	output, err := c.CapturePaneVisible(ctx, target)
	if err != nil {
		t.Fatalf("CapturePaneVisible: %v", err)
	}
	if output == "" {
		t.Error("expected non-empty capture output")
	}

	// Pane exists
	if !c.PaneExists(ctx, target) {
		t.Error("pane should exist")
	}

	// Get pane info
	info, err := c.GetPaneInfo(ctx, target)
	if err != nil {
		t.Fatalf("GetPaneInfo: %v", err)
	}
	if info.PID == 0 {
		t.Error("expected non-zero PID")
	}

	// Kill pane
	if err := c.KillPane(ctx, target); err != nil {
		t.Fatalf("KillPane: %v", err)
	}

	// Should no longer exist
	if c.PaneExists(ctx, target) {
		t.Error("pane should not exist after kill")
	}
}
