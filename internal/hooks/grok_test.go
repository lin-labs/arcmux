package hooks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGrokHook_WritesScriptAndRegistration(t *testing.T) {
	tmpDir := t.TempDir()
	grokDir := filepath.Join(tmpDir, "grok")
	installer := NewInstaller(filepath.Join(tmpDir, "out"))

	if err := installer.EnsureGrokHook(grokDir); err != nil {
		t.Fatalf("EnsureGrokHook: %v", err)
	}

	// The generic session hook script lands in <grokDir>/hooks, executable.
	script := GenericHookPath(grokDir)
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("generic hook script not created: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("hook script should be executable")
	}

	// The drop-in registration is valid JSON wiring each lifecycle event to
	// the script. (Grok merges ~/.grok/hooks/*.json — the file IS the
	// registration, no settings edit needed.)
	data, err := os.ReadFile(GrokHookConfigPath(grokDir))
	if err != nil {
		t.Fatalf("registration not created: %v", err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("registration is not valid JSON: %v", err)
	}
	for _, ev := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		entries, ok := cfg.Hooks[ev]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Fatalf("registration missing event %s", ev)
		}
		h := entries[0].Hooks[0]
		if h.Type != "command" || h.Command != script {
			t.Errorf("event %s: type=%q command=%q, want command=%q", ev, h.Type, h.Command, script)
		}
	}
}

func TestEnsureGrokHook_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	grokDir := filepath.Join(tmpDir, "grok")
	installer := NewInstaller(filepath.Join(tmpDir, "out"))

	if err := installer.EnsureGrokHook(grokDir); err != nil {
		t.Fatalf("first EnsureGrokHook: %v", err)
	}
	first, err := os.Stat(GrokHookConfigPath(grokDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := installer.EnsureGrokHook(grokDir); err != nil {
		t.Fatalf("second EnsureGrokHook: %v", err)
	}
	second, err := os.Stat(GrokHookConfigPath(grokDir))
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("re-install with identical content must not rewrite the registration")
	}
}

func TestEnsureGrokHook_RejectsRelativeDir(t *testing.T) {
	installer := NewInstaller(t.TempDir())
	for _, dir := range []string{"~/.grok", "relative/path", "."} {
		if err := installer.EnsureGrokHook(dir); err == nil {
			t.Errorf("EnsureGrokHook(%q) expected error, got nil", dir)
		}
	}
}

// TestGenericHook_ParsesGrokStdinJSON verifies the grok dialect end to end
// through the script: grok delivers {"hookEventName","toolName"} with
// snake_case event values; the script must record the parsed event in the
// JSONL audit AND invoke `arcmux hook` with the canonical event and the
// agent from ARCMUX_HOOK_AGENT.
func TestGenericHook_ParsesGrokStdinJSON(t *testing.T) {
	tmpDir := t.TempDir()
	grokDir := filepath.Join(tmpDir, "grok")
	outDir := filepath.Join(tmpDir, "out")
	installer := NewInstaller(outDir)
	if err := installer.EnsureGrokHook(grokDir); err != nil {
		t.Fatalf("EnsureGrokHook: %v", err)
	}
	script := GenericHookPath(grokDir)

	// Fake arcmux binary that records its argv, so the canonical mapping and
	// agent attribution are observable without a daemon.
	argvFile := filepath.Join(tmpDir, "argv.txt")
	fakeArcmux := filepath.Join(tmpDir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\necho \"$@\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(payload string) {
		t.Helper()
		cmd := exec.Command("/bin/sh", script)
		cmd.Env = append(os.Environ(),
			"ARCMUX_SESSION_ID=s-grok",
			"ARCMUX_HOOK_AGENT=grok",
			"ARCMUX_HOOK_OUTPUT_DIR="+outDir,
			"ARCMUX_BIN="+fakeArcmux,
		)
		cmd.Stdin = strings.NewReader(payload)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run hook: %v (%s)", err, out)
		}
	}

	run(`{"hookEventName":"user_prompt_submit","sessionId":"g-1","cwd":"/tmp"}`)
	run(`{"hookEventName":"pre_tool_use","toolName":"run_terminal_command","toolInput":{"command":"ls"}}`)
	run(`{"hookEventName":"stop","sessionId":"g-1"}`)

	// JSONL audit captured the raw grok event names.
	data, err := os.ReadFile(installer.OutputPath("s-grok"))
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("jsonl lines = %d, want 3 (%q)", len(lines), data)
	}
	ev, err := ParseHookEvent([]byte(lines[1]))
	if err != nil {
		t.Fatalf("parse event: %v", err)
	}
	if ev.Event != "pre_tool_use" || ev.Tool != "run_terminal_command" {
		t.Errorf("event = %+v, want pre_tool_use/run_terminal_command", ev)
	}

	// Canonical mutation used the grok agent and mapped snake_case events.
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(argv)), "\n")
	want := []string{
		"hook --agent grok --event prompt_submit --tool ",
		"hook --agent grok --event tool_start --tool run_terminal_command",
		"hook --agent grok --event turn_end --tool ",
	}
	for i, w := range want {
		if i >= len(calls) || strings.TrimSpace(calls[i]) != strings.TrimSpace(w) {
			t.Errorf("call %d = %q, want %q", i, calls[i], w)
		}
	}
}
