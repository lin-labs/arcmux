package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/lin-labs/arcmux/internal/profile"
)

// Config is the top-level configuration for the arcmux daemon.
type Config struct {
	Daemon DaemonConfig               `toml:"daemon"`
	Tmux   TmuxConfig                 `toml:"tmux"`
	Health HealthConfig               `toml:"health"`
	Hooks  HooksConfig                `toml:"hooks"`
	Pulse  PulseConfig                `toml:"pulse"`
	Agents map[string]profile.Profile `toml:"agents"`
}

// PulseConfig drives the in-daemon pulse supervisor: one Pulser per
// discovered project, with per-role cadence and a project-rescan cadence.
//
// Durations are stored as strings here so the TOML stays human-readable
// ("30s", "1m"). Parse them with the helpers below.
type PulseConfig struct {
	Enabled           bool               `toml:"enabled"`
	DataRoot          string             `toml:"data_root"`          // ~/data by default; supervisor scans <data_root>/arcmux/*/state.bolt
	Interval          string             `toml:"interval"`           // per-pulser tick interval ("10s")
	DiscoveryInterval string             `toml:"discovery_interval"` // how often to rescan for new/gone projects ("60s")
	Cadence           PulseCadenceConfig `toml:"cadence"`
}

// PulseCadenceConfig holds per-role review intervals. A wake fires for a
// target when (now - lastWakeAt) >= cadence, independent of inbox depth.
type PulseCadenceConfig struct {
	Elon    string `toml:"elon"`    // "30s"
	Manager string `toml:"manager"` // "10s"
	IC      string `toml:"ic"`      // "5s"
}

type DaemonConfig struct {
	Socket   string `toml:"socket"`
	LogDir   string `toml:"log_dir"`
	HTTPAddr string `toml:"http_addr"`
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
	defaultAgents := profile.DefaultProfiles()
	cfg := &Config{
		Daemon: DaemonConfig{
			Socket:   DefaultSocketPath(),
			LogDir:   defaultLogDir(),
			HTTPAddr: "127.0.0.1:7777",
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
		Pulse: PulseConfig{
			Enabled:           true,
			DataRoot:          defaultPulseDataRoot(),
			Interval:          "10s",
			DiscoveryInterval: "60s",
			Cadence: PulseCadenceConfig{
				Elon:    "30s",
				Manager: "10s",
				IC:      "5s",
			},
		},
		Agents: copyAgentProfiles(defaultAgents),
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

	// Snapshot pulse defaults so a partial [pulse] table in the user's
	// TOML inherits unspecified fields (cadence elon=30s etc.) rather than
	// silently zeroing them.
	pulseDefaults := cfg.Pulse

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.Agents = mergeAgentProfiles(defaultAgents, cfg.Agents)
	cfg.Pulse = mergePulseConfig(pulseDefaults, cfg.Pulse)

	return cfg, nil
}

// ParsePulse converts the user-facing string durations to time.Duration. It
// is the single point where invalid TOML produces an error, so callers can
// treat the returned struct as already-validated.
func (p PulseConfig) ParsePulse() (ParsedPulse, error) {
	var pp ParsedPulse
	pp.Enabled = p.Enabled
	pp.DataRoot = p.DataRoot

	var err error
	if pp.Interval, err = parseDur("pulse.interval", p.Interval); err != nil {
		return pp, err
	}
	if pp.DiscoveryInterval, err = parseDur("pulse.discovery_interval", p.DiscoveryInterval); err != nil {
		return pp, err
	}
	if pp.Cadence.Elon, err = parseDur("pulse.cadence.elon", p.Cadence.Elon); err != nil {
		return pp, err
	}
	if pp.Cadence.Manager, err = parseDur("pulse.cadence.manager", p.Cadence.Manager); err != nil {
		return pp, err
	}
	if pp.Cadence.IC, err = parseDur("pulse.cadence.ic", p.Cadence.IC); err != nil {
		return pp, err
	}
	return pp, nil
}

// ParsedPulse is the validated, duration-typed view of PulseConfig.
type ParsedPulse struct {
	Enabled           bool
	DataRoot          string
	Interval          time.Duration
	DiscoveryInterval time.Duration
	Cadence           ParsedCadence
}

// ParsedCadence holds per-role intervals as time.Duration.
type ParsedCadence struct {
	Elon    time.Duration
	Manager time.Duration
	IC      time.Duration
}

func parseDur(field, s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("config: %s is empty (set or remove the override)", field)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: parse %s=%q: %w", field, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be > 0, got %s", field, d)
	}
	return d, nil
}

// DefaultAgentProfiles returns the built-in agent profiles.
func DefaultAgentProfiles() map[string]profile.Profile {
	return profile.DefaultProfiles()
}

func defaultLogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "logs")
}

func mergeAgentProfiles(defaults, loaded map[string]profile.Profile) map[string]profile.Profile {
	merged := make(map[string]profile.Profile, len(defaults)+len(loaded))
	for name, prof := range defaults {
		merged[name] = prof
	}
	for name, prof := range loaded {
		if base, ok := defaults[name]; ok {
			if prof.Transport == "" {
				prof.Transport = base.Transport
			}
			if prof.ExecDriver == "" {
				prof.ExecDriver = base.ExecDriver
			}
			if prof.HookType == "" {
				prof.HookType = base.HookType
			}
		}
		merged[name] = prof
	}
	return merged
}

// mergePulseConfig fills empty fields in the user-loaded config with values
// from the defaults snapshot. Keeps partial overrides ergonomic: a user can
// write `[pulse] enabled = false` and still inherit cadence defaults.
func mergePulseConfig(defaults, loaded PulseConfig) PulseConfig {
	out := loaded
	if out.DataRoot == "" {
		out.DataRoot = defaults.DataRoot
	}
	if out.Interval == "" {
		out.Interval = defaults.Interval
	}
	if out.DiscoveryInterval == "" {
		out.DiscoveryInterval = defaults.DiscoveryInterval
	}
	if out.Cadence.Elon == "" {
		out.Cadence.Elon = defaults.Cadence.Elon
	}
	if out.Cadence.Manager == "" {
		out.Cadence.Manager = defaults.Cadence.Manager
	}
	if out.Cadence.IC == "" {
		out.Cadence.IC = defaults.Cadence.IC
	}
	return out
}

// defaultPulseDataRoot returns ~/data — where `arcmux manager` scaffolds
// `arcmux/<project>/state.bolt`. Mirrors the convention used by
// internal/manager/paths.ForProject.
func defaultPulseDataRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "data")
}

func copyAgentProfiles(src map[string]profile.Profile) map[string]profile.Profile {
	dst := make(map[string]profile.Profile, len(src))
	for name, prof := range src {
		dst[name] = prof
	}
	return dst
}
