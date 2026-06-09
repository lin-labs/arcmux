package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultProfiles(t *testing.T) {
	profiles := DefaultProfiles()

	for _, name := range []string{"codex", "claude", "grok", "codex_exec", "claude_exec", "grok_exec"} {
		p, ok := profiles[name]
		if !ok {
			t.Errorf("missing default profile: %s", name)
			continue
		}
		if p.Transport == TransportTmux && p.StartCommand == "" {
			t.Errorf("profile %s has empty StartCommand", name)
		}
		if p.Transport == TransportTmux && p.MaxNudgeRetries == 0 {
			t.Errorf("profile %s has zero MaxNudgeRetries", name)
		}
		if p.StuckTimeout == 0 {
			t.Errorf("profile %s has zero StuckTimeout", name)
		}
		if p.IdleTimeout == 0 {
			t.Errorf("profile %s has zero IdleTimeout", name)
		}
	}
}

func TestDefaultProfiles_Codex(t *testing.T) {
	p := DefaultProfiles()["codex"]

	if p.WorkingIndicator != "Working" {
		t.Errorf("WorkingIndicator = %q, want %q", p.WorkingIndicator, "Working")
	}
	if p.StartCommand != "codex --dangerously-bypass-approvals-and-sandbox --no-alt-screen" {
		t.Errorf("StartCommand = %q", p.StartCommand)
	}
	if p.TrustPromptPattern != "do you trust" {
		t.Errorf("TrustPromptPattern = %q, want %q", p.TrustPromptPattern, "do you trust")
	}
	if p.HookType != "codex_output" {
		t.Errorf("HookType = %q, want %q", p.HookType, "codex_output")
	}
}

func TestDefaultProfiles_Claude(t *testing.T) {
	p := DefaultProfiles()["claude"]

	if p.HookType != "claude_hooks" {
		t.Errorf("HookType = %q, want %q", p.HookType, "claude_hooks")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	want := filepath.Join(home, ".claude")
	if p.HookDir != want {
		t.Errorf("HookDir = %q, want %q", p.HookDir, want)
	}
	if !filepath.IsAbs(p.HookDir) {
		t.Errorf("HookDir = %q, must be absolute (no literal ~)", p.HookDir)
	}
	// Regression: the ready pattern must match a genuinely-ready Claude TUI,
	// not the old ">" which never appears in Claude Code v2.x (prompt glyph
	// is "❯"), and the working indicator must be set so the health monitor
	// doesn't falsely mark a busy-but-quiet session idle. See arcmux-jwf /
	// arcmux-u1c.
	if p.ReadyPattern == "" || p.ReadyPattern == ">" {
		t.Errorf("ReadyPattern = %q, must be a real ready signal (not empty or %q)", p.ReadyPattern, ">")
	}
	if p.WorkingIndicator == "" {
		t.Error("WorkingIndicator must be set so working->idle isn't driven by screen quiescence alone")
	}
	// Regression: Claude shows a folder-trust gate in any untrusted cwd. Without
	// a trust pattern the handshake hangs at that prompt until timeout (proven
	// by the real-agent e2e). TrustResponse "Enter" confirms the pre-highlighted
	// "Yes, I trust this folder".
	if p.TrustPromptPattern == "" {
		t.Error("TrustPromptPattern must be set so the handshake clears Claude's folder-trust gate")
	}
	if p.TrustResponse == "" {
		t.Error("TrustResponse must be set (Enter) to confirm the trust prompt")
	}
}

func TestDefaultProfiles_ExecDrivers(t *testing.T) {
	for name, wantDriver := range map[string]string{
		"codex_exec":  ExecDriverCodexExecJSON,
		"claude_exec": ExecDriverClaudePrintStreamJSON,
		"grok_exec":   ExecDriverGrokStreamJSON,
	} {
		p := DefaultProfiles()[name]
		if p.Transport != TransportExec {
			t.Fatalf("%s transport = %q, want %q", name, p.Transport, TransportExec)
		}
		if p.ExecDriver != wantDriver {
			t.Fatalf("%s driver = %q, want %q", name, p.ExecDriver, wantDriver)
		}
	}
}

func TestDefaultProfiles_Grok(t *testing.T) {
	p := DefaultProfiles()["grok"]

	if p.HookType != HookTypeGrok {
		t.Errorf("HookType = %q, want %q", p.HookType, HookTypeGrok)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if want := filepath.Join(home, ".grok"); p.HookDir != want {
		t.Errorf("HookDir = %q, want %q", p.HookDir, want)
	}
	if !filepath.IsAbs(p.HookDir) {
		t.Errorf("HookDir = %q, must be absolute (no literal ~)", p.HookDir)
	}
	// Observed live (Grok Build v0.2.x, --no-alt-screen): the bordered input
	// box renders "│ ❯" once the TUI is up; "Ctrl+Enter:interject" shows in
	// the footer only while a turn runs; the workspace gate "Run Grok Build
	// in a project directory?" wants Enter and can also appear after the
	// first prompt submit (hence it doubles as a stuck pattern).
	if p.ReadyPattern == "" {
		t.Error("ReadyPattern must be set")
	}
	if p.WorkingIndicator == "" {
		t.Error("WorkingIndicator must be set so working->idle isn't driven by screen quiescence alone")
	}
	if p.TrustPromptPattern == "" || p.TrustResponse == "" {
		t.Error("TrustPromptPattern/TrustResponse must clear grok's workspace gate")
	}
	found := false
	for _, s := range p.StuckTextPatterns {
		if s == p.TrustPromptPattern {
			found = true
		}
	}
	if !found {
		t.Error("workspace gate must also be a stuck pattern (it can appear post-handshake)")
	}
}

func TestClassesDeriveProfiles(t *testing.T) {
	classes := DefaultClasses()
	if len(classes) != 3 {
		t.Fatalf("DefaultClasses len = %d, want 3", len(classes))
	}
	profiles := DefaultProfiles()
	if len(profiles) != len(classes)*2 {
		t.Fatalf("DefaultProfiles len = %d, want %d (2 per class)", len(profiles), len(classes)*2)
	}
	for _, c := range classes {
		inter, ok := profiles[c.InteractiveProfileName()]
		if !ok {
			t.Fatalf("missing interactive profile for class %s", c.Name)
		}
		if inter.Transport != TransportTmux || inter.Class != c.Name {
			t.Errorf("class %s interactive: transport=%q class=%q", c.Name, inter.Transport, inter.Class)
		}
		ex, ok := profiles[c.ExecProfileName()]
		if !ok {
			t.Fatalf("missing exec profile for class %s", c.Name)
		}
		if ex.Transport != TransportExec || ex.Class != c.Name || ex.ExecDriver == "" {
			t.Errorf("class %s exec: transport=%q class=%q driver=%q", c.Name, ex.Transport, ex.Class, ex.ExecDriver)
		}
	}
}

func TestHookBacked(t *testing.T) {
	cases := map[string]bool{
		HookTypeClaude:           true,
		HookTypeCodexOutput:      true,
		HookTypeGrok:             true,
		HookTypeScreenOnly:       false,
		HookTypeStructuredOutput: false,
		"":                       false,
	}
	for hookType, want := range cases {
		if got := (Profile{HookType: hookType}).HookBacked(); got != want {
			t.Errorf("HookBacked(%q) = %v, want %v", hookType, got, want)
		}
	}
}
