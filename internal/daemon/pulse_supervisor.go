package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/pulse"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/mux"
)

// PulseSupervisor runs ONE pulse.Pulser goroutine per discovered project,
// living inside the arcmux daemon process. This consolidates what used to
// require a per-project `arcmux pulse --project X` shell into the single
// `arcmux start` runtime — matching Boyan's stated architecture: one
// arcmux, not one per role.
//
// Discovery model: scan <DataRoot>/arcmux/*/state.bolt for project slugs.
// On every discovery tick (default 60s) we (a) start a pulser for any
// project we don't already track and (b) stop pulsers whose state.bolt
// has disappeared (project archived or hand-deleted). Adding a new project
// via `arcmux manager <agent> <slug>` therefore becomes self-discovering
// — no daemon restart needed.
//
// Lock model: bbolt is single-writer-per-file. While the supervisor holds
// a project's state.bolt, direct `arcmux-cli` invocations against that
// project will block on the file lock. That's the explicit trade Boyan
// is making with "one arcmux"; the planned next step is to route
// arcmux-cli through the daemon's gRPC instead of opening bolt directly.
type PulseSupervisor struct {
	cfg    config.ParsedPulse
	mux    mux.Backend
	logger *slog.Logger
	now    func() time.Time

	// discoverProjects is the project-list source. Production callers
	// scan the filesystem; tests inject a fake. Returns project slugs
	// (not paths) so the supervisor can derive the bolt path itself.
	discoverProjects func() ([]string, error)

	// boltPathFor returns the on-disk bolt path for a given slug.
	// Production uses paths.ForProject; tests inject a tempdir.
	boltPathFor func(slug string) string

	// newPulser constructs a Pulser for a freshly-opened DB. Tests
	// inject a stub that just records the call.
	newPulser func(slug string, db *store.DB, m mux.Backend, cad pulse.Cadence) pulser

	mu       sync.Mutex
	projects map[string]*pulseEntry // key: project slug

	started bool
	done    chan struct{}
}

// pulser is the slice of pulse.Pulser we depend on. Letting tests inject a
// fake keeps the supervisor unit-testable without booting a real cmux.
type pulser interface {
	Run(ctx context.Context, interval time.Duration) error
}

// pulseEntry tracks one supervised project.
type pulseEntry struct {
	slug   string
	bolt   string
	cancel context.CancelFunc
	db     *store.DB
	done   chan struct{} // closed when the project's goroutine returns
}

// NewPulseSupervisor builds a supervisor wired against the real bolt
// filesystem layout and the configured mux backend.
func NewPulseSupervisor(pcfg config.ParsedPulse, backend mux.Backend, logger *slog.Logger) *PulseSupervisor {
	if logger == nil {
		logger = slog.Default()
	}
	dataRoot := pcfg.DataRoot
	if dataRoot == "" {
		home, _ := os.UserHomeDir()
		dataRoot = filepath.Join(home, "data")
	}
	return &PulseSupervisor{
		cfg:    pcfg,
		mux:    backend,
		logger: logger,
		now:    time.Now,
		discoverProjects: func() ([]string, error) {
			return scanProjects(dataRoot)
		},
		boltPathFor: func(slug string) string {
			return paths.ForProject(dataRoot, "" /*vault not needed*/, slug).StateBolt
		},
		newPulser: func(slug string, db *store.DB, m mux.Backend, cad pulse.Cadence) pulser {
			pp := pulse.New(slug, db, m)
			pp.Cadence = cad
			pp.Log = logger.With("project", slug)
			return pp
		},
		projects: map[string]*pulseEntry{},
		done:     make(chan struct{}),
	}
}

// scanProjects enumerates <dataRoot>/arcmux/*/ entries whose state.bolt
// exists. Anything else is ignored (in-progress scaffold, stray dir).
func scanProjects(dataRoot string) ([]string, error) {
	root := filepath.Join(dataRoot, "arcmux")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no arcmux projects on this machine yet
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		if _, err := paths.Validate(slug); err != nil {
			continue // not a valid project slug; ignore
		}
		bolt := filepath.Join(root, slug, "state.bolt")
		if st, err := os.Stat(bolt); err == nil && !st.IsDir() {
			out = append(out, slug)
		}
	}
	return out, nil
}

// Run drives the discovery loop until ctx cancels. Performs an immediate
// scan so projects come up without waiting for the first tick. Returns
// when ctx fires; the caller can drain the .Done() channel to wait for
// every project goroutine to clean up.
func (s *PulseSupervisor) Run(ctx context.Context) error {
	if !s.cfg.Enabled {
		s.logger.Info("pulse supervisor disabled in config; not scanning")
		close(s.done)
		return nil
	}
	if s.cfg.DiscoveryInterval <= 0 {
		close(s.done)
		return fmt.Errorf("pulse supervisor: discovery_interval must be > 0, got %s", s.cfg.DiscoveryInterval)
	}
	if s.cfg.Interval <= 0 {
		close(s.done)
		return fmt.Errorf("pulse supervisor: interval must be > 0, got %s", s.cfg.Interval)
	}

	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	s.logger.Info("pulse supervisor starting",
		"data_root", s.cfg.DataRoot,
		"interval", s.cfg.Interval,
		"discovery_interval", s.cfg.DiscoveryInterval,
		"cadence", s.cfg.Cadence.Interval,
	)

	// Immediate first scan so the supervisor isn't dead for a full
	// discovery interval after daemon start.
	s.rescan(ctx)

	t := time.NewTicker(s.cfg.DiscoveryInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			s.shutdownAll()
			close(s.done)
			return ctx.Err()
		case <-t.C:
			s.rescan(ctx)
		}
	}
}

// Done returns a channel closed when Run has fully drained — every
// supervised pulser has returned and every bolt handle is closed.
func (s *PulseSupervisor) Done() <-chan struct{} { return s.done }

// rescan reconciles the supervised set with the on-disk reality:
//   - start a pulser for any newly-discovered project
//   - stop a pulser whose state.bolt has vanished
func (s *PulseSupervisor) rescan(ctx context.Context) {
	slugs, err := s.discoverProjects()
	if err != nil {
		s.logger.Warn("pulse supervisor: project discovery failed", "err", err)
		return
	}

	seen := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		seen[slug] = struct{}{}
	}

	// Stop projects that have disappeared.
	s.mu.Lock()
	var toStop []*pulseEntry
	for slug, entry := range s.projects {
		if _, still := seen[slug]; !still {
			toStop = append(toStop, entry)
			delete(s.projects, slug)
		}
	}
	s.mu.Unlock()
	for _, e := range toStop {
		s.logger.Info("pulse supervisor: dropping project (state.bolt vanished)", "project", e.slug)
		e.cancel()
		<-e.done
		_ = e.db.Close()
	}

	// Start projects we don't yet supervise.
	for _, slug := range slugs {
		s.mu.Lock()
		_, already := s.projects[slug]
		s.mu.Unlock()
		if already {
			continue
		}
		if err := s.startProject(ctx, slug); err != nil {
			s.logger.Warn("pulse supervisor: start failed", "project", slug, "err", err)
		}
	}
}

// startProject opens the bolt store and launches a Pulser goroutine. On
// failure the project is left unregistered so the next rescan retries.
func (s *PulseSupervisor) startProject(parent context.Context, slug string) error {
	boltPath := s.boltPathFor(slug)
	// Open BEFORE registering so a failed open doesn't leave a half-tracked
	// entry. store.Open blocks on the bbolt file lock if another arcmux
	// holds it — acceptable contention with `arcmux-cli` until the planned
	// gRPC routing lands.
	db, err := store.Open(boltPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", boltPath, err)
	}

	cad := pulse.Cadence{Interval: s.cfg.Cadence.Interval}
	pp := s.newPulser(slug, db, s.mux, cad)

	ctx, cancel := context.WithCancel(parent)
	entry := &pulseEntry{
		slug:   slug,
		bolt:   boltPath,
		cancel: cancel,
		db:     db,
		done:   make(chan struct{}),
	}

	s.mu.Lock()
	s.projects[slug] = entry
	s.mu.Unlock()

	go func() {
		defer close(entry.done)
		if err := pp.Run(ctx, s.cfg.Interval); err != nil && ctx.Err() == nil {
			// Run returned for a reason other than ctx cancellation —
			// log it but don't crash the supervisor. The next rescan
			// will not restart the project automatically (it's still
			// in s.projects); a future improvement is to expose a
			// retry policy here.
			s.logger.Error("pulse goroutine exited unexpectedly",
				"project", slug, "err", err)
		}
	}()
	s.logger.Info("pulse supervisor: started project", "project", slug)
	return nil
}

// shutdownAll cancels every supervised project's context, drains their
// goroutines, and closes their bolt handles. Synchronous: returns only
// after every project is fully cleaned up. Safe to call multiple times.
func (s *PulseSupervisor) shutdownAll() {
	s.mu.Lock()
	entries := make([]*pulseEntry, 0, len(s.projects))
	for slug, e := range s.projects {
		entries = append(entries, e)
		delete(s.projects, slug)
	}
	s.mu.Unlock()

	for _, e := range entries {
		e.cancel()
	}
	for _, e := range entries {
		<-e.done
		_ = e.db.Close()
		s.logger.Info("pulse supervisor: stopped project", "project", e.slug)
	}
}

// Supervised returns a snapshot of the project slugs currently being
// pulsed. Used by tests and (eventually) by an `arcmux status` introspection.
func (s *PulseSupervisor) Supervised() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.projects))
	for slug := range s.projects {
		out = append(out, slug)
	}
	return out
}
