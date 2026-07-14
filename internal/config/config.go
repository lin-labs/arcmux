package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/lin-labs/arcmux/internal/profile"
)

// Config is the top-level configuration for the arcmux daemon.
type Config struct {
	Daemon   DaemonConfig               `toml:"daemon"`
	Mesh     MeshConfig                 `toml:"mesh"`
	Mux      MuxConfig                  `toml:"mux"`
	Tmux     TmuxConfig                 `toml:"tmux"`
	Health   HealthConfig               `toml:"health"`
	Hooks    HooksConfig                `toml:"hooks"`
	Pulse    PulseConfig                `toml:"pulse"`
	Delivery DeliveryConfig             `toml:"delivery"`
	Agents   map[string]profile.Profile `toml:"agents"`
	// DataRoot is the local data root used for per-session artifacts
	// (e.g. screen-recording logs). Defaults to ~/data when empty.
	// Mirrors Pulse.DataRoot but lives at the top level so non-pulse
	// features (recording, future tools) can share the same root without
	// depending on the pulse sub-config.
	DataRoot string `toml:"data_root"`
}

// MeshConfig controls the private, best-effort peer transport. Peer identities
// and credentials deliberately live in RegistryPath instead of config.toml so
// pairing commands can update them atomically without rewriting user TOML.
type MeshConfig struct {
	Enabled           bool   `toml:"enabled"`
	ListenAddr        string `toml:"listen_addr"`
	RegistryPath      string `toml:"registry_path"`
	HeartbeatInterval string `toml:"heartbeat_interval"`
	StaleAfter        string `toml:"stale_after"`
	DeadAfter         string `toml:"dead_after"`
	ReconnectMax      string `toml:"reconnect_max"`
	ReconnectMin      string `toml:"reconnect_min"`
	HandshakeTimeout  string `toml:"handshake_timeout"`
	MaxMessageBytes   int64  `toml:"max_message_bytes"`
	WriterQueue       int    `toml:"writer_queue"`
}

// ParsedMeshConfig is the validated runtime form of MeshConfig.
type ParsedMeshConfig struct {
	Enabled           bool
	ListenAddr        string
	RegistryPath      string
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
	DeadAfter         time.Duration
	ReconnectMax      time.Duration
	ReconnectMin      time.Duration
	HandshakeTimeout  time.Duration
	MaxMessageBytes   int64
	WriterQueue       int
}

// DeliveryConfig selects which prompt-delivery judge the daemon uses. The
// default "auto" is a cascade — hook events are ground truth and always win
// when the session's agent emits them; otherwise the typesafe judge assesses,
// itself degrading to the screen heuristic without an API key. Pin one of the
// other values only to bypass tiers deliberately (e.g. "hooks" to stay off
// the network, "typesafe" to ignore hook state).
type DeliveryConfig struct {
	Judge string `toml:"judge"` // "auto" (default) | "typesafe" | "hooks" | "heuristic"
}

// Validate rejects an unknown judge so a config typo fails loudly at load.
func (d DeliveryConfig) Validate() error {
	switch d.Judge {
	case "", "auto", "typesafe", "hooks", "heuristic":
		return nil
	default:
		return fmt.Errorf("config: delivery.judge %q is not one of auto|typesafe|hooks|heuristic", d.Judge)
	}
}

// MuxConfig selects the terminal multiplexer backend. Valid values are
// "cmux" (default) and "tmux". The choice is global per daemon.
type MuxConfig struct {
	Backend string `toml:"backend"`
}

// Validate returns an error if the backend value is unknown.
func (m MuxConfig) Validate() error {
	switch m.Backend {
	case "cmux", "tmux":
		return nil
	default:
		return fmt.Errorf("config: mux.backend %q is not one of cmux|tmux", m.Backend)
	}
}

// PulseConfig drives the in-daemon pulse supervisor: one Pulser per
// discovered project, with a per-target review cadence and a
// project-rescan cadence.
//
// Pre-C3 this struct held one cadence per role class (elon/manager/ic)
// because arcmux enumerated panes by role. After the pure-substrate
// demolition there is one wake target per project, so Cadence collapses
// to a single interval.
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

// PulseCadenceConfig holds the per-target review interval. A wake fires
// for a target when (now - lastWakeAt) >= cadence, independent of inbox
// depth. Single field after C3 (used to be per-role).
type PulseCadenceConfig struct {
	Interval string `toml:"interval"` // "30s"
}

type DaemonConfig struct {
	Socket      string `toml:"socket"`
	LogDir      string `toml:"log_dir"`
	HTTPAddr    string `toml:"http_addr"`
	ProfileName string `toml:"profile_name"`
	StatePath   string `toml:"state_path"`
	// HTTPAuthToken, when set, requires non-loopback HTTP callers to present
	// `Authorization: Bearer <token>`. Loopback requests stay open for local
	// dev. Empty (default) disables auth entirely. Required before exposing the
	// control plane off-localhost (e.g. over Tailscale).
	HTTPAuthToken string `toml:"http_auth_token"`
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
	CodexHookDir  string `toml:"codex_hook_dir"`
	GrokHookDir   string `toml:"grok_hook_dir"`
	HookOutputDir string `toml:"hook_output_dir"`
	// SessionStateDir holds per-session hook state docs
	// (<dir>/<id>.json, archived under <dir>/archived/). Default
	// ~/data/mux/sessions — the PROTOCOL state dir shared by every mux
	// subscriber (mission-control, cmux, ...), deliberately not named after
	// this application. Read by the hooks judge; written by the
	// `arcmux hook` CLI and seeded/archived by the daemon.
	SessionStateDir string `toml:"session_state_dir"`
	AutoInstall     bool   `toml:"auto_install"`
	AutoRegister    bool   `toml:"auto_register"`
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
		Mesh: MeshConfig{
			Enabled:           true,
			ListenAddr:        "127.0.0.1:7788",
			RegistryPath:      defaultMeshRegistryPath(),
			HeartbeatInterval: "15s",
			StaleAfter:        "35s",
			DeadAfter:         "60s",
			ReconnectMax:      "30s",
			ReconnectMin:      "500ms",
			HandshakeTimeout:  "10s",
			MaxMessageBytes:   64 << 10,
			WriterQueue:       32,
		},
		Mux: MuxConfig{
			Backend: "cmux",
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
			ClaudeHookDir:   defaultClaudeHookDir(),
			CodexHookDir:    defaultCodexHookDir(),
			GrokHookDir:     defaultGrokHookDir(),
			HookOutputDir:   defaultHookOutputDir(),
			SessionStateDir: defaultSessionStateDir(),
			AutoInstall:     true,
			AutoRegister:    false,
		},
		Delivery: DeliveryConfig{
			Judge: "auto",
		},
		Pulse: PulseConfig{
			Enabled:           true,
			DataRoot:          defaultPulseDataRoot(),
			Interval:          "10s",
			DiscoveryInterval: "60s",
			Cadence: PulseCadenceConfig{
				Interval: "30s",
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

	// Resolve any "~/..." paths the user wrote in TOML to absolute form
	// once, here at the boundary. Downstream code can then assume every
	// path is absolute and never has to guess again. (Go does NOT expand
	// "~" — filepath.Join("~", "x") yields the literal directory "~", and
	// os.MkdirAll happily creates it under the daemon's cwd.)
	expandConfigPaths(cfg)

	if cfg.Mux.Backend == "" {
		cfg.Mux.Backend = "cmux"
	}
	if err := cfg.Mux.Validate(); err != nil {
		return nil, err
	}

	// Fill an empty session_state_dir / hook_output_dir from the defaults so a
	// partial [hooks] table doesn't zero them. (Runs after expandConfigPaths,
	// so the defaults are already absolute.)
	if cfg.Hooks.SessionStateDir == "" {
		cfg.Hooks.SessionStateDir = defaultSessionStateDir()
	}
	if cfg.Hooks.HookOutputDir == "" {
		cfg.Hooks.HookOutputDir = defaultHookOutputDir()
	}

	if err := cfg.Delivery.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultMeshRegistryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "mesh.json")
}

// Parse validates mesh safety and converts duration strings. The listener is
// loopback-only by design; Tailscale Serve must proxy to it rather than making
// the arcmux control or mesh ports directly public.
func (m MeshConfig) Parse() (ParsedMeshConfig, error) {
	parsed := ParsedMeshConfig{
		Enabled:    m.Enabled,
		ListenAddr: m.ListenAddr, RegistryPath: m.RegistryPath,
		MaxMessageBytes: m.MaxMessageBytes, WriterQueue: m.WriterQueue,
	}
	if parsed.ListenAddr == "" {
		parsed.ListenAddr = "127.0.0.1:7788"
	}
	host, _, err := net.SplitHostPort(parsed.ListenAddr)
	if err != nil {
		return parsed, fmt.Errorf("config: mesh.listen_addr: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return parsed, fmt.Errorf("config: mesh.listen_addr must be loopback, got %q", parsed.ListenAddr)
	}
	if parsed.RegistryPath == "" {
		parsed.RegistryPath = defaultMeshRegistryPath()
	}
	parseDuration := func(name, value string, fallback time.Duration) (time.Duration, error) {
		if value == "" {
			return fallback, nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("config: mesh.%s must be a positive duration", name)
		}
		return d, nil
	}
	if parsed.HeartbeatInterval, err = parseDuration("heartbeat_interval", m.HeartbeatInterval, 15*time.Second); err != nil {
		return parsed, err
	}
	if parsed.StaleAfter, err = parseDuration("stale_after", m.StaleAfter, 35*time.Second); err != nil {
		return parsed, err
	}
	if parsed.DeadAfter, err = parseDuration("dead_after", m.DeadAfter, 60*time.Second); err != nil {
		return parsed, err
	}
	if parsed.ReconnectMax, err = parseDuration("reconnect_max", m.ReconnectMax, 30*time.Second); err != nil {
		return parsed, err
	}
	if parsed.ReconnectMin, err = parseDuration("reconnect_min", m.ReconnectMin, 500*time.Millisecond); err != nil {
		return parsed, err
	}
	if parsed.HandshakeTimeout, err = parseDuration("handshake_timeout", m.HandshakeTimeout, 10*time.Second); err != nil {
		return parsed, err
	}
	if parsed.StaleAfter >= parsed.DeadAfter {
		return parsed, fmt.Errorf("config: mesh.stale_after must be less than dead_after")
	}
	if parsed.ReconnectMin > parsed.ReconnectMax {
		return parsed, fmt.Errorf("config: mesh.reconnect_min must be at most reconnect_max")
	}
	if parsed.MaxMessageBytes == 0 {
		parsed.MaxMessageBytes = 64 << 10
	}
	if parsed.MaxMessageBytes < 1024 || parsed.MaxMessageBytes > 1<<20 {
		return parsed, fmt.Errorf("config: mesh.max_message_bytes must be between 1024 and 1048576")
	}
	if parsed.WriterQueue == 0 {
		parsed.WriterQueue = 32
	}
	if parsed.WriterQueue < 1 || parsed.WriterQueue > 4096 {
		return parsed, fmt.Errorf("config: mesh.writer_queue must be between 1 and 4096")
	}
	return parsed, nil
}

// expandTilde turns "~", "~/..." into an absolute path under the user's
// home directory. Any other input — including already-absolute paths and
// relative paths — is returned unchanged. The only path-form normalized
// here is the leading tilde; we explicitly do not expand "~user/...".
func expandTilde(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// expandConfigPaths normalizes every user-supplied filesystem path on the
// config struct after TOML load. Add new path fields here so the "all
// paths absolute after Load" invariant holds everywhere.
func expandConfigPaths(cfg *Config) {
	cfg.Daemon.Socket = expandTilde(cfg.Daemon.Socket)
	cfg.Daemon.LogDir = expandTilde(cfg.Daemon.LogDir)
	cfg.Daemon.StatePath = expandTilde(cfg.Daemon.StatePath)
	cfg.Mesh.RegistryPath = expandTilde(cfg.Mesh.RegistryPath)
	cfg.Hooks.ClaudeHookDir = expandTilde(cfg.Hooks.ClaudeHookDir)
	cfg.Hooks.CodexHookDir = expandTilde(cfg.Hooks.CodexHookDir)
	cfg.Hooks.GrokHookDir = expandTilde(cfg.Hooks.GrokHookDir)
	cfg.Hooks.HookOutputDir = expandTilde(cfg.Hooks.HookOutputDir)
	cfg.Hooks.SessionStateDir = expandTilde(cfg.Hooks.SessionStateDir)
	cfg.DataRoot = expandTilde(cfg.DataRoot)
	cfg.Pulse.DataRoot = expandTilde(cfg.Pulse.DataRoot)
	for name, prof := range cfg.Agents {
		prof.HookDir = expandTilde(prof.HookDir)
		cfg.Agents[name] = prof
	}
}

// defaultClaudeHookDir returns the absolute path to ~/.claude. Mirrors
// profile.defaultClaudeHookDir — duplicated to avoid a config -> profile
// dependency for one helper.
func defaultClaudeHookDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// defaultCodexHookDir returns ~/.codex/hooks — where arcmux materializes the
// codex lifecycle-hook bridge script. Registration in codex config stays
// manual (see docs/codex-hooks-findings.md).
func defaultCodexHookDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".codex", "hooks")
	}
	return filepath.Join(home, ".codex", "hooks")
}

// defaultGrokHookDir returns ~/.grok — grok loads drop-in hook files from
// <dir>/hooks/*.json (always trusted), so materializing arcmux's registration
// file there is the complete hook setup; no manual config edit is needed.
func defaultGrokHookDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".grok"
	}
	return filepath.Join(home, ".grok")
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
	if pp.Cadence.Interval, err = parseDur("pulse.cadence.interval", p.Cadence.Interval); err != nil {
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

// ParsedCadence holds the per-target review interval as time.Duration.
// Single field after C3 (used to be per-role: elon/manager/ic).
type ParsedCadence struct {
	Interval time.Duration
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

// DefaultSessionStateDir exposes the protocol session-state default so the
// daemon can detect "running on defaults" for the legacy migration sweep.
func DefaultSessionStateDir() string { return defaultSessionStateDir() }

// LegacySessionStateDir is the pre-protocol, application-named location
// (~/data/arcmux/sessions). Only consulted by the migration sweep.
func LegacySessionStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "data", "arcmux", "sessions")
}

// ScreenLogDir is where per-session voice screen-recording logs live:
// <DataRoot>/arcmux/sessions/. The startup migration sweep only moves *.json,
// so *.screen.log files here are untouched.
func (c *Config) ScreenLogDir() string {
	root := c.DataRoot
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "data")
	}
	return filepath.Join(root, "arcmux", "sessions")
}

func defaultLogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "logs")
}

// defaultSessionStateDir returns ~/data/mux/sessions — the persistent home for
// per-session hook state docs. ~/data/mux is the PROTOCOL state root (shared
// with every subscriber: mission-control, cmux, future tools), distinct from
// ~/data/arcmux/<project>/ which remains this application's private substrate.
func defaultSessionStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "data", "mux", "sessions")
}

// defaultHookOutputDir returns ~/data/mux/hook-output — the per-session raw
// hook-event JSONL audit. Lives beside the session state docs (it used to
// default to /tmp/arcmux-hooks, which silently lost the event trail on
// reboot and was named after the app rather than the protocol).
func defaultHookOutputDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "data", "mux", "hook-output")
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
	if out.Cadence.Interval == "" {
		out.Cadence.Interval = defaults.Cadence.Interval
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
