package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/session"
)

func writeProjectsTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projects.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedSession inserts a tmux-transport session into the daemon registry so the
// HTTP handlers can resolve it by name without spawning a real pane.
func seedSession(d *Daemon, id, name string) {
	sess := session.NewSession(id, name, "claude", "/home/blin/Projects/voxtop")
	sess.TmuxTarget = "%99"
	d.mu.Lock()
	d.sessions[id] = sess
	d.mu.Unlock()
}

func TestHandleSessionCapture(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	seedSession(d, "s-cap-1", "alpha")

	var gotHistory bool
	d.captureHook = func(_ context.Context, sessionID string, includeHistory bool) (string, error) {
		gotHistory = includeHistory
		if sessionID != "s-cap-1" {
			t.Errorf("capture got session_id %q, want s-cap-1", sessionID)
		}
		return "PANE CONTENTS\n$ ", nil
	}

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodGet, "/session/capture?name=alpha&history=1", nil)
	rec := httptest.NewRecorder()
	h.handleSessionCapture(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp sessionCaptureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Content != "PANE CONTENTS\n$ " {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Name != "alpha" || resp.SessionID != "s-cap-1" {
		t.Errorf("name/id = %q/%q", resp.Name, resp.SessionID)
	}
	if !gotHistory {
		t.Errorf("history=1 should request scrollback")
	}
}

func TestHandleSessionCaptureNotFound(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodGet, "/session/capture?name=ghost", nil)
	rec := httptest.NewRecorder()
	h.handleSessionCapture(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleSessionCaptureMissingName(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodGet, "/session/capture", nil)
	rec := httptest.NewRecorder()
	h.handleSessionCapture(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func seedSessionCWD(d *Daemon, id, name, cwd, owner string) {
	sess := session.NewSession(id, name, "claude", cwd)
	sess.TmuxTarget = "%" + id
	sess.SetOwnerID(owner)
	d.mu.Lock()
	d.sessions[id] = sess
	d.mu.Unlock()
}

func TestHandleSessionsListProjectFilter(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	// In-repo cwd match, owner-tag match, and a non-member.
	seedSessionCWD(d, "p1", "vox-a", "/home/blin/Projects/voxtop/VoxtopServer", "")
	seedSessionCWD(d, "p2", "vox-b", "/somewhere/else", "elonco:voxtop")
	seedSessionCWD(d, "p3", "arc-a", "/home/blin/Projects/arcmux", "")

	// Register voxtop so cwd matching has a repo root.
	reg, err := project.Load(writeProjectsTOML(t,
		`[[project]]
slug = "voxtop"
repo_cwd = "/home/blin/Projects/voxtop"
`))
	if err != nil {
		t.Fatal(err)
	}
	d.projects = reg

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodGet, "/sessions?project=voxtop", nil)
	rec := httptest.NewRecorder()
	h.handleSessionsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp sessionsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range resp.Sessions {
		names[s.Name] = true
	}
	if !names["vox-a"] || !names["vox-b"] {
		t.Errorf("expected vox-a and vox-b in %v", names)
	}
	if names["arc-a"] {
		t.Errorf("arc-a should be filtered out: %v", names)
	}
}

func TestHandleSessionsListUnknownProjectEmpty(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	seedSessionCWD(d, "x1", "a", "/home/blin/Projects/arcmux", "")

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodGet, "/sessions?project=ghostproj", nil)
	rec := httptest.NewRecorder()
	h.handleSessionsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown project is empty, not error)", rec.Code)
	}
	var resp sessionsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 0 {
		t.Errorf("unknown project should yield empty list, got %d", len(resp.Sessions))
	}
}

func TestHandleSessionsListRedactsTrustedPrivateContext(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	d.cfg.Hooks.SessionStateDir = t.TempDir()
	secretCWD := filepath.Join(t.TempDir(), "DO_NOT_LEAK_PRIVATE_CWD")
	sess := session.NewSession("private-session", "private handoff", "claude", secretCWD)
	sess.TmuxTarget = "%private"
	sess.SetCurrentCommand("DO_NOT_LEAK_PRIVATE_PROMPT")
	sess.SetOwnerID("arcmux-handoff:handoff-1")
	sess.MarkPrivate()
	d.mu.Lock()
	d.sessions[sess.Snapshot().ID] = sess
	d.mu.Unlock()
	if err := hooks.ApplyEventWithContract(
		d.cfg.Hooks.SessionStateDir,
		sess.Snapshot().ID,
		"claude",
		hooks.EventPromptSubmit,
		"",
		hooks.TurnContractUpdate{Goal: "DO_NOT_LEAK_PRIVATE_GOAL", LastUserMessage: "DO_NOT_LEAK_PRIVATE_MESSAGE"},
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rec := httptest.NewRecorder()
	h.handleSessionsList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var response sessionsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Sessions) != 1 {
		t.Fatalf("sessions = %+v", response.Sessions)
	}
	got := response.Sessions[0]
	if got.CWD != "" || got.CurrentCommand != "" || got.TurnContract != nil {
		t.Fatalf("private context was not redacted: %+v", got)
	}
	for _, forbidden := range []string{secretCWD, "DO_NOT_LEAK_PRIVATE_PROMPT", "DO_NOT_LEAK_PRIVATE_GOAL", "DO_NOT_LEAK_PRIVATE_MESSAGE"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("sessions response leaked %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestHandleSessionSend(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	seedSession(d, "s-snd-1", "beta")

	var gotText string
	var gotConfirm bool
	d.sendPromptHook = func(_ context.Context, sessionID, text string, confirm, _ bool) error {
		gotText = text
		gotConfirm = confirm
		if sessionID != "s-snd-1" {
			t.Errorf("send got session_id %q, want s-snd-1", sessionID)
		}
		return nil
	}

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodPost, "/session/send?name=beta&text=use+JWT&confirm=1", nil)
	rec := httptest.NewRecorder()
	h.handleSessionSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp sessionSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Delivered || resp.Name != "beta" || resp.SessionID != "s-snd-1" {
		t.Errorf("resp = %+v", resp)
	}
	if gotText != "use JWT" {
		t.Errorf("text = %q, want %q", gotText, "use JWT")
	}
	if !gotConfirm {
		t.Errorf("confirm=1 should pass confirmDelivery=true")
	}
}

func TestHandleSessionSendMissingText(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	seedSession(d, "s-snd-2", "gamma")

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodPost, "/session/send?name=gamma", nil)
	rec := httptest.NewRecorder()
	h.handleSessionSend(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSessionSendNotFound(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodPost, "/session/send?name=ghost&text=hi", nil)
	rec := httptest.NewRecorder()
	h.handleSessionSend(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
