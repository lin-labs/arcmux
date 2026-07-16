package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHookEvent(t *testing.T) {
	line := []byte(`{"event":"tool_use","tool":"file_write","ts":"2026-05-16T15:30:00Z","session_id":"s-123"}`)

	event, err := ParseHookEvent(line)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if event.Event != "tool_use" {
		t.Errorf("Event = %q, want %q", event.Event, "tool_use")
	}
	if event.Tool != "file_write" {
		t.Errorf("Tool = %q, want %q", event.Tool, "file_write")
	}
	if event.SessionID != "s-123" {
		t.Errorf("SessionID = %q, want %q", event.SessionID, "s-123")
	}
}

func TestParseHookEvent_Invalid(t *testing.T) {
	_, err := ParseHookEvent([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestInstaller_OutputPath(t *testing.T) {
	installer := NewInstaller("/tmp/arcmux-hooks")
	path := installer.OutputPath("s-abc")
	expected := "/tmp/arcmux-hooks/arcmux-hooks-s-abc.jsonl"
	if path != expected {
		t.Errorf("OutputPath = %q, want %q", path, expected)
	}
}

func TestInstaller_Install_Claude(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	installer := NewInstaller(tmpDir)

	path, err := installer.Install("s-test", "claude_hooks", hookDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if path == "" {
		t.Error("expected non-empty output path")
	}

	// The single GENERIC hook script must exist and be executable.
	scriptPath := GenericHookPath(hookDir)
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("generic hook script not created: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("hook script should be executable")
	}

	// No per-session script may be created any more.
	if _, err := os.Stat(filepath.Join(hookDir, "hooks", "arcmux-s-test.sh")); !os.IsNotExist(err) {
		t.Error("a per-session hook script must NOT be created")
	}
}

func TestInstaller_Install_Claude_GenericIsIdempotentAcrossSessions(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	installer := NewInstaller(tmpDir)

	for _, id := range []string{"s-1", "s-2", "s-3"} {
		if _, err := installer.Install(id, "claude_hooks", hookDir); err != nil {
			t.Fatalf("Install(%s): %v", id, err)
		}
	}

	// Exactly one script file in the hooks dir, regardless of session count.
	entries, err := filepath.Glob(filepath.Join(hookDir, "hooks", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || filepath.Base(entries[0]) != genericHookName {
		t.Errorf("expected exactly one generic hook script, got %v", entries)
	}

	// Content is the fixed generic script (idempotent).
	got, err := os.ReadFile(GenericHookPath(hookDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != genericHookScript {
		t.Error("generic hook content drifted from the fixed template")
	}
}

func TestInstaller_EnsureGenericHook_WithoutSession(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	installer := NewInstaller(filepath.Join(tmpDir, "out"))

	if err := installer.EnsureGenericHook(hookDir); err != nil {
		t.Fatalf("EnsureGenericHook: %v", err)
	}

	scriptPath := GenericHookPath(hookDir)
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("generic hook script not created: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("hook script should be executable")
	}
	got, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != genericHookScript {
		t.Error("generic hook content drifted from the fixed template")
	}
}

func TestGenericHookCannotStampTrustedOverallGoal(t *testing.T) {
	for _, forbidden := range []string{
		"hook-summary", "--overall-goal-provenance", "ARCMUX_SESSION_CWD", "glob.glob", "--vault-link",
	} {
		if strings.Contains(genericHookScript, forbidden) {
			t.Fatalf("generic hook exposes trusted summary writer %q", forbidden)
		}
	}
}

// TestGenericHook_DerivesPathFromEnv runs the generated script under /bin/sh
// and asserts it writes a valid JSON line to the env-derived per-session JSONL
// — and that with the env unset it no-ops (writes nothing, exits 0).
func TestGenericHook_DerivesPathFromEnv(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	// Watcher contract: when ARCMUX_HOOK_OUTPUT_DIR == installer.OutputDir,
	// the hook's derived path must equal OutputPath. Use the same dir.
	outDir := filepath.Join(tmpDir, "out")
	installer := NewInstaller(outDir)
	if _, err := installer.Install("s-env", "claude_hooks", hookDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	script := GenericHookPath(hookDir)

	// With env set: appends one JSON event at the derived path.
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-env",
		"ARCMUX_HOOK_OUTPUT_DIR="+outDir,
		"CLAUDE_HOOK_EVENT_TYPE=tool_use",
		"CLAUDE_TOOL_NAME=bash",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v (%s)", err, out)
	}
	derived := filepath.Join(outDir, "arcmux-hooks-s-env.jsonl")
	if derived != installer.OutputPath("s-env") {
		// The hook's path math must match OutputPath (watcher contract) when
		// ARCMUX_HOOK_OUTPUT_DIR == installer.OutputDir.
		t.Fatalf("derived path %q != OutputPath under same dir", derived)
	}
	data, err := os.ReadFile(derived)
	if err != nil {
		t.Fatalf("read derived jsonl: %v", err)
	}
	ev, err := ParseHookEvent([]byte(strings.TrimSpace(string(data))))
	if err != nil {
		t.Fatalf("parse event: %v (line=%q)", err, data)
	}
	if ev.Event != "tool_use" || ev.Tool != "bash" || ev.SessionID != "s-env" {
		t.Errorf("unexpected event: %+v", ev)
	}

	// With env unset: no-op, exit 0, nothing written.
	noEnv := exec.Command("/bin/sh", script)
	noEnv.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := noEnv.CombinedOutput(); err != nil {
		t.Fatalf("hook with no env should exit 0, got %v (%s)", err, out)
	}
}

// TestGenericHook_ParsesStdinJSON verifies the real Claude path: the hook reads
// the JSON payload on stdin (hook_event_name/tool_name) rather than relying on
// CLAUDE_* env vars, and records the parsed event in the JSONL audit.
func TestGenericHook_ParsesStdinJSON(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	outDir := filepath.Join(tmpDir, "out")
	installer := NewInstaller(outDir)
	if _, err := installer.Install("s-stdin", "claude_hooks", hookDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	script := GenericHookPath(hookDir)
	argvFile := filepath.Join(tmpDir, "argv.txt")
	fakeArcmux := filepath.Join(tmpDir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", script)
	// No CLAUDE_* env — the event must come from stdin.
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-stdin",
		"ARCMUX_HOOK_OUTPUT_DIR="+outDir,
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","tool_name":"","session_id":"x","prompt":"Fix arcmux goal routing"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v (%s)", err, out)
	}
	data, err := os.ReadFile(installer.OutputPath("s-stdin"))
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	ev, err := ParseHookEvent([]byte(strings.TrimSpace(string(data))))
	if err != nil {
		t.Fatalf("parse event: %v (line=%q)", err, data)
	}
	if ev.Event != "UserPromptSubmit" {
		t.Fatalf("event = %q, want UserPromptSubmit (parsed from stdin)", ev.Event)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	call := string(argv)
	// Unified recording semantics: the raw prompt is the last user message; the
	// gauged goal comes from the agent's "Your ask:" (not present here).
	for _, want := range []string{
		"--event prompt_submit",
		"--last-message Fix arcmux goal routing",
		"--contract-source UserPromptSubmit",
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("arcmux call %q missing %q", call, want)
		}
	}
}

func TestCleanupLegacyScripts(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	hooksSub := filepath.Join(hookDir, "hooks")
	if err := os.MkdirAll(hooksSub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed legacy per-session scripts + an unrelated script + the generic.
	legacy := []string{"arcmux-s-111.sh", "arcmux-s-222.sh", "arcmux-s-333.sh"}
	for _, n := range legacy {
		if err := os.WriteFile(filepath.Join(hooksSub, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(hooksSub, "skill-telemetry.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	installer := NewInstaller(tmpDir)
	if _, err := installer.Install("s-keep", "claude_hooks", hookDir); err != nil {
		t.Fatal(err)
	}

	n, err := installer.CleanupLegacyScripts(hookDir)
	if err != nil {
		t.Fatalf("CleanupLegacyScripts: %v", err)
	}
	if n != len(legacy) {
		t.Errorf("removed %d, want %d", n, len(legacy))
	}
	for _, name := range legacy {
		if _, err := os.Stat(filepath.Join(hooksSub, name)); !os.IsNotExist(err) {
			t.Errorf("legacy %s should be removed", name)
		}
	}
	// Generic + unrelated scripts survive.
	if _, err := os.Stat(GenericHookPath(hookDir)); err != nil {
		t.Errorf("generic hook must survive cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hooksSub, "skill-telemetry.sh")); err != nil {
		t.Errorf("unrelated hook must survive cleanup: %v", err)
	}
	// Idempotent: second sweep removes nothing.
	if n2, err := installer.CleanupLegacyScripts(hookDir); err != nil || n2 != 0 {
		t.Errorf("second sweep: n=%d err=%v, want 0/nil", n2, err)
	}
}

func TestInstaller_Install_Claude_RejectsRelativeHookDir(t *testing.T) {
	// Regression: a literal "~/.claude" (or any non-absolute string) used
	// to flow through and silently create a "~/.claude/hooks/..." tree
	// under the daemon's cwd. Now it must error out.
	installer := NewInstaller(t.TempDir())

	for _, hookDir := range []string{"~/.claude", "relative/path", "."} {
		_, err := installer.Install("s-test", "claude_hooks", hookDir)
		if err == nil {
			t.Errorf("Install with hookDir=%q expected error, got nil", hookDir)
		}
	}
}

func TestInstaller_Install_Codex(t *testing.T) {
	tmpDir := t.TempDir()
	installer := NewInstaller(tmpDir)

	path, err := installer.Install("s-test", "codex_output", "")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty output path")
	}
}

func TestInstaller_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	installer := NewInstaller(tmpDir)

	// Create the file
	path := installer.OutputPath("s-test")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("test"), 0o644)

	if err := installer.Cleanup("s-test"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should be removed after cleanup")
	}
}

func TestInstaller_Cleanup_PreservesGenericScript(t *testing.T) {
	// Cleanup removes the per-session JSONL output but must NOT remove the
	// shared generic hook script — it serves every other live session.
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	installer := NewInstaller(tmpDir)

	jsonlPath, err := installer.Install("s-test", "claude_hooks", hookDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	scriptPath := GenericHookPath(hookDir)
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("generic script not created: %v", err)
	}

	if err := installer.Cleanup("s-test"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Error("jsonl should be removed after cleanup")
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Error("generic hook script must survive per-session cleanup")
	}
}

func TestWatcher_LatestEvents_Empty(t *testing.T) {
	w := NewWatcher("/tmp", nil)
	events := w.LatestEvents("nonexistent")
	if events != nil {
		t.Error("expected nil for nonexistent session")
	}
}

func TestWatcher_RecordEvent(t *testing.T) {
	w := NewWatcher("/tmp", nil)

	event := HookEvent{
		Event:     "tool_use",
		Tool:      "bash",
		Timestamp: time.Now(),
		SessionID: "s-1",
	}

	w.recordEvent("s-1", event)

	events := w.LatestEvents("s-1")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Tool != "bash" {
		t.Errorf("Tool = %q, want %q", events[0].Tool, "bash")
	}
}
