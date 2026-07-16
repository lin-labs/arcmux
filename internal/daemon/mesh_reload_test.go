package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestDaemonRestartRecreatesConfiguredManagedTunnel(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	launchLog := filepath.Join(dir, "ssh-launches")
	if err := os.WriteFile(sshPath, []byte("#!/bin/sh\nif [ \"$1\" = \"-G\" ]; then\n  printf 'hostname devbox.internal\\n'\n  exit 0\nfi\nprintf 'launch\\n' >> \"$ARCMUX_TEST_SSH_LOG\"\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARCMUX_TEST_SSH_LOG", launchLog)

	registryPath := filepath.Join(dir, "mesh.json")
	if err := mesh.SaveRegistry(registryPath, &mesh.Registry{
		Version: mesh.RegistryVersion, DeviceID: "ref", Peers: []mesh.Peer{{
			ID: "devbox", URL: "ws://devbox.example:7788/v1/mesh", Token: "token",
			SSHTunnel: &mesh.SSHTunnel{
				Target: "devbox", LocalAddr: "127.0.0.1:18443", RemoteAddr: "127.0.0.1:7788",
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Mesh: config.MeshConfig{
		Enabled: true, ListenAddr: "127.0.0.1:0", RegistryPath: registryPath,
		HeartbeatInterval: "20ms", StaleAfter: "50ms", DeadAfter: "100ms",
		ReconnectMin: "10ms", ReconnectMax: "20ms", HandshakeTimeout: "50ms",
	}}
	newDaemon := func() *Daemon {
		return &Daemon{
			cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: context.Background(),
			sessions: map[string]*session.Session{"kept": session.NewSession("kept", "kept", "codex", t.TempDir())},
		}
	}
	waitLaunches := func(want int) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(launchLog)
			if strings.Count(string(data), "launch\n") >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		data, _ := os.ReadFile(launchLog)
		t.Fatalf("managed tunnel launches=%q, want at least %d", data, want)
	}

	first := newDaemon()
	if err := first.ReloadMesh(); err != nil {
		t.Fatal(err)
	}
	waitLaunches(1)
	first.stopMeshTransport()
	if first.sessions["kept"] == nil {
		t.Fatal("stopping mesh transport touched local sessions")
	}

	second := newDaemon()
	if err := second.ReloadMesh(); err != nil {
		t.Fatal(err)
	}
	waitLaunches(2)
	second.stopMeshTransport()
}
