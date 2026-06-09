package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacySessionState(t *testing.T) {
	tmp := t.TempDir()
	legacy := filepath.Join(tmp, "arcmux", "sessions")
	dir := filepath.Join(tmp, "mux", "sessions")
	if err := os.MkdirAll(filepath.Join(legacy, "archived"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(legacy, "s-1.json"), "legacy-1")
	write(filepath.Join(legacy, "s-2.json"), "legacy-2")
	write(filepath.Join(legacy, "archived", "s-old.json"), "legacy-old")
	// Destination already has s-2 — the new location is authoritative.
	write(filepath.Join(dir, "s-2.json"), "new-2")

	moved, err := MigrateLegacySessionState(legacy, dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2 (s-1 + archived/s-old; s-2 skipped)", moved)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "s-2.json")); string(got) != "new-2" {
		t.Fatalf("existing destination doc was overwritten: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "s-1.json")); string(got) != "legacy-1" {
		t.Fatalf("s-1 not migrated: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "archived", "s-old.json")); string(got) != "legacy-old" {
		t.Fatalf("archived doc not migrated: %q", got)
	}

	// Idempotent: second sweep moves nothing and errors nothing.
	if moved, err := MigrateLegacySessionState(legacy, dir); err != nil || moved != 0 {
		t.Fatalf("second sweep: moved=%d err=%v, want 0/nil", moved, err)
	}
	// Missing legacy dir is fine.
	if moved, err := MigrateLegacySessionState(filepath.Join(tmp, "absent"), dir); err != nil || moved != 0 {
		t.Fatalf("absent legacy: moved=%d err=%v, want 0/nil", moved, err)
	}
}
