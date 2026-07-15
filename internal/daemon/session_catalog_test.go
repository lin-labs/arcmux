package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
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
	alpha.sessions["s-alpha"].SetEnv(map[string]string{"API_TOKEN": "DO_NOT_PERSIST_SECRET", "ARCMUX_HANDOFF_INSTRUCTIONS": "/not-a-handoff.json"})

	alpha.persistSessions()
	beta.persistSessions()
	if alpha.persistPath() == beta.persistPath() {
		t.Fatalf("profile persistence paths collide: %q", alpha.persistPath())
	}
	for path, wantID := range map[string]string{alpha.persistPath(): "s-alpha", beta.persistPath(): "s-beta"} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("session inventory mode at %s = %v err=%v", path, info, err)
		}
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
		if strings.Contains(string(data), "DO_NOT_PERSIST_SECRET") || strings.Contains(string(data), "/not-a-handoff.json") {
			t.Fatalf("ordinary session env leaked into %s: %s", path, data)
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
	if len(restored.Snapshot().Env) != 0 {
		t.Fatalf("ordinary env restored unexpectedly: %+v", restored.Snapshot().Env)
	}
	if _, ok := restarted.GetSession("s-beta"); ok {
		t.Fatal("beta session leaked into alpha inventory")
	}
}

func TestSessionPersistenceIsAtomicSerializedAndRejectsUnsafeDestination(t *testing.T) {
	tmp := t.TempDir()
	d := newPersistenceTestDaemon(t, "alpha", filepath.Join(tmp, "sockets"), filepath.Join(tmp, "data"))
	addOwnedExecSession(d, "s-alpha", "owner-alpha")
	const writers = 24
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- d.persistSessionsChecked()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent persist: %v", err)
		}
	}
	data, err := os.ReadFile(d.persistPath())
	if err != nil {
		t.Fatal(err)
	}
	var records []persistedSession
	if err := json.Unmarshal(data, &records); err != nil || len(records) != 1 || records[0].ID != "s-alpha" {
		t.Fatalf("atomic inventory records=%+v err=%v data=%q", records, err, data)
	}
	entries, err := os.ReadDir(filepath.Dir(d.persistPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sessions-") {
			t.Fatalf("persistence left crash temp %q", entry.Name())
		}
	}
	if err := os.Remove(d.persistPath()); err != nil {
		t.Fatal(err)
	}
	secretTarget := filepath.Join(tmp, "DO_NOT_LEAK_TARGET")
	if err := os.WriteFile(secretTarget, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretTarget, d.persistPath()); err != nil {
		t.Fatal(err)
	}
	err = d.persistSessionsChecked()
	if err == nil || strings.Contains(err.Error(), tmp) || strings.Contains(err.Error(), secretTarget) {
		t.Fatalf("unsafe destination error=%v", err)
	}
	unchanged, err := os.ReadFile(secretTarget)
	if err != nil || string(unchanged) != "unchanged" {
		t.Fatalf("unsafe destination mutated target=%q err=%v", unchanged, err)
	}
}

func TestRestoredHandoffSessionCWDIsRedactedFromMeshInventory(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	d := newPersistenceTestDaemon(t, "", filepath.Join(tmp, "sockets"), dataDir)
	secretCWD := filepath.Join(tmp, "DO_NOT_LEAK_TARGET_WORKTREE")
	managed := session.NewSession("target-session", "handoff-deadbeef", "claude_exec", secretCWD)
	managed.SetTransport(profile.TransportExec)
	managed.SetOwnerID("arcmux-handoff:handoff-1")
	managed.MarkPrivate()
	managed.SetEnv(map[string]string{
		"ARCMUX_HANDOFF_INSTRUCTIONS": "/private/handoff-instructions.json",
		"API_TOKEN":                   "DO_NOT_PERSIST_HANDOFF_SECRET",
	})
	managed.SetCurrentCommand("arcmux-handoff-v1:safe-marker")
	managed.SetState(session.StateIdle)
	d.mu.Lock()
	d.sessions[managed.Snapshot().ID] = managed
	d.mu.Unlock()
	if err := d.persistSessionsChecked(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(d.persistPath())
	if err != nil || strings.Contains(string(persisted), "DO_NOT_PERSIST_HANDOFF_SECRET") || !strings.Contains(string(persisted), "/private/handoff-instructions.json") {
		t.Fatalf("handoff persistence allowlist data=%s err=%v", persisted, err)
	}
	restarted := newPersistenceTestDaemon(t, "", filepath.Join(tmp, "sockets"), dataDir)
	restarted.restoreSessions()
	restored, ok := restarted.GetSession("target-session")
	if !ok || !restored.Snapshot().Private || restored.Snapshot().Env["ARCMUX_HANDOFF_INSTRUCTIONS"] != "/private/handoff-instructions.json" || restored.Snapshot().Env["API_TOKEN"] != "" {
		t.Fatalf("restored handoff env = %+v ok=%t", restored, ok)
	}
	response := restarted.localMeshSessions()
	if len(response.Sessions) != 1 {
		t.Fatalf("mesh sessions = %+v", response.Sessions)
	}
	if response.Sessions[0].LaunchCWD != "" {
		t.Fatalf("restored handoff cwd leaked in mesh inventory: %+v", response.Sessions[0])
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secretCWD) {
		t.Fatalf("mesh inventory leaked private cwd: %s", encoded)
	}
}

func TestExternalOwnerPrefixCannotSpoofPrivateSessionPersistence(t *testing.T) {
	tmp := t.TempDir()
	d := newPersistenceTestDaemon(t, "", filepath.Join(tmp, "sockets"), filepath.Join(tmp, "data"))
	response, err := NewGRPCServer(d).CreateSession(context.Background(), &arcmuxv1.CreateSessionRequest{
		Agent:       "claude_exec",
		Cwd:         t.TempDir(),
		SessionName: "caller-spoof",
		OwnerId:     "arcmux-handoff:caller-controlled",
		Env: map[string]string{
			"ARCMUX_HANDOFF_INSTRUCTIONS": "/private/CALLER_CONTROLLED.json",
			"API_TOKEN":                   "DO_NOT_PERSIST_CALLER_SECRET",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := d.GetSession(response.SessionId)
	if !ok || sess.Snapshot().Private {
		t.Fatalf("external caller acquired private provenance: session=%v ok=%t", sess, ok)
	}
	data, err := os.ReadFile(d.persistPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "CALLER_CONTROLLED") || strings.Contains(string(data), "DO_NOT_PERSIST_CALLER_SECRET") || strings.Contains(string(data), `"private": true`) {
		t.Fatalf("caller-controlled private metadata persisted: %s", data)
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
