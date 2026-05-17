package profile

import "testing"

func TestDefaultProfiles(t *testing.T) {
	profiles := DefaultProfiles()

	for _, name := range []string{"codex", "claude", "grok"} {
		p, ok := profiles[name]
		if !ok {
			t.Errorf("missing default profile: %s", name)
			continue
		}
		if p.StartCommand == "" {
			t.Errorf("profile %s has empty StartCommand", name)
		}
		if p.MaxNudgeRetries == 0 {
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
	if p.HookDir != "~/.claude" {
		t.Errorf("HookDir = %q, want %q", p.HookDir, "~/.claude")
	}
}
