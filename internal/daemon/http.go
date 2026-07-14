package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/session"
)

// HTTPServer exposes a small HTTP API for managing sessions.
type HTTPServer struct {
	daemon    *Daemon
	srv       *http.Server
	authToken string
}

func NewHTTPServer(d *Daemon, addr string) *HTTPServer {
	h := &HTTPServer{daemon: d, authToken: d.cfg.Daemon.HTTPAuthToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/session/new", h.handleSessionNew)
	mux.HandleFunc("/session/close", h.handleSessionClose)
	mux.HandleFunc("/session/capture", h.handleSessionCapture)
	mux.HandleFunc("/session/send", h.handleSessionSend)
	mux.HandleFunc("/voice/record/start", h.handleVoiceRecordStart)
	mux.HandleFunc("/voice/record/stop", h.handleVoiceRecordStop)
	mux.HandleFunc("/voice/record/status", h.handleVoiceRecordStatus)
	mux.HandleFunc("/sessions", h.handleSessionsList)
	mux.HandleFunc("/babysit/new", h.handleBabysitNew)
	mux.HandleFunc("/babysit/context", h.handleBabysitContext)
	mux.HandleFunc("/profiles", h.handleProfilesList)
	mux.HandleFunc("/profiles/create", h.handleProfilesCreate)
	mux.HandleFunc("/profiles/remove", h.handleProfilesRemove)
	mux.HandleFunc("/mesh/status", h.handleMeshStatus)
	mux.HandleFunc("/mesh/ping", h.handleMeshPing)
	mux.HandleFunc("/mesh/reload", h.handleMeshReload)
	mux.HandleFunc("/mesh/sessions", h.meshOperatorOnly(h.handleMeshSessions))
	mux.HandleFunc("/mesh/sessions/sync", h.meshOperatorOnly(h.handleMeshSessionsSync))
	mux.HandleFunc("/mesh/session", h.meshOperatorOnly(h.handleMeshSession))
	mux.HandleFunc("/mesh/artifacts", h.meshOperatorOnly(h.handleMeshArtifacts))
	mux.HandleFunc("/mesh/artifacts/sync", h.meshOperatorOnly(h.handleMeshArtifactsSync))
	mux.HandleFunc("/mesh/artifact", h.meshOperatorOnly(h.handleMeshArtifact))
	mux.HandleFunc("/mesh/subscribe", h.meshOperatorOnly(h.handleMeshSubscribe))
	mux.HandleFunc("/mesh/surface-bindings", h.meshOperatorOnly(h.handleMeshSurfaceBindings))
	h.srv = &http.Server{Addr: addr, Handler: otelhttp.NewHandler(h.withAuth(mux), "arcmux-http")}
	return h
}

func (h *HTTPServer) handleMeshStatus(w http.ResponseWriter, r *http.Request) {
	enabled, peers := h.daemon.MeshStatus()
	if !enabled {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "peers": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "peers": peers})
}

func (h *HTTPServer) handleMeshPing(w http.ResponseWriter, r *http.Request) {
	peer := r.URL.Query().Get("peer")
	if peer == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing peer"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rtt, err := h.daemon.MeshPing(ctx, peer)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer_id": peer, "round_trip_ms": rtt.Milliseconds()})
}

func (h *HTTPServer) handleMeshReload(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "mesh reload is loopback-only"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST required"})
		return
	}
	if err := h.daemon.ReloadMesh(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	enabled, peers := h.daemon.MeshStatus()
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true, "enabled": enabled, "peers": peers})
}

// isLoopback reports whether a RemoteAddr (host:port or bare host) is a
// loopback address. Loopback callers bypass bearer auth for local dev.
func isLoopback(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authorized reports whether a request may proceed: auth disabled (no token),
// a loopback caller, or a matching bearer token.
func (h *HTTPServer) authorized(r *http.Request) bool {
	if h.authToken == "" {
		return true
	}
	if isLoopback(r.RemoteAddr) {
		return true
	}
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(h.authToken)) == 1
}

// withAuth wraps the mux, rejecting unauthorized non-loopback requests with 401.
func (h *HTTPServer) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// meshOperatorOnly prevents the mesh projection/control surface from becoming
// an unauthenticated LAN API under the legacy "empty token disables auth"
// setting. Loopback remains convenient; non-loopback requires configuring the
// bearer token, which withAuth verifies before this handler is reached.
func (h *HTTPServer) meshOperatorOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authToken == "" && !isLoopback(r.RemoteAddr) {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "mesh operator API requires loopback or configured HTTP auth"})
			return
		}
		next(w, r)
	}
}

func (h *HTTPServer) Serve() error {
	if err := h.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (h *HTTPServer) Shutdown(ctx context.Context) error {
	return h.srv.Shutdown(ctx)
}

type sessionNewResponse struct {
	SessionID      string `json:"session_id"`
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	State          string `json:"state,omitempty"`
	TmuxTarget     string `json:"tmux_target"`
	Command        string `json:"command"`
	CurrentCommand string `json:"current_command,omitempty"`
	RemoteServer   bool   `json:"remote_server,omitempty"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
}

type sessionCloseResponse struct {
	Name       string `json:"name"`
	SessionID  string `json:"session_id"`
	TmuxTarget string `json:"tmux_target"`
	Closed     bool   `json:"closed"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var nameSafe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	codexRemoteServerAgent   = "codex"
	codexRemoteServerCommand = "cdx remote-control"
)

// agentStartCommand returns the shell command that launches a remote-control
// session for the given agent, and whether the agent is supported. Claude uses
// the `cld` wrapper paired with the Vox / claude.ai mobile app; Codex uses the
// singleton remote server consumed by the Codex remote client.
func agentStartCommand(agent string) (string, bool) {
	switch agent {
	case "claude":
		return "cld --remote-control", true
	case "codex":
		return codexRemoteServerCommand, true
	default:
		return "", false
	}
}

func (h *HTTPServer) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "claude"
	}

	command, ok := agentStartCommand(agent)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errorResponse{
			Error: fmt.Sprintf("agent not implemented: %s", agent),
		})
		return
	}

	ctx := r.Context()
	name := r.URL.Query().Get("name")
	if name != "" && !nameSafe.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "name must match [A-Za-z0-9_-]{1,64}",
		})
		return
	}

	if agent == codexRemoteServerAgent {
		if existing := h.findActiveCodexRemoteServer(ctx); existing != nil {
			snap := existing.Snapshot()
			writeJSON(w, http.StatusOK, sessionNewResponse{
				SessionID:      snap.ID,
				Name:           snap.Name,
				Agent:          snap.Agent,
				State:          string(snap.State),
				TmuxTarget:     snap.TmuxTarget,
				Command:        snap.CurrentCommand,
				CurrentCommand: snap.CurrentCommand,
				RemoteServer:   true,
				AlreadyRunning: true,
			})
			return
		}
	}

	id := generateSessionID()
	if name == "" {
		// Use the full nanosecond suffix to avoid collisions on rapid creates.
		name = fmt.Sprintf("%s-%s", agent, id[2:])
	}

	if existing := h.findByName(name); existing != nil {
		writeJSON(w, http.StatusConflict, errorResponse{
			Error: fmt.Sprintf("session name already in use: %s", name),
		})
		return
	}

	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = filepath.Join(home, "Projects")
		}
	}

	tmuxSession := h.daemon.agentTmuxSessionName("", name, "", id)
	// Launch the agent as the tmux session's own command (exec, so the agent
	// is the pane's process) instead of send-keys into a shell — when it exits
	// the pane closes and the session is destroyed, leaving nothing lingering.
	target, err := h.daemon.setupTmuxPane(ctx, tmuxSession, name, cwd, nil, "exec "+command)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("setup tmux pane: %v", err),
		})
		return
	}

	sess := session.NewSession(id, name, agent, cwd)
	sess.SetTransport(profile.TransportTmux)
	sess.TmuxSessionName = tmuxSession
	sess.TmuxTarget = target
	sess.SetCurrentCommand(command)
	sess.SetState(session.StateIdle)

	h.daemon.mu.Lock()
	h.daemon.sessions[id] = sess
	h.daemon.mu.Unlock()
	h.daemon.persistSessions()

	h.daemon.logger.Info("http created remote agent session",
		"agent", agent, "session_id", id, "name", name, "tmux_target", target)

	writeJSON(w, http.StatusOK, sessionNewResponse{
		SessionID:      id,
		Name:           name,
		Agent:          agent,
		State:          string(session.StateIdle),
		TmuxTarget:     target,
		Command:        command,
		CurrentCommand: command,
		RemoteServer:   isCodexRemoteServerSnapshot(sess.Snapshot()),
	})
}

func (h *HTTPServer) handleSessionClose(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}

	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("session not found: %s", name),
		})
		return
	}

	snap := sess.Snapshot()
	if snap.TmuxSessionName != "" {
		if err := h.daemon.tmux.KillSession(r.Context(), snap.TmuxSessionName); err != nil {
			h.daemon.logger.Warn("close: kill tmux session failed", "name", name, "session", snap.TmuxSessionName, "error", err)
		}
	} else if snap.TmuxTarget != "" {
		if err := h.daemon.tmux.KillPane(r.Context(), snap.TmuxTarget); err != nil {
			h.daemon.logger.Warn("close: kill pane failed", "name", name, "error", err)
		}
	}

	h.daemon.mu.Lock()
	delete(h.daemon.sessions, snap.ID)
	h.daemon.mu.Unlock()
	sess.SetState(session.StateExited)
	h.daemon.persistSessions()

	h.daemon.logger.Info("http closed session", "session_id", snap.ID, "name", name)

	writeJSON(w, http.StatusOK, sessionCloseResponse{
		Name:       name,
		SessionID:  snap.ID,
		TmuxTarget: snap.TmuxTarget,
		Closed:     true,
	})
}

type sessionCaptureResponse struct {
	Name       string `json:"name"`
	SessionID  string `json:"session_id"`
	TmuxTarget string `json:"tmux_target"`
	Content    string `json:"content"`
}

type sessionSendResponse struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	Delivered bool   `json:"delivered"`
}

func boolParam(r *http.Request, key string) bool {
	v := r.URL.Query().Get(key)
	return v == "1" || v == "true"
}

// handleSessionCapture reads a session's pane contents. Thin HTTP shim over the
// same daemon.Capture path the gRPC Capture RPC uses. Pass history=1 for full
// scrollback (default: visible screen only).
func (h *HTTPServer) handleSessionCapture(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("session not found: %s", name),
		})
		return
	}
	snap := sess.Snapshot()
	content, err := h.daemon.Capture(r.Context(), snap.ID, boolParam(r, "history"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("capture: %v", err),
		})
		return
	}
	writeJSON(w, http.StatusOK, sessionCaptureResponse{
		Name:       name,
		SessionID:  snap.ID,
		TmuxTarget: snap.TmuxTarget,
		Content:    content,
	})
}

// handleSessionSend delivers text to a session. Thin HTTP shim over the same
// daemon.SendPrompt path the gRPC SendPrompt RPC uses. confirm=1 requests
// delivery confirmation; wait_idle=1 waits for a working agent to go idle.
func (h *HTTPServer) handleSessionSend(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	text := r.URL.Query().Get("text")
	if text == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing text"})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("session not found: %s", name),
		})
		return
	}
	snap := sess.Snapshot()
	if err := h.daemon.SendPrompt(r.Context(), snap.ID, text, boolParam(r, "confirm"), boolParam(r, "wait_idle")); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("send: %v", err),
		})
		return
	}
	h.daemon.logger.Info("http sent to session", "session_id", snap.ID, "name", name, "bytes", len(text))
	writeJSON(w, http.StatusOK, sessionSendResponse{
		Name:      name,
		SessionID: snap.ID,
		Delivered: true,
	})
}

type sessionSummary struct {
	SessionID      string `json:"session_id"`
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	State          string `json:"state"`
	TmuxTarget     string `json:"tmux_target"`
	CWD            string `json:"cwd,omitempty"`
	StartedAt      string `json:"started_at"`
	CurrentCommand string `json:"current_command,omitempty"`
	RemoteServer   bool   `json:"remote_server,omitempty"`
	// TurnContract is the recording snapshot from the per-session state doc
	// (goal / overall_goal / last_user_message / vault_link). Merged in so
	// clients (voxtop) get a bird's-eye view of what each agent is doing
	// without reading the state files themselves. Nil when no hook data yet.
	TurnContract *hooks.TurnContract `json:"turn_contract,omitempty"`
}

type sessionsListResponse struct {
	Sessions []sessionSummary `json:"sessions"`
}

type profilesResponse struct {
	Profiles []ProfileRecord `json:"profiles"`
}

func (h *HTTPServer) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	sessions := h.daemon.ListSessions()

	// Optional project scoping: ?project=<slug> returns only sessions whose cwd
	// is under the project's repo_cwd or whose owner_id tags the project. An
	// unknown project simply yields no matches (not an error).
	projectSlug := r.URL.Query().Get("project")
	var matcher project.Project
	if projectSlug != "" {
		matcher = h.daemon.projectMatcher(projectSlug)
	}

	out := sessionsListResponse{Sessions: make([]sessionSummary, 0, len(sessions))}
	for _, s := range sessions {
		snap := s.Snapshot()
		if projectSlug != "" && !matcher.Matches(snap.CWD, snap.OwnerID) {
			continue
		}
		summary := sessionSummary{
			SessionID:      snap.ID,
			Name:           snap.Name,
			Agent:          snap.Agent,
			State:          string(snap.State),
			TmuxTarget:     snap.TmuxTarget,
			CWD:            snap.CWD,
			StartedAt:      snap.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
			CurrentCommand: snap.CurrentCommand,
			RemoteServer:   isCodexRemoteServerSnapshot(snap),
		}
		// Merge the recording snapshot (best-effort: a missing/unreadable state
		// doc just leaves turn_contract nil, never fails the listing).
		if st, err := hooks.ReadSessionState(h.daemon.cfg.Hooks.SessionStateDir, snap.ID); err == nil && st != nil {
			summary.TurnContract = st.TurnContract
		}
		out.Sessions = append(out.Sessions, summary)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *HTTPServer) handleProfilesList(w http.ResponseWriter, r *http.Request) {
	if h.daemon.profileManager == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "profile manager unavailable on profile daemon"})
		return
	}
	writeJSON(w, http.StatusOK, profilesResponse{Profiles: h.daemon.profileManager.List()})
}

func (h *HTTPServer) handleProfilesCreate(w http.ResponseWriter, r *http.Request) {
	if h.daemon.profileManager == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "profile manager unavailable on profile daemon"})
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	rec, err := h.daemon.profileManager.Create(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *HTTPServer) handleProfilesRemove(w http.ResponseWriter, r *http.Request) {
	if h.daemon.profileManager == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "profile manager unavailable on profile daemon"})
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	purge := r.URL.Query().Get("purge") == "1" || r.URL.Query().Get("purge") == "true"
	rec, err := h.daemon.profileManager.Remove(r.Context(), name, purge)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *HTTPServer) findByName(name string) *session.Session {
	h.daemon.mu.RLock()
	defer h.daemon.mu.RUnlock()
	for _, s := range h.daemon.sessions {
		if s.Snapshot().Name == name {
			return s
		}
	}
	return nil
}

func (h *HTTPServer) findActiveCodexRemoteServer(ctx context.Context) *session.Session {
	for _, s := range h.daemon.ListSessions() {
		snap := s.Snapshot()
		if !isCodexRemoteServerSnapshot(snap) {
			continue
		}
		if snap.State == session.StateExited || snap.State == session.StateFailed {
			continue
		}
		if snap.TmuxTarget == "" || !h.daemon.tmux.PaneExists(ctx, snap.TmuxTarget) {
			continue
		}
		return s
	}
	return nil
}

func isCodexRemoteServerSnapshot(snap session.Snapshot) bool {
	return snap.Agent == codexRemoteServerAgent && snap.CurrentCommand == codexRemoteServerCommand
}

type voiceRecordResponse struct {
	Name      string `json:"name,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Recording bool   `json:"recording"`
	LogPath   string `json:"log_path,omitempty"`
	Since     string `json:"since,omitempty"`
}

func (h *HTTPServer) voiceSetRecording(w http.ResponseWriter, r *http.Request, on bool) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("session not found: %s", name)})
		return
	}
	snap := sess.Snapshot()
	logPath, err := h.daemon.SetRecording(snap.ID, on)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	recOn, _, since := h.daemon.RecordingStatus(snap.ID)
	resp := voiceRecordResponse{Name: name, SessionID: snap.ID, Recording: recOn, LogPath: logPath}
	if !since.IsZero() {
		resp.Since = since.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *HTTPServer) handleVoiceRecordStart(w http.ResponseWriter, r *http.Request) {
	h.voiceSetRecording(w, r, true)
}

func (h *HTTPServer) handleVoiceRecordStop(w http.ResponseWriter, r *http.Request) {
	h.voiceSetRecording(w, r, false)
}

func (h *HTTPServer) handleVoiceRecordStatus(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		// No name → list all recording sessions.
		ids := h.daemon.recordingSessions()
		out := make([]voiceRecordResponse, 0, len(ids))
		for _, id := range ids {
			on, lp, since := h.daemon.RecordingStatus(id)
			vr := voiceRecordResponse{SessionID: id, Recording: on, LogPath: lp}
			if !since.IsZero() {
				vr.Since = since.Format(time.RFC3339)
			}
			out = append(out, vr)
		}
		writeJSON(w, http.StatusOK, map[string]any{"recording": out})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("session not found: %s", name)})
		return
	}
	snap := sess.Snapshot()
	on, lp, since := h.daemon.RecordingStatus(snap.ID)
	resp := voiceRecordResponse{Name: name, SessionID: snap.ID, Recording: on, LogPath: lp}
	if !since.IsZero() {
		resp.Since = since.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}
