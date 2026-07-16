package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/tmux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestProfileManager_CreateRemoveRestart(t *testing.T) {
	home := filepath.Join("/tmp", fmt.Sprintf("arcmux-profile-home-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg := testProfileManagerConfig(t)
	parent := New(cfg, slog.Default())
	pm, err := NewProfileManager(parent)
	if err != nil {
		t.Fatalf("NewProfileManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = tmux.NewClient("arcmux-alpha").KillServer(ctx)
	_ = tmux.NewClient("arcmux-beta").KillServer(ctx)
	if err := pm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(pm.Stop)

	alpha, err := pm.Create(ctx, "alpha")
	if err != nil {
		t.Fatalf("Create alpha: %v", err)
	}
	beta, err := pm.Create(ctx, "beta")
	if err != nil {
		t.Fatalf("Create beta: %v", err)
	}
	if alpha.SocketPath == beta.SocketPath {
		t.Fatalf("profile sockets collide: %q", alpha.SocketPath)
	}
	if alpha.TmuxSocketName == beta.TmuxSocketName {
		t.Fatalf("tmux sockets collide: %q", alpha.TmuxSocketName)
	}
	wantAlphaState := filepath.Join(cfg.Hooks.SessionStateDir, "profiles", "alpha")
	wantBetaState := filepath.Join(cfg.Hooks.SessionStateDir, "profiles", "beta")
	if pm.daemons["alpha"].cfg.Hooks.SessionStateDir != wantAlphaState ||
		pm.daemons["beta"].cfg.Hooks.SessionStateDir != wantBetaState || wantAlphaState == wantBetaState {
		t.Fatalf("profile hook state is not isolated: alpha=%q beta=%q",
			pm.daemons["alpha"].cfg.Hooks.SessionStateDir, pm.daemons["beta"].cfg.Hooks.SessionStateDir)
	}
	wantAlphaOutput := filepath.Join(cfg.Hooks.HookOutputDir, "profiles", "alpha")
	wantBetaOutput := filepath.Join(cfg.Hooks.HookOutputDir, "profiles", "beta")
	if pm.daemons["alpha"].cfg.Hooks.HookOutputDir != wantAlphaOutput ||
		pm.daemons["beta"].cfg.Hooks.HookOutputDir != wantBetaOutput || wantAlphaOutput == wantBetaOutput {
		t.Fatalf("profile hook output is not isolated: alpha=%q beta=%q",
			pm.daemons["alpha"].cfg.Hooks.HookOutputDir, pm.daemons["beta"].cfg.Hooks.HookOutputDir)
	}
	if pm.daemons["alpha"].goalSummarySlots != parent.goalSummarySlots ||
		pm.daemons["beta"].goalSummarySlots != parent.goalSummarySlots {
		t.Fatal("profile daemons do not share the process-wide current-work limiter")
	}
	if pm.daemons["alpha"].mesh != nil || pm.daemons["beta"].mesh != nil {
		t.Fatal("profile daemon started the machine-scoped mesh")
	}
	assertProfileReachable(t, alpha.SocketPath)
	assertProfileReachable(t, beta.SocketPath)
	if _, err := pm.daemons["alpha"].setupTmuxPane(ctx, "worker", "win", home, map[string]string{"ARCMUX_PROFILE": "alpha"}, ""); err != nil {
		t.Fatalf("alpha setup tmux pane: %v", err)
	}
	if _, err := pm.daemons["beta"].setupTmuxPane(ctx, "worker", "win", home, map[string]string{"ARCMUX_PROFILE": "beta"}, ""); err != nil {
		t.Fatalf("beta setup tmux pane: %v", err)
	}
	if got, err := pm.daemons["alpha"].tmux.ShowEnvironment(ctx, "worker", "ARCMUX_PROFILE"); err != nil || got != "alpha" {
		t.Fatalf("alpha tmux env = %q, %v; want alpha", got, err)
	}
	if got, err := pm.daemons["beta"].tmux.ShowEnvironment(ctx, "worker", "ARCMUX_PROFILE"); err != nil || got != "beta" {
		t.Fatalf("beta tmux env = %q, %v; want beta", got, err)
	}

	pm.Stop()
	pm2, err := NewProfileManager(parent)
	if err != nil {
		t.Fatalf("NewProfileManager after restart: %v", err)
	}
	if err := pm2.Start(ctx); err != nil {
		t.Fatalf("Start after restart: %v", err)
	}
	t.Cleanup(pm2.Stop)
	assertProfileReachable(t, alpha.SocketPath)
	assertProfileReachable(t, beta.SocketPath)

	if _, err := pm2.Remove(ctx, "alpha", true); err != nil {
		t.Fatalf("Remove alpha: %v", err)
	}
	if _, err := os.Stat(alpha.SocketPath); !os.IsNotExist(err) {
		t.Fatalf("alpha socket should be gone after remove, stat err=%v", err)
	}
	assertProfileReachable(t, beta.SocketPath)

	pm2.Stop()
	pm3, err := NewProfileManager(parent)
	if err != nil {
		t.Fatalf("NewProfileManager after restart: %v", err)
	}
	if err := pm3.Start(ctx); err != nil {
		t.Fatalf("Start after restart: %v", err)
	}
	t.Cleanup(pm3.Stop)
	assertProfileReachable(t, beta.SocketPath)
}

func testProfileManagerConfig(t *testing.T) *config.Config {
	t.Helper()
	tmp := t.TempDir()
	registryPath := filepath.Join(tmp, "mesh.json")
	if err := mesh.SaveRegistry(registryPath, &mesh.Registry{
		Version:  mesh.RegistryVersion,
		DeviceID: "root",
		Serve:    true,
		Accept:   map[string]string{"peer": mesh.TokenHash("test-token")},
	}); err != nil {
		t.Fatalf("write mesh registry: %v", err)
	}
	return &config.Config{
		Daemon: config.DaemonConfig{
			Socket:   filepath.Join(tmp, "default.sock"),
			LogDir:   filepath.Join(tmp, "logs"),
			HTTPAddr: "",
		},
		Mux: config.MuxConfig{Backend: "tmux"},
		Tmux: config.TmuxConfig{
			SocketName:     "arcmux-profile-test-" + filepath.Base(tmp),
			DefaultSession: "agents",
		},
		Health: config.HealthConfig{
			CaptureInterval: "5s",
			IdleTimeout:     "60s",
			StuckTimeout:    "5m",
		},
		Hooks: config.HooksConfig{
			HookOutputDir:   filepath.Join(tmp, "hooks"),
			SessionStateDir: filepath.Join(tmp, "session-state"),
			AutoInstall:     false,
		},
		Pulse: config.PulseConfig{
			Enabled:           false,
			DataRoot:          filepath.Join(tmp, "data"),
			Interval:          "10s",
			DiscoveryInterval: "60s",
			Cadence:           config.PulseCadenceConfig{Interval: "30s"},
		},
		Mesh: config.MeshConfig{
			Enabled:      true,
			ListenAddr:   "127.0.0.1:0",
			RegistryPath: registryPath,
		},
		Agents: config.DefaultAgentProfiles(),
	}
}

func assertProfileReachable(t *testing.T, socketPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial profile %s: %v", socketPath, err)
	}
	defer conn.Close()
	c := arcmuxv1.NewAgentRuntimeClient(conn)
	if _, err := c.ListSessions(ctx, &arcmuxv1.ListSessionsRequest{}); err != nil {
		t.Fatalf("ListSessions via %s: %v", socketPath, err)
	}
}
