package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/session"
)

func TestHandleSessionCatalogIncludesProfilesAndRedactsPrivateCWD(t *testing.T) {
	root, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	rootSession := session.NewSession("s-root", "root-worker", "codex", "/repo/root")
	rootSession.SetTransport("tmux")
	rootSession.TmuxTarget = "%1"
	rootSession.TmuxSessionName = "root-worker"
	root.mu.Lock()
	root.sessions[rootSession.ID] = rootSession
	root.mu.Unlock()

	profile, profileCleanup := newCreateSessionTestDaemon(t)
	defer profileCleanup()
	profile.cfg.Daemon.ProfileName = "olympus"
	profile.cfg.Hooks.SessionStateDir = t.TempDir()
	privateSession := session.NewSession("s-prof", "pr-01", "claude", filepath.Join(t.TempDir(), "private"))
	privateSession.SetTransport("tmux")
	privateSession.TmuxTarget = "%2"
	privateSession.TmuxSessionName = "pr-01-worker"
	privateSession.MarkPrivate()
	profile.mu.Lock()
	profile.sessions[privateSession.ID] = privateSession
	profile.mu.Unlock()

	root.profileManager = &ProfileManager{daemons: map[string]*Daemon{"olympus": profile}}

	h := &HTTPServer{daemon: root}
	req := httptest.NewRequest(http.MethodGet, "/session-catalog", nil)
	rec := httptest.NewRecorder()
	h.handleSessionCatalog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload sessionCatalogHTTPResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 2 {
		t.Fatalf("sessions=%+v", payload.Sessions)
	}
	if payload.Sessions[0].RemoteSessionKey != "profile:olympus/s-prof" || payload.Sessions[1].RemoteSessionKey != "root/s-root" {
		t.Fatalf("order/keys=%+v", payload.Sessions)
	}
	if payload.Sessions[0].CWD != "" {
		t.Fatalf("private cwd leaked: %+v", payload.Sessions[0])
	}
	if payload.Sessions[1].CWD != "/repo/root" || payload.Sessions[1].TmuxSessionName != "root-worker" {
		t.Fatalf("root session missing fields: %+v", payload.Sessions[1])
	}
	if strings.Contains(rec.Body.String(), "private") {
		t.Fatalf("response leaked private cwd: %s", rec.Body.String())
	}
}
