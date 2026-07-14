package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if cfg.Hooks.AutoRegister {
		t.Error("AutoRegister should default to false")
	}
	if len(cfg.Agents) == 0 {
		t.Error("expected default agent profiles")
	}
	if _, ok := cfg.Agents["codex"]; !ok {
		t.Error("expected codex in default profiles")
	}
	mesh, err := cfg.Mesh.Parse()
	if err != nil {
		t.Fatalf("Mesh.Parse: %v", err)
	}
	if !mesh.Enabled || mesh.ListenAddr != "127.0.0.1:7788" || mesh.ReconnectMin != 500*time.Millisecond || mesh.DeadAfter != 60*time.Second {
		t.Fatalf("unexpected mesh defaults: %+v", mesh)
	}
}

func TestMeshParseRejectsUnsafeOrInvalidSettings(t *testing.T) {
	tests := []MeshConfig{
		{ListenAddr: "0.0.0.0:7788"},
		{ListenAddr: "127.0.0.1:7788", StaleAfter: "1m", DeadAfter: "30s"},
		{ListenAddr: "127.0.0.1:7788", ReconnectMin: "5s", ReconnectMax: "1s"},
		{ListenAddr: "127.0.0.1:7788", WriterQueue: -1},
	}
	for _, tc := range tests {
		if _, err := tc.Parse(); err == nil {
			t.Fatalf("Parse accepted unsafe config: %+v", tc)
		}
	}
}

func TestLoadInvalidMeshDoesNotBlockLocalConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[mesh]\nlisten_addr = \"0.0.0.0:7788\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load should leave mesh failure to best-effort daemon path: %v", err)
	}
	if _, err := cfg.Mesh.Parse(); err == nil {
		t.Fatal("Mesh.Parse accepted unsafe address")
	}
}

func TestLoad_HooksAutoRegisterOptIn(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[hooks]\nauto_register = true\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Hooks.AutoRegister {
		t.Fatal("AutoRegister should honor [hooks].auto_register = true")
	}
	if !cfg.Hooks.AutoInstall {
		t.Fatal("AutoInstall default should survive partial [hooks] override")
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

// TestPulse_Defaults verifies the canonical single-target cadence ships
// as the default when no [pulse] table is provided. Post-C3 the
// role-class cadences (elon/manager/ic) collapsed to one Interval.
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
	if pp.Cadence.Interval != 30*time.Second {
		t.Errorf("Cadence.Interval = %v, want 30s", pp.Cadence.Interval)
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
interval = "1m"
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
	if pp.Cadence.Interval != time.Minute {
		t.Errorf("Cadence.Interval = %v, want 1m (user override)", pp.Cadence.Interval)
	}
	if pp.Interval != 10*time.Second {
		t.Errorf("Interval = %v, want 10s (default carried)", pp.Interval)
	}
}

// TestPulse_BadDurationFailsLoud — a misconfigured cadence should error
// at parse time, not at runtime.
func TestPulse_BadDurationFailsLoud(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[pulse.cadence]\ninterval = \"twenty seconds\"\n"), 0o644); err != nil {
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

// TestPulse_DisableLeavesOtherFieldsParseable: setting enabled=false
// should not leave durations empty (would otherwise blow up ParsePulse
// callers who still want to introspect the cadences).
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
	if pp.Cadence.Interval != 30*time.Second {
		t.Errorf("Cadence.Interval dropped on disable: %v", pp.Cadence.Interval)
	}
}

// TestLoad_TildeExpansion is the regression for the "~/" directory created
// in the daemon's cwd. A literal "~/.claude" in TOML must resolve to an
// absolute path under $HOME; downstream code (especially hook installers)
// asserts IsAbs and would refuse otherwise.
func TestLoad_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	content := `
[daemon]
socket = "~/.config/arcmux/arcmux.sock"
log_dir = "~/arcmux-logs"

[hooks]
claude_hook_dir = "~/.claude"
hook_output_dir = "~/.claude/hooks"

[pulse]
data_root = "~/data"

[agents.claude]
hook_dir = "~/.claude"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"daemon.socket", cfg.Daemon.Socket, filepath.Join(home, ".config/arcmux/arcmux.sock")},
		{"daemon.log_dir", cfg.Daemon.LogDir, filepath.Join(home, "arcmux-logs")},
		{"hooks.claude_hook_dir", cfg.Hooks.ClaudeHookDir, filepath.Join(home, ".claude")},
		{"hooks.hook_output_dir", cfg.Hooks.HookOutputDir, filepath.Join(home, ".claude/hooks")},
		{"pulse.data_root", cfg.Pulse.DataRoot, filepath.Join(home, "data")},
		{"agents.claude.hook_dir", cfg.Agents["claude"].HookDir, filepath.Join(home, ".claude")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (must be absolute under $HOME, not a literal tilde)",
				c.name, c.got, c.want)
		}
		if !filepath.IsAbs(c.got) {
			t.Errorf("%s = %q, must be absolute after Load", c.name, c.got)
		}
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

// TestScreenLogDir verifies that ScreenLogDir returns <DataRoot>/arcmux/sessions,
// falling back to ~/data when DataRoot is empty.
func TestScreenLogDir(t *testing.T) {
	t.Run("explicit DataRoot", func(t *testing.T) {
		cfg := &Config{DataRoot: "/custom/data"}
		got := cfg.ScreenLogDir()
		want := "/custom/data/arcmux/sessions"
		if got != want {
			t.Errorf("ScreenLogDir = %q, want %q", got, want)
		}
	})

	t.Run("empty DataRoot falls back to home/data", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir: %v", err)
		}
		cfg := &Config{}
		got := cfg.ScreenLogDir()
		want := filepath.Join(home, "data", "arcmux", "sessions")
		if got != want {
			t.Errorf("ScreenLogDir = %q, want %q", got, want)
		}
	})

	t.Run("tilde DataRoot is expanded by expandConfigPaths", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir: %v", err)
		}
		cfg := &Config{DataRoot: "~/data"}
		expandConfigPaths(cfg)
		got := cfg.ScreenLogDir()
		if strings.Contains(got, "~") {
			t.Errorf("ScreenLogDir still contains literal ~: %q", got)
		}
		want := filepath.Join(home, "data", "arcmux", "sessions")
		if got != want {
			t.Errorf("ScreenLogDir = %q, want %q", got, want)
		}
	})
}
