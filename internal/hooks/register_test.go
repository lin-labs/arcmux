package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterClaudeHooksFreshFile(t *testing.T) {
	t.Parallel()
	claudeDir := filepath.Join(t.TempDir(), "claude")

	changed, err := RegisterClaudeHooks(claudeDir)
	if err != nil {
		t.Fatalf("RegisterClaudeHooks: %v", err)
	}
	if !changed {
		t.Fatal("RegisterClaudeHooks changed = false, want true for missing file")
	}

	var cfg hookConfig
	readHookConfig(t, ClaudeSettingsPath(claudeDir), &cfg)
	for _, event := range registrationEvents {
		entries := cfg.Hooks[event]
		if len(entries) != 1 || len(entries[0].Hooks) != 1 {
			t.Fatalf("%s entries = %+v, want one command hook", event, entries)
		}
		hook := entries[0].Hooks[0]
		if hook.Type != "command" {
			t.Errorf("%s type = %q, want command", event, hook.Type)
		}
		wantCommand := `test -f "` + GenericHookPath(claudeDir) + `" || exit 0; sh "` + GenericHookPath(claudeDir) + `"`
		if hook.Command != wantCommand {
			t.Errorf("%s command = %q, want %q", event, hook.Command, wantCommand)
		}
		if hook.Timeout != 5 {
			t.Errorf("%s timeout = %d, want 5", event, hook.Timeout)
		}
		if hook.StatusMessage != "" {
			t.Errorf("%s statusMessage = %q, want empty", event, hook.StatusMessage)
		}
	}
}

func TestRegisterCodexHooksPreservesExistingHooksAndKeys(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	codexDir := filepath.Join(tmp, ".codex")
	hookDir := filepath.Join(codexDir, "hooks")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "alpha": {"untouched": true},
  "hooks": {
    "Stop": [
      {
        "matcher": "keep",
        "hooks": [
          {"type":"command","command":"echo keep","timeout":12,"extra":{"x":true}}
        ]
      }
    ],
    "SessionStart": [
      {"hooks":[{"type":"command","command":"echo session","timeout":3}]}
    ]
  },
  "omega": ["preserve", 1]
}
`
	path := CodexHooksConfigPath(hookDir)
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := RegisterCodexHooks(hookDir)
	if err != nil {
		t.Fatalf("RegisterCodexHooks: %v", err)
	}
	if !changed {
		t.Fatal("RegisterCodexHooks changed = false, want true")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 preserved", info.Mode().Perm())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"alpha": {"untouched": true}`) {
		t.Fatal("unrelated top-level key formatting was not preserved")
	}
	if !strings.Contains(string(got), `"matcher": "keep"`) || !strings.Contains(string(got), `"extra":{"x":true}`) {
		t.Fatal("existing hook entry was not preserved")
	}

	var cfg hookConfig
	if err := json.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("updated hooks.json is invalid JSON: %v\n%s", err, got)
	}
	if len(cfg.Hooks["SessionStart"]) != 1 {
		t.Fatalf("SessionStart entries = %d, want existing unrelated event preserved", len(cfg.Hooks["SessionStart"]))
	}
	stop := cfg.Hooks["Stop"]
	if len(stop) != 2 {
		t.Fatalf("Stop entries = %d, want existing + arcmux", len(stop))
	}
	for _, event := range registrationEvents {
		if countHooksWithScript(cfg.Hooks[event], CodexHookPath(hookDir)) != 1 {
			t.Fatalf("%s arcmux hook count = %d, want 1", event, countHooksWithScript(cfg.Hooks[event], CodexHookPath(hookDir)))
		}
	}
}

func TestRegisterHooksIdempotentReRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		target   func(root string) string
		register func(string) (bool, error)
		path     func(string) string
	}{
		{
			name:     "claude",
			target:   func(root string) string { return filepath.Join(root, ".claude") },
			register: RegisterClaudeHooks,
			path:     ClaudeSettingsPath,
		},
		{
			name:     "codex",
			target:   func(root string) string { return filepath.Join(root, ".codex", "hooks") },
			register: RegisterCodexHooks,
			path:     CodexHooksConfigPath,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := tc.target(t.TempDir())
			changed, err := tc.register(target)
			if err != nil {
				t.Fatalf("first register: %v", err)
			}
			if !changed {
				t.Fatal("first register changed = false, want true")
			}
			first, err := os.ReadFile(tc.path(target))
			if err != nil {
				t.Fatal(err)
			}
			changed, err = tc.register(target)
			if err != nil {
				t.Fatalf("second register: %v", err)
			}
			if changed {
				t.Fatal("second register changed = true, want false")
			}
			second, err := os.ReadFile(tc.path(target))
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Fatalf("second register rewrote file\nfirst:\n%s\nsecond:\n%s", first, second)
			}
		})
	}
}

func TestRegisterHooksMalformedJSONRefusesOverwrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		target   func(root string) string
		register func(string) (bool, error)
		path     func(string) string
	}{
		{
			name:     "claude",
			target:   func(root string) string { return filepath.Join(root, ".claude") },
			register: RegisterClaudeHooks,
			path:     ClaudeSettingsPath,
		},
		{
			name:     "codex",
			target:   func(root string) string { return filepath.Join(root, ".codex", "hooks") },
			register: RegisterCodexHooks,
			path:     CodexHooksConfigPath,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := tc.target(t.TempDir())
			path := tc.path(target)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			before := []byte(`{"hooks":`)
			if err := os.WriteFile(path, before, 0o644); err != nil {
				t.Fatal(err)
			}
			changed, err := tc.register(target)
			if err == nil {
				t.Fatal("register accepted malformed JSON")
			}
			if changed {
				t.Fatal("register changed = true for malformed JSON")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("malformed file overwritten: got %q want %q", after, before)
			}
		})
	}
}

func TestRegisterCodexHooksDetectsHomePathAlreadyRegistered(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	hookDir := filepath.Join(tmp, ".codex", "hooks")
	if err := os.MkdirAll(filepath.Dir(hookDir), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "test -f \"$HOME/.codex/hooks/arcmux-codex-hook.sh\" || exit 0; sh \"$HOME/.codex/hooks/arcmux-codex-hook.sh\" Stop",
            "timeout": 5000,
            "statusMessage": "arcmux session state"
          }
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(CodexHooksConfigPath(hookDir), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RegisterCodexHooks(hookDir)
	if err != nil {
		t.Fatalf("RegisterCodexHooks: %v", err)
	}
	if !changed {
		t.Fatal("RegisterCodexHooks changed = false, want missing events added")
	}
	var cfg hookConfig
	readHookConfig(t, CodexHooksConfigPath(hookDir), &cfg)
	if countHooksWithScript(cfg.Hooks["Stop"], "$HOME/.codex/hooks/arcmux-codex-hook.sh") != 1 {
		t.Fatalf("Stop arcmux hook should not duplicate existing $HOME command: %+v", cfg.Hooks["Stop"])
	}
}

type hookConfig struct {
	Hooks map[string][]hookEntry `json:"hooks"`
}

type hookEntry struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []commandHook `json:"hooks"`
}

type commandHook struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout"`
	StatusMessage string `json:"statusMessage,omitempty"`
}

func readHookConfig(t *testing.T, path string, out *hookConfig) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
}

func countHooksWithScript(entries []hookEntry, scriptPath string) int {
	count := 0
	for _, entry := range entries {
		for _, hook := range entry.Hooks {
			if strings.Contains(hook.Command, scriptPath) {
				count++
			}
		}
	}
	return count
}
