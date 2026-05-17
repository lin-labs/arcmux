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
	"github.com/lin-labs/arcmux/internal/health"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/tmux"
	"google.golang.org/grpc"
)

// Daemon is the main arcmux runtime service.
type Daemon struct {
	cfg      *config.Config
	tmux     *tmux.Client
	hooks    *hooks.Installer
	watcher  *hooks.Watcher
	profiles map[string]profile.Profile
	logger   *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*session.Session
	monitors map[string]context.CancelFunc

	healthEvents chan health.HealthEvent
	eventBus     *EventBus

	server   *grpc.Server
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
}

// New creates a new daemon instance.
func New(cfg *config.Config, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	return &Daemon{
		cfg:          cfg,
		tmux:         tmux.NewClient(cfg.Tmux.SocketName),
		hooks:        hooks.NewInstaller(cfg.Hooks.HookOutputDir),
		watcher:      hooks.NewWatcher(cfg.Hooks.HookOutputDir, logger),
		profiles:     cfg.Agents,
		logger:       logger,
		sessions:     make(map[string]*session.Session),
		monitors:     make(map[string]context.CancelFunc),
		healthEvents: make(chan health.HealthEvent, 64),
		eventBus:     NewEventBus(),
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

	// Start gRPC server (blocking in goroutine)
	go func() {
		if err := d.server.Serve(listener); err != nil {
			d.logger.Error("grpc serve error", "error", err)
		}
	}()

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

	if d.cancel != nil {
		d.cancel()
	}

	d.eventBus.Close()
}

// CreateSession starts a new agent session.
func (d *Daemon) CreateSession(ctx context.Context, req CreateSessionRequest) (*session.Session, error) {
	prof, ok := d.profiles[req.Agent]
	if !ok {
		return nil, fmt.Errorf("unknown agent profile: %s", req.Agent)
	}

	id := generateSessionID()
	name := req.Name
	if name == "" {
		name = fmt.Sprintf("%s-%s", req.Agent, id[2:10])
	}
	sess := session.NewSession(id, name, req.Agent, req.CWD)

	d.logger.Info("creating session",
		"session_id", id,
		"agent", req.Agent,
		"cwd", req.CWD,
	)

	// Determine tmux target
	tmuxSession := d.cfg.Tmux.DefaultSession
	if req.TmuxSession != "" {
		tmuxSession = req.TmuxSession
	}
	window := req.TmuxWindow
	if window == "" {
		window = name
	}

	// Create tmux session or window
	target, err := d.setupTmuxPane(ctx, tmuxSession, window, req.CWD)
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
			d.logger.Error("initial prompt delivery failed", "session_id", id, "error", err)
			d.emitEvent(Event{
				SessionID: id,
				Type:      "prompt_failed",
				Message:   err.Error(),
				Timestamp: time.Now(),
			})
			return
		}
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

	// Wait for idle if requested and agent is working
	if waitIdle && snap.State == session.StateWorking {
		if err := d.tmux.WaitIdle(ctx, snap.TmuxTarget, prof.StuckTimeout, 2*time.Second); err != nil {
			return fmt.Errorf("wait for idle: %w", err)
		}
	}

	if err := d.deliverPrompt(ctx, sess, prof, text, confirmDelivery); err != nil {
		return err
	}

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

func (d *Daemon) setupTmuxPane(ctx context.Context, tmuxSession, window, cwd string) (string, error) {
	// Try creating a new session first
	err := d.tmux.NewSession(ctx, tmuxSession, window, cwd)
	if err == nil {
		return fmt.Sprintf("%s:%s", tmuxSession, window), nil
	}

	// Session exists, create a new window
	paneID, err := d.tmux.NewWindow(ctx, tmuxSession, window, cwd)
	if err != nil {
		return "", fmt.Errorf("create window: %w", err)
	}
	return paneID, nil
}

// --- Persistence ---

type persistedSession struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Agent      string         `json:"agent"`
	CWD        string         `json:"cwd"`
	TmuxTarget string         `json:"tmux_target"`
	State      session.State  `json:"state"`
	StartedAt  time.Time      `json:"started_at"`
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
			ID:         snap.ID,
			Name:       snap.Name,
			Agent:      snap.Agent,
			CWD:        snap.CWD,
			TmuxTarget: snap.TmuxTarget,
			State:      snap.State,
			StartedAt:  snap.StartedAt,
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
		// Check if the tmux pane still exists
		if !d.tmux.PaneExists(ctx, rec.TmuxTarget) {
			d.logger.Info("restored session pane gone, skipping",
				"session_id", rec.ID, "target", rec.TmuxTarget)
			continue
		}

		sess := session.NewSession(rec.ID, rec.Name, rec.Agent, rec.CWD)
		sess.TmuxTarget = rec.TmuxTarget
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
}

func generateSessionID() string {
	return fmt.Sprintf("s-%d", time.Now().UnixNano())
}
