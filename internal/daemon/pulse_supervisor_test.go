package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/manager/pulse"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// stubPulser captures Run() calls and blocks until ctx cancels — the way a
// real pulse.Pulser behaves. Tests assert on cadence + invocation count
// without booting cmux.
type stubPulser struct {
	slug     string
	cadence  pulse.Cadence
	interval atomic.Int64 // ns
	started  chan struct{}
	stopped  chan struct{}
}

func (s *stubPulser) Run(ctx context.Context, interval time.Duration) error {
	s.interval.Store(int64(interval))
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return ctx.Err()
}

// supervisorHarness builds a PulseSupervisor against an injected discovery
// + bolt-path layout so tests don't need a real ~/data/arcmux tree.
type supervisorHarness struct {
	t        *testing.T
	dataRoot string

	mu         sync.Mutex
	pulsers    map[string]*stubPulser
	newPulserN int
}

func newHarness(t *testing.T, pcfg config.ParsedPulse) (*supervisorHarness, *PulseSupervisor) {
	t.Helper()
	dir := t.TempDir()
	pcfg.DataRoot = dir
	h := &supervisorHarness{t: t, dataRoot: dir, pulsers: map[string]*stubPulser{}}

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &PulseSupervisor{
		cfg:    pcfg,
		cmux:   nil, // stubPulser ignores it
		logger: silent,
		now:    time.Now,
		discoverProjects: func() ([]string, error) {
			return scanProjects(dir)
		},
		boltPathFor: func(slug string) string {
			return filepath.Join(dir, "arcmux", slug, "state.bolt")
		},
		newPulser: func(slug string, db *store.DB, _ pulseCmux, cad pulse.Cadence) pulser {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.newPulserN++
			sp := &stubPulser{
				slug:    slug,
				cadence: cad,
				started: make(chan struct{}),
				stopped: make(chan struct{}),
			}
			h.pulsers[slug] = sp
			return sp
		},
		projects: map[string]*pulseEntry{},
		done:     make(chan struct{}),
	}
	return h, s
}

// seedProject creates a bolt file at the production-shaped path so
// scanProjects picks it up.
func (h *supervisorHarness) seedProject(slug string) {
	h.t.Helper()
	dir := filepath.Join(h.dataRoot, "arcmux", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	db, err := store.Open(filepath.Join(dir, "state.bolt"))
	if err != nil {
		h.t.Fatalf("seed %s: %v", slug, err)
	}
	_ = db.Close()
}

func (h *supervisorHarness) removeProject(slug string) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.dataRoot, "arcmux", slug)); err != nil {
		h.t.Fatalf("remove: %v", err)
	}
}

func (h *supervisorHarness) pulser(slug string) *stubPulser {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pulsers[slug]
}

func defaultPCfg() config.ParsedPulse {
	return config.ParsedPulse{
		Enabled:           true,
		Interval:          50 * time.Millisecond,
		DiscoveryInterval: 50 * time.Millisecond,
		Cadence: config.ParsedCadence{
			Elon:    30 * time.Second,
			Manager: 10 * time.Second,
			IC:      5 * time.Second,
		},
	}
}

// waitFor polls cond until it returns true or the deadline expires.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor timed out: %s", msg)
}

// TestSupervisor_DiscoversInitialProjects: projects existing at startup
// must be picked up by the immediate first scan, not the first ticker
// firing.
func TestSupervisor_DiscoversInitialProjects(t *testing.T) {
	h, s := newHarness(t, defaultPCfg())
	h.seedProject("alpha")
	h.seedProject("beta")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	waitFor(t, time.Second, func() bool {
		return len(s.Supervised()) == 2
	}, "two projects discovered immediately")

	supervised := s.Supervised()
	sort.Strings(supervised)
	if supervised[0] != "alpha" || supervised[1] != "beta" {
		t.Errorf("Supervised = %v, want [alpha beta]", supervised)
	}

	// Each pulser must have been started with the configured interval.
	alpha := h.pulser("alpha")
	if alpha == nil {
		t.Fatal("alpha pulser never constructed")
	}
	<-alpha.started
	if got := time.Duration(alpha.interval.Load()); got != 50*time.Millisecond {
		t.Errorf("alpha interval = %v, want 50ms", got)
	}
}

// TestSupervisor_PicksUpNewProject: a project that appears after start
// must be picked up by the next discovery tick — proving the dynamic
// rescan, not a startup-only scan.
func TestSupervisor_PicksUpNewProject(t *testing.T) {
	h, s := newHarness(t, defaultPCfg())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	// Wait for the initial scan to settle on zero projects.
	waitFor(t, time.Second, func() bool {
		return len(s.Supervised()) == 0
	}, "initial empty scan")

	h.seedProject("late")
	waitFor(t, time.Second, func() bool {
		return len(s.Supervised()) == 1
	}, "late project picked up by rescan")

	if s.Supervised()[0] != "late" {
		t.Errorf("got %v, want [late]", s.Supervised())
	}
}

// TestSupervisor_DropsVanishedProject: a project whose state.bolt
// disappears must be retired (pulser ctx cancelled, bolt handle closed).
func TestSupervisor_DropsVanishedProject(t *testing.T) {
	h, s := newHarness(t, defaultPCfg())
	h.seedProject("doomed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return len(s.Supervised()) == 1 }, "initial pick-up")
	doomed := h.pulser("doomed")
	<-doomed.started

	h.removeProject("doomed")
	waitFor(t, time.Second, func() bool { return len(s.Supervised()) == 0 }, "vanished project dropped")

	// Goroutine must have observed ctx cancel.
	select {
	case <-doomed.stopped:
	case <-time.After(time.Second):
		t.Fatal("pulser goroutine for vanished project never stopped")
	}
}

// TestSupervisor_CadencePropagates: per-role cadence overrides reach the
// constructed pulser, not just the supervisor's own struct.
func TestSupervisor_CadencePropagates(t *testing.T) {
	pcfg := defaultPCfg()
	pcfg.Cadence = config.ParsedCadence{
		Elon:    7 * time.Second,
		Manager: 3 * time.Second,
		IC:      1 * time.Second,
	}
	h, s := newHarness(t, pcfg)
	h.seedProject("alpha")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return h.pulser("alpha") != nil }, "pulser constructed")
	got := h.pulser("alpha").cadence
	if got.Elon != 7*time.Second || got.Manager != 3*time.Second || got.IC != time.Second {
		t.Errorf("cadence = %+v, want {7s 3s 1s}", got)
	}
}

// TestSupervisor_GracefulShutdown: cancelling the parent ctx must drain
// every supervised pulser, close the Done channel, and release every bolt.
func TestSupervisor_GracefulShutdown(t *testing.T) {
	h, s := newHarness(t, defaultPCfg())
	h.seedProject("a")
	h.seedProject("b")

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()

	waitFor(t, time.Second, func() bool { return len(s.Supervised()) == 2 }, "both projects up")
	a := h.pulser("a")
	b := h.pulser("b")
	<-a.started
	<-b.started

	cancel()

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor Done() never closed after cancel")
	}
	select {
	case <-a.stopped:
	case <-time.After(time.Second):
		t.Fatal("pulser a goroutine never stopped")
	}
	select {
	case <-b.stopped:
	case <-time.After(time.Second):
		t.Fatal("pulser b goroutine never stopped")
	}

	// After shutdown another arcmux can reopen the bolt — proves the
	// supervisor released the file lock on every project.
	for _, slug := range []string{"a", "b"} {
		db, err := store.Open(filepath.Join(h.dataRoot, "arcmux", slug, "state.bolt"))
		if err != nil {
			t.Fatalf("reopen %s after shutdown failed: %v (lock leaked?)", slug, err)
		}
		_ = db.Close()
	}
}

// TestSupervisor_DisabledSkipsScan: enabled=false must short-circuit
// before any project is discovered or any pulser is constructed.
func TestSupervisor_DisabledSkipsScan(t *testing.T) {
	pcfg := defaultPCfg()
	pcfg.Enabled = false
	h, s := newHarness(t, pcfg)
	h.seedProject("alpha")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	select {
	case <-s.Done():
		// disabled supervisor returns immediately
	case <-time.After(time.Second):
		t.Fatal("disabled supervisor never returned")
	}
	if got := s.Supervised(); len(got) != 0 {
		t.Errorf("disabled supervisor picked up projects: %v", got)
	}
	h.mu.Lock()
	if h.newPulserN != 0 {
		t.Errorf("disabled supervisor constructed %d pulsers", h.newPulserN)
	}
	h.mu.Unlock()
}

// TestScanProjects_IgnoresStrayFiles: stray files / directories that don't
// have a state.bolt must not be reported as projects.
func TestScanProjects_IgnoresStrayFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "arcmux", "real"), 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if _, err := os.Create(filepath.Join(dir, "arcmux", "real", "state.bolt")); err != nil {
		t.Fatalf("touch state.bolt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "arcmux", "no-bolt-here"), 0o755); err != nil {
		t.Fatalf("mkdir no-bolt-here: %v", err)
	}
	// stray file at the root: must be ignored (paths.Validate may accept the
	// slug but the missing state.bolt drops it).
	if _, err := os.Create(filepath.Join(dir, "arcmux", "rogue.txt")); err != nil {
		t.Fatalf("create rogue file: %v", err)
	}

	slugs, err := scanProjects(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != "real" {
		t.Errorf("scanProjects = %v, want [real]", slugs)
	}
}

// TestScanProjects_MissingRootIsEmpty: a fresh box with no ~/data/arcmux
// directory must return no projects and no error.
func TestScanProjects_MissingRootIsEmpty(t *testing.T) {
	slugs, err := scanProjects(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(slugs) != 0 {
		t.Errorf("scan on missing root returned %v, want empty", slugs)
	}
}
