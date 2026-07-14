package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

func TestSessionCatalogAggregatesRootAndProfilesWithDuplicateIDs(t *testing.T) {
	now := time.Date(2026, 7, 14, 22, 30, 0, 0, time.UTC)
	root := newCatalogTestDaemon(t, "")
	alpha := newCatalogTestDaemon(t, "alpha")
	beta := newCatalogTestDaemon(t, "beta")
	addCatalogSession(root, "s-duplicate", "root session", now)
	addCatalogSession(alpha, "s-duplicate", "alpha session", now)
	addCatalogSession(beta, "s-beta", "beta session", now)

	if err := hooks.ApplyEventWithContract(
		root.cfg.Hooks.SessionStateDir,
		"s-duplicate",
		"codex",
		hooks.EventPromptSubmit,
		"",
		hooks.TurnContractUpdate{
			Goal:            "summarized root ask",
			LastUserMessage: "raw root prompt",
			VaultLink:       "/Users/test/agents/histories/root.md",
			Source:          "test",
		},
		now,
	); err != nil {
		t.Fatalf("write root hook state: %v", err)
	}
	if err := os.MkdirAll(alpha.cfg.Hooks.SessionStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha.cfg.Hooks.SessionStateDir, "s-duplicate.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	root.profileManager = &ProfileManager{
		daemons: map[string]*Daemon{"alpha": alpha, "beta": beta},
		records: map[string]ProfileRecord{},
	}
	catalog := NewSessionCatalog(root)
	catalog.now = func() time.Time { return now }

	list := catalog.List()
	if len(list.Sessions) != 3 {
		t.Fatalf("sessions = %d, want 3: %#v", len(list.Sessions), list.Sessions)
	}
	wantLocators := []string{"profile:alpha/s-duplicate", "profile:beta/s-beta", "root/s-duplicate"}
	for i, want := range wantLocators {
		got := string(list.Sessions[i].Locator.ProfileScope) + "/" + list.Sessions[i].Locator.SessionID
		if got != want {
			t.Fatalf("locator[%d] = %q, want %q", i, got, want)
		}
		if !list.Sessions[i].Freshness.ObservedAt.Equal(now) {
			t.Fatalf("observed_at[%d] = %v", i, list.Sessions[i].Freshness.ObservedAt)
		}
	}
	if list.Sessions[0].Work != nil {
		t.Fatalf("corrupt alpha hook state should be ignored: %#v", list.Sessions[0].Work)
	}
	if list.Sessions[2].Work == nil || list.Sessions[2].Work.Goal != "summarized root ask" {
		t.Fatalf("root work projection missing: %#v", list.Sessions[2].Work)
	}

	alphaLocator, _ := sessionview.NewLocator(sessionview.ProfileScope("profile:alpha"), "s-duplicate")
	alphaDetail, ok := catalog.Get(alphaLocator)
	if !ok || alphaDetail.Summary.Name != "alpha session" {
		t.Fatalf("alpha Get = %#v, %v", alphaDetail, ok)
	}
	rootLocator, _ := sessionview.NewLocator(sessionview.RootProfileScope, "s-duplicate")
	rootDetail, ok := catalog.Get(rootLocator)
	if !ok || rootDetail.Summary.Name != "root session" {
		t.Fatalf("root Get = %#v, %v", rootDetail, ok)
	}
}

func TestProfileManagerSnapshotDaemonsConcurrentMutation(t *testing.T) {
	pm := &ProfileManager{daemons: map[string]*Daemon{}, records: map[string]ProfileRecord{}}
	children := []*Daemon{
		newCatalogTestDaemon(t, "alpha"),
		newCatalogTestDaemon(t, "beta"),
	}
	addCatalogSession(children[0], "s-alpha", "alpha", time.Now())
	addCatalogSession(children[1], "s-beta", "beta", time.Now())
	root := newCatalogTestDaemon(t, "")
	root.profileManager = pm
	catalog := NewSessionCatalog(root)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			pm.mu.Lock()
			pm.daemons["alpha"] = children[i%len(children)]
			if i%2 == 0 {
				delete(pm.daemons, "alpha")
			}
			pm.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			for _, child := range pm.SnapshotDaemons() {
				_ = child.ListSessions()
			}
			_ = catalog.List()
		}
	}()
	wg.Wait()
}

func TestProfileSessionPersistenceIsolatedAndPreservesOwner(t *testing.T) {
	tmp := t.TempDir()
	socketDir := filepath.Join(tmp, "shared-sockets")
	alpha := newPersistenceTestDaemon(t, "alpha", socketDir, filepath.Join(tmp, "profiles", "alpha"))
	beta := newPersistenceTestDaemon(t, "beta", socketDir, filepath.Join(tmp, "profiles", "beta"))
	addOwnedExecSession(alpha, "s-alpha", "owner-alpha")
	addOwnedExecSession(beta, "s-beta", "owner-beta")

	alpha.persistSessions()
	beta.persistSessions()
	if alpha.persistPath() == beta.persistPath() {
		t.Fatalf("profile persistence paths collide: %q", alpha.persistPath())
	}
	for path, wantID := range map[string]string{alpha.persistPath(): "s-alpha", beta.persistPath(): "s-beta"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var records []persistedSession
		if err := json.Unmarshal(data, &records); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if len(records) != 1 || records[0].ID != wantID {
			t.Fatalf("records at %s = %#v, want %s", path, records, wantID)
		}
	}

	restarted := newPersistenceTestDaemon(t, "alpha", socketDir, filepath.Join(tmp, "profiles", "alpha"))
	restarted.restoreSessions()
	restored, ok := restarted.GetSession("s-alpha")
	if !ok {
		t.Fatal("alpha session was not restored")
	}
	if got := restored.Snapshot().OwnerID; got != "owner-alpha" {
		t.Fatalf("restored owner_id = %q, want owner-alpha", got)
	}
	if _, ok := restarted.GetSession("s-beta"); ok {
		t.Fatal("beta session leaked into alpha inventory")
	}
}

func newCatalogTestDaemon(t *testing.T, profileName string) *Daemon {
	t.Helper()
	tmp := t.TempDir()
	return New(&config.Config{
		Daemon: config.DaemonConfig{
			Socket:      filepath.Join(tmp, "daemon.sock"),
			LogDir:      filepath.Join(tmp, "logs"),
			ProfileName: profileName,
		},
		Mux:  config.MuxConfig{Backend: "tmux"},
		Tmux: config.TmuxConfig{SocketName: "catalog-test"},
		Hooks: config.HooksConfig{
			HookOutputDir:   filepath.Join(tmp, "hooks"),
			SessionStateDir: filepath.Join(tmp, "session-state"),
		},
		Agents: config.DefaultAgentProfiles(),
	}, slog.Default())
}

func addCatalogSession(daemon *Daemon, id, name string, now time.Time) {
	managed := session.NewSession(id, name, "codex", "/launch/cwd")
	managed.SetTransport(profile.TransportTmux)
	managed.SetState(session.StateIdle)
	managed.StartedAt = now.Add(-time.Hour)
	daemon.mu.Lock()
	daemon.sessions[id] = managed
	daemon.mu.Unlock()
}

func newPersistenceTestDaemon(t *testing.T, profileName, socketDir, dataDir string) *Daemon {
	t.Helper()
	return New(&config.Config{
		Daemon: config.DaemonConfig{
			Socket:      filepath.Join(socketDir, profileName+".sock"),
			LogDir:      filepath.Join(dataDir, "logs"),
			ProfileName: profileName,
			StatePath:   filepath.Join(dataDir, "state.bolt"),
		},
		Mux:  config.MuxConfig{Backend: "tmux"},
		Tmux: config.TmuxConfig{SocketName: "persist-test-" + profileName},
		Hooks: config.HooksConfig{
			HookOutputDir:   filepath.Join(dataDir, "hooks"),
			SessionStateDir: filepath.Join(dataDir, "session-state"),
		},
		Agents: config.DefaultAgentProfiles(),
	}, slog.Default())
}

func addOwnedExecSession(daemon *Daemon, id, owner string) {
	managed := session.NewSession(id, id, "claude_exec", "/launch/cwd")
	managed.SetTransport(profile.TransportExec)
	managed.SetOwnerID(owner)
	managed.SetState(session.StateIdle)
	daemon.mu.Lock()
	daemon.sessions[id] = managed
	daemon.mu.Unlock()
}
