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
	// Codex now installs the unified script (agent from env, not hardcoded),
	// byte-identical to the generic claude/grok hook.
	if string(data) != genericHookScript {
		t.Fatalf("codex hook should be the unified generic script")
	}
	if !strings.Contains(string(data), "hook --agent") || !strings.Contains(string(data), "ARCMUX_HOOK_AGENT") {
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

func TestEnsureCodexHookRefusesSymlinkedForeignHook(t *testing.T) {
	tmpDir := t.TempDir()
	dir := filepath.Join(tmpDir, "codex", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmpDir, "agents-repo-hook.sh")
	foreign := []byte("#!/bin/sh\n# shared agents hook\nprintf preserved\\n\n")
	if err := os.WriteFile(target, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	path := CodexHookPath(dir)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	inst := NewInstaller(t.TempDir())
	err := inst.EnsureCodexHook(dir)
	if err == nil || !strings.Contains(err.Error(), "refusing to replace symlinked hook") {
		t.Fatalf("EnsureCodexHook error = %v, want explicit symlink refusal", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(foreign) {
		t.Fatalf("foreign codex hook was mutated:\n%s", got)
	}
}

// On a user prompt the unified hook records the raw last user message (recording,
// not steering — no system-message injection), with the agent taken from the env.
func TestCodexHookRecordsLastUserMessage(t *testing.T) {
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
		"ARCMUX_HOOK_AGENT=codex",
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"Record turn objective"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run codex hook: %v (%s)", err, out)
	}
	// Recording, not steering: no turn-contract system message is emitted.
	if strings.Contains(string(out), "Arcmux turn contract") {
		t.Fatalf("hook should not emit steering context: %s", out)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	call := string(argv)
	for _, want := range []string{
		"--agent codex",
		"--event prompt_submit",
		"--last-message Record turn objective",
		"--contract-source UserPromptSubmit",
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("arcmux call %q missing %q", call, want)
		}
	}
}

// On Stop the hook gauges the goal from the agent's "Your ask:" line and records
// the raw last user message, both extracted from the codex transcript.
func TestCodexHookExtractsRecordingFromTranscript(t *testing.T) {
	dir := t.TempDir()
	inst := NewInstaller(t.TempDir())
	if err := inst.EnsureCodexHook(dir); err != nil {
		t.Fatalf("EnsureCodexHook: %v", err)
	}

	transcript := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(
		`{"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Implement arcmux turn contract state."}],"metadata":{"turn_id":"turn-1"}}}`+"\n"+
			`{"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Your ask: implement arcmux turn contract state.\nPatched hook state and bridge scripts."}],"metadata":{"turn_id":"turn-1"}}}`+"\n",
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
		"ARCMUX_HOOK_AGENT=codex",
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
		"--agent codex",
		"--goal implement arcmux turn contract state.",
		"--last-message Implement arcmux turn contract state.",
		"--contract-source Stop",
	} {
		if !strings.Contains(call, want) {
			t.Fatalf("arcmux call %q missing %q", call, want)
		}
	}
}
