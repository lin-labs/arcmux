package profile

import (
	"os"
	"path/filepath"
	"time"
)

// defaultClaudeHookDir returns an absolute path to the user's ~/.claude
// directory. Built once so the default Profile values never carry a
// literal "~" — filepath.Join doesn't expand tildes, and a literal "~"
// flowing into os.MkdirAll silently creates a "~" subdirectory under the
// caller's cwd (which has happened in the wild — see git history).
func defaultClaudeHookDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

const (
	TransportTmux = "tmux"
	TransportExec = "exec"

	ExecDriverCodexExecJSON         = "codex_exec_json"
	ExecDriverClaudePrintStreamJSON = "claude_print_stream_json"
)

// Profile defines agent-specific behavior for the runtime service.
type Profile struct {
	Transport          string        `toml:"transport"`
	ExecDriver         string        `toml:"exec_driver"`
	Name               string        `toml:"name"`
	StartCommand       string        `toml:"start_command"`
	ReadyPattern       string        `toml:"ready_pattern"`
	TrustPromptPattern string        `toml:"trust_prompt_pattern"`
	TrustResponse      string        `toml:"trust_response"`
	WorkingIndicator   string        `toml:"working_indicator"`
	StuckTextPatterns  []string      `toml:"stuck_text_patterns"`
	StuckTimeout       time.Duration `toml:"stuck_timeout"`
	IdleTimeout        time.Duration `toml:"idle_timeout"`
	NudgeCommand       string        `toml:"nudge_command"`
	MaxNudgeRetries    int           `toml:"max_nudge_retries"`
	HookType           string        `toml:"hook_type"` // "claude_hooks", "codex_output", "screen_only"
	HookDir            string        `toml:"hook_dir"`
}

// DefaultProfiles returns built-in profiles for known agents.
func DefaultProfiles() map[string]Profile {
	return map[string]Profile{
		"codex": {
			Transport:          TransportTmux,
			Name:               "codex",
			StartCommand:       "codex --dangerously-bypass-approvals-and-sandbox --no-alt-screen",
			ReadyPattern:       "›",
			TrustPromptPattern: "do you trust",
			TrustResponse:      "Enter",
			WorkingIndicator:   "Working",
			StuckTextPatterns:  []string{"permission prompt", "do you want to allow"},
			StuckTimeout:       5 * time.Minute,
			IdleTimeout:        60 * time.Second,
			NudgeCommand:       "Enter",
			MaxNudgeRetries:    3,
			HookType:           "codex_output",
		},
		"claude": {
			Transport:    TransportTmux,
			Name:         "claude",
			StartCommand: "claude --dangerously-skip-permissions --remote-control",
			// Claude Code v2.x renders its prompt as "❯" (U+276F) and shows
			// no bare ">" on screen, so the old ">" ready pattern never
			// matched and the handshake always timed out into StateFailed.
			// "Remote Control active" is printed by --remote-control (which
			// both the default StartCommand and the HTTP "cld --remote-control"
			// launch path pass, and which arcmux *requires* to drive the pane
			// at all) and appears only once the TUI is fully up — so it's the
			// most robust readiness signal and, unlike "❯", can't match the
			// pre-launch shell prompt line.
			ReadyPattern: "Remote Control active",
			// WorkingIndicator gates the health monitor's working->idle
			// transition: while it is visible the session is never declared
			// idle. Claude shows "esc to interrupt" in its footer throughout
			// a turn (thinking, streaming, and silent tool execution) and
			// drops it ("← for agents") only when done. Without this, a long
			// silent tool call whose screen happens not to change for the
			// quiescence window could be falsely marked idle and have a queued
			// inbox message delivered mid-execution. See arcmux-u1c.
			WorkingIndicator:  "esc to interrupt",
			StuckTextPatterns: []string{"tool denied", "would you like"},
			StuckTimeout:      5 * time.Minute,
			IdleTimeout:       60 * time.Second,
			NudgeCommand:      "Enter",
			MaxNudgeRetries:   3,
			HookType:          "claude_hooks",
			HookDir:           defaultClaudeHookDir(),
		},
		"codex_exec": {
			Transport:       TransportExec,
			ExecDriver:      ExecDriverCodexExecJSON,
			Name:            "codex_exec",
			StuckTimeout:    30 * time.Minute,
			IdleTimeout:     60 * time.Second,
			MaxNudgeRetries: 0,
			HookType:        "structured_output",
		},
		"claude_exec": {
			Transport:       TransportExec,
			ExecDriver:      ExecDriverClaudePrintStreamJSON,
			Name:            "claude_exec",
			StuckTimeout:    30 * time.Minute,
			IdleTimeout:     60 * time.Second,
			MaxNudgeRetries: 0,
			HookType:        "structured_output",
		},
	}
}
