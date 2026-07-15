package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/meshstate"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

func TestMeshHTTPArtifactValidationAndServerTimestamp(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	h := NewHTTPServer(d, "127.0.0.1:0")

	unknown := meshHTTPRequest(h, http.MethodPut, "/mesh/artifact", []byte(`{"id":"doc","kind":"document","provenance":"test","unknown":true}`))
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	wrongMethod := meshHTTPRequest(h, http.MethodPost, "/mesh/artifact", nil)
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method status=%d", wrongMethod.Code)
	}

	callerTime := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	artifact := meshstate.ArtifactEnvelope{
		ID: "doc", Kind: meshstate.ArtifactDocument, Title: "Mesh design",
		PathHint: "~/plans/mesh.md", Provenance: "human", ReceivedAt: callerTime,
	}
	body, _ := json.Marshal(artifact)
	put := meshHTTPRequest(h, http.MethodPut, "/mesh/artifact", body)
	if put.Code != http.StatusOK {
		t.Fatalf("artifact put status=%d body=%s", put.Code, put.Body.String())
	}
	var stored meshstate.ArtifactEnvelope
	if err := json.Unmarshal(put.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ReceivedAt.Equal(callerTime) || stored.ReceivedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("caller controlled received_at: %v", stored.ReceivedAt)
	}
	get := meshHTTPRequest(h, http.MethodGet, "/mesh/artifact?kind=document&id=doc", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("artifact get status=%d body=%s", get.Code, get.Body.String())
	}
	list := meshHTTPRequest(h, http.MethodGet, "/mesh/artifacts?kind=document", nil)
	if list.Code != http.StatusOK || !bytes.Contains(list.Body.Bytes(), []byte(`"id":"doc"`)) {
		t.Fatalf("artifact list status=%d body=%s", list.Code, list.Body.String())
	}
}

func TestMeshHTTPSurfaceBindingIdentityAndExplicitReplacement(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	h := NewHTTPServer(d, "127.0.0.1:0")
	binding := meshstate.SurfaceBinding{
		BindingID: "binding-a", LocalDeviceID: "spoofed", Mux: "cmux",
		SurfaceID:   "11111111-1111-4111-8111-111111111111",
		WorkspaceID: "22222222-2222-4222-8222-222222222222",
		Locator: meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion, DeviceID: "devbox",
			ProfileScope: meshstate.RootProfileScope, SessionID: "s-one",
		},
		Source: "human",
	}
	body, _ := json.Marshal(binding)
	spoof := meshHTTPRequest(h, http.MethodPut, "/mesh/surface-bindings", body)
	if spoof.Code != http.StatusBadRequest {
		t.Fatalf("spoofed local device accepted: status=%d body=%s", spoof.Code, spoof.Body.String())
	}

	binding.LocalDeviceID = ""
	body, _ = json.Marshal(binding)
	first := meshHTTPRequest(h, http.MethodPut, "/mesh/surface-bindings", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first binding status=%d body=%s", first.Code, first.Body.String())
	}
	var stored meshstate.SurfaceBinding
	if err := json.Unmarshal(first.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.LocalDeviceID != "ref" {
		t.Fatalf("server did not stamp local device: %#v", stored)
	}

	replacement := binding
	replacement.BindingID = "binding-b"
	replacement.Locator.SessionID = "s-two"
	body, _ = json.Marshal(replacement)
	conflict := meshHTTPRequest(h, http.MethodPut, "/mesh/surface-bindings", body)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("implicit replacement status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	replaced := meshHTTPRequest(h, http.MethodPut, "/mesh/surface-bindings?replace=1", body)
	if replaced.Code != http.StatusOK {
		t.Fatalf("explicit replacement status=%d body=%s", replaced.Code, replaced.Body.String())
	}
	get := meshHTTPRequest(h, http.MethodGet, "/mesh/surface-bindings?surface_id="+binding.SurfaceID, nil)
	if get.Code != http.StatusOK || !bytes.Contains(get.Body.Bytes(), []byte(`"session_id":"s-two"`)) {
		t.Fatalf("replacement get status=%d body=%s", get.Code, get.Body.String())
	}
	deleted := meshHTTPRequest(h, http.MethodDelete, "/mesh/surface-bindings?surface_id="+binding.SurfaceID, nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}

func TestMeshHTTPSessionsValidation(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	h := NewHTTPServer(d, "127.0.0.1:0")
	badList := meshHTTPRequest(h, http.MethodGet, "/mesh/sessions?profile=bad", nil)
	if badList.Code != http.StatusBadRequest {
		t.Fatalf("invalid profile status=%d body=%s", badList.Code, badList.Body.String())
	}
	badGet := meshHTTPRequest(h, http.MethodGet, "/mesh/session?peer=devbox", nil)
	if badGet.Code != http.StatusBadRequest {
		t.Fatalf("missing locator status=%d body=%s", badGet.Code, badGet.Body.String())
	}
	badSync := meshHTTPRequest(h, http.MethodPost, "/mesh/sessions/sync", nil)
	if badSync.Code != http.StatusBadRequest {
		t.Fatalf("missing sync peer status=%d body=%s", badSync.Code, badSync.Body.String())
	}
	badArtifactSync := meshHTTPRequest(h, http.MethodPost, "/mesh/artifacts/sync", nil)
	if badArtifactSync.Code != http.StatusBadRequest {
		t.Fatalf("missing artifact sync peer status=%d body=%s", badArtifactSync.Code, badArtifactSync.Body.String())
	}
	wrongArtifactSyncMethod := meshHTTPRequest(h, http.MethodGet, "/mesh/artifacts/sync?peer=devbox", nil)
	if wrongArtifactSyncMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("artifact sync method status=%d body=%s", wrongArtifactSyncMethod.Code, wrongArtifactSyncMethod.Body.String())
	}
}

func TestMeshHTTPResolvedSurfaceBindingIncludesTargetAndFreshness(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	now := time.Now().UTC()
	response := meshSessionsListResponse{
		SourceEpoch: "boot-devbox", SourceRevision: 1,
		ProfileScopes: []sessionview.ProfileScope{sessionview.RootProfileScope},
		Sessions: []sessionview.Summary{{
			Locator: sessionview.Locator{Version: sessionview.LocatorVersion, ProfileScope: sessionview.RootProfileScope, SessionID: "s-target"},
			Agent:   "codex", State: "idle", StartedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
			Freshness: sessionview.Freshness{ObservedAt: now, SourceUpdatedAt: now.Add(-time.Minute)},
		}},
	}
	if err := d.commitRemoteSessions("devbox", response); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(d, "127.0.0.1:0")
	binding := meshstate.SurfaceBinding{
		BindingID: "binding-resolved", Mux: "cmux",
		SurfaceID:   "11111111-1111-4111-8111-111111111111",
		WorkspaceID: "22222222-2222-4222-8222-222222222222",
		Locator: meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion, DeviceID: "devbox",
			ProfileScope: meshstate.RootProfileScope, SessionID: "s-target",
		},
		Source: "human",
	}
	body, _ := json.Marshal(binding)
	put := meshHTTPRequest(h, http.MethodPost, "/mesh/surface-bindings/validated", body)
	if put.Code != http.StatusOK {
		t.Fatalf("atomic surface open status=%d body=%s", put.Code, put.Body.String())
	}
	resolved := meshHTTPRequest(h, http.MethodGet, "/mesh/surface-bindings?surface_id="+binding.SurfaceID+"&resolved=1", nil)
	if resolved.Code != http.StatusOK || !bytes.Contains(resolved.Body.Bytes(), []byte(`"session_id":"s-target"`)) ||
		!bytes.Contains(resolved.Body.Bytes(), []byte(`"peer_freshness":"fresh"`)) ||
		!bytes.Contains(resolved.Body.Bytes(), []byte(`"effective_freshness":"fresh"`)) {
		t.Fatalf("resolved binding status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	response.SourceRevision = 2
	response.Sessions = nil
	if err := d.commitRemoteSessions("devbox", response); err != nil {
		t.Fatal(err)
	}
	goneBinding := binding
	goneBinding.BindingID = "binding-gone"
	goneBinding.SurfaceID = "33333333-3333-4333-8333-333333333333"
	body, _ = json.Marshal(goneBinding)
	rejected := meshHTTPRequest(h, http.MethodPost, "/mesh/surface-bindings/validated", body)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("gone target status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	missing := meshHTTPRequest(h, http.MethodGet, "/mesh/surface-bindings?surface_id="+goneBinding.SurfaceID, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("gone target wrote binding: status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestMeshOperatorHTTPRequiresLoopbackOrConfiguredBearer(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	h := NewHTTPServer(d, "127.0.0.1:0")
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/mesh/sessions"},
		{http.MethodPost, "/mesh/sessions/sync?peer=devbox"},
		{http.MethodGet, "/mesh/session?peer=devbox&profile=root&session=s-one"},
		{http.MethodGet, "/mesh/artifacts"},
		{http.MethodPost, "/mesh/artifacts/sync?peer=devbox"},
		{http.MethodGet, "/mesh/artifact?kind=document&id=doc"},
		{http.MethodPut, "/mesh/subscribe"},
		{http.MethodGet, "/mesh/surface-bindings"},
		{http.MethodPost, "/mesh/surface-bindings/validated"},
	}
	for _, item := range paths {
		t.Run(item.path, func(t *testing.T) {
			request := httptest.NewRequest(item.method, item.path, nil)
			request.RemoteAddr = "100.64.0.2:1234"
			recorder := httptest.NewRecorder()
			h.srv.Handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s, want 403", recorder.Code, recorder.Body.String())
			}
		})
	}

	d.cfg.Daemon.HTTPAuthToken = "mesh-secret"
	authenticated := NewHTTPServer(d, "127.0.0.1:0")
	request := httptest.NewRequest(http.MethodGet, "/mesh/artifacts", nil)
	request.RemoteAddr = "100.64.0.2:1234"
	request.Header.Set("Authorization", "Bearer mesh-secret")
	recorder := httptest.NewRecorder()
	authenticated.srv.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated remote status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func meshHTTPRequest(h *HTTPServer, method, target string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(recorder, request)
	return recorder
}
