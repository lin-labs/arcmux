package scratchpad

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRole(t *testing.T) {
	good := []string{"elon", "manager-foo", "ic.bar.1", "x", "A_B_C"}
	bad := []string{"", "../etc", "ic/escape", ".hidden", "-leading-dash", strings.Repeat("a", 65)}
	for _, r := range good {
		if err := ValidateRole(r); err != nil {
			t.Errorf("ValidateRole(%q) returned %v, want nil", r, err)
		}
	}
	for _, r := range bad {
		if err := ValidateRole(r); err == nil {
			t.Errorf("ValidateRole(%q) returned nil, want error", r)
		}
	}
}

func TestPathEnsuresParentDir(t *testing.T) {
	dataRoot := t.TempDir()
	p, err := Path(dataRoot, "smoke", "elon")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(dataRoot, "arcmux", "smoke", "scratchpads", "elon.json")
	if p != want {
		t.Errorf("Path = %q, want %q", p, want)
	}
	info, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatalf("parent dir missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("parent perm = %o, want 0700", got)
	}
}

func TestPathRejectsBadInput(t *testing.T) {
	cases := []struct{ project, role string }{
		{"", "elon"},
		{"smoke", ""},
		{"../etc", "elon"},
		{"smoke", "../etc"},
		{"smoke", "ic/escape"},
	}
	for _, tc := range cases {
		if _, err := Path(t.TempDir(), tc.project, tc.role); err == nil {
			t.Errorf("Path(%q,%q) returned nil error", tc.project, tc.role)
		}
	}
}

func TestWriteAtomicAndPerms(t *testing.T) {
	dataRoot := t.TempDir()
	p, err := Path(dataRoot, "smoke", "elon")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	body := []byte(`{"focus":"turn-5"}`)
	if err := Write(p, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("content mismatch: got %q, want %q", got, body)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file perm = %o, want 0600", got)
	}

	// Overwrite — no leftover tmpfiles in dir.
	if err := Write(p, []byte(`{"focus":"turn-6"}`)); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover tmpfile: %s", e.Name())
		}
	}
}

func TestWriteEmptyBody(t *testing.T) {
	dataRoot := t.TempDir()
	p, err := Path(dataRoot, "smoke", "elon")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := Write(p, nil); err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}
