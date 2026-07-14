package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestReloadMeshConcurrentDoesNotTouchSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.json")
	if err := mesh.SaveRegistry(path, &mesh.Registry{Version: 1, DeviceID: "ref", Serve: true, Accept: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		cfg:    &config.Config{Mesh: config.MeshConfig{Enabled: true, ListenAddr: "127.0.0.1:0", RegistryPath: path, HeartbeatInterval: "20ms", StaleAfter: "50ms", DeadAfter: "100ms", ReconnectMin: "10ms", ReconnectMax: "20ms", HandshakeTimeout: "100ms"}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: context.Background(),
		sessions: map[string]*session.Session{"kept": session.NewSession("kept", "kept", "codex", t.TempDir())},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.ReloadMesh(); err != nil {
				t.Errorf("ReloadMesh: %v", err)
			}
		}()
	}
	wg.Wait()
	if enabled, _ := d.MeshStatus(); !enabled {
		t.Fatal("mesh not enabled after reload")
	}
	if got := d.sessions["kept"]; got == nil || got.ID != "kept" {
		t.Fatal("mesh reload mutated local sessions")
	}

	h := NewHTTPServer(d, "127.0.0.1:0")
	req := httptest.NewRequest(http.MethodPost, "/mesh/reload", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload endpoint status=%d body=%s", rec.Code, rec.Body.String())
	}
	remoteReq := httptest.NewRequest(http.MethodPost, "/mesh/reload", nil)
	remoteReq.RemoteAddr = "100.64.0.2:1234"
	remoteRec := httptest.NewRecorder()
	h.srv.Handler.ServeHTTP(remoteRec, remoteReq)
	if remoteRec.Code != http.StatusForbidden {
		t.Fatalf("remote reload status=%d", remoteRec.Code)
	}

	d.meshMu.Lock()
	current := d.mesh
	d.mesh = nil
	d.meshMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	current.Stop(ctx)
}

func TestReloadMeshInvalidConfigIsIsolated(t *testing.T) {
	d := &Daemon{cfg: &config.Config{Mesh: config.MeshConfig{ListenAddr: "0.0.0.0:7788"}}, logger: slog.Default(), ctx: context.Background(), sessions: map[string]*session.Session{"kept": session.NewSession("kept", "kept", "codex", t.TempDir())}}
	if err := d.ReloadMesh(); err == nil {
		t.Fatal("unsafe mesh address accepted")
	}
	if d.sessions["kept"] == nil {
		t.Fatal("invalid mesh config affected local sessions")
	}
}

func TestReloadMeshReportsListenerBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	path := filepath.Join(t.TempDir(), "mesh.json")
	if err := mesh.SaveRegistry(path, &mesh.Registry{Version: 1, DeviceID: "ref", Serve: true, Accept: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		cfg: &config.Config{Mesh: config.MeshConfig{
			Enabled: true, ListenAddr: ln.Addr().String(), RegistryPath: path,
		}},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:      context.Background(),
		sessions: map[string]*session.Session{"kept": session.NewSession("kept", "kept", "codex", t.TempDir())},
	}
	if err := d.ReloadMesh(); err == nil {
		t.Fatal("listener conflict reported a successful mesh reload")
	}
	if d.mesh != nil || d.sessions["kept"] == nil {
		t.Fatal("failed mesh reload installed a manager or touched local sessions")
	}
}
