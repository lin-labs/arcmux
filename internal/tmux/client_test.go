package tmux

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func TestNewClient(t *testing.T) {
	c := NewClient("test-atrs")
	if c.socket != "test-atrs" {
		t.Errorf("socket = %q, want %q", c.socket, "test-atrs")
	}
}

func TestNewClient_DefaultSocket(t *testing.T) {
	c := NewClient("")
	if c.socket != defaultSocket {
		t.Errorf("socket = %q, want %q", c.socket, defaultSocket)
	}
}

func TestIntegration_SessionLifecycle(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}

	ctx := context.Background()
	c := NewClient("atrs-test")

	// Ensure server
	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	// Create session
	sessionName := "atrs-test-session"
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
