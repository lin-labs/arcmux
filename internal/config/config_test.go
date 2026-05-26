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
	if cfg.Mux.Backend != "cmux" {
		t.Errorf("Mux.Backend default = %q, want %q", cfg.Mux.Backend, "cmux")
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

// TestPulse_Defaults verifies the canonical lab-service cadences ship as
// defaults when no [pulse] table is provided.
func TestPulse_Defaults(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Pulse.Enabled {
		t.Error("Pulse.Enabled should default to true")
	}
	pp, err := cfg.Pulse.ParsePulse()
	if err != nil {
		t.Fatalf("ParsePulse: %v", err)
	}
	if pp.Cadence.Elon != 30*time.Second {
		t.Errorf("Elon cadence = %v, want 30s", pp.Cadence.Elon)
	}
	if pp.Cadence.Manager != 10*time.Second {
		t.Errorf("Manager cadence = %v, want 10s", pp.Cadence.Manager)
	}
	if pp.Cadence.IC != 5*time.Second {
		t.Errorf("IC cadence = %v, want 5s", pp.Cadence.IC)
	}
	if pp.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want 10s", pp.Interval)
	}
	if pp.DiscoveryInterval != 60*time.Second {
		t.Errorf("DiscoveryInterval = %v, want 60s", pp.DiscoveryInterval)
	}
	if pp.DataRoot == "" {
		t.Error("DataRoot default empty; expected ~/data")
	}
}

// TestPulse_PartialOverride proves that a [pulse] table specifying just
// one knob inherits the rest from defaults instead of silently zeroing.
func TestPulse_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	content := `
[pulse]
enabled = true
[pulse.cadence]
elon = "1m"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pp, err := cfg.Pulse.ParsePulse()
	if err != nil {
		t.Fatalf("ParsePulse: %v", err)
	}
	if pp.Cadence.Elon != time.Minute {
		t.Errorf("Elon = %v, want 1m (user override)", pp.Cadence.Elon)
	}
	if pp.Cadence.Manager != 10*time.Second {
		t.Errorf("Manager = %v, want 10s (default carried)", pp.Cadence.Manager)
	}
	if pp.Cadence.IC != 5*time.Second {
		t.Errorf("IC = %v, want 5s (default carried)", pp.Cadence.IC)
	}
	if pp.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want 10s (default carried)", pp.Interval)
	}
}

// TestPulse_BadDurationFailsLoud — a misconfigured cadence should error at
// parse time, not at runtime.
func TestPulse_BadDurationFailsLoud(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[pulse.cadence]\nelon = \"twenty seconds\"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := cfg.Pulse.ParsePulse(); err == nil {
		t.Fatal("ParsePulse accepted invalid duration; want error")
	}
}

// TestPulse_DisableLeavesOtherFieldsParseable: setting enabled=false should
// not leave durations empty (would otherwise blow up ParsePulse callers
// who still want to introspect the cadences).
func TestPulse_DisableLeavesOtherFieldsParseable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[pulse]\nenabled = false\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pp, err := cfg.Pulse.ParsePulse()
	if err != nil {
		t.Fatalf("ParsePulse with enabled=false: %v", err)
	}
	if pp.Enabled {
		t.Error("Pulse.Enabled override lost")
	}
	if pp.Cadence.Elon != 30*time.Second {
		t.Errorf("Elon cadence dropped on disable: %v", pp.Cadence.Elon)
	}
}

func TestLoad_MuxBackend(t *testing.T) {
	t.Run("explicit tmux", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte("[mux]\nbackend = \"tmux\"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Mux.Backend != "tmux" {
			t.Errorf("Mux.Backend = %q, want %q", cfg.Mux.Backend, "tmux")
		}
	})

	t.Run("no [mux] section defaults to cmux", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte("[daemon]\nsocket = \"/tmp/x.sock\"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Mux.Backend != "cmux" {
			t.Errorf("Mux.Backend = %q, want default cmux", cfg.Mux.Backend)
		}
	})

	t.Run("unknown backend is rejected", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte("[mux]\nbackend = \"bogus\"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("expected error on unknown backend, got nil")
		}
	})
}
