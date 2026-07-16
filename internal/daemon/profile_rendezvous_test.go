package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/profile"
)

func TestCreateSessionDuplicateIDsUseProfileScopedHookRendezvous(t *testing.T) {
	const profileName = "alpha"
	duplicateID := fmt.Sprintf("s-create-profile-race-%d", time.Now().UnixNano())
	rootDir := t.TempDir()
	root := newRendezvousCreateDaemon(t, rootDir, "")
	profileDaemon := newRendezvousCreateDaemon(t, rootDir, profileName)
	for _, daemon := range []*Daemon{root, profileDaemon} {
		daemon.sessionIDGenerator = func() string { return duplicateID }
	}

	rootPath, err := hooks.SessionEnvFilePath(hooks.SessionEnvDir, "root", duplicateID)
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := hooks.SessionEnvFilePath(hooks.SessionEnvDir, "profile:"+profileName, duplicateID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(rootPath)
		_ = os.Remove(profilePath)
		root.watcher.Unwatch(duplicateID)
		profileDaemon.watcher.Unwatch(duplicateID)
	})

	// Hold both production CreateSession calls inside setupTmuxPane until each
	// has written its rendezvous file. If the file were still keyed by session
	// ID alone, the second write would overwrite the first before either loader
	// command could run.
	var barrierMu sync.Mutex
	arrived := 0
	ready := make(chan struct{})
	commands := make(map[string]string)
	installHook := func(scope string, daemon *Daemon) {
		daemon.setupTmuxPaneHook = func(_ context.Context, _, _, _ string, _ map[string]string, command string) (string, error) {
			barrierMu.Lock()
			commands[scope] = command
			arrived++
			if arrived == 2 {
				close(ready)
			}
			barrierMu.Unlock()
			<-ready
			return "%test-" + strings.ReplaceAll(scope, ":", "-"), nil
		}
	}
	installHook("root", root)
	installHook("profile:"+profileName, profileDaemon)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, daemon := range []*Daemon{root, profileDaemon} {
		wg.Add(1)
		go func(d *Daemon) {
			defer wg.Done()
			_, err := d.CreateSession(context.Background(), CreateSessionRequest{
				Agent: "codex", CWD: rootDir, Name: "duplicate-rendezvous",
			})
			errs <- err
		}(daemon)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if rootPath == profilePath {
		t.Fatalf("profile-scoped rendezvous paths collide: %q", rootPath)
	}
	assertScopedRendezvous := func(scope, wantStateDir, wantSocket string) {
		t.Helper()
		exports, err := hooks.LoadSessionEnvExports(hooks.SessionEnvDir, scope, duplicateID)
		if err != nil {
			t.Fatalf("load %s rendezvous: %v", scope, err)
		}
		joined := strings.Join(exports, "\n")
		for _, want := range []string{
			"ARCMUX_PROFILE_SCOPE='" + scope + "'",
			"ARCMUX_SESSION_STATE_DIR='" + wantStateDir + "'",
			"ARCMUX_DAEMON_SOCKET='" + wantSocket + "'",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("%s rendezvous missing %q:\n%s", scope, want, joined)
			}
		}
		if !strings.Contains(commands[scope], "hook-env '"+scope+"' '"+duplicateID+"'") {
			t.Fatalf("%s loader command lacks exact locator: %s", scope, commands[scope])
		}
	}
	assertScopedRendezvous("root", root.cfg.Hooks.SessionStateDir, root.cfg.Daemon.Socket)
	assertScopedRendezvous("profile:"+profileName, profileDaemon.cfg.Hooks.SessionStateDir, profileDaemon.cfg.Daemon.Socket)
}

func newRendezvousCreateDaemon(t *testing.T, root, profileName string) *Daemon {
	t.Helper()
	scopeDir := "root"
	if profileName != "" {
		scopeDir = filepath.Join("profiles", profileName)
	}
	profiles := config.DefaultAgentProfiles()
	codex := profiles["codex"]
	codex.StartCommand = "true"
	profiles = map[string]profile.Profile{"codex": codex}
	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket:      filepath.Join(root, scopeDir, "daemon.sock"),
			LogDir:      filepath.Join(root, scopeDir, "logs"),
			ProfileName: profileName,
		},
		Mux:  config.MuxConfig{Backend: "tmux"},
		Tmux: config.TmuxConfig{SocketName: "rendezvous-" + strings.ReplaceAll(scopeDir, string(filepath.Separator), "-")},
		Hooks: config.HooksConfig{
			HookOutputDir:   filepath.Join(root, scopeDir, "hooks"),
			SessionStateDir: filepath.Join(root, scopeDir, "sessions"),
			AutoInstall:     true,
		},
		Agents: profiles,
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	d.ctx, d.cancel = context.WithCancel(context.Background())
	t.Cleanup(d.cancel)
	return d
}
