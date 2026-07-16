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

func TestPromptDeliveryPlan_UsesLiteralBodyThenNamedEnter(t *testing.T) {
	plan := newPromptDeliveryPlan("hello Enter C-m")

	if want := []string{"-l", "hello Enter C-m"}; !reflect.DeepEqual(plan.bodyKeys, want) {
		t.Fatalf("body keys = %v, want %v", plan.bodyKeys, want)
	}
	if want := []string{"Enter"}; !reflect.DeepEqual(plan.submitKeys, want) {
		t.Fatalf("submit keys = %v, want %v", plan.submitKeys, want)
	}
	if plan.wait != promptSubmitDelay {
		t.Fatalf("wait = %s, want %s", plan.wait, promptSubmitDelay)
	}
}

func TestPromptDeliveryResult_TypedOnlyUntilSubmitSucceeds(t *testing.T) {
	result := PromptDeliveryResult{
		Status:    PromptDeliveryTypedOnly,
		BodySent:  true,
		Submitted: false,
		BodyMode:  "literal",
		SubmitKey: "Enter",
		Wait:      promptSubmitDelay,
	}

	if result.Status == PromptDeliverySubmitted {
		t.Fatal("typed-only result must not report submitted")
	}
	if !result.BodySent || result.Submitted {
		t.Fatalf("result = %+v, want body sent but not submitted", result)
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
	if err := c.NewSessionWithEnv(ctx, sessionName, "win", "", env, ""); err != nil {
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

// TestIntegration_PaneIDRoutingSurvivesDuplicateNames is the regression
// for the elonco "all prompts paste into the same pane" bug. The setup
// recreates the failure mode directly: a tmux session containing two
// windows that share a name. Targeting by `<session>:<window-name>` is
// ambiguous in that configuration and tmux send-keys routes to whichever
// window its index-resolver picks (typically the most-recently-active
// one). Targeting by `%pane_id` is unambiguous.
//
// We assert that each pane created via NewSessionWithEnvPaneID /
// NewWindowPaneID receives EXACTLY its own SendKeys payload, even though
// the windows collide on name.
func TestIntegration_PaneIDRoutingSurvivesDuplicateNames(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	ctx := context.Background()
	socket := fmt.Sprintf("arcmux-paneid-%d", time.Now().UnixNano())
	c := NewClient(socket)
	if err := c.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	sessionName := fmt.Sprintf("arcmux-paneid-session-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = c.KillSession(context.Background(), sessionName)
	})

	// First window via NewSessionWithEnvPaneID → returns %pane_id of pane 1.
	pid1, err := c.NewSessionWithEnvPaneID(ctx, sessionName, "dup", "", nil, "")
	if err != nil {
		t.Fatalf("NewSessionWithEnvPaneID: %v", err)
	}
	if !strings.HasPrefix(pid1, "%") {
		t.Fatalf("pid1 = %q, want %% prefix", pid1)
	}

	// Two more windows, all sharing the same name "dup".
	pid2, err := c.NewWindowPaneID(ctx, sessionName, "dup", "", nil)
	if err != nil {
		t.Fatalf("NewWindowPaneID 2: %v", err)
	}
	pid3, err := c.NewWindowPaneID(ctx, sessionName, "dup", "", nil)
	if err != nil {
		t.Fatalf("NewWindowPaneID 3: %v", err)
	}

	// All three pane_ids must be distinct.
	if pid1 == pid2 || pid2 == pid3 || pid1 == pid3 {
		t.Fatalf("pane ids collide: %q %q %q", pid1, pid2, pid3)
	}

	// Send a unique payload into each pane_id. With pane_id targeting,
	// each payload must land in exactly its own pane — even though all
	// three windows share the name "dup".
	payloads := map[string]string{
		pid1: "PANE_ONE_MARKER",
		pid2: "PANE_TWO_MARKER",
		pid3: "PANE_THREE_MARKER",
	}
	for pid, p := range payloads {
		if err := c.SendKeys(ctx, pid, "echo "+p, "Enter"); err != nil {
			t.Fatalf("SendKeys %s: %v", pid, err)
		}
	}

	// Wait for output to settle, then capture each pane and assert it
	// contains its OWN marker and NONE of the other markers.
	deadline := time.Now().Add(3 * time.Second)
	for pid, want := range payloads {
		var out string
		for time.Now().Before(deadline) {
			got, err := c.CapturePaneHistory(ctx, pid)
			if err != nil {
				t.Fatalf("CapturePaneHistory %s: %v", pid, err)
			}
			if strings.Contains(got, want) {
				out = got
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if out == "" {
			final, _ := c.CapturePaneHistory(ctx, pid)
			t.Fatalf("pane %s never showed %q; final capture:\n%s", pid, want, final)
		}
		// And it must NOT contain the other panes' markers.
		for otherPid, otherMark := range payloads {
			if otherPid == pid {
				continue
			}
			if strings.Contains(out, otherMark) {
				t.Errorf("pane %s contains foreign marker %q (cross-routing leak):\n%s",
					pid, otherMark, out)
			}
		}
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
	if err := c.NewSessionWithEnv(ctx, sessionName, "first", "", nil, ""); err != nil {
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
