//go:build darwin || linux

package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return 0
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestStopKillsExecProcessTreeButLeavesExternalTmuxProcess(t *testing.T) {
	tmpDir := t.TempDir()
	childPIDPath := filepath.Join(tmpDir, "child.pid")
	fakeClaude := filepath.Join(tmpDir, "claude")
	script := "#!/bin/sh\n" +
		"(\n" +
		"  trap '' INT TERM\n" +
		"  while :; do sleep 1; done\n" +
		") &\n" +
		"child=$!\n" +
		"printf '%s\\n' \"$child\" > \"$CHILD_PID_FILE\"\n" +
		"trap 'exit 0' INT TERM\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: filepath.Join(tmpDir, "arcmux.sock"),
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Hooks:  config.HooksConfig{HookOutputDir: filepath.Join(tmpDir, "hooks")},
		Agents: config.DefaultAgentProfiles(),
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	d.ctx = context.Background()

	sess := session.NewSession("s-exec", "exec-child-tree", "claude_exec", tmpDir)
	sess.SetTransport(profile.TransportExec)
	sess.SetState(session.StateIdle)
	sess.SetEnv(map[string]string{"CHILD_PID_FILE": childPIDPath})
	d.sessions[sess.ID] = sess

	tmuxSurrogate := exec.Command("sleep", "30")
	if err := tmuxSurrogate.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tmuxSurrogate.Process.Kill()
		_ = tmuxSurrogate.Wait()
	}()

	if err := d.SendPrompt(context.Background(), sess.ID, "block", false, false); err != nil {
		t.Fatal(err)
	}
	childPID := waitForPIDFile(t, childPIDPath)
	execPID := sess.Snapshot().PID
	defer func() {
		if processAlive(execPID) {
			_ = syscall.Kill(execPID, syscall.SIGKILL)
		}
		if processAlive(childPID) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()

	d.Stop()

	if processAlive(execPID) {
		t.Errorf("exec process %d survived daemon Stop", execPID)
	}
	if processAlive(childPID) {
		t.Errorf("exec grandchild %d survived daemon Stop", childPID)
	}
	if err := tmuxSurrogate.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("external tmux surrogate was terminated: %v", err)
	}

	d.mu.RLock()
	running := len(d.processes)
	d.mu.RUnlock()
	if running != 0 {
		t.Errorf("exec process registry still has %d entries", running)
	}
	inventory, err := os.ReadFile(filepath.Join(tmpDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inventory), "\"state\": \"working\"") {
		t.Errorf("shutdown persisted a working exec session:\n%s", inventory)
	}
}

func TestSendExecPromptRejectsAfterStopBegins(t *testing.T) {
	tmpDir := t.TempDir()
	sentinel := filepath.Join(tmpDir, "started")
	fakeClaude := filepath.Join(tmpDir, "claude")
	script := "#!/bin/sh\n" +
		": > \"$START_SENTINEL\"\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"unexpected\"}]}}'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: filepath.Join(tmpDir, "arcmux.sock"),
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Hooks:  config.HooksConfig{HookOutputDir: filepath.Join(tmpDir, "hooks")},
		Agents: config.DefaultAgentProfiles(),
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	d.ctx = context.Background()
	sess := session.NewSession("s-after-stop", "after-stop", "claude_exec", tmpDir)
	sess.SetTransport(profile.TransportExec)
	sess.SetState(session.StateIdle)
	sess.SetEnv(map[string]string{"START_SENTINEL": sentinel})
	d.sessions[sess.ID] = sess

	d.Stop()
	err := d.SendPrompt(context.Background(), sess.ID, "must not start", false, false)
	if err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("SendPrompt after Stop error = %v, want daemon stopping", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("exec process started after daemon shutdown began")
	}
}

func TestOverallGoalModelCancellationKillsDescendant(t *testing.T) {
	tmpDir := t.TempDir()
	childPIDPath := filepath.Join(tmpDir, "goal-child.pid")
	fakeGrok := filepath.Join(tmpDir, "grok")
	script := "#!/bin/sh\n" +
		"sleep 30 &\n" +
		"child=$!\n" +
		"printf '%s\\n' \"$child\" > \"$GOAL_CHILD_PID_FILE\"\n" +
		"wait \"$child\"\n"
	if err := os.WriteFile(fakeGrok, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_GOAL_BIN", fakeGrok)
	t.Setenv("ARCMUX_GOAL_TIMEOUT", "30s")
	t.Setenv("GOAL_CHILD_PID_FILE", childPIDPath)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runOverallGoalModel(ctx, "", "conversation")
		result <- err
	}()
	childPID := waitForPIDFile(t, childPIDPath)
	defer func() {
		if processAlive(childPID) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()

	cancel()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		<-result
		t.Fatal("goal summarizer did not drain after cancellation")
	}
	if processAlive(childPID) {
		t.Fatalf("goal summarizer descendant %d survived cancellation", childPID)
	}
}

func TestRootStopDrainsProfileExecBeforeAncillaryShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	childPIDPath := filepath.Join(tmpDir, "profile-child.pid")
	fakeClaude := filepath.Join(tmpDir, "claude")
	script := "#!/bin/sh\n" +
		"sleep 30 &\n" +
		"child=$!\n" +
		"printf '%s\\n' \"$child\" > \"$CHILD_PID_FILE\"\n" +
		"wait \"$child\"\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	newTestDaemon := func(name string) *Daemon {
		cfg := &config.Config{
			Daemon: config.DaemonConfig{
				Socket: filepath.Join(tmpDir, name+".sock"),
				LogDir: filepath.Join(tmpDir, name+"-logs"),
			},
			Hooks:  config.HooksConfig{HookOutputDir: filepath.Join(tmpDir, name+"-hooks")},
			Agents: config.DefaultAgentProfiles(),
		}
		d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		d.ctx, d.cancel = context.WithCancel(context.Background())
		return d
	}
	root := newTestDaemon("root")
	child := newTestDaemon("profile")
	sess := session.NewSession("s-profile-exec", "profile-exec", "claude_exec", tmpDir)
	sess.SetTransport(profile.TransportExec)
	sess.SetState(session.StateIdle)
	sess.SetEnv(map[string]string{"CHILD_PID_FILE": childPIDPath})
	child.sessions[sess.ID] = sess
	if err := child.SendPrompt(context.Background(), sess.ID, "block", false, false); err != nil {
		t.Fatal(err)
	}
	childPID := waitForPIDFile(t, childPIDPath)
	defer func() {
		if processAlive(childPID) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	}()

	root.profileManager = &ProfileManager{
		parent:  root,
		records: map[string]ProfileRecord{"profile": {Name: "profile"}},
		daemons: map[string]*Daemon{"profile": child},
	}
	aliveDuringAncillary := true
	root.otelShutdown = func(context.Context) error {
		aliveDuringAncillary = processAlive(childPID)
		return nil
	}

	root.Stop()

	if aliveDuringAncillary {
		t.Fatal("profile-owned exec descendant survived into ancillary shutdown")
	}
	if processAlive(childPID) {
		t.Fatal("profile-owned exec descendant survived root Stop")
	}
}
