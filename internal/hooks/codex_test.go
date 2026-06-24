package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestCodexHookPassesTurnContract(t *testing.T) {
	dir := t.TempDir()
	inst := NewInstaller(t.TempDir())
	if err := inst.EnsureCodexHook(dir); err != nil {
		t.Fatalf("EnsureCodexHook: %v", err)
	}

	argvFile := filepath.Join(dir, "argv.txt")
	fakeArcmux := filepath.Join(dir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", CodexHookPath(dir))
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-codex",
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"Record turn objective","success_verification":"turn_contract fields are present","path":"Wire Codex bridge to arcmux hook"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run codex hook: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "Arcmux turn contract") {
		t.Fatalf("prompt-submit hook did not emit turn-contract context: %s", out)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	call := string(argv)
	for _, want := range []string{
		"--goal Record turn objective",
		"--verification turn_contract fields are present",
		"--path Wire Codex bridge to arcmux hook",
		"--contract-source UserPromptSubmit",
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("arcmux call %q missing %q", call, want)
		}
	}
}

func TestCodexHookExtractsStopContractFromTranscript(t *testing.T) {
	dir := t.TempDir()
	inst := NewInstaller(t.TempDir())
	if err := inst.EnsureCodexHook(dir); err != nil {
		t.Fatalf("EnsureCodexHook: %v", err)
	}

	transcript := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(
		`{"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Implement arcmux turn contract state."}],"metadata":{"turn_id":"turn-1"}}}`+"\n"+
			`{"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Your ask: implement arcmux turn contract state.\nPatched hook state and bridge scripts.\nVerification: go test ./... passed."}],"metadata":{"turn_id":"turn-1"}}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	argvFile := filepath.Join(dir, "argv.txt")
	fakeArcmux := filepath.Join(dir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", CodexHookPath(dir))
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-codex",
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop","turn_id":"turn-1","transcript_path":"` + transcript + `"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run codex hook: %v (%s)", err, out)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	call := string(argv)
	for _, want := range []string{
		"--event turn_end",
		"--goal implement arcmux turn contract state.",
		"--verification Verification: go test ./... passed.",
		"--path Your ask: implement arcmux turn contract state. Patched hook state and bridge scripts. Verification: go test ./... passed.",
		"--contract-source Stop",
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("arcmux call %q missing %q", call, want)
		}
	}
}
