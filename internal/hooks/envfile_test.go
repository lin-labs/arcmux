package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionEnvFile_WriteAndLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	path, err := WriteSessionEnvFile(dir, "s-rt", map[string]string{
		"ARCMUX_SESSION_ID":      "s-rt",
		"ARCMUX_HOOK_OUTPUT_DIR": "/tmp/arcmux-hooks",
	})
	if err != nil {
		t.Fatalf("WriteSessionEnvFile: %v", err)
	}

	// File perms: 0600, dir perms: 0700.
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file perm = %#o, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Lstat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir perm = %#o, want 0700", di.Mode().Perm())
	}

	exports, err := LoadSessionEnvExports(dir, "s-rt")
	if err != nil {
		t.Fatalf("LoadSessionEnvExports: %v", err)
	}
	joined := strings.Join(exports, "\n")
	if !strings.Contains(joined, `export ARCMUX_SESSION_ID='s-rt'`) ||
		!strings.Contains(joined, `export ARCMUX_HOOK_OUTPUT_DIR='/tmp/arcmux-hooks'`) {
		t.Errorf("unexpected exports:\n%s", joined)
	}
}

// TestSessionEnvFile_MaliciousValueRoundTripsAsLiteral is the core safety
// proof: a value crafted to break out and run a command must survive as a
// plain string when the loader eval's arcmux's quoted output.
func TestSessionEnvFile_MaliciousValueRoundTripsAsLiteral(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	marker := filepath.Join(t.TempDir(), "PWNED")
	payload := "'; touch " + marker + "; echo '"
	if _, err := WriteSessionEnvFile(dir, "s-evil", map[string]string{
		"ARCMUX_HOOK_OUTPUT_DIR": payload,
		"ARCMUX_SESSION_ID":      "s-evil",
	}); err != nil {
		t.Fatalf("WriteSessionEnvFile: %v", err)
	}
	exports, err := LoadSessionEnvExports(dir, "s-evil")
	if err != nil {
		t.Fatalf("LoadSessionEnvExports: %v", err)
	}

	// eval the exports in a real shell, then print the var back. If quoting
	// is correct, the marker file is NOT created and the var equals payload.
	script := strings.Join(exports, "\n") + "\nprintf '%s' \"$ARCMUX_HOOK_OUTPUT_DIR\"\n"
	out, err := exec.Command("/bin/sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("eval exports: %v", err)
	}
	if string(out) != payload {
		t.Errorf("value mangled by quoting: got %q want %q", out, payload)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("SHELL INJECTION: marker file was created — quoting failed")
	}
}

func TestSessionEnvFile_RejectsDisallowedKeyOnWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	if _, err := WriteSessionEnvFile(dir, "s-x", map[string]string{"PATH": "/evil"}); err == nil {
		t.Error("expected error writing non-ARCMUX_ key")
	}
	if _, err := WriteSessionEnvFile(dir, "s-x", map[string]string{"ARCMUX_X": "a\nb"}); err == nil {
		t.Error("expected error for value with newline")
	}
}

func TestLoadSessionEnvExports_RejectsWorldWritableFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	if _, err := WriteSessionEnvFile(dir, "s-perm", map[string]string{"ARCMUX_SESSION_ID": "s-perm"}); err != nil {
		t.Fatal(err)
	}
	path := SessionEnvFilePath(dir, "s-perm")
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionEnvExports(dir, "s-perm"); err == nil {
		t.Error("expected error for group/world-readable env file")
	}
}

func TestLoadSessionEnvExports_RejectsSymlinkFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	// Make the dir safely first.
	if _, err := WriteSessionEnvFile(dir, "s-seed", map[string]string{"ARCMUX_SESSION_ID": "s-seed"}); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "real.env")
	if err := os.WriteFile(real, []byte("ARCMUX_SESSION_ID=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := SessionEnvFilePath(dir, "s-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionEnvExports(dir, "s-link"); err == nil {
		t.Error("expected error for symlinked env file")
	}
}

func TestLoadSessionEnvExports_RejectsDisallowedKeyInFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := SessionEnvFilePath(dir, "s-bad")
	if err := os.WriteFile(path, []byte("PATH=/evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionEnvExports(dir, "s-bad"); err == nil {
		t.Error("expected error for non-allowlisted key in file")
	}
}

func TestLoadSessionEnvExports_MissingFileErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arcmux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionEnvExports(dir, "s-none"); err == nil {
		t.Error("expected error for missing env file")
	}
}
