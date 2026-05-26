package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/delivery"
	"github.com/lin-labs/arcmux/internal/health"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/mux"
	"github.com/lin-labs/arcmux/internal/muxbuild"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/tmux"
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
	sessions  map[string]*session.Session
	monitors  map[string]context.CancelFunc
	processes map[string]*os.Process

	healthEvents chan health.HealthEvent
	eventBus     *EventBus
	delivery     *delivery.Controller

	server   *grpc.Server
	listener net.Listener
	httpSrv  *HTTPServer
	ctx      context.Context
	cancel   context.CancelFunc

	pulse *PulseSupervisor

	// state is the daemon-level bbolt store backing the C1 substrate RPCs
	// (Send/PeekInbox/AckInbox/QueryAudit). Opened at Start, lazily on
	// first need if Start hasn't been called (test path). One file:
	// <DataRoot>/arcmux/_daemon/state.bolt. Distinct from per-project
	// state.bolt files the pulse supervisor opens — those still live at
	// <DataRoot>/arcmux/<project>/state.bolt.
	stateMu sync.Mutex
	state   *store.DB
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
	return &Daemon{
		cfg:          cfg,
		tmux:         tmux.NewClient(cfg.Tmux.SocketName),
		mux:          backend,
		hooks:        hooks.NewInstaller(cfg.Hooks.HookOutputDir),
		watcher:      hooks.NewWatcher(cfg.Hooks.HookOutputDir, logger),
		profiles:     cfg.Agents,
		logger:       logger,
		sessions:     make(map[string]*session.Session),
		monitors:     make(map[string]context.CancelFunc),
		processes:    make(map[string]*os.Process),
		healthEvents: make(chan health.HealthEvent, 64),
		eventBus:     NewEventBus(),
		delivery:     delivery.NewController(delivery.NewJudge(), delivery.DefaultControllerConfig()),
	}
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

	// Open daemon-level state.bolt for the C1 substrate surfaces
	// (Send/PeekInbox/AckInbox/QueryAudit). Best-effort: a failure to
	// open is non-fatal — the daemon still serves legacy RPCs. The C1
	// handlers re-check d.state and return Unavailable if it's nil.
	if err := d.openState(); err != nil {
		d.logger.Warn("open daemon state.bolt failed; C1 inbox/audit RPCs will return Unavailable",
			"error", err)
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

	d.server = grpc.NewServer()
	arcmuxv1.RegisterAgentRuntimeServer(d.server, NewGRPCServer(d))

	// Start background loops
	go d.relayHealthEvents()
	go d.watcher.Run(d.ctx)
	go d.persistLoop()

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

	if d.cancel != nil {
		d.cancel()
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
	dir := filepath.Join(root, "arcmux", "_daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "state.bolt")
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
func (d *Daemon) CreateSession(ctx context.Context, req CreateSessionRequest) (*session.Session, error) {
	prof, ok := d.profiles[req.Agent]
	if !ok {
		return nil, fmt.Errorf("unknown agent profile: %s", req.Agent)
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
		name = fmt.Sprintf("%s-%s", req.Agent, id[2:10])
	}
	sess := session.NewSession(id, name, req.Agent, req.CWD)
	sess.SetTransport(prof.Transport)
	sess.SetEnv(req.Env)
	sess.SetAutoClose(req.AutoClose)
	sess.SetOwnerID(req.OwnerID)

	// Audit the create. Best-effort — the store may not be open yet (test
	// paths that don't call Start), in which case auditState returns nil
	// and we skip the row.
	d.auditSessionEvent("session.create", sess, map[string]any{
		"agent": req.Agent,
		"cwd":   req.CWD,
		"name":  name,
	})

	d.logger.Info("session.create",
		"session_id", id,
		"name", name,
		"agent", req.Agent,
		"cwd", req.CWD,
		"owner_id", req.OwnerID,
		"transport", string(prof.Transport),
	)

	if prof.Transport == profile.TransportExec {
		sess.SetState(session.StateIdle)
		d.mu.Lock()
		d.sessions[id] = sess
		d.mu.Unlock()
		d.persistSessions()
		d.emitStateChanged(id, session.StateIdle, "session ready")
		if prompt := req.Prompt; prompt != "" {
			go func() {
				if err := d.SendPrompt(d.ctx, id, prompt, true, false); err != nil {
					sess.SetState(session.StateStuck)
					d.emitStateChanged(id, session.StateStuck, "prompt delivery failed")
					d.logger.Error("initial exec prompt failed", "session_id", id, "error", err)
				}
			}()
		}
		return sess, nil
	}

	// Determine tmux target
	tmuxSession := d.cfg.Tmux.DefaultSession
	if req.TmuxSession != "" {
		tmuxSession = req.TmuxSession
	}
	window := req.TmuxWindow
	if window == "" {
		window = name
	}

	// Create tmux session or window. Pass the caller-supplied env through
	// so ARCMUX_PROJECT / ARCMUX_ROLE_FILE / OBS_AGENTS / ... are visible
	// to the spawned shell — every elon role file relies on this contract.
	target, err := d.setupTmuxPane(ctx, tmuxSession, window, req.CWD, req.Env)
	if err != nil {
		sess.SetState(session.StateFailed)
		return nil, fmt.Errorf("setup tmux pane: %w", err)
	}
	sess.TmuxTarget = target

	// Install hooks
	if d.cfg.Hooks.AutoInstall {
		hookPath, err := d.hooks.Install(id, req.Agent, prof.HookDir)
		if err != nil {
			d.logger.Warn("hook install failed (non-fatal)", "error", err)
		} else {
			d.watcher.Watch(id, hookPath)
		}
	}

	// Set up output streaming via pipe-pane
	outputFile := d.outputFilePath(id)
	if err := d.tmux.PipePaneStart(ctx, target, outputFile); err != nil {
		d.logger.Warn("pipe-pane setup failed", "error", err)
	}

	// Store session before starting async work
	d.mu.Lock()
	d.sessions[id] = sess
	d.mu.Unlock()
	d.persistSessions()

	// Start agent and handshake in background
	go d.startAgentLifecycle(id, sess, prof, req.Prompt)

	return sess, nil
}

func (d *Daemon) startAgentLifecycle(id string, sess *session.Session, prof profile.Profile, prompt string) {
	ctx := d.ctx
	target := sess.Snapshot().TmuxTarget

	// Start the agent process
	if err := d.tmux.SendKeys(ctx, target, prof.StartCommand, "Enter"); err != nil {
		sess.SetState(session.StateFailed)
		d.emitStateChanged(id, session.StateFailed, "failed to start agent")
		return
	}

	// Perform handshake
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
		if err := d.deliverPrompt(ctx, sess, prof, prompt, true); err != nil {
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
	monitor := health.NewMonitor(d.tmux, captureInterval, d.healthEvents)
	go monitor.Run(monitorCtx, sess, prof)
}

// SendPrompt sends a prompt to a running session.
func (d *Daemon) SendPrompt(ctx context.Context, sessionID, text string, confirmDelivery, waitIdle bool) error {
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

	// Stop monitor
	d.mu.Lock()
	if cancel, ok := d.monitors[sessionID]; ok {
		cancel()
		delete(d.monitors, sessionID)
	}
	d.mu.Unlock()

	if graceful {
		// Send Ctrl-C and wait
		_ = d.tmux.SendKeys(ctx, snap.TmuxTarget, "C-c")
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_ = d.tmux.WaitIdle(waitCtx, snap.TmuxTarget, timeout, 1*time.Second)
	}

	// Kill the pane
	if err := d.tmux.KillPane(ctx, snap.TmuxTarget); err != nil {
		d.logger.Warn("kill pane failed", "error", err)
	}

	// Stop pipe-pane and cleanup
	d.tmux.PipePaneStop(ctx, snap.TmuxTarget)
	d.watcher.Unwatch(sessionID)
	d.hooks.Cleanup(sessionID)

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
	if err := d.SendPrompt(d.ctx, snap.ID, msg.Body, true, false); err != nil {
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

func (d *Daemon) setupTmuxPane(ctx context.Context, tmuxSession, window, cwd string, env map[string]string) (string, error) {
	// Try creating a new session first. tmux exports the supplied env vars
	// into the spawned shell via repeated `-e KEY=VAL` flags.
	err := d.tmux.NewSessionWithEnv(ctx, tmuxSession, window, cwd, env)
	if err == nil {
		return fmt.Sprintf("%s:%s", tmuxSession, window), nil
	}

	// Session exists, create a new window. NewWindowCanonical returns the
	// canonical `<session>:<window-name>` form so downstream code never
	// has to reconcile between %pane_id, :idx, and :name shapes.
	target, err := d.tmux.NewWindowCanonical(ctx, tmuxSession, window, cwd, env)
	if err != nil {
		return "", fmt.Errorf("create window: %w", err)
	}
	return target, nil
}

// --- Persistence ---

type persistedSession struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Agent            string        `json:"agent"`
	CWD              string        `json:"cwd"`
	Transport        string        `json:"transport"`
	TmuxTarget       string        `json:"tmux_target"`
	CurrentCommand   string        `json:"current_command"`
	BackendSessionID string        `json:"backend_session_id"`
	State            session.State `json:"state"`
	StartedAt        time.Time     `json:"started_at"`
}

func (d *Daemon) persistPath() string {
	return filepath.Join(filepath.Dir(d.cfg.Daemon.Socket), "sessions.json")
}

func (d *Daemon) persistSessions() {
	d.mu.RLock()
	records := make([]persistedSession, 0, len(d.sessions))
	for _, s := range d.sessions {
		snap := s.Snapshot()
		// Only persist non-terminal sessions
		if snap.State == session.StateExited || snap.State == session.StateFailed {
			continue
		}
		records = append(records, persistedSession{
			ID:               snap.ID,
			Name:             snap.Name,
			Agent:            snap.Agent,
			CWD:              snap.CWD,
			Transport:        snap.Transport,
			TmuxTarget:       snap.TmuxTarget,
			CurrentCommand:   snap.CurrentCommand,
			BackendSessionID: snap.BackendSessionID,
			State:            snap.State,
			StartedAt:        snap.StartedAt,
		})
	}
	d.mu.RUnlock()

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		d.logger.Error("persist sessions marshal", "error", err)
		return
	}

	path := d.persistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		d.logger.Error("persist sessions mkdir", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		d.logger.Error("persist sessions write", "error", err)
	}
}

func (d *Daemon) restoreSessions() {
	path := d.persistPath()
	data, err := os.ReadFile(path)
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
