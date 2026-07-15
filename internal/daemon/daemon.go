package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/delivery"
	"github.com/lin-labs/arcmux/internal/health"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/manager/store"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/mux"
	"github.com/lin-labs/arcmux/internal/muxbuild"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/tmux"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

// Daemon is the main arcmux runtime service.
type Daemon struct {
	cfg      *config.Config
	tmux     *tmux.Client
	mux      mux.Backend
	hooks    *hooks.Installer
	watcher  *hooks.Watcher
	profiles map[string]profile.Profile
	logger   *slog.Logger

	mu        sync.RWMutex
	persistMu sync.Mutex
	sessions  map[string]*session.Session
	monitors  map[string]context.CancelFunc
	processes map[string]*os.Process
	recorders map[string]*recorder // sessionID → active screen recorder

	healthEvents chan health.HealthEvent
	eventBus     *EventBus
	delivery     *delivery.Controller

	server       *grpc.Server
	listener     net.Listener
	httpSrv      *HTTPServer
	meshMu       sync.RWMutex
	meshReloadMu sync.Mutex
	mesh         *arcmuxmesh.Manager
	meshApp      *meshApplication
	ctx          context.Context
	cancel       context.CancelFunc

	// otelShutdown flushes the OTLP trace provider on daemon Stop (LabOps
	// observability). Nil when tracing failed to init — never fatal.
	otelShutdown func(context.Context) error

	pulse *PulseSupervisor

	profileManager *ProfileManager

	// sendPromptHook, when non-nil, replaces the production SendPrompt
	// transport dispatch. Test-only hook so unit tests can observe the
	// arguments the C1 Send RPC passes through to daemon.SendPrompt
	// (notably confirmDelivery). Production code never sets this.
	sendPromptHook func(ctx context.Context, sessionID, text string, confirmDelivery, waitIdle bool) error

	// captureHook, when non-nil, replaces the production Capture pane read.
	// Test-only hook (mirrors sendPromptHook) so unit tests of the HTTP
	// capture shim don't need a live tmux pane. Production code never sets it.
	captureHook func(ctx context.Context, sessionID string, includeHistory bool) (string, error)

	// projects is the project registry (slug -> repo_cwd + plan_globs) used to
	// scope sessions to a project (HTTP /sessions?project=) and to mint babysit
	// call contexts. May be nil/empty when no projects.toml is present.
	projects *project.Registry

	// state is the daemon-level bbolt store backing the C1 substrate RPCs
	// (Send/PeekInbox/AckInbox/QueryAudit). Opened at Start, lazily on
	// first need if Start hasn't been called (test path). One file:
	// <DataRoot>/arcmux/_daemon/state.bolt. Distinct from per-project
	// state.bolt files the pulse supervisor opens — those still live at
	// <DataRoot>/arcmux/<project>/state.bolt.
	stateMu sync.Mutex
	state   *store.DB

	goalSummaryMu       sync.Mutex
	goalSummaryAttempts map[string]goalSummaryAttempt
	goalSummaryRunner   func(context.Context, string, string) (string, error)
	goalHistoryRoot     string
}

// New creates a new daemon instance.
func New(cfg *config.Config, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	// Pick the configured backend. Falls back to cmux silently if Validate
	// missed something (defense-in-depth — config.Load already validates).
	backend, err := muxbuild.New(cfg)
	if err != nil {
		logger.Error("mux backend init failed; falling back to cmux", "error", err)
		backend, _ = muxbuild.New(&config.Config{
			Mux:  config.MuxConfig{Backend: "cmux"},
			Tmux: cfg.Tmux,
		})
	}
	// Project registry is optional; a missing or malformed projects.toml must
	// not stop the daemon, so log and continue with an empty registry.
	projects, perr := project.Load("")
	if perr != nil {
		logger.Warn("project registry load failed; continuing without it", "error", perr)
		projects, _ = project.Load(filepath.Join(os.TempDir(), "arcmux-no-such-projects.toml"))
	}
	return &Daemon{
		cfg:                 cfg,
		tmux:                tmux.NewClient(cfg.Tmux.SocketName),
		mux:                 backend,
		hooks:               hooks.NewInstaller(cfg.Hooks.HookOutputDir),
		watcher:             hooks.NewWatcher(cfg.Hooks.HookOutputDir, logger),
		profiles:            cfg.Agents,
		projects:            projects,
		logger:              logger,
		sessions:            make(map[string]*session.Session),
		monitors:            make(map[string]context.CancelFunc),
		processes:           make(map[string]*os.Process),
		recorders:           make(map[string]*recorder),
		healthEvents:        make(chan health.HealthEvent, 64),
		eventBus:            NewEventBus(),
		delivery:            delivery.NewController(newDeliveryJudge(cfg, logger), delivery.DefaultControllerConfig()),
		goalSummaryAttempts: make(map[string]goalSummaryAttempt),
	}
}

// newDeliveryJudge selects the prompt-delivery judge from config. An unknown
// judge value is already rejected at config load, so a build error here is a
// defensive fallback to the always-available heuristic rather than a crash.
func newDeliveryJudge(cfg *config.Config, logger *slog.Logger) delivery.Judge {
	judge, err := delivery.NewJudge(delivery.JudgeOptions{
		Kind:            delivery.JudgeKind(cfg.Delivery.Judge),
		SessionStateDir: cfg.Hooks.SessionStateDir,
	})
	if err != nil {
		logger.Error("build delivery judge failed; falling back to heuristic",
			"judge", cfg.Delivery.Judge, "error", err)
		return delivery.HeuristicJudge{}
	}
	logger.Info("delivery judge selected", "judge", cfg.Delivery.Judge,
		"session_state_dir", cfg.Hooks.SessionStateDir)
	return judge
}

// projectMatcher returns a project.Project for the slug — the registered one
// when present, otherwise an ephemeral Project carrying just the slug so
// owner_id-tag matching still works for unregistered projects. Nil-safe.
func (d *Daemon) projectMatcher(slug string) project.Project {
	if p, ok := d.projects.Resolve(slug); ok {
		return p
	}
	return project.Project{Slug: slug}
}

// Start begins the daemon: starts the gRPC server and background loops.
func (d *Daemon) Start(ctx context.Context) error {
	d.ctx, d.cancel = context.WithCancel(ctx)

	// Ensure tmux server is running
	if err := d.tmux.EnsureServer(d.ctx); err != nil {
		return fmt.Errorf("start tmux server: %w", err)
	}

	// Ensure directories exist
	socketPath := d.cfg.Daemon.Socket
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.MkdirAll(d.cfg.Daemon.LogDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if err := os.MkdirAll(d.cfg.Hooks.HookOutputDir, 0o755); err != nil {
		return fmt.Errorf("create hook output dir: %w", err)
	}
	// Coded migration: the session-state contract moved from the
	// application-named ~/data/arcmux/sessions to the protocol dir
	// ~/data/mux/sessions. Sweep legacy docs across on every startup while
	// running on defaults (idempotent, best-effort; overridden configs are
	// the operator's business).
	if d.cfg.Hooks.SessionStateDir == config.DefaultSessionStateDir() {
		if n, err := hooks.MigrateLegacySessionState(config.LegacySessionStateDir(), d.cfg.Hooks.SessionStateDir); err != nil {
			d.logger.Warn("legacy session-state migration incomplete (non-fatal)", "error", err)
		} else if n > 0 {
			d.logger.Info("migrated legacy session-state docs to protocol dir",
				"moved", n, "from", config.LegacySessionStateDir(), "to", d.cfg.Hooks.SessionStateDir)
		}
	}
	// Sweep legacy per-session hook scripts (arcmux-s-*.sh) left by the old
	// generator, then materialize the single generic hook. Both are coded
	// migrations, not one-off shell cleanup: they run on every startup,
	// idempotently. Best-effort / non-fatal so legacy RPCs can still serve if
	// the user's Claude hook dir is temporarily unavailable.
	if d.cfg.Hooks.AutoInstall {
		if !filepath.IsAbs(d.cfg.Hooks.ClaudeHookDir) {
			d.logger.Warn("hook install skipped: claude hook dir is not absolute", "hook_dir", d.cfg.Hooks.ClaudeHookDir)
		} else {
			if n, err := d.hooks.CleanupLegacyScripts(d.cfg.Hooks.ClaudeHookDir); err != nil {
				d.logger.Warn("legacy hook script cleanup failed (non-fatal)", "error", err)
			} else if n > 0 {
				d.logger.Info("swept legacy per-session hook scripts", "removed", n)
			}
			if err := d.hooks.EnsureGenericHook(d.cfg.Hooks.ClaudeHookDir); err != nil {
				d.logger.Warn("generic hook install failed (non-fatal)", "error", err)
			} else {
				d.logger.Info("ensured generic session hook", "path", hooks.GenericHookPath(d.cfg.Hooks.ClaudeHookDir))
			}
		}
		// Materialize the codex bridge script. Registration in codex config is
		// opt-in via [hooks].auto_register because it mutates user config.
		if filepath.IsAbs(d.cfg.Hooks.CodexHookDir) {
			if err := d.hooks.EnsureCodexHook(d.cfg.Hooks.CodexHookDir); err != nil {
				d.logger.Warn("codex hook install failed (non-fatal)", "error", err)
			} else {
				d.logger.Info("ensured codex bridge hook", "path", hooks.CodexHookPath(d.cfg.Hooks.CodexHookDir))
			}
		}
		if d.cfg.Hooks.AutoRegister {
			d.registerAgentHooks()
		}
		// Materialize the grok hook script + drop-in registration. Grok merges
		// ~/.grok/hooks/*.json at session start (always trusted), so this is
		// the complete setup — no manual registration step. Best-effort.
		if filepath.IsAbs(d.cfg.Hooks.GrokHookDir) {
			if err := d.hooks.EnsureGrokHook(d.cfg.Hooks.GrokHookDir); err != nil {
				d.logger.Warn("grok hook install failed (non-fatal)", "error", err)
			} else {
				d.logger.Info("ensured grok session hook", "path", hooks.GrokHookConfigPath(d.cfg.Hooks.GrokHookDir))
			}
		}
	}

	// Open daemon-level state.bolt for the C1 substrate surfaces
	// (Send/PeekInbox/AckInbox/QueryAudit). Best-effort: a failure to
	// open is non-fatal — the daemon still serves legacy RPCs. The C1
	// handlers re-check d.state and return Unavailable if it's nil.
	if err := d.openState(); err != nil {
		d.logger.Warn("open daemon state.bolt failed; C1 inbox/audit RPCs will return Unavailable",
			"error", err)
	}
	if d.cfg.Daemon.ProfileName == "" {
		if err := d.initMeshApplication(); err != nil {
			d.logger.Warn("open mesh protocol state failed; remote projections unavailable", "error", err)
		}
	}

	// Restore sessions from persistence
	d.restoreSessions()

	// Remove stale socket
	os.Remove(socketPath)

	// Start gRPC server
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	d.listener = listener

	// OTel tracing (LabOps observability). Exports OTLP per OTEL_* env; harmless
	// if unconfigured. See monitoring repo docs/otel-conventions.md.
	if shutdown, oerr := initTracer(d.ctx); oerr != nil {
		d.logger.Warn("otel init failed; continuing without tracing", "error", oerr)
	} else {
		d.otelShutdown = shutdown
	}

	d.server = grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	arcmuxv1.RegisterAgentRuntimeServer(d.server, NewGRPCServer(d))

	// Start background loops
	go d.relayHealthEvents()
	go d.watcher.Run(d.ctx)
	go d.persistLoop()
	go d.runOverallGoalSummarizer(d.ctx)

	// Pulse supervisor: one Pulser per discovered project under
	// <Pulse.DataRoot>/arcmux/*/state.bolt. Disabled? Skip silently.
	pcfg, err := d.cfg.Pulse.ParsePulse()
	if err != nil {
		return fmt.Errorf("pulse config: %w", err)
	}
	if pcfg.Enabled {
		d.pulse = NewPulseSupervisor(pcfg, d.mux, d.logger)
		go func() {
			if err := d.pulse.Run(d.ctx); err != nil && err != context.Canceled {
				d.logger.Error("pulse supervisor exited", "error", err)
			}
		}()
	} else {
		d.logger.Info("pulse supervisor disabled (config.pulse.enabled=false)")
	}

	if d.cfg.Daemon.ProfileName == "" && d.cfg.Daemon.HTTPAddr != "" {
		pm, err := NewProfileManager(d)
		if err != nil {
			return fmt.Errorf("profile manager: %w", err)
		}
		d.profileManager = pm
		if err := pm.Start(d.ctx); err != nil {
			return fmt.Errorf("start profile manager: %w", err)
		}
	}

	// Start gRPC server (blocking in goroutine)
	go func() {
		if err := d.server.Serve(listener); err != nil {
			d.logger.Error("grpc serve error", "error", err)
		}
	}()

	// Start HTTP server if configured
	if addr := d.cfg.Daemon.HTTPAddr; addr != "" {
		d.httpSrv = NewHTTPServer(d, addr)
		go func() {
			if err := d.httpSrv.Serve(); err != nil {
				d.logger.Error("http serve error", "error", err)
			}
		}()
		d.logger.Info("http server listening", "addr", addr)
	}

	// Mesh transport is strictly best-effort. A missing/invalid machine-local
	// registry or an unavailable Tailscale path must never prevent local agent
	// sessions from starting and continuing normally.
	// The mesh is machine-scoped and belongs to the root daemon only. Profile
	// daemons share the machine registry; letting each profile start it would
	// duplicate outbound dials and contend for the same listener.
	if d.cfg.Daemon.ProfileName == "" {
		if err := d.ReloadMesh(); err != nil {
			d.logger.Warn("mesh disabled; local sessions unaffected", "error", err)
		}
	}

	d.logger.Info("daemon started",
		"socket", socketPath,
		"tmux_socket", d.cfg.Tmux.SocketName,
	)
	return nil
}

// Stop gracefully shuts down the daemon.
func (d *Daemon) Stop() {
	d.logger.Info("daemon stopping")

	// Persist session state before shutdown
	d.persistSessions()

	// Stop all monitors
	d.mu.Lock()
	for id, cancel := range d.monitors {
		cancel()
		delete(d.monitors, id)
	}
	d.mu.Unlock()

	// Stop gRPC server
	if d.server != nil {
		d.server.GracefulStop()
	}

	// Stop HTTP server
	if d.httpSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.httpSrv.Shutdown(shutdownCtx)
		cancel()
	}
	d.stopMeshTransport()

	// Flush OTLP traces (LabOps observability).
	if d.otelShutdown != nil {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.otelShutdown(flushCtx)
		cancel()
	}

	if d.cancel != nil {
		d.cancel()
	}

	if d.profileManager != nil {
		d.profileManager.Stop()
	}

	// Wait for pulse supervisor to drain so every bolt handle is closed
	// before the daemon process exits — leftover locks would block the
	// next `arcmux start`.
	if d.pulse != nil {
		select {
		case <-d.pulse.Done():
		case <-time.After(5 * time.Second):
			d.logger.Warn("pulse supervisor did not drain within 5s; forcing exit")
		}
	}

	d.eventBus.Close()

	// Close daemon state.bolt last so any in-flight C1 audits flush.
	d.stateMu.Lock()
	if d.state != nil {
		_ = d.state.Close()
		d.state = nil
	}
	d.stateMu.Unlock()
}

// ReloadMesh atomically swaps only the best-effort mesh transport. It never
// restarts the daemon, gRPC server, tmux server, or any managed agent pane.
func (d *Daemon) ReloadMesh() error {
	d.meshReloadMu.Lock()
	defer d.meshReloadMu.Unlock()
	if d.cfg.Daemon.ProfileName != "" {
		return errors.New("mesh is owned by the root daemon, not profile daemons")
	}
	meshCfg, err := d.cfg.Mesh.Parse()
	if err != nil {
		return err
	}
	if !meshCfg.Enabled {
		d.detachMeshTransport()
		return nil
	}
	registry, err := arcmuxmesh.LoadRegistry(meshCfg.RegistryPath)
	if err != nil {
		return err
	}
	d.setMeshDeviceID(registry.DeviceID)
	d.detachMeshTransport()
	if registry.DeviceID == "" || (!registry.Serve && len(registry.Peers) == 0) {
		return nil
	}
	next := arcmuxmesh.New(meshCfg, registry, d.logger)
	if err := d.registerMeshApplication(next); err != nil {
		return err
	}
	if err := next.Start(d.ctx); err != nil {
		return err
	}
	d.meshMu.Lock()
	d.mesh = next
	d.meshMu.Unlock()
	d.startMeshApplicationRuntime(next)
	return nil
}

func (d *Daemon) MeshStatus() (bool, []arcmuxmesh.Status) {
	d.meshMu.RLock()
	defer d.meshMu.RUnlock()
	if d.mesh == nil {
		return false, nil
	}
	return true, d.mesh.Status()
}

func (d *Daemon) MeshPing(ctx context.Context, peer string) (time.Duration, error) {
	d.meshMu.RLock()
	m := d.mesh
	if m == nil {
		d.meshMu.RUnlock()
		return 0, errors.New("mesh is disabled")
	}
	d.meshMu.RUnlock()
	return m.Ping(ctx, peer)
}

// openState opens the daemon-level state.bolt under
// <DataRoot>/arcmux/_daemon/state.bolt. Called from Start; safe to call
// once. A nil d.state after this is the signal to C1 RPCs that the
// substrate is unavailable.
func (d *Daemon) openState() error {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.state != nil {
		return nil
	}
	root := d.cfg.Pulse.DataRoot
	if root == "" {
		// Pulse may be disabled and DataRoot left empty; fall back to a
		// stable per-user path so the daemon still has somewhere to
		// write. Match the install convention: ~/data/arcmux.
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home: %w", err)
		}
		root = filepath.Join(home, "data")
	}
	path := d.cfg.Daemon.StatePath
	if path == "" {
		dir := filepath.Join(root, "arcmux", "_daemon")
		path = filepath.Join(dir, "state.bolt")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	db, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	d.state = db
	d.logger.Info("daemon state.bolt opened", "path", path)
	return nil
}

// State returns the daemon-level bbolt store backing the C1 substrate
// RPCs, or nil if it wasn't opened. C1 RPC handlers use this to short-
// circuit cleanly when the substrate is unavailable.
func (d *Daemon) State() *store.DB {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.state
}

// Logger exposes the daemon's structured logger for sibling packages
// (e.g. C1 RPC handlers in grpc_c1.go) that want to emit at the same
// level + with the same handler chain as core daemon logs.
func (d *Daemon) Logger() *slog.Logger {
	return d.logger
}

// SetState lets tests inject a pre-opened bbolt store. Mirrors the
// pulse_supervisor_test injection pattern so we can exercise the C1
// RPCs without going through Start().
func (d *Daemon) SetState(s *store.DB) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	d.state = s
}

// auditSessionEvent appends an audit row tagged with the session's
// owner_id + session_id when the daemon-level store is open. Best-effort:
// no-op if the store hasn't been opened (test paths that bypass Start).
func (d *Daemon) auditSessionEvent(action string, sess *session.Session, detail map[string]any) {
	st := d.State()
	if st == nil || sess == nil {
		return
	}
	snap := sess.Snapshot()
	if detail == nil {
		detail = map[string]any{}
	}
	// Mirror substrate-wide convention: owner_id + session_id travel in
	// Detail so the existing AuditEntry shape doesn't have to grow new
	// top-level columns in C1 (it will when C2/C3 land — see plan §11).
	if _, ok := detail["owner_id"]; !ok && snap.OwnerID != "" {
		detail["owner_id"] = snap.OwnerID
	}
	if _, ok := detail["session_id"]; !ok {
		detail["session_id"] = snap.ID
	}
	_ = st.AppendAudit(store.AuditEntry{
		Timestamp: time.Now(),
		Action:    action,
		Actor:     "arcmux",
		Subject:   snap.Name,
		Detail:    detail,
	})
}

// FindSessionByName returns the in-memory session with the given Name,
// or nil if none exists. The C1 RPCs key off the human-readable name
// rather than the opaque session_id because (a) elonco-style callers
// manage their own naming, and (b) tests can assert against stable
// names instead of generated IDs. Same lookup helper the HTTP layer
// uses via findByName.
func (d *Daemon) FindSessionByName(name string) *session.Session {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		if s.Snapshot().Name == name {
			return s
		}
	}
	return nil
}

// CreateSession starts a new agent session.
//
// Idempotency: when (req.Name, req.OwnerID) matches an existing
// non-terminal session, the existing session is returned unchanged and
// the returned `created` is false. This lets orchestrators (elonco etc.)
// retry CreateSession after a transient hiccup without producing duplicate
// tmux windows / spawned agents. The match key requires BOTH name and
// owner_id to be set so legacy callers (empty owner_id) don't accidentally
// dedupe across unrelated callers that happen to pick the same name.
func (d *Daemon) CreateSession(ctx context.Context, req CreateSessionRequest) (*session.Session, error) {
	sess, _, err := d.createSessionWithIdempotency(ctx, req)
	return sess, err
}

// createSessionWithIdempotency is the underlying CreateSession entry point
// that also reports whether the returned session was freshly spawned
// (created=true) or matched an existing non-terminal session
// (created=false). The plain CreateSession wraps this to preserve the
// existing call-site signature.
func (d *Daemon) createSessionWithIdempotency(ctx context.Context, req CreateSessionRequest) (*session.Session, bool, error) {
	prof, ok := d.profiles[req.Agent]
	if !ok {
		return nil, false, fmt.Errorf("unknown agent profile: %s", req.Agent)
	}

	// Default CWD to ~/Projects so agent sessions land in a sensible place
	// rather than inheriting the daemon's launch directory.
	if req.CWD == "" {
		if home, err := os.UserHomeDir(); err == nil {
			req.CWD = filepath.Join(home, "Projects")
		}
	}

	id := generateSessionID()
	name := req.Name
	if name == "" {
		// Full id suffix, matching the HTTP path: id[2:10] is only the first 8
		// digits of the nanosecond timestamp (~100s resolution), so two
		// same-agent creates in that window generated IDENTICAL names — and
		// with the same owner_id the idempotency check then returned caller
		// A's session to caller B (observed live: a second `arcmux-cli exec`
		// dispatch was misrouted into the first one's session).
		name = fmt.Sprintf("%s-%s", req.Agent, id[2:])
	}

	// Idempotency check: if a non-terminal session already exists with the
	// same (Name, OwnerID), return it unchanged. Requires BOTH to be set —
	// legacy callers leave OwnerID empty and historically expected each
	// CreateSession to produce a fresh session even when the name happened
	// to collide. Match scoping by owner_id keeps that contract intact.
	if name != "" && req.OwnerID != "" {
		if existing := d.findNonTerminalByNameOwner(name, req.OwnerID, req.private); existing != nil {
			detail := map[string]any{
				"agent": req.Agent,
				"name":  name,
			}
			if !req.private {
				detail["cwd"] = req.CWD
			}
			d.auditSessionEvent("session.create.idempotent_hit", existing, detail)
			snap := existing.Snapshot()
			attributes := []any{
				"session_id", snap.ID,
				"name", snap.Name,
				"owner_id", snap.OwnerID,
				"state", string(snap.State),
			}
			if !req.private {
				attributes = append(attributes, "cwd", req.CWD)
			}
			d.logger.Info("session.create.idempotent_hit", attributes...)
			return existing, false, nil
		}
	}

	sess := session.NewSession(id, name, req.Agent, req.CWD)
	sess.SetTransport(prof.Transport)
	sess.SetEnv(req.Env)
	sess.SetAutoClose(req.AutoClose)
	sess.SetOwnerID(req.OwnerID)
	if req.private {
		sess.MarkPrivate()
	}

	// Audit the create. Best-effort — the store may not be open yet (test
	// paths that don't call Start), in which case auditState returns nil
	// and we skip the row.
	detail := map[string]any{
		"agent": req.Agent,
		"name":  name,
	}
	if !req.private {
		detail["cwd"] = req.CWD
	}
	d.auditSessionEvent("session.create", sess, detail)

	attributes := []any{
		"session_id", id,
		"name", name,
		"agent", req.Agent,
		"owner_id", req.OwnerID,
		"transport", string(prof.Transport),
	}
	if !req.private {
		attributes = append(attributes, "cwd", req.CWD)
	}
	d.logger.Info("session.create", attributes...)

	if prof.Transport == profile.TransportExec {
		sess.SetState(session.StateIdle)
		d.mu.Lock()
		d.sessions[id] = sess
		d.mu.Unlock()
		d.persistSessions()
		d.emitStateChanged(id, session.StateIdle, "session ready")
		if prompt := req.Prompt; prompt != "" {
			go func() {
				if err := d.SendPrompt(d.ctx, id, prompt, false, false); err != nil {
					sess.SetState(session.StateStuck)
					d.emitStateChanged(id, session.StateStuck, "prompt delivery failed")
					d.logger.Error("initial exec prompt failed", "session_id", id, "error", err)
				}
			}()
		}
		return sess, true, nil
	}

	window := req.TmuxWindow
	if window == "" {
		window = name
	}
	tmuxSession := d.agentTmuxSessionName(req.TmuxSession, name, req.OwnerID, id)

	// Install hooks (one generic, idempotent script) + write the per-session
	// env file the agent loads at startup. This MUST happen before the tmux
	// session is created, because the agent is now launched as that session's
	// own command and runs `arcmux hook-env <id>` immediately — the env file
	// has to already exist. The env file lives under /tmp/arcmux with
	// restrictive perms; the loader reads it via `arcmux hook-env` (validated
	// + re-quoted), never sourcing it raw.
	if d.cfg.Hooks.AutoInstall {
		hookPath, err := d.hooks.Install(id, prof.HookType, prof.HookDir)
		if err != nil {
			if req.private {
				d.logger.Warn("private session hook install failed (non-fatal)", "session_id", id)
			} else {
				d.logger.Warn("hook install failed (non-fatal)", "error", err)
			}
		} else {
			d.watcher.Watch(id, hookPath)
		}
		// Seed the per-session hook state doc + drop the env file the agent
		// hooks read. Every hook-backed agent (claude, codex, grok) calls
		// `arcmux hook`, which writes the state doc the hooks judge reads —
		// so give it the session id and the state dir.
		if prof.HookBacked() {
			if err := hooks.InitSessionState(d.cfg.Hooks.SessionStateDir, id, req.Agent, req.Prompt, time.Now()); err != nil {
				if req.private {
					d.logger.Warn("private session state initialization failed (non-fatal)", "session_id", id)
				} else {
					d.logger.Warn("init session state failed (non-fatal)", "error", err)
				}
			}
			sessionEnv := map[string]string{
				"ARCMUX_SESSION_ID":        id,
				"ARCMUX_HOOK_AGENT":        req.Agent,
				"ARCMUX_HOOK_OUTPUT_DIR":   d.cfg.Hooks.HookOutputDir,
				"ARCMUX_SESSION_STATE_DIR": d.cfg.Hooks.SessionStateDir,
				// Lets the hook's vault-link resolver match this session's cwd
				// against the history logs' frontmatter.
				"ARCMUX_SESSION_CWD": req.CWD,
			}
			// Point the hook at this exact arcmux binary so it doesn't depend on
			// `arcmux` being on the agent shell's PATH.
			if exe, err := os.Executable(); err == nil {
				sessionEnv["ARCMUX_BIN"] = exe
			}
			if _, err := hooks.WriteSessionEnvFile(hooks.SessionEnvDir, id, sessionEnv); err != nil {
				if req.private {
					d.logger.Warn("private session hook environment write failed (non-fatal)", "session_id", id)
				} else {
					d.logger.Warn("write session hook env failed (non-fatal)", "error", err)
				}
			}
		}
	}

	// The agent runs AS the tmux session's command (not send-keys'd into an
	// interactive shell). This makes the session transactional with the agent:
	// when the agent exits, tmux closes the pane and destroys the (single-window)
	// session, so a dead agent never leaves a lingering tmux session behind. The
	// health monitor observes the vanished pane and marks the session exited.
	startCommand := d.agentStartCommand(id, req.Agent, prof.StartCommand)

	// Create a dedicated tmux session for this agent, launching the agent as
	// its command. Pass caller-supplied env through so ARCMUX_PROJECT /
	// ARCMUX_ROLE_FILE / OBS_AGENTS / ... are visible to the agent process.
	target, err := d.setupTmuxPane(ctx, tmuxSession, window, req.CWD, req.Env, startCommand)
	if err != nil {
		sess.SetState(session.StateFailed)
		if req.private {
			return nil, false, errors.New("setup private tmux pane failed")
		}
		return nil, false, fmt.Errorf("setup tmux pane: %w", err)
	}
	sess.TmuxSessionName = tmuxSession
	sess.TmuxTarget = target

	// Set up output streaming via pipe-pane
	outputFile := d.outputFilePath(id)
	if err := d.tmux.PipePaneStart(ctx, target, outputFile); err != nil {
		if req.private {
			d.logger.Warn("private session output capture setup failed", "session_id", id)
		} else {
			d.logger.Warn("pipe-pane setup failed", "error", err)
		}
	}

	// Store session before starting async work
	d.mu.Lock()
	d.sessions[id] = sess
	d.mu.Unlock()
	d.persistSessions()

	// Start agent and handshake in background
	go d.startAgentLifecycle(id, sess, prof, req.Prompt)

	return sess, true, nil
}

// findNonTerminalByNameOwner returns an existing session that matches the
// given (name, ownerID) tuple and is NOT in a terminal state. Terminal
// states are StateExited and StateFailed — those sessions are dead handles
// and a fresh CreateSession SHOULD spawn a new session to replace them.
func (d *Daemon) findNonTerminalByNameOwner(name, ownerID string, private bool) *session.Session {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		snap := s.Snapshot()
		if snap.Name != name || snap.OwnerID != ownerID || snap.Private != private {
			continue
		}
		switch snap.State {
		case session.StateExited, session.StateFailed:
			continue
		}
		return s
	}
	return nil
}

// agentStartCommand returns the command the tmux loader runs to launch the
// agent. For hook-backed agents (claude, codex, grok) it prefixes a fail-safe
// env loader:
//
//	eval "$('<abs-arcmux>' hook-env '<id>')" ; <StartCommand>
//
// We use the daemon's own absolute binary path (os.Executable) rather than a
// bare `arcmux`, because PATH is not guaranteed inside the spawned pane. The
// eval consumes arcmux's OWN single-quote-escaped output — it never sources
// the raw /tmp/arcmux file. If `hook-env` errors it prints nothing and exits
// 0, so the eval is a no-op and the agent still launches with no injected env
// (the generic hook then safely no-ops). For non-hook-backed agents or when
// AutoInstall is off, the StartCommand is returned unchanged.
func (d *Daemon) agentStartCommand(id, agent, startCommand string) string {
	prof, ok := d.profiles[agent]

	// Render per-session start args (e.g. grok's private leader socket).
	// Placeholder values are substituted as individually shell-quoted tokens;
	// adjacent quoted segments concatenate, so templates may embed them in
	// larger arguments ("{hook_dir}/x-{session_id}.sock").
	if ok && prof.SessionStartArgs != "" {
		args := strings.ReplaceAll(prof.SessionStartArgs, "{session_id}", shellSingleQuote(id))
		args = strings.ReplaceAll(args, "{hook_dir}", shellSingleQuote(prof.HookDir))
		startCommand = startCommand + " " + args
	}

	// `exec` so the agent REPLACES the `sh -c` wrapper that tmux launches it
	// under: the pane's process becomes the agent itself, so the pane (and the
	// single-window session) dies exactly when the agent exits — no leftover
	// shell holding it open. Even without hooks the agent is exec'd.
	if !d.cfg.Hooks.AutoInstall || !ok || !prof.HookBacked() {
		return "exec " + startCommand
	}
	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = "arcmux" // fall back to PATH resolution
	}
	return fmt.Sprintf(`eval "$(%s hook-env %s)" ; exec %s`,
		shellSingleQuote(bin), shellSingleQuote(id), startCommand)
}

// shellSingleQuote wraps s in single quotes, POSIX-escaping embedded quotes,
// so it is a single safe shell token in the loader command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (d *Daemon) startAgentLifecycle(id string, sess *session.Session, prof profile.Profile, prompt string) {
	ctx := d.ctx

	// The agent was already launched as the tmux session's own command (see
	// CreateSession + NewSessionWithEnv): no send-keys launch step, so there is
	// no shell-readiness race and a dead agent leaves no lingering session.
	// Proceed straight to the handshake.
	sess.SetState(session.StateHandshaking)
	d.emitStateChanged(id, session.StateHandshaking, "")

	if err := d.performHandshake(ctx, sess, prof); err != nil {
		sess.SetState(session.StateFailed)
		d.emitStateChanged(id, session.StateFailed, err.Error())
		d.logger.Error("handshake failed", "session_id", id, "error", err)
		return
	}

	sess.SetState(session.StateIdle)
	d.emitStateChanged(id, session.StateIdle, "agent ready")

	// Deliver initial prompt if provided
	if prompt != "" {
		if err := d.deliverPrompt(ctx, sess, prof, prompt, false); err != nil {
			sess.SetState(session.StateStuck)
			d.emitStateChanged(id, session.StateStuck, "prompt delivery failed")
			d.logger.Error("initial prompt delivery failed", "session_id", id, "error", err)
			d.emitEvent(Event{
				SessionID: id,
				Type:      "prompt_failed",
				Message:   err.Error(),
				Timestamp: time.Now(),
			})
			return
		}
		sess.SetCurrentCommand(truncatePreview(prompt, 200))
		sess.SetState(session.StateWorking)
		sess.ResetNudge()
		d.emitStateChanged(id, session.StateWorking, "prompt delivered")
		d.emitEvent(Event{
			SessionID: id,
			Type:      "prompt_delivered",
			Timestamp: time.Now(),
		})
	}

	// Start health monitor
	d.startMonitor(id, sess, prof)
}

func (d *Daemon) startMonitor(id string, sess *session.Session, prof profile.Profile) {
	monitorCtx, monitorCancel := context.WithCancel(d.ctx)
	d.mu.Lock()
	d.monitors[id] = monitorCancel
	d.mu.Unlock()

	captureInterval := d.cfg.CaptureInterval()
	monitor := health.NewMonitor(d.tmux, captureInterval, d.healthEvents, d.cfg.Hooks.SessionStateDir)
	go monitor.Run(monitorCtx, sess, prof)
}

// SendPrompt sends a prompt to a running session.
func (d *Daemon) SendPrompt(ctx context.Context, sessionID, text string, confirmDelivery, waitIdle bool) error {
	// Test hook: when a sendPromptHook is installed (unit tests only),
	// route the call there so the test can observe the arguments and
	// short-circuit the transport dispatch.
	if d.sendPromptHook != nil {
		return d.sendPromptHook(ctx, sessionID, text, confirmDelivery, waitIdle)
	}
	sess, ok := d.GetSession(sessionID)
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	snap := sess.Snapshot()
	prof, ok := d.profiles[snap.Agent]
	if !ok {
		return fmt.Errorf("unknown agent profile: %s", snap.Agent)
	}

	d.logger.Info("session.send",
		"session_id", sessionID,
		"name", snap.Name,
		"bytes", len(text),
		"preview", truncatePreview(text, 50),
	)

	if prof.Transport == profile.TransportExec {
		return d.sendExecPrompt(ctx, sess, prof, text, confirmDelivery, waitIdle)
	}

	// Wait for idle if requested and agent is working
	if waitIdle && snap.State == session.StateWorking {
		if err := d.tmux.WaitIdle(ctx, snap.TmuxTarget, prof.StuckTimeout, 2*time.Second); err != nil {
			return fmt.Errorf("wait for idle: %w", err)
		}
	}

	if err := d.deliverPrompt(ctx, sess, prof, text, confirmDelivery); err != nil {
		sess.SetState(session.StateStuck)
		d.emitStateChanged(sessionID, session.StateStuck, "prompt delivery failed")
		return err
	}

	// Track the last prompt as the session's current_command so the
	// persisted sessions.json reflects what the agent is working on,
	// and arcmux-cli capture / list responses show it without scraping
	// the pane. Mirrors what the exec transport already sets for
	// subprocess commands.
	sess.SetCurrentCommand(truncatePreview(text, 200))
	sess.SetState(session.StateWorking)
	sess.ResetNudge()
	d.emitStateChanged(sessionID, session.StateWorking, "prompt delivered")
	d.emitEvent(Event{
		SessionID: sessionID,
		Type:      "prompt_delivered",
		Timestamp: time.Now(),
	})
	return nil
}

// Capture returns the current pane output.
func (d *Daemon) Capture(ctx context.Context, sessionID string, includeHistory bool) (string, error) {
	// Test hook: when installed (unit tests only), short-circuit before any
	// tmux/exec read so the HTTP capture shim can be tested without a pane.
	if d.captureHook != nil {
		return d.captureHook(ctx, sessionID, includeHistory)
	}
	sess, ok := d.GetSession(sessionID)
	if !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}
	snap := sess.Snapshot()

	if snap.Transport == profile.TransportExec {
		data, err := os.ReadFile(d.outputFilePath(sessionID))
		if err != nil {
			if os.IsNotExist(err) {
				return "", nil
			}
			return "", err
		}
		return string(data), nil
	}

	if includeHistory {
		return d.tmux.CapturePaneHistory(ctx, snap.TmuxTarget)
	}
	return d.tmux.CapturePaneVisible(ctx, snap.TmuxTarget)
}

// Status returns the current session status.
func (d *Daemon) Status(sessionID string) (*session.Session, error) {
	sess, ok := d.GetSession(sessionID)
	if !ok {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return sess, nil
}

// Kill terminates a session.
func (d *Daemon) Kill(ctx context.Context, sessionID string, graceful bool, timeout time.Duration) error {
	sess, ok := d.GetSession(sessionID)
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	snap := sess.Snapshot()

	d.logger.Info("session.close",
		"session_id", sessionID,
		"name", snap.Name,
		"agent", snap.Agent,
		"graceful", graceful,
		"final_state", string(snap.State),
	)

	if snap.Transport == profile.TransportExec {
		return d.killExecSession(ctx, sess, graceful, timeout)
	}

	// Stop monitor and any active screen recorder.
	d.mu.Lock()
	if cancel, ok := d.monitors[sessionID]; ok {
		cancel()
		delete(d.monitors, sessionID)
	}
	if r, ok := d.recorders[sessionID]; ok {
		delete(d.recorders, sessionID)
		go r.stop()
	}
	d.mu.Unlock()

	if graceful {
		// Send Ctrl-C and wait
		_ = d.tmux.SendKeys(ctx, snap.TmuxTarget, "C-c")
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_ = d.tmux.WaitIdle(waitCtx, snap.TmuxTarget, timeout, 1*time.Second)
	}

	if snap.TmuxSessionName != "" {
		if err := d.tmux.KillSession(ctx, snap.TmuxSessionName); err != nil {
			d.logger.Warn("kill tmux session failed", "session", snap.TmuxSessionName, "error", err)
		}
	} else if err := d.tmux.KillPane(ctx, snap.TmuxTarget); err != nil {
		d.logger.Warn("kill pane failed", "error", err)
	}

	// Stop pipe-pane and cleanup
	d.tmux.PipePaneStop(ctx, snap.TmuxTarget)
	d.watcher.Unwatch(sessionID)
	d.hooks.Cleanup(sessionID)
	// Retire the session's hook state doc into archived/ now that nothing is
	// watching it. Best-effort: a missing file (screen-only agents) is fine.
	if err := hooks.ArchiveSessionState(d.cfg.Hooks.SessionStateDir, sessionID); err != nil {
		d.logger.Warn("archive session state failed (non-fatal)", "session", sessionID, "error", err)
	}

	sess.SetState(session.StateExited)
	d.persistSessions()
	d.emitStateChanged(sessionID, session.StateExited, "killed")

	return nil
}

// ListSessions returns all managed sessions.
func (d *Daemon) ListSessions() []*session.Session {
	d.mu.RLock()
	defer d.mu.RUnlock()
	list := make([]*session.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		list = append(list, s)
	}
	return list
}

// ListAgentProfiles returns a copy of the registered agent profiles
// (built-in classes merged with config [agents] overrides). The map is
// immutable after construction, so no lock is needed; copying keeps callers
// from mutating the registry.
func (d *Daemon) ListAgentProfiles() map[string]profile.Profile {
	out := make(map[string]profile.Profile, len(d.profiles))
	for name, prof := range d.profiles {
		out[name] = prof
	}
	return out
}

// GetSession returns a session by ID.
func (d *Daemon) GetSession(id string) (*session.Session, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sess, ok := d.sessions[id]
	return sess, ok
}

// OutputFilePath returns the output log path for a session.
func (d *Daemon) outputFilePath(id string) string {
	return filepath.Join(d.cfg.Hooks.HookOutputDir, fmt.Sprintf("arcmux-output-%s.log", id))
}

// ScreenLogPath returns the screen-recording log path for a session.
func (d *Daemon) ScreenLogPath(sessionID string) string {
	return filepath.Join(d.cfg.ScreenLogDir(), sessionID+".screen.log")
}

// SetRecording enables (on=true) or cancels (on=false) aggressive screen
// recording for a session. Enable is idempotent. Recording is decoupled from
// any client: it stops only via this cancel or session close — never on a
// client/context disconnect.
func (d *Daemon) SetRecording(sessionID string, on bool) (string, error) {
	if _, ok := d.GetSession(sessionID); !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}
	logPath := d.ScreenLogPath(sessionID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.recorders == nil {
		d.recorders = map[string]*recorder{}
	}
	if on {
		if _, exists := d.recorders[sessionID]; exists {
			return logPath, nil // idempotent
		}
		if err := os.MkdirAll(d.cfg.ScreenLogDir(), 0o755); err != nil {
			return "", err
		}
		capture := func(ctx context.Context) (string, error) {
			return d.Capture(ctx, sessionID, false)
		}
		r := newRecorder(logPath, capture, time.Second, d.logger)
		r.start(d.ctx)
		d.recorders[sessionID] = r
		d.logger.Info("voice recording started", "session_id", sessionID, "log", logPath)
		return logPath, nil
	}
	if r, exists := d.recorders[sessionID]; exists {
		delete(d.recorders, sessionID)
		go r.stop() // stop outside the lock-sensitive path; file is kept
		d.logger.Info("voice recording stopped", "session_id", sessionID)
	}
	return logPath, nil
}

// RecordingStatus reports whether a session is being recorded and its log path.
func (d *Daemon) RecordingStatus(sessionID string) (bool, string, time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if r, ok := d.recorders[sessionID]; ok {
		return true, r.logPath, r.startedAt
	}
	return false, d.ScreenLogPath(sessionID), time.Time{}
}

// recordingSessions returns the IDs of all sessions currently recording.
func (d *Daemon) recordingSessions() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.recorders))
	for id := range d.recorders {
		out = append(out, id)
	}
	return out
}

// Subscribe returns a channel of events. Call Unsubscribe to stop.
func (d *Daemon) Subscribe(sessionID string) (<-chan Event, int) {
	return d.eventBus.Subscribe(sessionID)
}

// Unsubscribe removes an event subscription.
func (d *Daemon) Unsubscribe(subID int) {
	d.eventBus.Unsubscribe(subID)
}

func (d *Daemon) emitEvent(event Event) {
	d.eventBus.Publish(event)
}

func (d *Daemon) emitStateChanged(sessionID string, state session.State, message string) {
	d.emitEvent(Event{
		SessionID: sessionID,
		Type:      "state_changed",
		State:     string(state),
		Message:   message,
		Timestamp: time.Now(),
	})
	// On every transition INTO idle, opportunistically drain the
	// session_inbox so queued messages don't sit forever waiting on a
	// caller to poll Ready + reissue Send. Closes the "first Send after
	// CreateSession queued, then never re-delivered" gap and the wider
	// "messages sit forever if the agent doesn't poll" hole.
	//
	// Best-effort + fire-and-forget — a slow drain must NOT block the
	// state machine. drainInboxOnIdle re-checks state inside the
	// goroutine, ignores empty/missing inboxes, and short-circuits if
	// the session has already moved away from idle.
	if state == session.StateIdle {
		go d.drainInboxOnIdle(sessionID)
	}
}

func (d *Daemon) registerAgentHooks() {
	if !d.cfg.Hooks.AutoRegister {
		return
	}

	if !filepath.IsAbs(d.cfg.Hooks.ClaudeHookDir) {
		d.logger.Warn("claude hook registration skipped: claude hook dir is not absolute",
			"hook_dir", d.cfg.Hooks.ClaudeHookDir)
	} else if changed, err := hooks.RegisterClaudeHooks(d.cfg.Hooks.ClaudeHookDir); err != nil {
		d.logger.Warn("claude hook registration skipped (non-fatal)",
			"path", hooks.ClaudeSettingsPath(d.cfg.Hooks.ClaudeHookDir), "error", err)
	} else if changed {
		d.logger.Info("registered claude hook config",
			"path", hooks.ClaudeSettingsPath(d.cfg.Hooks.ClaudeHookDir))
	}

	if !filepath.IsAbs(d.cfg.Hooks.CodexHookDir) {
		d.logger.Warn("codex hook registration skipped: codex hook dir is not absolute",
			"hook_dir", d.cfg.Hooks.CodexHookDir)
	} else if changed, err := hooks.RegisterCodexHooks(d.cfg.Hooks.CodexHookDir); err != nil {
		d.logger.Warn("codex hook registration skipped (non-fatal)",
			"path", hooks.CodexHooksConfigPath(d.cfg.Hooks.CodexHookDir), "error", err)
	} else if changed {
		d.logger.Info("registered codex hook config",
			"path", hooks.CodexHooksConfigPath(d.cfg.Hooks.CodexHookDir))
	}
}

// drainInboxOnIdle is the state-transition hook for inbox draining. Runs
// once per idle transition, delivers at most one queued message, then
// returns. SendPrompt itself will flip the session back to Working, which
// produces another idle transition when the prompt completes — that next
// transition picks up the next queued message. This avoids any recursive
// loop while still making forward progress one message at a time.
func (d *Daemon) drainInboxOnIdle(sessionID string) {
	sess, ok := d.GetSession(sessionID)
	if !ok {
		return
	}
	if !sessionReady(sess) {
		return
	}
	snap := sess.Snapshot()
	if snap.Name == "" {
		// Inbox is keyed by session name (not the opaque session_id).
		// Sessions without a stable name are out of scope for the C1
		// substrate; nothing to drain.
		return
	}
	st := d.State()
	if st == nil {
		return
	}
	msgs, err := st.PeekSessionInbox(snap.Name, 1)
	if err != nil || len(msgs) == 0 {
		return
	}
	msg := msgs[0]
	// Deliver via the normal SendPrompt path so all the usual side
	// effects (state→Working, prompt_delivered event, audit row) fire.
	if err := d.SendPrompt(d.ctx, snap.ID, msg.Body, false, false); err != nil {
		d.logger.Warn("inbox drain: SendPrompt failed; leaving message queued",
			"session_id", snap.ID, "name", snap.Name, "msg_id", msg.ID, "error", err)
		return
	}
	// Ack — message left the queue and is now in the agent.
	if err := st.AckSessionInbox(snap.Name, msg.ID); err != nil {
		d.logger.Warn("inbox drain: ack failed (msg already delivered)",
			"session_id", snap.ID, "name", snap.Name, "msg_id", msg.ID, "error", err)
	}
	d.auditSessionEvent("inbox.drain.delivered", sess, map[string]any{
		"msg_id": msg.ID,
		"from":   msg.From,
	})
	d.logger.Info("session.send.drained",
		"session_id", snap.ID,
		"name", snap.Name,
		"msg_id", msg.ID,
		"from", msg.From,
		"bytes", len(msg.Body),
	)
}

func (d *Daemon) relayHealthEvents() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case he := <-d.healthEvents:
			d.emitEvent(Event{
				SessionID: he.SessionID,
				Type:      he.Type,
				State:     "", // health events don't carry state directly
				Message:   he.Reason,
				Timestamp: he.Timestamp,
				Data:      map[string]string{"output": he.Output},
			})
		}
	}
}

func (d *Daemon) setupTmuxPane(ctx context.Context, tmuxSession, window, cwd string, env map[string]string, command string) (string, error) {
	// Create exactly one tmux session per agent. tmux stores environment at
	// session scope; creating sibling agents as new windows under a shared
	// session leaves `show-environment -t <session>` pinned to whichever
	// profile created the session first. A fresh session per agent gives each
	// launch its own session-scoped env while keeping the daemon on one socket.
	//
	// Crucially, we return the freshly-created pane's `%pane_id`, NOT the
	// canonical `<session>:<window-name>` form. Window names are mutable
	// and non-unique in tmux: two windows can share a name within one
	// session, and tmux `send-keys -t <session>:<name>` is ambiguous when
	// that happens — it routes to whichever pane tmux's index-resolution
	// picks. pane_id is unique server-wide and stable for the pane's
	// lifetime, so SendKeys / pipe-pane / capture-pane are unambiguous.
	//
	// This closes the elonco bug where rapid CreateSession calls produced
	// 13 correctly-named windows but every prompt got pasted into a
	// pre-existing pane because the routing target was a window name that
	// resolved to the wrong window.
	pid, err := d.tmux.NewSessionWithEnvPaneID(ctx, tmuxSession, window, cwd, env, command)
	if err == nil {
		return pid, nil
	}
	return "", fmt.Errorf("create tmux session %q: %w", tmuxSession, err)
}

func (d *Daemon) agentTmuxSessionName(requested, name, ownerID, sessionID string) string {
	if requested != "" {
		return safeTmuxSessionName(requested, "")
	}
	parts := []string{name}
	suffix := sessionID
	if ownerID != "" {
		parts = []string{ownerID, name}
		suffix = ""
	}
	return safeTmuxSessionName(strings.Join(parts, "-"), suffix)
}

func safeTmuxSessionName(raw, sessionID string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = r == '-'
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "agent"
	}
	if len(name) > 80 {
		name = strings.TrimRight(name[:80], "-")
	}
	if sessionID != "" {
		suffix := sessionID
		if len(suffix) > 10 {
			suffix = suffix[:10]
		}
		if !strings.Contains(name, suffix) {
			name = name + "-" + suffix
		}
	}
	return name
}

// --- Persistence ---

type persistedSession struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	Agent               string        `json:"agent"`
	CWD                 string        `json:"cwd"`
	Transport           string        `json:"transport"`
	TmuxSessionName     string        `json:"tmux_session_name,omitempty"`
	TmuxTarget          string        `json:"tmux_target"`
	CurrentCommand      string        `json:"current_command"`
	BackendSessionID    string        `json:"backend_session_id"`
	State               session.State `json:"state"`
	StartedAt           time.Time     `json:"started_at"`
	OwnerID             string        `json:"owner_id,omitempty"`
	Private             bool          `json:"private,omitempty"`
	HandoffInstructions string        `json:"handoff_instructions,omitempty"`
}

func (d *Daemon) persistPath() string {
	// Named profiles share one socket directory, so the socket path cannot
	// define ownership of their session inventory. StatePath is profile-local
	// and is the authoritative persistence root. LogDir is a compatibility
	// fallback for hand-built profile configs that predate StatePath.
	if d.cfg.Daemon.StatePath != "" {
		return filepath.Join(filepath.Dir(d.cfg.Daemon.StatePath), "sessions.json")
	}
	if d.cfg.Daemon.ProfileName != "" && d.cfg.Daemon.LogDir != "" {
		return filepath.Join(filepath.Dir(d.cfg.Daemon.LogDir), "sessions.json")
	}
	return filepath.Join(filepath.Dir(d.cfg.Daemon.Socket), "sessions.json")
}

func (d *Daemon) legacyPersistPath() string {
	return filepath.Join(filepath.Dir(d.cfg.Daemon.Socket), "sessions.json")
}

func (d *Daemon) persistSessions() {
	if err := d.persistSessionsChecked(); err != nil {
		d.logger.Error("persist sessions", "error", err)
	}
}

// persistSessionsChecked synchronously writes the current inventory and lets
// callers that establish a durable protocol boundary fail closed when the
// write did not complete. Ordinary background persistence uses
// persistSessions, which preserves the historical best-effort behavior.
func (d *Daemon) persistSessionsChecked() error {
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	d.mu.RLock()
	records := make([]persistedSession, 0, len(d.sessions))
	for _, s := range d.sessions {
		snap := s.Snapshot()
		// Only persist non-terminal sessions
		if snap.State == session.StateExited || snap.State == session.StateFailed {
			continue
		}
		record := persistedSession{
			ID:               snap.ID,
			Name:             snap.Name,
			Agent:            snap.Agent,
			CWD:              snap.CWD,
			Transport:        snap.Transport,
			TmuxSessionName:  snap.TmuxSessionName,
			TmuxTarget:       snap.TmuxTarget,
			CurrentCommand:   snap.CurrentCommand,
			BackendSessionID: snap.BackendSessionID,
			State:            snap.State,
			StartedAt:        snap.StartedAt,
			OwnerID:          snap.OwnerID,
			Private:          snap.Private,
		}
		if snap.Private {
			record.HandoffInstructions = snap.Env["ARCMUX_HANDOFF_INSTRUCTIONS"]
		}
		records = append(records, record)
	}
	d.mu.RUnlock()

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return errors.New("marshal session inventory")
	}

	path := d.persistPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.New("create session inventory directory")
	}
	if existing, statErr := os.Lstat(path); statErr == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return errors.New("session inventory path is unsafe")
		}
	} else if !os.IsNotExist(statErr) {
		return errors.New("inspect session inventory")
	}
	tmp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return errors.New("create session inventory temp")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("secure session inventory temp")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("write session inventory")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("sync session inventory")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("close session inventory")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("publish session inventory")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("secure session inventory")
	}
	directory, err := os.Open(dir)
	if err != nil {
		return errors.New("open session inventory directory")
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return errors.New("sync session inventory directory")
	}
	if err := directory.Close(); err != nil {
		return errors.New("close session inventory directory")
	}
	return nil
}

func (d *Daemon) restoreSessions() {
	path := d.persistPath()
	data, err := os.ReadFile(path)
	// Root daemons may migrate from the historical socket-adjacent inventory.
	// Named profiles must not read it: that old location was shared by every
	// profile and has no ownership marker, so guessing would cross-contaminate
	// independent catalogs.
	if os.IsNotExist(err) && d.cfg.Daemon.ProfileName == "" && path != d.legacyPersistPath() {
		legacyPath := d.legacyPersistPath()
		if legacyData, legacyErr := os.ReadFile(legacyPath); legacyErr == nil {
			data, err = legacyData, nil
			d.logger.Info("restoring legacy socket-adjacent session inventory", "path", legacyPath)
		}
	}
	if err != nil {
		if !os.IsNotExist(err) {
			d.logger.Warn("restore sessions read", "error", err)
		}
		return
	}

	var records []persistedSession
	if err := json.Unmarshal(data, &records); err != nil {
		d.logger.Warn("restore sessions parse", "error", err)
		return
	}

	ctx := context.Background()
	restored := 0
	for _, rec := range records {
		if _, ok := d.profiles[rec.Agent]; !ok {
			d.logger.Info("restored session agent unknown, skipping",
				"session_id", rec.ID, "agent", rec.Agent)
			continue
		}
		if rec.Transport == "" {
			rec.Transport = profile.TransportTmux
			if prof, ok := d.profiles[rec.Agent]; ok && prof.Transport != "" {
				rec.Transport = prof.Transport
			}
		}

		if rec.Transport == profile.TransportExec {
			sess := session.NewSession(rec.ID, rec.Name, rec.Agent, rec.CWD)
			sess.SetTransport(rec.Transport)
			sess.SetOwnerID(rec.OwnerID)
			if rec.Private {
				sess.MarkPrivate()
			}
			if rec.Private && rec.HandoffInstructions != "" {
				sess.SetEnv(map[string]string{"ARCMUX_HANDOFF_INSTRUCTIONS": rec.HandoffInstructions})
			}
			sess.SetCurrentCommand(rec.CurrentCommand)
			sess.SetBackendSessionID(rec.BackendSessionID)
			sess.StartedAt = rec.StartedAt
			if rec.State == session.StateWorking {
				sess.SetState(session.StateIdle)
			} else {
				sess.SetState(rec.State)
			}

			d.mu.Lock()
			d.sessions[rec.ID] = sess
			d.mu.Unlock()
			restored++
			continue
		}

		// Check if the tmux pane still exists
		if !d.tmux.PaneExists(ctx, rec.TmuxTarget) {
			d.logger.Info("restored session pane gone, skipping",
				"session_id", rec.ID, "target", rec.TmuxTarget)
			continue
		}

		sess := session.NewSession(rec.ID, rec.Name, rec.Agent, rec.CWD)
		sess.SetTransport(rec.Transport)
		sess.SetOwnerID(rec.OwnerID)
		if rec.Private {
			sess.MarkPrivate()
		}
		if rec.Private && rec.HandoffInstructions != "" {
			sess.SetEnv(map[string]string{"ARCMUX_HANDOFF_INSTRUCTIONS": rec.HandoffInstructions})
		}
		sess.TmuxSessionName = rec.TmuxSessionName
		sess.TmuxTarget = rec.TmuxTarget
		sess.SetCurrentCommand(rec.CurrentCommand)
		sess.SetBackendSessionID(rec.BackendSessionID)
		sess.StartedAt = rec.StartedAt
		sess.SetState(rec.State)

		d.mu.Lock()
		d.sessions[rec.ID] = sess
		d.mu.Unlock()

		// Restart monitor for active sessions
		if rec.State == session.StateWorking || rec.State == session.StateIdle || rec.State == session.StateStuck {
			if prof, ok := d.profiles[rec.Agent]; ok {
				d.startMonitor(rec.ID, sess, prof)
			}
		}

		// Restart hook watcher
		hookPath := d.hooks.OutputPath(rec.ID)
		if _, err := os.Stat(hookPath); err == nil {
			d.watcher.Watch(rec.ID, hookPath)
		}
		// Ensure the per-session hook state doc still exists for restored
		// hook-backed agents (idempotent: preserves existing event fields).
		if prof, ok := d.profiles[rec.Agent]; ok && prof.HookBacked() {
			if err := hooks.InitSessionState(d.cfg.Hooks.SessionStateDir, rec.ID, rec.Agent, "", time.Now()); err != nil {
				d.logger.Warn("init session state on restore failed (non-fatal)", "session", rec.ID, "error", err)
			}
		}

		restored++
	}

	if restored > 0 {
		d.logger.Info("restored sessions", "count", restored)
	}
}

func (d *Daemon) persistLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.persistSessions()
		}
	}
}

// --- Helpers ---

// CreateSessionRequest is the input for creating a session.
type CreateSessionRequest struct {
	Agent       string
	CWD         string
	Prompt      string
	Name        string
	TmuxSession string
	TmuxWindow  string
	Env         map[string]string
	AutoClose   bool // exec transport only: transition to StateExited on subprocess exit
	// OwnerID is a free-form caller-attribution tag (e.g. "elonco:my-project"),
	// recorded on the Session and on every audit row the daemon writes for
	// that session. Empty default for legacy callers.
	OwnerID string
	// private is trusted internal provenance. Network request surfaces cannot
	// set it; only the handoff launcher in this package may opt in.
	private bool
}

func generateSessionID() string {
	return fmt.Sprintf("s-%d", time.Now().UnixNano())
}

// truncatePreview returns up to n runes of s, with newlines collapsed to a
// single space so a log line stays single-line, and "…" appended when
// truncated. Used by session.send logging so we don't dump entire prompts
// into INFO output but still get a useful glimpse for debugging.
func truncatePreview(s string, n int) string {
	// Collapse whitespace runs to single space first.
	flat := make([]rune, 0, len(s))
	prevSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		flat = append(flat, r)
	}
	if len(flat) <= n {
		return string(flat)
	}
	return string(flat[:n]) + "…"
}
