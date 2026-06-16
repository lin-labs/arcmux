package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestVoiceRecordStartStop(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	seedSession(d, "s-voice-1", "alpha")
	d.captureHook = func(_ context.Context, _ string, _ bool) (string, error) {
		return "anchor line alpha one\nanchor line bravo two\n", nil
	}
	// Point DataRoot at a temp dir so the log writes there, not ~/data.
	d.cfg.DataRoot = t.TempDir()

	h := &HTTPServer{daemon: d}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/voice/record/start?name=alpha", nil)
	h.handleVoiceRecordStart(rec, req)
	if rec.Code != 200 {
		t.Fatalf("start code %d body %s", rec.Code, rec.Body.String())
	}
	var resp voiceRecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if !resp.Recording || resp.LogPath == "" {
		t.Fatalf("expected recording=true with log path, got %+v", resp)
	}

	// Stop recording.
	h.handleVoiceRecordStop(httptest.NewRecorder(), httptest.NewRequest("POST", "/voice/record/stop?name=alpha", nil))

	// Status should report off.
	rec3 := httptest.NewRecorder()
	h.handleVoiceRecordStatus(rec3, httptest.NewRequest("GET", "/voice/record/status?name=alpha", nil))
	var st voiceRecordResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if st.Recording {
		t.Fatalf("expected recording=false after stop, got %+v", st)
	}
}

func TestVoiceRecordStatusMissingName(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	h := &HTTPServer{daemon: d}

	// Status with no name returns the list of recording sessions (empty array).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/voice/record/status", nil)
	h.handleVoiceRecordStatus(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestVoiceRecordStartNotFound(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	h := &HTTPServer{daemon: d}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/voice/record/start?name=ghost", nil)
	h.handleVoiceRecordStart(rec, req)
	if rec.Code != 404 {
		t.Fatalf("expected 404, got %d body %s", rec.Code, rec.Body.String())
	}
}

func TestVoiceRecordStartMissingName(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	h := &HTTPServer{daemon: d}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/voice/record/start", nil)
	h.handleVoiceRecordStart(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d body %s", rec.Code, rec.Body.String())
	}
}
