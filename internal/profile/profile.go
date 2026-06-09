package profile

import (
	"os"
	"path/filepath"
	"time"
)

// defaultHookDir returns an absolute path to the given dotted config
// directory under the user's home (e.g. ".claude" → ~/.claude). Built once so
// the default Profile values never carry a literal "~" — filepath.Join
// doesn't expand tildes, and a literal "~" flowing into os.MkdirAll silently
// creates a "~" subdirectory under the caller's cwd (which has happened in
// the wild — see git history).
func defaultHookDir(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return dir
	}
	return filepath.Join(home, dir)
}

const (
	TransportTmux = "tmux"
	TransportExec = "exec"

	ExecDriverCodexExecJSON         = "codex_exec_json"
	ExecDriverClaudePrintStreamJSON = "claude_print_stream_json"
	ExecDriverGrokStreamJSON        = "grok_stream_json"

	// Hook types. Hook-backed types feed the centralized session state under
	// ~/data/arcmux (via the `arcmux hook` CLI) that the hooks judge and any
	// external subscriber read.
	HookTypeClaude           = "claude_hooks"
	HookTypeCodexOutput      = "codex_output"
	HookTypeGrok             = "grok_hooks"
	HookTypeScreenOnly       = "screen_only"
	HookTypeStructuredOutput = "structured_output"
)

// Profile defines agent-specific behavior for the runtime service.
type Profile struct {
	Class        string `toml:"class"` // LLM class this profile belongs to ("codex", "claude", "grok")
	Transport    string `toml:"transport"`
	ExecDriver   string `toml:"exec_driver"`
	Name         string `toml:"name"`
	StartCommand string `toml:"start_command"`
	// SessionStartArgs is an optional template appended to StartCommand when a
	// session launches. Placeholders {session_id} and {hook_dir} are replaced
	// with shell-quoted per-session values. Used by grok to give every session
	// its own leader process: hooks execute inside the leader, and a shared
	// leader spawned by an earlier non-arcmux client would not carry this
	// session's ARCMUX_* env — so each session binds a private leader socket
	// that inherits the pane env (the leader exits when its last client
	// disconnects, so these clean up with the session).
	SessionStartArgs   string        `toml:"session_start_args"`
	ReadyPattern       string        `toml:"ready_pattern"`
	TrustPromptPattern string        `toml:"trust_prompt_pattern"`
	TrustResponse      string        `toml:"trust_response"`
	WorkingIndicator   string        `toml:"working_indicator"`
	StuckTextPatterns  []string      `toml:"stuck_text_patterns"`
	StuckTimeout       time.Duration `toml:"stuck_timeout"`
	IdleTimeout        time.Duration `toml:"idle_timeout"`
	NudgeCommand       string        `toml:"nudge_command"`
	MaxNudgeRetries    int           `toml:"max_nudge_retries"`
	HookType           string        `toml:"hook_type"` // see HookType* constants
	HookDir            string        `toml:"hook_dir"`
}

// HookBacked reports whether this profile's agent emits lifecycle hook events
// into the centralized per-session state (via `arcmux hook`). Hook-backed
// sessions get the env loader prefix, a seeded session-state doc, and a
// per-session env file; screen-only and structured-output agents do not.
func (p Profile) HookBacked() bool {
	switch p.HookType {
	case HookTypeClaude, HookTypeCodexOutput, HookTypeGrok:
		return true
	default:
		return false
	}
}

// Class groups the two run modes of one LLM under a single identity: the
// interactive TUI spawned in a tmux pane, and the headless one-shot exec
// (codex exec / claude -p / grok -p). Adding a new LLM means adding one
// Class here — the flat profile map, hook gating, and exec-driver routing
// all derive from it.
type Class struct {
	Name        string  // "codex", "claude", "grok"
	Interactive Profile // tmux transport profile
	Exec        Profile // exec transport profile (one-shot/headless)
}

// InteractiveProfileName / ExecProfileName are the keys the class's profiles
// get in the flat profile map ("<class>" and "<class>_exec"). These names are
// the public contract callers use as the `agent` field in CreateSession.
func (c Class) InteractiveProfileName() string { return c.Name }
func (c Class) ExecProfileName() string        { return c.Name + "_exec" }

// DefaultClasses returns the built-in LLM classes.
func DefaultClasses() []Class {
	return []Class{
		{
			Name: "codex",
			Interactive: Profile{
				Class:              "codex",
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
				HookType:           HookTypeCodexOutput,
			},
			Exec: Profile{
				Class:           "codex",
				Transport:       TransportExec,
				ExecDriver:      ExecDriverCodexExecJSON,
				Name:            "codex_exec",
				StuckTimeout:    30 * time.Minute,
				IdleTimeout:     60 * time.Second,
				MaxNudgeRetries: 0,
				HookType:        HookTypeStructuredOutput,
			},
		},
		{
			Name: "claude",
			Interactive: Profile{
				Class:        "claude",
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
				// Claude shows a folder-trust gate in any untrusted cwd:
				// "Is this a project you created or one you trust? … 1. Yes, I
				// trust this folder" with "Yes" pre-highlighted, so Enter confirms.
				// Without this the handshake hangs at the trust prompt in a fresh
				// directory. "trust this folder" matches option 1's label.
				TrustPromptPattern: "trust this folder",
				TrustResponse:      "Enter",
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
				HookType:          HookTypeClaude,
				HookDir:           defaultHookDir(".claude"),
			},
			Exec: Profile{
				Class:           "claude",
				Transport:       TransportExec,
				ExecDriver:      ExecDriverClaudePrintStreamJSON,
				Name:            "claude_exec",
				StuckTimeout:    30 * time.Minute,
				IdleTimeout:     60 * time.Second,
				MaxNudgeRetries: 0,
				HookType:        HookTypeStructuredOutput,
			},
		},
		{
			Name: "grok",
			Interactive: Profile{
				Class:        "grok",
				Transport:    TransportTmux,
				Name:         "grok",
				StartCommand: "grok --no-alt-screen --permission-mode bypassPermissions",
				// Verified live: grok runs hooks in its LEADER process, not the
				// TUI client. A pre-existing shared leader (~/.grok/leader.sock,
				// e.g. spawned by cmux) lacks this session's ARCMUX_* env, so
				// hooks would silently no-op. A per-session leader socket makes
				// the client spawn a fresh leader that inherits the pane env.
				// The ~/.grok/leader-*.sock naming keeps `grok leader list/kill`
				// able to discover it.
				SessionStartArgs: "--leader-socket {hook_dir}/leader-arcmux-{session_id}.sock",
				// Grok Build's TUI draws a bordered input box ("│ ❯") once it is
				// up; the box-drawing prefix keeps it from matching a bare zsh
				// "❯" prompt on the pre-launch shell line.
				ReadyPattern: "│ ❯",
				// Grok shows a workspace gate ("Run Grok Build in a project
				// directory?") with the current directory pre-selected, so Enter
				// confirms. Unlike claude's folder-trust gate it can also appear
				// after the first prompt submit, hence the same text in
				// StuckTextPatterns so a nudge resolves it mid-delivery.
				TrustPromptPattern: "Run Grok Build in a project directory",
				TrustResponse:      "Enter",
				// Footer shows "Ctrl+Enter:interject" only while a turn is
				// running and drops it when idle — same role as claude's
				// "esc to interrupt".
				WorkingIndicator:  "Ctrl+Enter:interject",
				StuckTextPatterns: []string{"Run Grok Build in a project directory"},
				StuckTimeout:      5 * time.Minute,
				IdleTimeout:       60 * time.Second,
				NudgeCommand:      "Enter",
				MaxNudgeRetries:   3,
				// Grok loads drop-in hook files from ~/.grok/hooks/*.json
				// (always trusted), so arcmux's hook registration is fully
				// automatic — no manual settings edit like claude/codex.
				HookType: HookTypeGrok,
				HookDir:  defaultHookDir(".grok"),
			},
			Exec: Profile{
				Class:           "grok",
				Transport:       TransportExec,
				ExecDriver:      ExecDriverGrokStreamJSON,
				Name:            "grok_exec",
				StuckTimeout:    30 * time.Minute,
				IdleTimeout:     60 * time.Second,
				MaxNudgeRetries: 0,
				HookType:        HookTypeStructuredOutput,
			},
		},
	}
}

// DefaultProfiles returns built-in profiles for known agents, derived from
// DefaultClasses: each class contributes "<name>" (interactive tmux) and
// "<name>_exec" (headless one-shot).
func DefaultProfiles() map[string]Profile {
	classes := DefaultClasses()
	profiles := make(map[string]Profile, len(classes)*2)
	for _, c := range classes {
		profiles[c.InteractiveProfileName()] = c.Interactive
		profiles[c.ExecProfileName()] = c.Exec
	}
	return profiles
}
