package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliveryDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Default is the auto cascade: hooks ground truth when available,
	// typesafe otherwise, heuristic without an API key.
	if cfg.Delivery.Judge != "auto" {
		t.Fatalf("default judge = %q, want auto", cfg.Delivery.Judge)
	}
	if cfg.Hooks.SessionStateDir == "" || !filepath.IsAbs(cfg.Hooks.SessionStateDir) {
		t.Fatalf("session_state_dir = %q, want absolute default", cfg.Hooks.SessionStateDir)
	}
	// ~/data/mux is the PROTOCOL state root — shared by every subscriber,
	// deliberately not named after this application.
	if !strings.HasSuffix(cfg.Hooks.SessionStateDir, filepath.Join("mux", "sessions")) {
		t.Fatalf("session_state_dir = %q, want .../mux/sessions", cfg.Hooks.SessionStateDir)
	}
	if !strings.HasSuffix(cfg.Hooks.HookOutputDir, filepath.Join("mux", "hook-output")) {
		t.Fatalf("hook_output_dir = %q, want .../mux/hook-output", cfg.Hooks.HookOutputDir)
	}
}

func TestDeliveryJudgeOverrideAndExpand(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`
[delivery]
judge = "hooks"

[hooks]
session_state_dir = "~/data/arcmux/sessions"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Delivery.Judge != "hooks" {
		t.Fatalf("judge = %q, want hooks", cfg.Delivery.Judge)
	}
	if strings.HasPrefix(cfg.Hooks.SessionStateDir, "~") {
		t.Fatalf("session_state_dir not expanded: %q", cfg.Hooks.SessionStateDir)
	}
}

func TestDeliveryUnknownJudgeFailsLoud(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[delivery]\njudge = \"magic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown delivery.judge")
	}
}

func TestCurrentWorkProviderPersistsWithoutServiceEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(`
[current_work]
provider = "openai"
model = "gpt-5.4-mini"
api_key_file = "~/.config/arcmux/openai-api-key"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentWork.Provider != "openai" || cfg.CurrentWork.Model != "gpt-5.4-mini" ||
		cfg.CurrentWork.APIKeyFile != filepath.Join(home, ".config", "arcmux", "openai-api-key") {
		t.Fatalf("current-work config=%+v", cfg.CurrentWork)
	}
}

func TestCurrentWorkProviderRejectsUnknownOrRelativeCredentialFile(t *testing.T) {
	for name, body := range map[string]string{
		"unknown provider": `[current_work]
provider = "agent-cli"
`,
		"relative key file": `[current_work]
provider = "openai"
api_key_file = "relative.key"
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("unsafe current-work config was accepted")
			}
		})
	}
}

func TestProtocolStateRootFollowsSessionStateDirectory(t *testing.T) {
	cases := []struct {
		name     string
		stateDir string
		want     string
	}{
		{name: "protocol sessions", stateDir: "/var/lib/mux/sessions", want: "/var/lib/mux"},
		{name: "custom state", stateDir: "/var/lib/custom/state", want: "/var/lib/custom/mesh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Hooks: HooksConfig{SessionStateDir: tc.stateDir}}
			got, err := cfg.ProtocolStateRoot()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ProtocolStateRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}
