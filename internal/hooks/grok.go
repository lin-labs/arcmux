package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// grokHookConfigName is the drop-in hook registration file arcmux installs
// into <grokHookDir>/hooks. Grok Build merges every ~/.grok/hooks/*.json at
// session start and global hooks are always trusted, so — unlike claude
// (settings.json) and codex (config.toml) — materializing this file IS the
// registration: no manual config edit or trust step is needed.
const grokHookConfigName = "arcmux-session.json"

// GrokHookConfigPath returns the absolute path of the grok hook registration
// file inside grokHookDir (typically ~/.grok).
func GrokHookConfigPath(grokHookDir string) string {
	return filepath.Join(grokHookDir, "hooks", grokHookConfigName)
}

// grokHookEvents are the grok lifecycle events arcmux subscribes to. They are
// delivered to the generic session hook script, whose payload parser already
// understands grok's {"hookEventName","toolName"} dialect and maps the
// snake_case event values onto the canonical arcmux events.
var grokHookEvents = []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// EnsureGrokHook installs (idempotently) both grok-side artifacts under
// grokHookDir/hooks:
//  1. the generic session hook script (same content as the claude install —
//     one script, two registration surfaces), and
//  2. the drop-in JSON registration pointing grok's lifecycle events at it.
//
// The script no-ops unless the spawned session carries ARCMUX_SESSION_ID, so
// the global registration is safe for non-arcmux grok sessions.
func (i *Installer) EnsureGrokHook(grokHookDir string) error {
	if !filepath.IsAbs(grokHookDir) {
		return fmt.Errorf("grok hook dir must be absolute, got %q", grokHookDir)
	}
	if err := i.installGenericHook(grokHookDir); err != nil {
		return err
	}

	scriptPath := GenericHookPath(grokHookDir)
	hookEntry := []map[string]any{
		{
			"hooks": []map[string]any{
				{"type": "command", "command": scriptPath, "timeout": 10},
			},
		},
	}
	events := make(map[string]any, len(grokHookEvents))
	for _, ev := range grokHookEvents {
		events[ev] = hookEntry
	}
	content, err := json.MarshalIndent(map[string]any{"hooks": events}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal grok hook config: %w", err)
	}
	content = append(content, '\n')

	path := GrokHookConfigPath(grokHookDir)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return nil
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write grok hook config: %w", err)
	}
	return nil
}
