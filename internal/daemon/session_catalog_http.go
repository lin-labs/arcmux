package daemon

import (
	"net/http"
	"sort"
	"time"

	"github.com/lin-labs/arcmux/internal/sessionview"
)

type sessionCatalogHTTPRecord struct {
	RemoteSessionKey string  `json:"remote_session_key"`
	ProfileScope     string  `json:"profile_scope"`
	SessionID        string  `json:"session_id"`
	SessionName      string  `json:"session_name"`
	Agent            string  `json:"agent"`
	State            string  `json:"state"`
	OwnerID          string  `json:"owner_id,omitempty"`
	Transport        string  `json:"transport,omitempty"`
	CWD              string  `json:"cwd,omitempty"`
	TmuxTarget       string  `json:"tmux_target,omitempty"`
	TmuxSessionName  string  `json:"tmux_session_name,omitempty"`
	StartedAt        string  `json:"started_at"`
	LastActivityAt   string  `json:"last_activity_at"`
	HistoryBasename  *string `json:"history_basename"`
}

type sessionCatalogHTTPResponse struct {
	ObservedAt string                     `json:"observed_at"`
	Sessions   []sessionCatalogHTTPRecord `json:"sessions"`
}

func sessionCatalogRemoteKey(scope, sessionID string) string {
	return scope + "/" + sessionID
}

func (h *HTTPServer) handleSessionCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET required"})
		return
	}
	observedAt := time.Now().UTC()
	records := make([]sessionCatalogHTTPRecord, 0)
	for _, source := range h.daemon.SessionCatalog().sources() {
		for _, managed := range source.daemon.ListSessions() {
			snap := managed.Snapshot()
			record := sessionCatalogHTTPRecord{
				RemoteSessionKey: sessionCatalogRemoteKey(string(source.scope), snap.ID),
				ProfileScope:     string(source.scope),
				SessionID:        snap.ID,
				SessionName:      snap.Name,
				Agent:            snap.Agent,
				State:            string(snap.State),
				OwnerID:          snap.OwnerID,
				Transport:        snap.Transport,
				TmuxTarget:       snap.TmuxTarget,
				TmuxSessionName:  snap.TmuxSessionName,
				StartedAt:        snap.StartedAt.UTC().Format(time.RFC3339),
				LastActivityAt:   snap.LastActivityAt.UTC().Format(time.RFC3339),
			}
			if !snap.Private {
				record.CWD = snap.CWD
			}
			if detail, ok := h.daemon.SessionCatalog().Get(sessionview.Locator{
				Version: sessionview.LocatorVersion, ProfileScope: source.scope, SessionID: snap.ID,
			}); ok && detail.Summary.History != nil {
				basename := detail.Summary.History.Basename
				record.HistoryBasename = &basename
			}
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].ProfileScope != records[j].ProfileScope {
			return records[i].ProfileScope < records[j].ProfileScope
		}
		return records[i].SessionID < records[j].SessionID
	})
	writeJSON(w, http.StatusOK, sessionCatalogHTTPResponse{
		ObservedAt: observedAt.Format(time.RFC3339),
		Sessions:   records,
	})
}
