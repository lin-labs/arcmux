package meshstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	testSurfaceA   = "11111111-1111-4111-8111-111111111111"
	testSurfaceB   = "22222222-2222-4222-8222-222222222222"
	testWorkspaceA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mesh-state"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testLocator(sessionID string) RemoteSessionLocator {
	return RemoteSessionLocator{
		SchemaVersion: SchemaVersion,
		DeviceID:      "devbox",
		ProfileScope:  RootProfileScope,
		SessionID:     sessionID,
	}
}

func commitInventory(t *testing.T, store *Store, revision uint64, ids ...string) {
	t.Helper()
	snapshot, err := store.BeginSnapshot("devbox", RootProfileScope, "boot-1", revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		metadata := json.RawMessage(fmt.Sprintf(`{"name":%q,"state":"working"}`, id))
		if err := snapshot.Upsert(testLocator(id), metadata); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteSessionLocatorIdentityIgnoresTransportBinding(t *testing.T) {
	a := testLocator("s-1")
	b := a
	b.TransportBindingID = "transport-2"
	if !a.EqualIdentity(b) {
		t.Fatal("transport binding changed locator identity")
	}
	b.ProfileScope = NamedProfileScope("olympus")
	if a.EqualIdentity(b) {
		t.Fatal("profile scope did not change locator identity")
	}
}

func TestValidationRejectsTraversalAndUnsafeMetadata(t *testing.T) {
	bad := testLocator("../escape")
	if err := bad.Validate(); err == nil {
		t.Fatal("traversal session id accepted")
	}
	bad = testLocator("s-1")
	bad.DeviceID = "devbox/other"
	if err := bad.Validate(); err == nil {
		t.Fatal("path separator in device id accepted")
	}
	bad = testLocator("s-1")
	bad.ProfileScope = "profile:../oops"
	if err := bad.Validate(); err == nil {
		t.Fatal("traversal profile accepted")
	}
	if err := validateMetadata(json.RawMessage(`{"state":"working","authorization":"Bearer nope"}`)); err == nil {
		t.Fatal("secret metadata key accepted")
	}
	if err := validateMetadata(json.RawMessage(`[]`)); err == nil {
		t.Fatal("non-object metadata accepted")
	}
	store := openTestStore(t)
	if err := store.PutArtifact(ArtifactEnvelope{ID: "../escape", Kind: ArtifactGoal, Provenance: "test"}); err == nil {
		t.Fatal("artifact traversal accepted")
	}
}

func TestSnapshotAbortNeverMarksGoneAndCommitDoes(t *testing.T) {
	store := openTestStore(t)
	commitInventory(t, store, 1, "s-a", "s-b")

	incomplete, err := store.BeginSnapshot("devbox", RootProfileScope, "boot-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := incomplete.Upsert(testLocator("s-a"), json.RawMessage(`{"name":"a"}`)); err != nil {
		t.Fatal(err)
	}
	incomplete.Abort()
	for _, id := range []string{"s-a", "s-b"} {
		projection, err := store.GetRemoteSession(testLocator(id))
		if err != nil {
			t.Fatal(err)
		}
		if projection.Freshness != FreshnessSyncing {
			t.Fatalf("%s after incomplete inventory = %s, want syncing", id, projection.Freshness)
		}
	}

	commitInventory(t, store, 3, "s-a")
	a, _ := store.GetRemoteSession(testLocator("s-a"))
	b, _ := store.GetRemoteSession(testLocator("s-b"))
	if a.Freshness != FreshnessFresh {
		t.Fatalf("present session = %s, want fresh", a.Freshness)
	}
	if b.Freshness != FreshnessGone || b.SourceRevision != 3 {
		t.Fatalf("omitted session = %+v, want gone at revision 3", b)
	}
	peer, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if peer.Freshness != FreshnessFresh || peer.Inventories[string(RootProfileScope)].SourceRevision != 3 {
		t.Fatalf("peer cursor = %+v", peer)
	}

	// A later complete legacy snapshot must advance already-gone records to its
	// cursor; otherwise effective reads would mistake them for an interrupted
	// partial write.
	commitInventory(t, store, 4, "s-a")
	b, err = store.GetRemoteSession(testLocator("s-b"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Freshness != FreshnessGone || b.SourceRevision != 4 {
		t.Fatalf("repeated omitted session = %+v, want gone at revision 4", b)
	}
}

func TestSnapshotRejectsStaleRevision(t *testing.T) {
	store := openTestStore(t)
	commitInventory(t, store, 5, "s-a")
	if _, err := store.BeginSnapshot("devbox", RootProfileScope, "boot-1", 5); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("same revision begin error = %v, want ErrStaleSnapshot", err)
	}
	projection, err := store.GetRemoteSession(testLocator("s-a"))
	if err != nil {
		t.Fatal(err)
	}
	if projection.Freshness != FreshnessFresh {
		t.Fatalf("rejected stale snapshot changed freshness to %s", projection.Freshness)
	}
	// A new source epoch establishes a new revision sequence.
	snapshot, err := store.BeginSnapshot("devbox", RootProfileScope, "boot-2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatalf("new epoch revision rejected: %v", err)
	}
}

func TestDisconnectAndReconnectFreshnessPreserveGone(t *testing.T) {
	store := openTestStore(t)
	commitInventory(t, store, 1, "s-a", "s-b")
	commitInventory(t, store, 2, "s-a") // s-b is gone

	if err := store.MarkPeerDisconnected("devbox"); err != nil {
		t.Fatal(err)
	}
	a, _ := store.GetRemoteSession(testLocator("s-a"))
	b, _ := store.GetRemoteSession(testLocator("s-b"))
	if a.Freshness != FreshnessStale || b.Freshness != FreshnessGone {
		t.Fatalf("disconnect freshness a=%s b=%s", a.Freshness, b.Freshness)
	}
	if err := store.MarkPeerSyncing("devbox", "boot-2"); err != nil {
		t.Fatal(err)
	}
	a, _ = store.GetRemoteSession(testLocator("s-a"))
	b, _ = store.GetRemoteSession(testLocator("s-b"))
	if a.Freshness != FreshnessSyncing || b.Freshness != FreshnessGone {
		t.Fatalf("reconnect freshness a=%s b=%s", a.Freshness, b.Freshness)
	}
}

func TestSurfaceBindingRequiresExplicitReplacementAndCanUnbind(t *testing.T) {
	store := openTestStore(t)
	binding := SurfaceBinding{
		SchemaVersion: SchemaVersion,
		BindingID:     "binding-a",
		LocalDeviceID: "ref",
		Mux:           "cmux",
		SurfaceID:     testSurfaceA,
		WorkspaceID:   testWorkspaceA,
		Locator:       testLocator("s-a"),
		Source:        "mission-control",
	}
	if err := store.PutSurfaceBinding(binding, false); err != nil {
		t.Fatal(err)
	}
	// An idempotent write is not a retarget.
	if err := store.PutSurfaceBinding(binding, false); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	replacement := binding
	replacement.BindingID = "binding-b"
	replacement.Locator = testLocator("s-b")
	if err := store.PutSurfaceBinding(replacement, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("implicit replacement error = %v", err)
	}
	if err := store.PutSurfaceBinding(replacement, true); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSurfaceBinding(testSurfaceA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Locator.SessionID != "s-b" {
		t.Fatalf("binding locator = %+v", got.Locator)
	}
	if err := store.MarkPeerDisconnected("devbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSurfaceBinding(testSurfaceA); err != nil {
		t.Fatalf("outage removed binding: %v", err)
	}
	if err := store.DeleteSurfaceBinding(testSurfaceA); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSurfaceBinding(testSurfaceA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("binding after delete error = %v", err)
	}
}

func TestArtifactValidationAndPersistence(t *testing.T) {
	store := openTestStore(t)
	observed := time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
	artifact := ArtifactEnvelope{
		ID:               "pr-3",
		Kind:             ArtifactPullRequest,
		Title:            "Mesh protocol",
		State:            "open",
		URL:              "https://github.com/lin-labs/arcmux/pull/3?view=files",
		PathHint:         "~/agents/histories/2026-07-14-arcmux.md",
		Repo:             &RepoRef{Repo: "lin-labs/arcmux", Ref: "boyan/mesh", Commit: "abcdef1234567"},
		Session:          ptrLocator(testLocator("s-a")),
		Provenance:       "github-api",
		Revision:         "etag-3",
		RemoteObservedAt: &observed,
	}
	if err := store.PutArtifact(artifact); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetArtifact(ArtifactPullRequest, "pr-3")
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceivedAt.IsZero() || got.Title != artifact.Title {
		t.Fatalf("artifact = %+v", got)
	}

	bad := artifact
	bad.ID = "secret-url"
	bad.URL = "https://example.com/x?access_token=secret"
	if err := store.PutArtifact(bad); err == nil {
		t.Fatal("secret-bearing URL accepted")
	}
	bad = artifact
	bad.ID = "userinfo"
	bad.URL = "https://user:pass@example.com/x"
	if err := store.PutArtifact(bad); err == nil {
		t.Fatal("URL userinfo accepted")
	}
	bad = artifact
	bad.ID = "absolute-path"
	bad.PathHint = "/Users/blin/secret"
	if err := store.PutArtifact(bad); err == nil {
		t.Fatal("absolute path accepted")
	}
	bad = artifact
	bad.ID = "traversal-path"
	bad.PathHint = "~/agents/../secret"
	if err := store.PutArtifact(bad); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func ptrLocator(locator RemoteSessionLocator) *RemoteSessionLocator { return &locator }

func TestRestartPersistenceAndPrivatePermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	commitInventory(t, store, 1, "s-a")
	if err := store.PutSurfaceBinding(SurfaceBinding{
		BindingID: "binding-a", LocalDeviceID: "ref", Mux: "cmux",
		SurfaceID: testSurfaceA, WorkspaceID: testWorkspaceA,
		Locator: testLocator("s-a"), Source: "test",
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.PutArtifact(ArtifactEnvelope{ID: "goal-a", Kind: ArtifactGoal, Title: "Ship mesh", Provenance: "hook"}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetRemoteSession(testLocator("s-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetSurfaceBinding(testSurfaceA); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetArtifact(ArtifactGoal, "goal-a"); err != nil {
		t.Fatal(err)
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %o", rootInfo.Mode().Perm())
	}
	projectionPath, _ := reopened.remoteSessionPath(testLocator("s-a"))
	fileInfo, err := os.Stat(projectionPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("projection mode = %o", fileInfo.Mode().Perm())
	}
	if dirInfo, err := os.Stat(filepath.Dir(projectionPath)); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("projection dir mode = %v err=%v", dirInfo.Mode().Perm(), err)
	}
}

func TestDeterministicLists(t *testing.T) {
	store := openTestStore(t)
	commitInventory(t, store, 1, "s-c", "s-a", "s-b")
	sessions, err := store.ListRemoteSessions("", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"s-a", "s-b", "s-c"} {
		if sessions[i].Locator.SessionID != want {
			t.Fatalf("sessions[%d] = %s, want %s", i, sessions[i].Locator.SessionID, want)
		}
	}
	for _, artifact := range []ArtifactEnvelope{
		{ID: "z", Kind: ArtifactGoal, Provenance: "test"},
		{ID: "a", Kind: ArtifactDocument, Provenance: "test"},
		{ID: "a", Kind: ArtifactGoal, Provenance: "test"},
	} {
		if err := store.PutArtifact(artifact); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, err := store.ListArtifacts("")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{string(artifacts[0].Kind) + "/" + artifacts[0].ID, string(artifacts[1].Kind) + "/" + artifacts[1].ID, string(artifacts[2].Kind) + "/" + artifacts[2].ID}
	want := []string{"document/a", "goal/a", "goal/z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("artifact order = %v", got)
		}
	}
}

func TestWatcherEmitsGapOnOverflow(t *testing.T) {
	store := openTestStore(t)
	events, cancel := store.Watch(1)
	defer cancel()
	if err := store.PutArtifact(ArtifactEnvelope{ID: "a", Kind: ArtifactGoal, Provenance: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutArtifact(ArtifactEnvelope{ID: "b", Kind: ArtifactGoal, Provenance: "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Type != ChangeGap {
			t.Fatalf("overflow event = %+v, want gap", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watcher gap")
	}
	if err := store.PutArtifact(ArtifactEnvelope{ID: "c", Kind: ArtifactGoal, Provenance: "test"}); err != nil {
		t.Fatal(err)
	}
	if event := <-events; event.Type != ChangeUpsert || event.Key != "goal/c" {
		t.Fatalf("post-gap event = %+v", event)
	}
}

func TestConcurrentStoreAccess(t *testing.T) {
	store := openTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("artifact-%02d", i)
			if err := store.PutArtifact(ArtifactEnvelope{ID: id, Kind: ArtifactDocument, Provenance: "race-test"}); err != nil {
				t.Errorf("put %s: %v", id, err)
				return
			}
			if _, err := store.GetArtifact(ArtifactDocument, id); err != nil {
				t.Errorf("get %s: %v", id, err)
			}
			if _, err := store.ListArtifacts(ArtifactDocument); err != nil {
				t.Errorf("list: %v", err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			surface := fmt.Sprintf("%08x-0000-4000-8000-%012x", i+1, i+1)
			binding := SurfaceBinding{
				BindingID: fmt.Sprintf("binding-%d", i), LocalDeviceID: "ref", Mux: "cmux",
				SurfaceID: surface, WorkspaceID: testWorkspaceA,
				Locator: testLocator(fmt.Sprintf("s-%d", i)), Source: "race-test",
			}
			if err := store.PutSurfaceBinding(binding, false); err != nil {
				t.Errorf("put binding: %v", err)
			}
		}()
	}
	wg.Wait()
	artifacts, err := store.ListArtifacts(ArtifactDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 24 {
		t.Fatalf("artifact count = %d", len(artifacts))
	}
	bindings, err := store.ListSurfaceBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 8 {
		t.Fatalf("binding count = %d", len(bindings))
	}
}

func TestSymlinkTraversalIsRejected(t *testing.T) {
	store := openTestStore(t)
	outside := t.TempDir()
	base, err := store.path("artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, string(ArtifactGoal))); err != nil {
		t.Fatal(err)
	}
	err = store.PutArtifact(ArtifactEnvelope{ID: "escape", Kind: ArtifactGoal, Provenance: "test"})
	if err == nil {
		t.Fatal("write followed symlink outside store")
	}
}

func locatorFor(deviceID string, scope ProfileScope, sessionID string) RemoteSessionLocator {
	return RemoteSessionLocator{
		SchemaVersion: SchemaVersion,
		DeviceID:      deviceID,
		ProfileScope:  scope,
		SessionID:     sessionID,
	}
}

func commitSessionInventory(t *testing.T, store *Store, deviceID, epoch string, revision uint64, scopes []ProfileScope, locators ...RemoteSessionLocator) {
	t.Helper()
	snapshot, err := store.BeginSessionInventory(deviceID, scopes, epoch, revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, locator := range locators {
		metadata := json.RawMessage(fmt.Sprintf(`{"name":%q,"state":"working"}`, locator.SessionID))
		if err := snapshot.Upsert(locator, metadata); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
}

func commitArtifactInventory(t *testing.T, store *Store, deviceID, epoch string, revision uint64, artifacts ...ArtifactEnvelope) {
	t.Helper()
	snapshot, err := store.BeginArtifactInventory(deviceID, epoch, revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if err := snapshot.Upsert(artifact); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
}

func testArtifact(id, title string) ArtifactEnvelope {
	return ArtifactEnvelope{ID: id, Kind: ArtifactGoal, Title: title, Provenance: "mesh-test"}
}

func TestSessionInventoryCommitsAllScopesAtomicallyAndRemovesScope(t *testing.T) {
	store := openTestStore(t)
	profile := NamedProfileScope("olympus")
	rootSession := locatorFor("devbox", RootProfileScope, "root-a")
	profileSession := locatorFor("devbox", profile, "profile-a")
	commitSessionInventory(t, store, "devbox", "boot-1", 1,
		[]ProfileScope{RootProfileScope, profile}, rootSession, profileSession)

	commitSessionInventory(t, store, "devbox", "boot-1", 2,
		[]ProfileScope{RootProfileScope}, rootSession)
	root, err := store.GetRemoteSession(rootSession)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.GetRemoteSession(profileSession)
	if err != nil {
		t.Fatal(err)
	}
	if root.Freshness != FreshnessFresh || root.SourceRevision != 2 {
		t.Fatalf("root projection = %+v", root)
	}
	if removed.Freshness != FreshnessGone || removed.SourceRevision != 2 {
		t.Fatalf("removed profile projection = %+v", removed)
	}
	peer, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if peer.SessionInventory == nil || peer.SessionInventory.SourceRevision != 2 {
		t.Fatalf("session inventory cursor = %+v", peer.SessionInventory)
	}
	if peer.Inventories[string(profile)].SourceRevision != 2 {
		t.Fatalf("removed profile cursor was not advanced: %+v", peer.Inventories)
	}
}

func TestInterruptedSessionInventoryNeverExposesPartialFreshAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	profile := NamedProfileScope("olympus")
	old := locatorFor("devbox", RootProfileScope, "old")
	partial := locatorFor("devbox", profile, "partial")
	commitSessionInventory(t, store, "devbox", "boot-1", 1,
		[]ProfileScope{RootProfileScope, profile}, old)

	snapshot, err := store.BeginSessionInventory("devbox", []ProfileScope{RootProfileScope, profile}, "boot-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process dying after one projection rename but before the final
	// peer cursor rename. The read path must use the durable cursor as the
	// visibility marker, including after reopening the store.
	now := time.Now().UTC()
	store.mu.Lock()
	err = store.writeRemoteSessionLocked(RemoteSessionProjection{
		SchemaVersion: SchemaVersion,
		Locator:       partial,
		Metadata:      json.RawMessage(`{"name":"partial"}`),
		ReceivedAt:    now, FreshnessChangedAt: now,
		SourceEpoch: "boot-1", SourceRevision: 2, Freshness: FreshnessFresh,
	})
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Abort()

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, locator := range []RemoteSessionLocator{old, partial} {
		projection, err := reopened.GetRemoteSession(locator)
		if err != nil {
			t.Fatal(err)
		}
		if projection.Freshness != FreshnessSyncing {
			t.Fatalf("%s freshness = %s, want syncing", locator.SessionID, projection.Freshness)
		}
	}
}

func TestRemoteArtifactInventoryIdentityMissingAndPeerFreshness(t *testing.T) {
	store := openTestStore(t)
	local := testArtifact("same", "local")
	if err := store.PutArtifact(local); err != nil {
		t.Fatal(err)
	}
	commitArtifactInventory(t, store, "devbox", "boot-1", 1,
		testArtifact("same", "devbox"), testArtifact("removed", "removed"))
	commitArtifactInventory(t, store, "laptop", "boot-2", 1,
		testArtifact("same", "laptop"))
	commitArtifactInventory(t, store, "devbox", "boot-1", 2,
		testArtifact("same", "devbox-v2"))

	all, err := store.ListAllArtifacts(ArtifactGoal)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("artifact count = %d, want local + two peers + gone record: %+v", len(all), all)
	}
	localGot, err := store.GetArtifact(ArtifactGoal, "same")
	if err != nil || localGot.OriginDeviceID != "" || localGot.Title != "local" {
		t.Fatalf("local artifact = %+v err=%v", localGot, err)
	}
	outbound, err := store.ListArtifacts(ArtifactGoal)
	if err != nil || len(outbound) != 1 || outbound[0].OriginDeviceID != "" {
		t.Fatalf("local/outbound inventory leaked remote artifacts: %+v err=%v", outbound, err)
	}
	for deviceID, wantTitle := range map[string]string{"devbox": "devbox-v2", "laptop": "laptop"} {
		got, err := store.GetRemoteArtifact(deviceID, ArtifactGoal, "same")
		if err != nil {
			t.Fatal(err)
		}
		if got.OriginDeviceID != deviceID || got.SourceID != "same" || got.Title != wantTitle {
			t.Fatalf("%s artifact = %+v", deviceID, got)
		}
	}
	removed, err := store.GetRemoteArtifact("devbox", ArtifactGoal, "removed")
	if err != nil || removed.Freshness != FreshnessGone || removed.SourceRevision != 2 {
		t.Fatalf("removed artifact = %+v err=%v", removed, err)
	}

	if err := store.MarkPeerDisconnected("devbox"); err != nil {
		t.Fatal(err)
	}
	active, _ := store.GetRemoteArtifact("devbox", ArtifactGoal, "same")
	removed, _ = store.GetRemoteArtifact("devbox", ArtifactGoal, "removed")
	if active.Freshness != FreshnessStale || removed.Freshness != FreshnessGone {
		t.Fatalf("disconnect artifact freshness active=%s removed=%s", active.Freshness, removed.Freshness)
	}
	if err := store.MarkPeerSyncing("devbox", "boot-1"); err != nil {
		t.Fatal(err)
	}
	active, _ = store.GetRemoteArtifact("devbox", ArtifactGoal, "same")
	removed, _ = store.GetRemoteArtifact("devbox", ArtifactGoal, "removed")
	if active.Freshness != FreshnessSyncing || removed.Freshness != FreshnessGone {
		t.Fatalf("reconnect artifact freshness active=%s removed=%s", active.Freshness, removed.Freshness)
	}
}

func TestInterruptedArtifactInventoryNeverExposesPartialFreshAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	commitArtifactInventory(t, store, "devbox", "boot-1", 1, testArtifact("old", "old"))
	snapshot, err := store.BeginArtifactInventory("devbox", "boot-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	partial := testArtifact("partial", "partial")
	partial.SchemaVersion = SchemaVersion
	partial.SourceID = partial.ID
	partial.OriginDeviceID = "devbox"
	partial.SourceEpoch = "boot-1"
	partial.SourceRevision = 2
	partial.ReceivedAt = now
	partial.FreshnessChangedAt = now
	partial.Freshness = FreshnessFresh
	store.mu.Lock()
	err = store.writeRemoteArtifactLocked(partial)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Abort()

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "partial"} {
		artifact, err := reopened.GetRemoteArtifact("devbox", ArtifactGoal, id)
		if err != nil {
			t.Fatal(err)
		}
		if artifact.Freshness != FreshnessSyncing {
			t.Fatalf("%s freshness = %s, want syncing", id, artifact.Freshness)
		}
	}
}

func TestArtifactOriginAndCursorAreStoreOwned(t *testing.T) {
	store := openTestStore(t)
	spoofed := testArtifact("a", "spoofed")
	spoofed.OriginDeviceID = "other"
	if err := store.PutArtifact(spoofed); err == nil {
		t.Fatal("PutArtifact accepted remote origin")
	}
	snapshot, err := store.BeginArtifactInventory("devbox", "boot-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Upsert(spoofed); err == nil {
		t.Fatal("remote inventory accepted caller-controlled origin")
	}
	withCursor := testArtifact("b", "cursor")
	withCursor.SourceEpoch = "fake"
	withCursor.SourceRevision = 99
	if err := snapshot.Upsert(withCursor); err == nil {
		t.Fatal("remote inventory accepted caller-controlled cursor")
	}
	snapshot.Abort()
}

func TestArtifactTextRejectsTerminalControlCharacters(t *testing.T) {
	base := testArtifact("safe", "safe")
	tests := []struct {
		name   string
		mutate func(*ArtifactEnvelope)
	}{
		{"title", func(a *ArtifactEnvelope) { a.Title = "hello\x1b[31m" }},
		{"state", func(a *ArtifactEnvelope) { a.State = "open\nclosed" }},
		{"url", func(a *ArtifactEnvelope) { a.URL = "https://example.com/\x1b[31m" }},
		{"path", func(a *ArtifactEnvelope) { a.PathHint = "~/agents/\x1b[31m" }},
		{"repo-ref", func(a *ArtifactEnvelope) { a.Repo = &RepoRef{Repo: "lin-labs/arcmux", Ref: "main\x1b"} }},
		{"provenance", func(a *ArtifactEnvelope) { a.Provenance = "mesh\u0085peer" }},
		{"revision", func(a *ArtifactEnvelope) { a.Revision = "etag\rnext" }},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			artifact := base
			artifact.ID = fmt.Sprintf("unsafe-%d", i)
			tc.mutate(&artifact)
			if err := store.PutArtifact(artifact); err == nil {
				t.Fatalf("%s control text accepted", tc.name)
			}
		})
	}
}

func TestResolveSurfaceBindingReturnsExactTargetAndEffectiveFreshness(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	target := locatorFor("devbox", RootProfileScope, "target")
	commitSessionInventory(t, store, "devbox", "boot-1", 1, []ProfileScope{RootProfileScope}, target)
	binding := SurfaceBinding{
		BindingID: "binding-target", LocalDeviceID: "ref", Mux: "cmux",
		SurfaceID: testSurfaceA, WorkspaceID: testWorkspaceA,
		Locator: target, Source: "test",
	}
	if err := store.PutSurfaceBinding(binding, false); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveSurfaceBinding(testSurfaceA)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Projection == nil || resolved.Projection.Locator.SessionID != "target" || resolved.EffectiveFreshness != FreshnessFresh {
		t.Fatalf("fresh resolution = %+v", resolved)
	}
	if err := store.MarkPeerDisconnected("devbox"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = reopened.ResolveSurfaceBinding(testSurfaceA)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Binding.BindingID != "binding-target" || resolved.EffectiveFreshness != FreshnessStale {
		t.Fatalf("stale resolution = %+v", resolved)
	}
}

func TestArtifactCommitCannotHideInterruptedSessionInventory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	target := locatorFor("devbox", RootProfileScope, "target")
	commitSessionInventory(t, store, "devbox", "boot-1", 1, []ProfileScope{RootProfileScope}, target)
	commitArtifactInventory(t, store, "devbox", "boot-1", 1, testArtifact("goal", "goal"))
	if err := store.PutSurfaceBinding(SurfaceBinding{
		BindingID: "binding-target", LocalDeviceID: "ref", Mux: "cmux",
		SurfaceID: testSurfaceA, WorkspaceID: testWorkspaceA,
		Locator: target, Source: "test",
	}, false); err != nil {
		t.Fatal(err)
	}

	incomplete, err := store.BeginSessionInventory("devbox", []ProfileScope{RootProfileScope}, "boot-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	// Completing another inventory must not reset the independent session
	// visibility marker to fresh while this transaction is unfinished.
	commitArtifactInventory(t, store, "devbox", "boot-1", 2, testArtifact("goal", "goal-v2"))
	incomplete.Abort()

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := reopened.ResolveSurfaceBinding(testSurfaceA)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EffectiveFreshness != FreshnessSyncing || resolved.PeerFreshness != FreshnessSyncing {
		t.Fatalf("interrupted resolution = %+v", resolved)
	}
	peer, err := reopened.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if peer.SessionFreshness != FreshnessSyncing || peer.ArtifactFreshness != FreshnessFresh {
		t.Fatalf("independent peer freshness = %+v", peer)
	}
}

func TestTopicSpecificReconnectPreservesOtherInventoryAndBindingFreshness(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	target := locatorFor("devbox", RootProfileScope, "target")
	commitSessionInventory(t, store, "devbox", "boot-1", 1, []ProfileScope{RootProfileScope}, target)
	commitArtifactInventory(t, store, "devbox", "boot-1", 1, testArtifact("goal", "goal"))
	if err := store.PutSurfaceBinding(SurfaceBinding{
		BindingID: "binding-target", LocalDeviceID: "ref", Mux: "cmux",
		SurfaceID: testSurfaceA, WorkspaceID: testWorkspaceA,
		Locator: target, Source: "test",
	}, false); err != nil {
		t.Fatal(err)
	}

	// Simulate the additive migration from a peer file written before topic
	// markers existed. The old aggregate freshness represented both topics.
	peer, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	peer.SessionFreshness = ""
	peer.SessionFreshnessChangedAt = time.Time{}
	peer.ArtifactFreshness = ""
	peer.ArtifactFreshnessChangedAt = time.Time{}
	store.mu.Lock()
	err = store.writePeerLocked(peer)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkArtifactSyncing("devbox", "boot-1"); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.ResolveSurfaceBinding(testSurfaceA)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PeerFreshness != FreshnessSyncing || resolved.EffectiveFreshness != FreshnessFresh {
		t.Fatalf("artifact-only sync changed session resolution: %+v", resolved)
	}
	session, _ := store.GetRemoteSession(target)
	artifact, _ := store.GetRemoteArtifact("devbox", ArtifactGoal, "goal")
	if session.Freshness != FreshnessFresh || artifact.Freshness != FreshnessSyncing {
		t.Fatalf("artifact-only sync session=%s artifact=%s", session.Freshness, artifact.Freshness)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = reopened.ResolveSurfaceBinding(testSurfaceA)
	if err != nil || resolved.EffectiveFreshness != FreshnessFresh {
		t.Fatalf("artifact-only sync after restart = %+v err=%v", resolved, err)
	}

	if err := reopened.MarkPeerDisconnected("devbox"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkSessionSyncing("devbox", "boot-2"); err != nil {
		t.Fatal(err)
	}
	resolved, err = reopened.ResolveSurfaceBinding(testSurfaceA)
	if err != nil || resolved.EffectiveFreshness != FreshnessSyncing {
		t.Fatalf("session reconnect resolution = %+v err=%v", resolved, err)
	}
	artifact, err = reopened.GetRemoteArtifact("devbox", ArtifactGoal, "goal")
	if err != nil || artifact.Freshness != FreshnessStale {
		t.Fatalf("session reconnect changed stale artifact = %+v err=%v", artifact, err)
	}

	commitSessionInventory(t, reopened, "devbox", "boot-2", 1, []ProfileScope{RootProfileScope}, target)
	resolved, err = reopened.ResolveSurfaceBinding(testSurfaceA)
	if err != nil || resolved.EffectiveFreshness != FreshnessFresh {
		t.Fatalf("session refresh resolution = %+v err=%v", resolved, err)
	}
	artifact, err = reopened.GetRemoteArtifact("devbox", ArtifactGoal, "goal")
	if err != nil || artifact.Freshness != FreshnessStale {
		t.Fatalf("session refresh changed stale artifact = %+v err=%v", artifact, err)
	}
}

func TestSessionSyncingDoesNotAlterArtifactOnlyPeer(t *testing.T) {
	store := openTestStore(t)
	commitArtifactInventory(t, store, "devbox", "boot-1", 1, testArtifact("goal", "cached"))
	if err := store.MarkSessionSyncing("devbox", "boot-1"); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.GetRemoteArtifact("devbox", ArtifactGoal, "goal")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Freshness != FreshnessFresh {
		t.Fatalf("session-only sync changed artifact-only cache to %s", artifact.Freshness)
	}
	peer, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if peer.SessionFreshness != FreshnessSyncing || peer.ArtifactFreshness != FreshnessFresh {
		t.Fatalf("topic markers = %+v", peer)
	}
}

func TestDesiredTopicsPersistAndPreservePeerState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mesh-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	target := locatorFor("devbox", RootProfileScope, "target")
	commitSessionInventory(t, store, "devbox", "boot-1", 1, []ProfileScope{RootProfileScope}, target)
	commitArtifactInventory(t, store, "devbox", "boot-1", 1, testArtifact("goal", "goal"))
	before, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetDesiredTopics("devbox", []string{"sessions", "artifacts", "sessions"}); err != nil {
		t.Fatal(err)
	}
	topics, err := store.DesiredTopics("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(topics), "[artifacts sessions]"; got != want {
		t.Fatalf("desired topics = %s, want %s", got, want)
	}
	after, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if after.Freshness != before.Freshness || after.SessionFreshness != before.SessionFreshness ||
		after.ArtifactFreshness != before.ArtifactFreshness ||
		after.SessionInventory.SourceRevision != before.SessionInventory.SourceRevision ||
		after.ArtifactInventory.SourceRevision != before.ArtifactInventory.SourceRevision ||
		!after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("desired-topic RMW changed unrelated peer state\nbefore=%+v\nafter=%+v", before, after)
	}

	if err := store.MarkPeerSyncing("devbox", "boot-2"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	topics, err = reopened.DesiredTopics("devbox")
	if err != nil || fmt.Sprint(topics) != "[artifacts sessions]" {
		t.Fatalf("desired topics after mark/restart = %v err=%v", topics, err)
	}
	if err := reopened.SetDesiredTopics("devbox", nil); err != nil {
		t.Fatal(err)
	}
	topics, err = reopened.DesiredTopics("devbox")
	if err != nil || len(topics) != 0 {
		t.Fatalf("cleared desired topics = %v err=%v", topics, err)
	}

	if err := reopened.SetDesiredTopics("devbox", []string{"bad\x1btopic"}); err == nil {
		t.Fatal("desired topic accepted terminal control character")
	}
	tooMany := make([]string, maxDesiredTopics+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("topic-%d", i)
	}
	if err := reopened.SetDesiredTopics("devbox", tooMany); err == nil {
		t.Fatal("unbounded desired topics accepted")
	}
}

func TestPeerWideSyncWaitsForBothInventoryCommits(t *testing.T) {
	store := openTestStore(t)
	target := locatorFor("devbox", RootProfileScope, "target")
	commitSessionInventory(t, store, "devbox", "boot-1", 1, []ProfileScope{RootProfileScope}, target)
	commitArtifactInventory(t, store, "devbox", "boot-1", 1, testArtifact("goal", "goal"))
	if err := store.SetDesiredTopics("devbox", []string{"sessions", "artifacts"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPeerSyncing("devbox", "boot-2"); err != nil {
		t.Fatal(err)
	}
	commitSessionInventory(t, store, "devbox", "boot-2", 1, []ProfileScope{RootProfileScope}, target)
	peer, err := store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if peer.Freshness != FreshnessSyncing || peer.SessionFreshness != FreshnessFresh || peer.ArtifactFreshness != FreshnessSyncing {
		t.Fatalf("peer became fresh before artifact commit: %+v", peer)
	}
	if got, err := store.DesiredTopics("devbox"); err != nil || fmt.Sprint(got) != "[artifacts sessions]" {
		t.Fatalf("sync changed desired topics = %v err=%v", got, err)
	}
	commitArtifactInventory(t, store, "devbox", "boot-2", 1, testArtifact("goal", "goal-v2"))
	peer, err = store.GetPeer("devbox")
	if err != nil {
		t.Fatal(err)
	}
	if peer.Freshness != FreshnessFresh || peer.SessionFreshness != FreshnessFresh || peer.ArtifactFreshness != FreshnessFresh {
		t.Fatalf("peer not fresh after both commits: %+v", peer)
	}
}

func TestLegacyLocalArtifactIsNormalizedOnRead(t *testing.T) {
	store := openTestStore(t)
	received := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	file, err := store.artifactPath(ArtifactGoal, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	err = store.writeJSONLocked(file, map[string]any{
		"schema_version": SchemaVersion,
		"id":             "legacy",
		"kind":           ArtifactGoal,
		"title":          "old shape",
		"provenance":     "legacy",
		"received_at":    received,
	})
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.GetArtifact(ArtifactGoal, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.SourceID != "legacy" || artifact.Freshness != FreshnessFresh || !artifact.FreshnessChangedAt.Equal(received) {
		t.Fatalf("normalized legacy artifact = %+v", artifact)
	}
}
