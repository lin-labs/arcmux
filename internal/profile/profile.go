package profile

import "time"

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
			Transport:         TransportTmux,
			Name:              "claude",
			StartCommand:      "claude --dangerously-skip-permissions",
			ReadyPattern:      ">",
			StuckTextPatterns: []string{"tool denied", "would you like"},
			StuckTimeout:      5 * time.Minute,
			IdleTimeout:       60 * time.Second,
			NudgeCommand:      "Enter",
			MaxNudgeRetries:   3,
			HookType:          "claude_hooks",
			HookDir:           "~/.claude",
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
