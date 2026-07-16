package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/tmux"
)

var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$|^[a-z0-9]$`)

type ProfileRecord struct {
	Name           string `json:"name"`
	SocketPath     string `json:"socket_path"`
	DataDir        string `json:"data_dir"`
	StateBolt      string `json:"state_bolt"`
	TmuxSocketName string `json:"tmux_socket_name"`
	CreatedAt      string `json:"created_at"`
	OwningPID      int    `json:"owning_pid,omitempty"`
}

type profileIndex struct {
	Profiles map[string]ProfileRecord `json:"profiles"`
}

type ProfileManager struct {
	parent       *Daemon
	registryPath string
	mu           sync.Mutex
	records      map[string]ProfileRecord
	daemons      map[string]*Daemon
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewProfileManager(parent *Daemon) (*ProfileManager, error) {
	path, err := DefaultProfileRegistryPath()
	if err != nil {
		return nil, err
	}
	pm := &ProfileManager{
		parent:       parent,
		registryPath: path,
		records:      map[string]ProfileRecord{},
		daemons:      map[string]*Daemon{},
	}
	if err := pm.load(); err != nil {
		return nil, err
	}
	return pm, nil
}

func DefaultProfileRegistryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, "data", "arcmux", "profiles", "index.json"), nil
}

func ProfileSocketPath(name string) (string, error) {
	slug, err := NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".config", "arcmux", "sockets", slug+".sock"), nil
}

func NormalizeProfileName(name string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strings.ReplaceAll(slug, ".", "-")
	slug = strings.ReplaceAll(slug, " ", "-")
	if !profileNameRe.MatchString(slug) {
		return "", fmt.Errorf("profile name %q must normalize to [a-z0-9][a-z0-9_-]{0,62}[a-z0-9]", name)
	}
	return slug, nil
}

func (pm *ProfileManager) Start(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	pm.ctx = runCtx
	pm.cancel = cancel
	names := make([]string, 0, len(pm.records))
	for name := range pm.records {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := pm.records[name]
		if err := pm.startLocked(runCtx, rec); err != nil {
			return err
		}
	}
	return nil
}

func (pm *ProfileManager) Stop() {
	daemons := pm.beginStop()
	for _, d := range daemons {
		d.Stop()
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for name := range pm.daemons {
		delete(pm.daemons, name)
	}
}

func (pm *ProfileManager) beginStop() []*Daemon {
	if pm == nil {
		return nil
	}
	pm.mu.Lock()
	if pm.cancel != nil {
		pm.cancel()
		pm.cancel = nil
		pm.ctx = nil
	}
	daemons := make([]*Daemon, 0, len(pm.daemons))
	for _, d := range pm.daemons {
		daemons = append(daemons, d)
	}
	pm.mu.Unlock()

	for _, d := range daemons {
		d.beginStop()
	}
	return daemons
}

func (pm *ProfileManager) List() []ProfileRecord {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make([]ProfileRecord, 0, len(pm.records))
	for _, rec := range pm.records {
		if _, ok := pm.daemons[rec.Name]; ok {
			rec.OwningPID = os.Getpid()
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SnapshotDaemons returns a shallow copy of the currently running profile
// daemons. It deliberately releases the manager lock before callers enter a
// child daemon, preventing lock inversion during profile create/remove.
func (pm *ProfileManager) SnapshotDaemons() map[string]*Daemon {
	if pm == nil {
		return nil
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make(map[string]*Daemon, len(pm.daemons))
	for name, daemon := range pm.daemons {
		out[name] = daemon
	}
	return out
}

func (pm *ProfileManager) Create(ctx context.Context, name string) (ProfileRecord, error) {
	slug, err := NormalizeProfileName(name)
	if err != nil {
		return ProfileRecord{}, err
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	rec, ok := pm.records[slug]
	if !ok {
		rec, err = newProfileRecord(slug)
		if err != nil {
			return ProfileRecord{}, err
		}
		pm.records[slug] = rec
		if err := pm.saveLocked(); err != nil {
			return ProfileRecord{}, err
		}
	}
	if pm.cancel != nil {
		if err := pm.startLocked(pm.ctx, rec); err != nil {
			return ProfileRecord{}, err
		}
		rec.OwningPID = os.Getpid()
	}
	return rec, nil
}

func (pm *ProfileManager) Remove(ctx context.Context, name string, purge bool) (ProfileRecord, error) {
	slug, err := NormalizeProfileName(name)
	if err != nil {
		return ProfileRecord{}, err
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	rec, ok := pm.records[slug]
	if !ok {
		return ProfileRecord{}, fmt.Errorf("profile %q not found", slug)
	}
	if d := pm.daemons[slug]; d != nil {
		d.Stop()
		delete(pm.daemons, slug)
	}
	if rec.TmuxSocketName != "" {
		_ = tmux.NewClient(rec.TmuxSocketName).KillServer(ctx)
	}
	_ = os.Remove(rec.SocketPath)
	delete(pm.records, slug)
	if err := pm.saveLocked(); err != nil {
		return ProfileRecord{}, err
	}
	if purge {
		_ = os.RemoveAll(rec.DataDir)
	}
	return rec, nil
}

func (pm *ProfileManager) startLocked(ctx context.Context, rec ProfileRecord) error {
	if _, ok := pm.daemons[rec.Name]; ok {
		return nil
	}
	cfg := cloneConfig(pm.parent.cfg)
	cfg.Daemon.ProfileName = rec.Name
	cfg.Daemon.Socket = rec.SocketPath
	cfg.Daemon.StatePath = rec.StateBolt
	cfg.Daemon.HTTPAddr = ""
	cfg.Daemon.LogDir = filepath.Join(rec.DataDir, "logs")
	cfg.Tmux.SocketName = rec.TmuxSocketName
	cfg.Tmux.DefaultSession = rec.Name
	cfg.Pulse.Enabled = false
	cfg.Pulse.DataRoot = rec.DataDir
	// Mesh connectivity is machine-scoped. Only the root daemon owns the
	// shared registry, listener, and outbound peer loops.
	cfg.Mesh.Enabled = false

	d := New(cfg, pm.parent.logger.With("profile", rec.Name))
	if err := d.Start(ctx); err != nil {
		return fmt.Errorf("start profile %s: %w", rec.Name, err)
	}
	pm.daemons[rec.Name] = d
	pm.parent.forwardProfileSessionEvents(ctx, rec.Name, d)
	pm.parent.logger.Info("profile listening", "profile", rec.Name, "socket", rec.SocketPath, "tmux_socket", rec.TmuxSocketName)
	return nil
}

func (pm *ProfileManager) load() error {
	data, err := os.ReadFile(pm.registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read profile registry: %w", err)
	}
	var idx profileIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("parse profile registry: %w", err)
	}
	if idx.Profiles == nil {
		idx.Profiles = map[string]ProfileRecord{}
	}
	pm.records = idx.Profiles
	return nil
}

func (pm *ProfileManager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(pm.registryPath), 0o755); err != nil {
		return err
	}
	idx := profileIndex{Profiles: pm.records}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pm.registryPath, append(data, '\n'), 0o644)
}

func newProfileRecord(name string) (ProfileRecord, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ProfileRecord{}, fmt.Errorf("resolve home: %w", err)
	}
	socketPath, err := ProfileSocketPath(name)
	if err != nil {
		return ProfileRecord{}, err
	}
	dataDir := filepath.Join(home, "data", "arcmux", "profiles", name)
	return ProfileRecord{
		Name:           name,
		SocketPath:     socketPath,
		DataDir:        dataDir,
		StateBolt:      filepath.Join(dataDir, "state.bolt"),
		TmuxSocketName: "arcmux-" + name,
		CreatedAt:      time.Now().Format(time.RFC3339Nano),
	}, nil
}

func cloneConfig(in *config.Config) *config.Config {
	out := *in
	out.Agents = make(map[string]profile.Profile, len(in.Agents))
	for k, v := range in.Agents {
		out.Agents[k] = v
	}
	return &out
}
