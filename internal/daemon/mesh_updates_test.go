package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
)

func startUpdatesPair(t *testing.T, scopes []string) (string, *arcmuxmesh.Manager) {
	t.Helper()
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	dir := t.TempDir()
	server.cfg.Mesh.UpdatesDir = dir
	_, clientManager := startDaemonMeshPairWithScopes(t, server, client, scopes)
	return dir, clientManager
}

func callUpdates(t *testing.T, m *arcmuxmesh.Manager, p updatesListParams) (updatesListResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var result updatesListResult
	err := m.Call(ctx, "server", meshMethodUpdatesList, p, &result)
	return result, err
}

func writeUpdate(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonOfSize(n int) []byte {
	return []byte(`{"pad":"` + strings.Repeat("x", n-10) + `"}`)
}

func updateNames(r updatesListResult) []string {
	names := []string{}
	for _, f := range r.Files {
		names = append(names, f.Name)
	}
	return names
}

func TestUpdatesListReturnsSessionFiles(t *testing.T) {
	dir, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeUpdatesRead})
	writeUpdate(t, dir, "b.json", []byte(`{"id":"b"}`))
	writeUpdate(t, dir, "a.json", []byte(`{"id":"a"}`))
	r, err := callUpdates(t, m, updatesListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateNames(r); !reflect.DeepEqual(got, []string{"a.json", "b.json"}) {
		t.Fatalf("names=%v", got)
	}
	if !bytes.Equal(r.Files[0].Data, []byte(`{"id":"a"}`)) {
		t.Fatalf("data=%s", r.Files[0].Data)
	}
}

func TestUpdatesListSinceFiltersByMtime(t *testing.T) {
	dir, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeUpdatesRead})
	writeUpdate(t, dir, "a.json", []byte(`{}`))
	writeUpdate(t, dir, "b.json", []byte(`{}`))
	for name, day := range map[string]int{"a.json": 1, "b.json": 4} {
		mt := time.Date(2026, 11, day, 12, 0, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(dir, name), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	r, err := callUpdates(t, m, updatesListParams{Since: "2026-11-02T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateNames(r); !reflect.DeepEqual(got, []string{"b.json"}) {
		t.Fatalf("names=%v", got)
	}
}

func TestUpdatesListPagesUnderBudget(t *testing.T) {
	dir, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeUpdatesRead})
	for _, n := range []string{"a.json", "b.json", "c.json"} {
		writeUpdate(t, dir, n, jsonOfSize(20<<10))
	}
	r, err := callUpdates(t, m, updatesListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateNames(r); !reflect.DeepEqual(got, []string{"a.json", "b.json"}) || r.NextCursor != "b.json" {
		t.Fatalf("page1 names=%v cursor=%q", got, r.NextCursor)
	}
	r, err = callUpdates(t, m, updatesListParams{Cursor: r.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateNames(r); !reflect.DeepEqual(got, []string{"c.json"}) || r.NextCursor != "" {
		t.Fatalf("page2 names=%v cursor=%q", got, r.NextCursor)
	}
}

func TestUpdatesListSkipsOversizeFile(t *testing.T) {
	dir, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeUpdatesRead})
	writeUpdate(t, dir, "big.json", jsonOfSize(50<<10))
	writeUpdate(t, dir, "ok.json", []byte(`{}`))
	r, err := callUpdates(t, m, updatesListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateNames(r); !reflect.DeepEqual(got, []string{"ok.json"}) || !reflect.DeepEqual(r.Skipped, []string{"big.json"}) {
		t.Fatalf("names=%v skipped=%v", got, r.Skipped)
	}
}

func TestUpdatesListMonitorAndHintsAreNotFiles(t *testing.T) {
	dir, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeUpdatesRead})
	writeUpdate(t, dir, "monitor.json", []byte(`{"beat":1}`))
	writeUpdate(t, dir, "hints.json", []byte(`[]`))
	writeUpdate(t, dir, ".tmp.json", []byte(`{}`))
	writeUpdate(t, dir, "s.json", []byte(`{}`))
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := callUpdates(t, m, updatesListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateNames(r); !reflect.DeepEqual(got, []string{"s.json"}) {
		t.Fatalf("names=%v", got)
	}
	if string(r.Monitor) != `{"beat":1}` {
		t.Fatalf("monitor=%s", r.Monitor)
	}
}

func TestUpdatesListWritesHints(t *testing.T) {
	dir, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeUpdatesRead})
	hints := []surfaceHint{{MoshPort: 60001, SurfaceID: "3f2b8c1e-5a4d-4e6f-8a7b-9c0d1e2f3a4b", WorkspaceID: "w1"}}
	if _, err := callUpdates(t, m, updatesListParams{Hints: hints}); err != nil {
		t.Fatal(err)
	}
	var got []surfaceHint
	b, err := os.ReadFile(filepath.Join(dir, "hints.json"))
	if err != nil || json.Unmarshal(b, &got) != nil || !reflect.DeepEqual(got, hints) {
		t.Fatalf("hints.json=%s err=%v", b, err)
	}
	os.Remove(filepath.Join(dir, "hints.json"))
	bad := []surfaceHint{{MoshPort: 60001, SurfaceID: "not-a-uuid", WorkspaceID: "w1"}}
	_, err = callUpdates(t, m, updatesListParams{Hints: bad})
	var rpcErr *arcmuxmesh.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != arcmuxmesh.ErrorInvalidRequest {
		t.Fatalf("err=%v want invalid_request", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hints.json")); !os.IsNotExist(err) {
		t.Fatalf("hints.json written for invalid hint: %v", err)
	}
}

func TestUpdatesListRequiresScope(t *testing.T) {
	_, m := startUpdatesPair(t, []string{arcmuxmesh.ScopeSessionsRead})
	if _, err := callUpdates(t, m, updatesListParams{}); !isMeshPermissionDenied(err) {
		t.Fatalf("err=%v want permission_denied", err)
	}
}
