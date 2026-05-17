package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/lin-labs/arcmux/internal/profile"
)

// Config is the top-level configuration for the arcmux daemon.
type Config struct {
	Daemon  DaemonConfig             `toml:"daemon"`
	Tmux    TmuxConfig               `toml:"tmux"`
	Health  HealthConfig             `toml:"health"`
	Hooks   HooksConfig              `toml:"hooks"`
	Agents  map[string]profile.Profile `toml:"agents"`
}

type DaemonConfig struct {
	Socket  string `toml:"socket"`
	LogDir  string `toml:"log_dir"`
}

type TmuxConfig struct {
	SocketName     string `toml:"socket_name"`
	DefaultSession string `toml:"default_session"`
}

type HealthConfig struct {
	CaptureInterval string `toml:"capture_interval"`
	IdleTimeout     string `toml:"idle_timeout_default"`
	StuckTimeout    string `toml:"stuck_timeout_default"`
}

type HooksConfig struct {
	ClaudeHookDir string `toml:"claude_hook_dir"`
	HookOutputDir string `toml:"hook_output_dir"`
	AutoInstall   bool   `toml:"auto_install"`
}

// DefaultConfigPath returns ~/.config/arcmux/config.toml.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "config.toml")
}

// DefaultSocketPath returns ~/.config/arcmux/arcmux.sock.
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "arcmux.sock")
}

// Load reads configuration from the given path.
// Returns defaults if the file does not exist.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Daemon: DaemonConfig{
			Socket: DefaultSocketPath(),
			LogDir: defaultLogDir(),
		},
		Tmux: TmuxConfig{
			SocketName:     "arcmux",
			DefaultSession: "agents",
		},
		Health: HealthConfig{
			CaptureInterval: "5s",
			IdleTimeout:     "60s",
			StuckTimeout:    "5m",
		},
		Hooks: HooksConfig{
			ClaudeHookDir: "~/.claude",
			HookOutputDir: "/tmp/arcmux-hooks",
			AutoInstall:   true,
		},
		Agents: profile.DefaultProfiles(),
	}

	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	return cfg, nil
}

// DefaultAgentProfiles returns the built-in agent profiles.
func DefaultAgentProfiles() map[string]profile.Profile {
	return profile.DefaultProfiles()
}

func defaultLogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "logs")
}
