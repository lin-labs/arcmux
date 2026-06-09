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
	if !strings.HasSuffix(cfg.Hooks.SessionStateDir, filepath.Join("arcmux", "sessions")) {
		t.Fatalf("session_state_dir = %q, want .../arcmux/sessions", cfg.Hooks.SessionStateDir)
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
