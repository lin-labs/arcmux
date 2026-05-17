package hooks

import (
	"os"
	"path/filepath"
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
	installer := NewInstaller("/tmp/atrs-hooks")
	path := installer.OutputPath("s-abc")
	expected := "/tmp/atrs-hooks/atrs-hooks-s-abc.jsonl"
	if path != expected {
		t.Errorf("OutputPath = %q, want %q", path, expected)
	}
}

func TestInstaller_Install_Claude(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	installer := NewInstaller(tmpDir)

	path, err := installer.Install("s-test", "claude", hookDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if path == "" {
		t.Error("expected non-empty output path")
	}

	// Check hook script was created
	scriptPath := filepath.Join(hookDir, "hooks", "atrs-s-test.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("hook script not created: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("hook script should be executable")
	}
}

func TestInstaller_Install_Codex(t *testing.T) {
	tmpDir := t.TempDir()
	installer := NewInstaller(tmpDir)

	path, err := installer.Install("s-test", "codex", "")
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
