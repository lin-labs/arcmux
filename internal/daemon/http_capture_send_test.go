package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lin-labs/arcmux/internal/session"
)

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
