package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunVoiceStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/voice/record/start" || r.URL.Query().Get("name") != "agents:1" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": "agents:1", "recording": true, "log_path": "/tmp/x.screen.log"})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runVoice([]string{"start", "agents:1"}, srv.URL, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/tmp/x.screen.log") {
		t.Fatalf("expected log path in output, got %q", out.String())
	}
}

func TestRunVoiceConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/voice/record/start":
			json.NewEncoder(w).Encode(map[string]any{"recording": true, "log_path": "/tmp/x.screen.log"})
		case "/babysit/new":
			json.NewEncoder(w).Encode(map[string]any{"connect_url": "ws://h/v1/realtime/converse?context=tok", "context_id": "ctx-abc"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runVoice([]string{"agents:1"}, srv.URL, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "converse?context=tok") {
		t.Fatalf("expected connect URL in output, got %q", out.String())
	}
}
