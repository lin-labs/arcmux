package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Tmux.SocketName != "arcmux" {
		t.Errorf("SocketName = %q, want %q", cfg.Tmux.SocketName, "arcmux")
	}
	if cfg.Tmux.DefaultSession != "agents" {
		t.Errorf("DefaultSession = %q, want %q", cfg.Tmux.DefaultSession, "agents")
	}
	if cfg.Hooks.AutoInstall != true {
		t.Error("AutoInstall should default to true")
	}
	if len(cfg.Agents) == 0 {
		t.Error("expected default agent profiles")
	}
	if _, ok := cfg.Agents["codex"]; !ok {
		t.Error("expected codex in default profiles")
	}
}

func TestLoad_CustomFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
[daemon]
socket = "/tmp/test-arcmux.sock"

[tmux]
socket_name = "test-arcmux"
default_session = "test-agents"

[health]
capture_interval = "10s"
idle_timeout_default = "120s"
stuck_timeout_default = "10m"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Daemon.Socket != "/tmp/test-arcmux.sock" {
		t.Errorf("Socket = %q, want %q", cfg.Daemon.Socket, "/tmp/test-arcmux.sock")
	}
	if cfg.Tmux.SocketName != "test-arcmux" {
		t.Errorf("SocketName = %q, want %q", cfg.Tmux.SocketName, "test-arcmux")
	}
	if cfg.Tmux.DefaultSession != "test-agents" {
		t.Errorf("DefaultSession = %q, want %q", cfg.Tmux.DefaultSession, "test-agents")
	}
}

func TestLoad_CustomAgentsRetainBuiltInExecProfiles(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
[agents.codex]
name = "codex"
start_command = "codex custom"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Agents["codex"].Transport != "tmux" {
		t.Fatalf("codex transport = %q, want tmux", cfg.Agents["codex"].Transport)
	}
	if _, ok := cfg.Agents["codex_exec"]; !ok {
		t.Fatal("expected codex_exec profile to remain available")
	}
	if _, ok := cfg.Agents["claude_exec"]; !ok {
		t.Fatal("expected claude_exec profile to remain available")
	}
}

func TestCaptureInterval(t *testing.T) {
	cfg := &Config{Health: HealthConfig{CaptureInterval: "10s"}}
	if d := cfg.CaptureInterval(); d != 10*time.Second {
		t.Errorf("CaptureInterval = %v, want 10s", d)
	}
}

func TestCaptureInterval_Default(t *testing.T) {
	cfg := &Config{Health: HealthConfig{CaptureInterval: ""}}
	if d := cfg.CaptureInterval(); d != 5*time.Second {
		t.Errorf("CaptureInterval = %v, want 5s", d)
	}
}

func TestStuckTimeout(t *testing.T) {
	cfg := &Config{Health: HealthConfig{StuckTimeout: "10m"}}
	if d := cfg.StuckTimeout(); d != 10*time.Minute {
		t.Errorf("StuckTimeout = %v, want 10m", d)
	}
}
