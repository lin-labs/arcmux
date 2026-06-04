package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultProfiles(t *testing.T) {
	profiles := DefaultProfiles()

	for _, name := range []string{"codex", "claude", "codex_exec", "claude_exec"} {
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
}

func TestDefaultProfiles_ExecDrivers(t *testing.T) {
	codex := DefaultProfiles()["codex_exec"]
	if codex.Transport != TransportExec {
		t.Fatalf("codex_exec transport = %q, want %q", codex.Transport, TransportExec)
	}
	if codex.ExecDriver != ExecDriverCodexExecJSON {
		t.Fatalf("codex_exec driver = %q, want %q", codex.ExecDriver, ExecDriverCodexExecJSON)
	}

	claude := DefaultProfiles()["claude_exec"]
	if claude.Transport != TransportExec {
		t.Fatalf("claude_exec transport = %q, want %q", claude.Transport, TransportExec)
	}
	if claude.ExecDriver != ExecDriverClaudePrintStreamJSON {
		t.Fatalf("claude_exec driver = %q, want %q", claude.ExecDriver, ExecDriverClaudePrintStreamJSON)
	}
}
