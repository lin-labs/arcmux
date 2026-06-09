package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
)

func TestDaemonRegisterAgentHooksHonorsAutoRegister(t *testing.T) {
	tmp := t.TempDir()
	cfg := daemonHookRegistrationTestConfig(tmp)
	d := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	d.registerAgentHooks()
	if _, err := os.Stat(hooks.ClaudeSettingsPath(cfg.Hooks.ClaudeHookDir)); !os.IsNotExist(err) {
		t.Fatalf("AutoRegister=false should not create claude settings, err=%v", err)
	}
	if _, err := os.Stat(hooks.CodexHooksConfigPath(cfg.Hooks.CodexHookDir)); !os.IsNotExist(err) {
		t.Fatalf("AutoRegister=false should not create codex hooks.json, err=%v", err)
	}

	cfg.Hooks.AutoRegister = true
	d = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.registerAgentHooks()
	if _, err := os.Stat(hooks.ClaudeSettingsPath(cfg.Hooks.ClaudeHookDir)); err != nil {
		t.Fatalf("AutoRegister=true did not create claude settings: %v", err)
	}
	if _, err := os.Stat(hooks.CodexHooksConfigPath(cfg.Hooks.CodexHookDir)); err != nil {
		t.Fatalf("AutoRegister=true did not create codex hooks.json: %v", err)
	}
}

func daemonHookRegistrationTestConfig(tmp string) *config.Config {
	return &config.Config{
		Daemon: config.DaemonConfig{
			Socket:   filepath.Join(tmp, "arcmux.sock"),
			LogDir:   filepath.Join(tmp, "logs"),
			HTTPAddr: "",
		},
		Mux:  config.MuxConfig{Backend: "cmux"},
		Tmux: config.TmuxConfig{SocketName: "arcmux-test"},
		Hooks: config.HooksConfig{
			ClaudeHookDir:   filepath.Join(tmp, ".claude"),
			CodexHookDir:    filepath.Join(tmp, ".codex", "hooks"),
			GrokHookDir:     filepath.Join(tmp, ".grok"),
			HookOutputDir:   filepath.Join(tmp, "hook-output"),
			SessionStateDir: filepath.Join(tmp, "sessions"),
			AutoInstall:     true,
			AutoRegister:    false,
		},
		Delivery: config.DeliveryConfig{Judge: "heuristic"},
	}
}
