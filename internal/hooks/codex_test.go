package hooks

import (
	"os"
	"strings"
	"testing"
)

func TestEnsureCodexHookWritesIdempotently(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	inst := NewInstaller(t.TempDir())

	if err := inst.EnsureCodexHook(dir); err != nil {
		t.Fatalf("EnsureCodexHook: %v", err)
	}
	path := CodexHookPath(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed hook: %v", err)
	}
	if !strings.Contains(string(data), "--agent codex") {
		t.Fatalf("installed script missing arcmux contract invocation")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed script not executable: %v", info.Mode())
	}

	// Second call is a no-op (idempotent) and must not error.
	if err := inst.EnsureCodexHook(dir); err != nil {
		t.Fatalf("EnsureCodexHook (2nd): %v", err)
	}
}

func TestEnsureCodexHookRejectsRelativeDir(t *testing.T) {
	t.Parallel()
	inst := NewInstaller(t.TempDir())
	if err := inst.EnsureCodexHook("relative/dir"); err == nil {
		t.Fatal("expected error for relative codex hook dir")
	}
}
