package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

// HTTPServer exposes a small HTTP API for managing sessions.
type HTTPServer struct {
	daemon *Daemon
	srv    *http.Server
}

func NewHTTPServer(d *Daemon, addr string) *HTTPServer {
	h := &HTTPServer{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/session/new", h.handleSessionNew)
	mux.HandleFunc("/session/close", h.handleSessionClose)
	mux.HandleFunc("/sessions", h.handleSessionsList)
	mux.HandleFunc("/profiles", h.handleProfilesList)
	mux.HandleFunc("/profiles/create", h.handleProfilesCreate)
	mux.HandleFunc("/profiles/remove", h.handleProfilesRemove)
	h.srv = &http.Server{Addr: addr, Handler: mux}
	return h
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
	SessionID  string `json:"session_id"`
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	TmuxTarget string `json:"tmux_target"`
	Command    string `json:"command"`
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

func (h *HTTPServer) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "claude"
	}

	if agent != "claude" {
		writeJSON(w, http.StatusNotImplemented, errorResponse{
			Error: fmt.Sprintf("agent not implemented: %s", agent),
		})
		return
	}

	ctx := r.Context()
	id := generateSessionID()

	name := r.URL.Query().Get("name")
	if name == "" {
		// Use the full nanosecond suffix to avoid collisions on rapid creates.
		name = fmt.Sprintf("claude-%s", id[2:])
	} else if !nameSafe.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "name must match [A-Za-z0-9_-]{1,64}",
		})
		return
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
	target, err := h.daemon.setupTmuxPane(ctx, tmuxSession, name, cwd, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("setup tmux pane: %v", err),
		})
		return
	}

	command := "cld --remote-control"
	if err := h.daemon.tmux.SendKeys(ctx, target, command, "Enter"); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("send start command: %v", err),
		})
		return
	}

	sess := session.NewSession(id, name, agent, cwd)
	sess.SetTransport(profile.TransportTmux)
	sess.TmuxSessionName = tmuxSession
	sess.TmuxTarget = target
	sess.SetState(session.StateIdle)

	h.daemon.mu.Lock()
	h.daemon.sessions[id] = sess
	h.daemon.mu.Unlock()
	h.daemon.persistSessions()

	h.daemon.logger.Info("http created claude remote-control session",
		"session_id", id, "name", name, "tmux_target", target)

	writeJSON(w, http.StatusOK, sessionNewResponse{
		SessionID:  id,
		Name:       name,
		Agent:      agent,
		TmuxTarget: target,
		Command:    command,
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
	if err := h.daemon.tmux.KillPane(r.Context(), snap.TmuxTarget); err != nil {
		h.daemon.logger.Warn("close: kill pane failed", "name", name, "error", err)
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

type sessionSummary struct {
	SessionID  string `json:"session_id"`
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	State      string `json:"state"`
	TmuxTarget string `json:"tmux_target"`
	CWD        string `json:"cwd,omitempty"`
	StartedAt  string `json:"started_at"`
}

type sessionsListResponse struct {
	Sessions []sessionSummary `json:"sessions"`
}

type profilesResponse struct {
	Profiles []ProfileRecord `json:"profiles"`
}

func (h *HTTPServer) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	sessions := h.daemon.ListSessions()
	out := sessionsListResponse{Sessions: make([]sessionSummary, 0, len(sessions))}
	for _, s := range sessions {
		snap := s.Snapshot()
		out.Sessions = append(out.Sessions, sessionSummary{
			SessionID:  snap.ID,
			Name:       snap.Name,
			Agent:      snap.Agent,
			State:      string(snap.State),
			TmuxTarget: snap.TmuxTarget,
			CWD:        snap.CWD,
			StartedAt:  snap.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
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
