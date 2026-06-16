package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultBabysitTTL is how long a minted call context stays valid.
const DefaultBabysitTTL = 10 * time.Minute

// BabysitPane is one project pane surfaced into a call context.
type BabysitPane struct {
	Name       string `json:"name"`
	SessionID  string `json:"session_id"`
	TmuxTarget string `json:"tmux_target"`
	State      string `json:"state"`
	CWD        string `json:"cwd,omitempty"`
}

// BabysitContext is the ephemeral, project-scoped record the voxtop relay loads
// on connect to bring up a tool-ready voice call. Persisted (JSON) to the
// daemon bbolt store under its Token, with lazy TTL expiry on read.
type BabysitContext struct {
	ContextID  string            `json:"context_id"`
	Token      string            `json:"token"`
	Project    string            `json:"project"`
	RepoCWD    string            `json:"repo_cwd,omitempty"`
	PlanRefs   []string          `json:"plan_refs"`
	Panes      []BabysitPane     `json:"panes"`
	ScreenLogs map[string]string `json:"screen_logs,omitempty"` // pane name → screen-recording log path
	Server     string            `json:"server,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// resolvePlanRefs expands the project's plan_globs against repo_cwd into a
// sorted, de-duplicated list of existing files.
func resolvePlanRefs(repoCWD string, globs []string) []string {
	if repoCWD == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, g := range globs {
		matches, err := filepath.Glob(filepath.Join(repoCWD, g))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// connectURL builds the WS handle the client opens. Loopback hosts get ws://,
// everything else wss://. Empty server yields an empty URL (caller composes).
func connectURL(server, token string) string {
	if server == "" {
		return ""
	}
	scheme := "wss"
	host := server
	if i := strings.IndexByte(server, ':'); i >= 0 {
		host = server[:i]
	}
	if host == "localhost" || host == "127.0.0.1" {
		scheme = "ws"
	}
	return fmt.Sprintf("%s://%s/v1/realtime/converse?context=%s", scheme, server, token)
}

type babysitNewResponse struct {
	ContextID  string            `json:"context_id"`
	Token      string            `json:"token"`
	Project    string            `json:"project"`
	ConnectURL string            `json:"connect_url,omitempty"`
	RepoCWD    string            `json:"repo_cwd,omitempty"`
	PlanRefs   []string          `json:"plan_refs"`
	Panes      []BabysitPane     `json:"panes"`
	ScreenLogs map[string]string `json:"screen_logs,omitempty"`
	ExpiresAt  string            `json:"expires_at"`
}

// handleBabysitNew mints a call context, persists it to the daemon bbolt store
// with a TTL, and returns a connect handle. Query params:
//   - name (single-screen alternative): mints a context scoped to one named pane and enables recording.
//   - project (project-scoped): mints a context for all panes matching the project slug.
//   - server (optional voxtop host for connect_url), ttl (optional seconds, default 600).
func (h *HTTPServer) handleBabysitNew(w http.ResponseWriter, r *http.Request) {
	if name := r.URL.Query().Get("name"); name != "" {
		sess := h.findByName(name)
		if sess == nil {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("session not found: %s", name)})
			return
		}
		snap := sess.Snapshot()
		// Enable recording for this screen (decoupled from any voice client).
		logPath, err := h.daemon.SetRecording(snap.ID, true)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: fmt.Sprintf("enable recording: %v", err)})
			return
		}
		st := h.daemon.State()
		if st == nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "daemon state store unavailable"})
			return
		}
		ttl := DefaultBabysitTTL
		if v := r.URL.Query().Get("ttl"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				ttl = time.Duration(secs) * time.Second
			}
		}
		token, err := randomToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "token generation failed"})
			return
		}
		now := time.Now()
		pane := BabysitPane{Name: snap.Name, SessionID: snap.ID, TmuxTarget: snap.TmuxTarget, State: string(snap.State), CWD: snap.CWD}
		ctx := BabysitContext{
			ContextID:  "ctx-" + token[:12],
			Token:      token,
			Project:    "screen:" + snap.Name,
			RepoCWD:    snap.CWD,
			Panes:      []BabysitPane{pane},
			ScreenLogs: map[string]string{snap.Name: logPath},
			Server:     r.URL.Query().Get("server"),
			CreatedAt:  now,
			ExpiresAt:  now.Add(ttl),
		}
		blob, err := json.Marshal(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "marshal context failed"})
			return
		}
		if err := st.PutBabysitContext(token, blob); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: fmt.Sprintf("persist context: %v", err)})
			return
		}
		h.daemon.logger.Info("babysit context minted (single screen)", "screen", snap.Name, "context_id", ctx.ContextID, "log", logPath)
		writeJSON(w, http.StatusOK, babysitNewResponse{
			ContextID:  ctx.ContextID,
			Token:      token,
			Project:    ctx.Project,
			ConnectURL: connectURL(ctx.Server, token),
			RepoCWD:    ctx.RepoCWD,
			PlanRefs:   []string{},
			Panes:      ctx.Panes,
			ScreenLogs: ctx.ScreenLogs,
			ExpiresAt:  ctx.ExpiresAt.Format(time.RFC3339),
		})
		return
	}

	st := h.daemon.State()
	if st == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "daemon state store unavailable"})
		return
	}
	projectSlug := r.URL.Query().Get("project")
	if projectSlug == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing project"})
		return
	}

	matcher := h.daemon.projectMatcher(projectSlug)
	panes := make([]BabysitPane, 0)
	for _, s := range h.daemon.ListSessions() {
		snap := s.Snapshot()
		if !matcher.Matches(snap.CWD, snap.OwnerID) {
			continue
		}
		panes = append(panes, BabysitPane{
			Name:       snap.Name,
			SessionID:  snap.ID,
			TmuxTarget: snap.TmuxTarget,
			State:      string(snap.State),
			CWD:        snap.CWD,
		})
	}

	ttl := DefaultBabysitTTL
	if v := r.URL.Query().Get("ttl"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			ttl = time.Duration(secs) * time.Second
		}
	}

	token, err := randomToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "token generation failed"})
		return
	}
	now := time.Now()
	ctx := BabysitContext{
		ContextID: "ctx-" + token[:12],
		Token:     token,
		Project:   projectSlug,
		RepoCWD:   matcher.RepoCWD,
		PlanRefs:  resolvePlanRefs(matcher.RepoCWD, matcher.PlanGlobs),
		Panes:     panes,
		Server:    r.URL.Query().Get("server"),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	blob, err := json.Marshal(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "marshal context failed"})
		return
	}
	if err := st.PutBabysitContext(token, blob); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: fmt.Sprintf("persist context: %v", err)})
		return
	}

	h.daemon.logger.Info("babysit context minted",
		"project", projectSlug, "context_id", ctx.ContextID, "panes", len(panes), "ttl", ttl.String())

	writeJSON(w, http.StatusOK, babysitNewResponse{
		ContextID:  ctx.ContextID,
		Token:      token,
		Project:    projectSlug,
		ConnectURL: connectURL(ctx.Server, token),
		RepoCWD:    ctx.RepoCWD,
		PlanRefs:   ctx.PlanRefs,
		Panes:      panes,
		ExpiresAt:  ctx.ExpiresAt.Format(time.RFC3339),
	})
}

// handleBabysitContext resolves a minted context by token (the voxtop relay
// calls this on connect). Expired or unknown tokens return 404; expired tokens
// are deleted lazily.
func (h *HTTPServer) handleBabysitContext(w http.ResponseWriter, r *http.Request) {
	st := h.daemon.State()
	if st == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "daemon state store unavailable"})
		return
	}
	token := r.URL.Query().Get("context")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing context token"})
		return
	}
	blob, err := st.GetBabysitContext(token)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: fmt.Sprintf("read context: %v", err)})
		return
	}
	if blob == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown context token"})
		return
	}
	var ctx BabysitContext
	if err := json.Unmarshal(blob, &ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "corrupt context"})
		return
	}
	if time.Now().After(ctx.ExpiresAt) {
		_ = st.DeleteBabysitContext(token)
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "context expired"})
		return
	}
	writeJSON(w, http.StatusOK, ctx)
}
