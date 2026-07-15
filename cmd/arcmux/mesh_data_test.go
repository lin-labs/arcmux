package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/meshstate"
)

func meshDataTestConfig(t *testing.T, serverURL string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "mesh.json")
	registry := &mesh.Registry{
		Version:  mesh.RegistryVersion,
		DeviceID: "ref",
		Accept:   map[string]string{},
		Grants:   map[string][]string{},
	}
	if err := mesh.SaveRegistry(registryPath, registry); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	address := strings.TrimPrefix(serverURL, "http://")
	content := "[daemon]\nhttp_addr = \"" + address + "\"\n[mesh]\nregistry_path = \"" + registryPath + "\"\nlisten_addr = \"127.0.0.1:0\"\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, registryPath
}

func TestMeshSessionsSyncsBeforeReadingProjection(t *testing.T) {
	var mu sync.Mutex
	requests := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"synced":true}`))
			return
		}
		_, _ = w.Write([]byte(`[{"freshness":"fresh"}]`))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	var out bytes.Buffer
	if err := cmdMesh([]string{"sessions", "devbox", "--profile", "root", "--config", cfg}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"POST /mesh/sessions/sync?peer=devbox",
		"GET /mesh/sessions?peer=devbox&profile=root",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests=%v", requests)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("request[%d]=%q want %q", i, requests[i], want[i])
		}
	}
	if !strings.Contains(out.String(), `"freshness":"fresh"`) {
		t.Fatalf("output=%q", out.String())
	}
}

func TestMeshSessionUsesLiveDetailWithoutArtifactScope(t *testing.T) {
	var request string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r.Method + " " + r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"summary":{"locator":{"profile_scope":"root","session_id":"s-1"}},"nudge_count":2}`))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	if err := cmdMesh([]string{"session", "devbox", "root", "s-1", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if request != "GET /mesh/session?live=1&peer=devbox&profile=root&session=s-1" {
		t.Fatalf("request=%q", request)
	}
}

func TestMeshArtifactsUsesArtifactOnlySyncAndPeerFilter(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"artifacts":[]}`))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	if err := cmdMesh([]string{"artifacts", "devbox", "--kind", "pull_request", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /mesh/artifacts/sync?peer=devbox",
		"GET /mesh/artifacts?peer=devbox&kind=pull_request",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests=%v", requests)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("request[%d]=%q want %q", i, requests[i], want[i])
		}
	}
}

func TestMeshAPIUsesConfiguredBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer local-control-token" {
			t.Errorf("authorization=%q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"enabled":false,"peers":[]}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	address := strings.TrimPrefix(server.URL, "http://")
	content := "[daemon]\nhttp_addr = \"" + address + "\"\nhttp_auth_token = \"local-control-token\"\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdMesh([]string{"status", "--json", "--config", configPath}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactRecordSendsReferenceOnlyEnvelope(t *testing.T) {
	var received meshstate.ArtifactEnvelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/mesh/artifact" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	args := []string{
		"record", "--kind", "pull_request", "--id", "pr-3",
		"--title", "Mesh protocol", "--url", "https://github.com/lin-labs/arcmux/pull/3",
		"--repo", "lin-labs/arcmux", "--ref", "boyan/mesh", "--config", cfg,
	}
	if err := cmdArtifact(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if received.Kind != meshstate.ArtifactPullRequest || received.ID != "pr-3" || received.Repo == nil || received.Repo.Ref != "boyan/mesh" {
		t.Fatalf("artifact=%+v", received)
	}
	if received.ReceivedAt.IsZero() || received.Provenance != "local-cli" {
		t.Fatalf("artifact receipt/provenance missing: %+v", received)
	}
}

func TestSurfaceBindUsesStableCmuxUUIDAndExplicitReplace(t *testing.T) {
	var received meshstate.SurfaceBinding
	var requestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.URL.RequestURI()
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(received)
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	surfaceID := "643895FC-1111-4222-8333-123456789ABC"
	workspaceID := "E17AFC74-4444-4555-8666-ABCDEF123456"
	args := []string{
		"bind", "devbox", "profile:olympus", "s-123", "--replace",
		"--surface", surfaceID, "--workspace", workspaceID, "--config", cfg,
	}
	if err := cmdSurface(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if requestURI != "/mesh/surface-bindings?replace=1" {
		t.Fatalf("request=%q", requestURI)
	}
	if received.SurfaceID != surfaceID || received.WorkspaceID != workspaceID || received.LocalDeviceID != "ref" {
		t.Fatalf("binding=%+v", received)
	}
	if received.Locator.DeviceID != "devbox" || received.Locator.ProfileScope != "profile:olympus" || received.Locator.SessionID != "s-123" {
		t.Fatalf("locator=%+v", received.Locator)
	}
	wantBindingID := "bnd-643895fc111142228333123456789abc"
	if received.BindingID != wantBindingID {
		t.Fatalf("binding id=%q want %q", received.BindingID, wantBindingID)
	}
}

func TestMeshOpenValidatesCachedLocatorAndBindsCallingSurface(t *testing.T) {
	now := time.Now().UTC()
	projection := meshstate.RemoteSessionProjection{
		SchemaVersion: meshstate.SchemaVersion,
		Locator: meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion, DeviceID: "devbox",
			ProfileScope: meshstate.RootProfileScope, SessionID: "s-123",
		},
		Metadata:   json.RawMessage(`{"locator":{"version":1,"profile_scope":"root","session_id":"s-123"},"agent":"codex","state":"working"}`),
		ReceivedAt: now, FreshnessChangedAt: now,
		SourceEpoch: "boot-devbox", SourceRevision: 7, Freshness: meshstate.FreshnessStale,
	}
	var requests []string
	var received meshstate.SurfaceBinding
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(projection)
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(received)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	surfaceID := "643895FC-1111-4222-8333-123456789ABC"
	workspaceID := "E17AFC74-4444-4555-8666-ABCDEF123456"
	var out bytes.Buffer
	err := cmdMesh([]string{
		"open", "devbox", "root", "s-123",
		"--surface", surfaceID, "--workspace", workspaceID, "--config", cfg,
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatal(err)
	}
	wantRequests := []string{
		"GET /mesh/session?peer=devbox&profile=root&session=s-123",
		"PUT /mesh/surface-bindings",
	}
	if len(requests) != len(wantRequests) {
		t.Fatalf("requests=%v", requests)
	}
	for i, want := range wantRequests {
		if requests[i] != want {
			t.Fatalf("request[%d]=%q want %q", i, requests[i], want)
		}
	}
	if received.SurfaceID != surfaceID || received.WorkspaceID != workspaceID ||
		received.Source != "mesh-open" || received.LocalDeviceID != "ref" {
		t.Fatalf("binding=%+v", received)
	}
	var result meshOpenResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SurfaceID != surfaceID || result.WorkspaceID != workspaceID ||
		!result.Locator.EqualIdentity(projection.Locator) ||
		result.Session.Freshness != meshstate.FreshnessStale {
		t.Fatalf("auditable open result=%+v", result)
	}
}

func TestMeshOpenRejectsGoneOrMismatchedCachedSessionBeforeBinding(t *testing.T) {
	now := time.Now().UTC()
	base := meshstate.RemoteSessionProjection{
		SchemaVersion: meshstate.SchemaVersion,
		Locator: meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion, DeviceID: "devbox",
			ProfileScope: meshstate.RootProfileScope, SessionID: "s-123",
		},
		Metadata:   json.RawMessage(`{"agent":"codex","state":"idle"}`),
		ReceivedAt: now, FreshnessChangedAt: now,
		SourceEpoch: "boot-devbox", SourceRevision: 1, Freshness: meshstate.FreshnessFresh,
	}
	for _, test := range []struct {
		name string
		edit func(*meshstate.RemoteSessionProjection)
	}{
		{name: "gone", edit: func(p *meshstate.RemoteSessionProjection) { p.Freshness = meshstate.FreshnessGone }},
		{name: "mismatch", edit: func(p *meshstate.RemoteSessionProjection) { p.Locator.SessionID = "s-other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			projection := base
			test.edit(&projection)
			putCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					putCalled = true
				}
				_ = json.NewEncoder(w).Encode(projection)
			}))
			defer server.Close()
			cfg, _ := meshDataTestConfig(t, server.URL)
			err := cmdMesh([]string{
				"open", "devbox", "root", "s-123",
				"--surface", "643895FC-1111-4222-8333-123456789ABC",
				"--workspace", "E17AFC74-4444-4555-8666-ABCDEF123456",
				"--config", cfg,
			}, strings.NewReader(""), &bytes.Buffer{})
			if err == nil {
				t.Fatal("unsafe cached projection was accepted")
			}
			if putCalled {
				t.Fatal("surface binding was written before cached locator validation")
			}
		})
	}
}

func TestMeshOpenRequiresCompleteCmuxIdentity(t *testing.T) {
	t.Setenv("CMUX_SURFACE_ID", "643895FC-1111-4222-8333-123456789ABC")
	t.Setenv("CMUX_WORKSPACE_ID", "")
	if err := cmdMeshOpen([]string{"devbox", "root", "s-123"}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "both required") {
		t.Fatalf("partial cmux identity error=%v", err)
	}
}
