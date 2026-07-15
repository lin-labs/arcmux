package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
)

func TestHandoffHTTPStrictJSONAndOperatorAuthorization(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	d := newMeshApplicationTestDaemon(t, "ref")
	h := NewHTTPServer(d, "127.0.0.1:0")
	h.handoffOutbox = func() (*sourceHandoffOutbox, error) { return fixture.outbox, nil }

	unknown := meshHTTPRequest(h, http.MethodPost, "/mesh/handoffs", []byte(`{
        "profile_scope":"root","session_id":"session-1","target_peer":"devbox","target_agent":"codex",
        "project":"demo","goal":"Continue safely","conversation_id":"conversation-1","source_device":"spoofed"
    }`))
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "unknown field") {
		t.Fatalf("unknown field status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	nonLoopback := httptest.NewRequest(http.MethodGet, "/mesh/handoffs", nil)
	nonLoopback.RemoteAddr = "100.64.0.9:1234"
	recorder := httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(recorder, nonLoopback)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated operator route status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandoffHTTPListShowRetryAndStrictQueries(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	fixture.remote = func(_ context.Context, _ string, _ meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		return meshHandoffStatus{}, arcmuxmesh.ErrPeerDisconnected
	}
	d := newMeshApplicationTestDaemon(t, "ref")
	h := NewHTTPServer(d, "127.0.0.1:0")
	h.handoffOutbox = func() (*sourceHandoffOutbox, error) { return fixture.outbox, nil }

	request := []byte(`{"profile_scope":"root","session_id":"session-1","target_peer":"devbox","target_agent":"codex","project":"demo","goal":"Continue safely","conversation_id":"conversation-1"}`)
	created := meshHTTPRequest(h, http.MethodPost, "/mesh/handoffs", request)
	if created.Code != http.StatusAccepted || !strings.Contains(created.Body.String(), `"state":"retry_wait"`) {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	listed := meshHTTPRequest(h, http.MethodGet, "/mesh/handoffs", nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "handoff-test-1") {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	shown := meshHTTPRequest(h, http.MethodGet, "/mesh/handoffs?id=handoff-test-1", nil)
	if shown.Code != http.StatusOK || strings.Contains(shown.Body.String(), "Continue safely") {
		t.Fatalf("show status=%d body=%s", shown.Code, shown.Body.String())
	}
	unknownQuery := meshHTTPRequest(h, http.MethodGet, "/mesh/handoffs?goal=leak", nil)
	if unknownQuery.Code != http.StatusBadRequest {
		t.Fatalf("unknown query status=%d", unknownQuery.Code)
	}
	retryWithBody := meshHTTPRequest(h, http.MethodPost, "/mesh/handoffs/retry?id=handoff-test-1", []byte(`{}`))
	if retryWithBody.Code != http.StatusBadRequest {
		t.Fatalf("retry body status=%d", retryWithBody.Code)
	}
}
