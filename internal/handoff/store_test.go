package handoff

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "handoff-state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func TestStorePermissionsAndSymlinkRejection(t *testing.T) {
	store, root := openTestStore(t)
	record, _, err := store.QueueSource(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "handoffs"), filepath.Join(root, "handoffs", "source"), filepath.Join(root, "handoffs", "target")} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s mode = %v", dir, info.Mode())
		}
	}
	file := filepath.Join(root, "handoffs", "source", record.Manifest.HandoffID+".json")
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %v", info.Mode())
	}

	outside := filepath.Join(t.TempDir(), "outside.json")
	manifest := testManifest()
	manifest.HandoffID = "symlink-file"
	symlinkFile := filepath.Join(root, "handoffs", "source", manifest.HandoffID+".json")
	if err := os.Symlink(outside, symlinkFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueSource(manifest); err == nil {
		t.Fatal("store followed a record symlink")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside file was written: %v", err)
	}

	targetDir := filepath.Join(root, "handoffs", "target")
	if err := os.Remove(targetDir); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, targetDir); err != nil {
		t.Fatal(err)
	}
	manifest.HandoffID = "symlink-dir"
	if _, _, err := store.ReceiveTarget(manifest); err == nil {
		t.Fatal("store followed a directory symlink")
	}
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside directory received files: %v", entries)
	}
}

func TestOpenRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestManifestReplayAndConflict(t *testing.T) {
	store, _ := openTestStore(t)
	manifest := testManifest()
	first, replay, err := store.QueueSource(manifest)
	if err != nil || replay {
		t.Fatalf("first queue record=%+v replay=%v err=%v", first, replay, err)
	}
	second, replay, err := store.QueueSource(manifest)
	if err != nil || !replay || second.Revision != first.Revision || second.Digest != first.Digest {
		t.Fatalf("source replay record=%+v replay=%v err=%v", second, replay, err)
	}
	manifest.Goal.Text = "A conflicting next goal."
	if _, _, err := store.QueueSource(manifest); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("source conflict error = %v", err)
	}

	manifest = testManifest()
	firstTarget, replay, err := store.ReceiveTarget(manifest)
	if err != nil || replay {
		t.Fatalf("first target receive record=%+v replay=%v err=%v", firstTarget, replay, err)
	}
	_, replay, err = store.ReceiveTarget(manifest)
	if err != nil || !replay {
		t.Fatalf("target replay replay=%v err=%v", replay, err)
	}
	manifest.Goal.Text = "Conflicting goal."
	if _, _, err := store.ReceiveTarget(manifest); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("target conflict error = %v", err)
	}
}

func TestStoredManifestIsImmutableFromCallerMutations(t *testing.T) {
	store, _ := openTestStore(t)
	manifest := testManifest()
	originalArtifactID := manifest.Artifacts[0].ID
	originalRepo := manifest.Artifacts[0].Repo.Repo
	record, _, err := store.QueueSource(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Artifacts[0].ID = "caller-mutated"
	manifest.Artifacts[0].Repo.Repo = "other/repo"
	record.Manifest.Artifacts[0].ID = "return-mutated"
	record.Manifest.Artifacts[0].Repo.Repo = "return/repo"

	persisted, err := store.GetSource(record.Manifest.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Manifest.Artifacts[0].ID != originalArtifactID || persisted.Manifest.Artifacts[0].Repo.Repo != originalRepo {
		t.Fatalf("stored manifest was mutated: %+v", persisted.Manifest.Artifacts[0])
	}
}

func TestSourceTransitionsUseCASAndPreserveManifest(t *testing.T) {
	store, _ := openTestStore(t)
	start := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	queued, _, err := store.QueueSource(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	digest := queued.Digest
	preparing, err := store.TransitionSource(queued.Manifest.HandoffID, queued.Revision, SourcePreparingRemote, Transition{At: start.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if preparing.Attempts != 1 || preparing.Revision != 2 || preparing.Digest != digest {
		t.Fatalf("preparing record = %+v", preparing)
	}
	if _, err := store.TransitionSource(queued.Manifest.HandoffID, queued.Revision, SourceRemotePrepared, Transition{At: start.Add(2 * time.Second)}); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	if _, err := store.TransitionSource(queued.Manifest.HandoffID, preparing.Revision, SourceAccepted, Transition{At: start.Add(2 * time.Second)}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("illegal transition error = %v", err)
	}
	prepared, err := store.TransitionSource(queued.Manifest.HandoffID, preparing.Revision, SourceRemotePrepared, Transition{At: start.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	launching, err := store.TransitionSource(queued.Manifest.HandoffID, prepared.Revision, SourceLaunchingRemote, Transition{At: start.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	locator := testTargetLocator()
	accepted, err := store.TransitionSource(queued.Manifest.HandoffID, launching.Revision, SourceAccepted, Transition{At: start.Add(4 * time.Second), TargetLocator: &locator})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != SourceAccepted || accepted.TargetLocator == nil || accepted.Manifest.Goal.Text != queued.Manifest.Goal.Text {
		t.Fatalf("accepted record = %+v", accepted)
	}
}

func TestTargetTransitionsLaunchToAccepted(t *testing.T) {
	store, _ := openTestStore(t)
	start := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	received, _, err := store.ReceiveTarget(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	validating, err := store.TransitionTarget(received.Manifest.HandoffID, received.Revision, TargetValidating, Transition{At: start.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if validating.Attempts != 1 {
		t.Fatalf("validating attempts = %d", validating.Attempts)
	}
	prepared, err := store.TransitionTarget(received.Manifest.HandoffID, validating.Revision, TargetPrepared, Transition{At: start.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	locator := testTargetLocator()
	launching, err := store.TransitionTarget(received.Manifest.HandoffID, prepared.Revision, TargetLaunching, Transition{At: start.Add(3 * time.Second), TargetLocator: &locator})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := store.TransitionTarget(received.Manifest.HandoffID, launching.Revision, TargetAccepted, Transition{At: start.Add(4 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.TargetLocator == nil || accepted.TargetLocator.SessionID != locator.SessionID {
		t.Fatalf("target locator was not preserved: %+v", accepted)
	}
	if accepted.TargetLocator.ProfileScope != "root" || accepted.Manifest.Target.Profile != "codex" {
		t.Fatalf("session scope was incorrectly coupled to requested agent profile: %+v", accepted)
	}
	if _, err := store.TransitionTarget(received.Manifest.HandoffID, accepted.Revision, TargetLaunching, Transition{At: start.Add(5 * time.Second)}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("accepted state was not terminal: %v", err)
	}
}

func TestTargetLocatorLifecycleIsStateBound(t *testing.T) {
	store, _ := openTestStore(t)
	start := time.Date(2026, 7, 15, 1, 30, 0, 0, time.UTC)
	store.now = func() time.Time { return start }

	source, _, err := store.QueueSource(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	locator := testTargetLocator()
	failure := Failure{Code: FailureInternal, Message: "stopped", At: start.Add(time.Second)}
	if _, err := store.TransitionSource(source.Manifest.HandoffID, source.Revision, SourceFailed, Transition{
		At: start.Add(time.Second), Failure: &failure, TargetLocator: &locator,
	}); err == nil {
		t.Fatal("source failure accepted a target locator")
	}
	unchanged, err := store.GetSource(source.Manifest.HandoffID)
	if err != nil || unchanged.State != SourceQueued || unchanged.TargetLocator != nil {
		t.Fatalf("rejected source transition mutated record: %+v err=%v", unchanged, err)
	}

	target, _, err := store.ReceiveTarget(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	validating, _ := store.TransitionTarget(target.Manifest.HandoffID, target.Revision, TargetValidating, Transition{At: start.Add(time.Second)})
	prepared, _ := store.TransitionTarget(target.Manifest.HandoffID, validating.Revision, TargetPrepared, Transition{At: start.Add(2 * time.Second)})
	launching, err := store.TransitionTarget(target.Manifest.HandoffID, prepared.Revision, TargetLaunching, Transition{
		At: start.Add(3 * time.Second), TargetLocator: &locator,
	})
	if err != nil {
		t.Fatal(err)
	}
	retryAt := start.Add(time.Minute)
	retryFailure := Failure{Code: FailureUnavailable, Message: "launch interrupted", Retryable: true, At: start.Add(4 * time.Second)}
	if _, err := store.TransitionTarget(target.Manifest.HandoffID, launching.Revision, TargetWaitingAssets, Transition{
		At: start.Add(4 * time.Second), NextRetry: &retryAt, Failure: &retryFailure, TargetLocator: &locator,
	}); err == nil {
		t.Fatal("retry transition accepted a supplied target locator")
	}
	waiting, err := store.TransitionTarget(target.Manifest.HandoffID, launching.Revision, TargetWaitingAssets, Transition{
		At: start.Add(4 * time.Second), NextRetry: &retryAt, Failure: &retryFailure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.TargetLocator != nil {
		t.Fatalf("retry retained target locator: %+v", waiting.TargetLocator)
	}
	validating, _ = store.TransitionTarget(target.Manifest.HandoffID, waiting.Revision, TargetValidating, Transition{At: retryAt})
	prepared, _ = store.TransitionTarget(target.Manifest.HandoffID, validating.Revision, TargetPrepared, Transition{At: retryAt.Add(time.Second)})
	launching, _ = store.TransitionTarget(target.Manifest.HandoffID, prepared.Revision, TargetLaunching, Transition{
		At: retryAt.Add(2 * time.Second), TargetLocator: &locator,
	})
	rejectedFailure := Failure{Code: FailureLaunch, Message: "launch rejected", At: retryAt.Add(3 * time.Second)}
	if _, err := store.TransitionTarget(target.Manifest.HandoffID, launching.Revision, TargetRejected, Transition{
		At: retryAt.Add(3 * time.Second), Failure: &rejectedFailure, TargetLocator: &locator,
	}); err == nil {
		t.Fatal("rejection transition accepted a supplied target locator")
	}
	rejected, err := store.TransitionTarget(target.Manifest.HandoffID, launching.Revision, TargetRejected, Transition{
		At: retryAt.Add(3 * time.Second), Failure: &rejectedFailure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.TargetLocator != nil {
		t.Fatalf("rejection retained target locator: %+v", rejected.TargetLocator)
	}
}

func TestTargetLocatorRejectsInvalidScopeAndWrongDevice(t *testing.T) {
	for _, locator := range []TargetLocator{
		{DeviceID: "devbox", ProfileScope: "codex", SessionID: "target-session"},
		{DeviceID: "labs", ProfileScope: "root", SessionID: "target-session"},
	} {
		store, _ := openTestStore(t)
		start := time.Date(2026, 7, 15, 1, 45, 0, 0, time.UTC)
		store.now = func() time.Time { return start }
		received, _, _ := store.ReceiveTarget(testManifest())
		validating, _ := store.TransitionTarget(received.Manifest.HandoffID, received.Revision, TargetValidating, Transition{At: start.Add(time.Second)})
		prepared, _ := store.TransitionTarget(received.Manifest.HandoffID, validating.Revision, TargetPrepared, Transition{At: start.Add(2 * time.Second)})
		if _, err := store.TransitionTarget(received.Manifest.HandoffID, prepared.Revision, TargetLaunching, Transition{
			At: start.Add(3 * time.Second), TargetLocator: &locator,
		}); err == nil {
			t.Fatalf("invalid locator accepted: %+v", locator)
		}
	}
}

func TestRetryDueAndRestart(t *testing.T) {
	store, root := openTestStore(t)
	start := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	queued, _, err := store.QueueSource(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	preparing, err := store.TransitionSource(queued.Manifest.HandoffID, queued.Revision, SourcePreparingRemote, Transition{At: start.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	retryAt := start.Add(time.Minute)
	failure := Failure{Code: FailureUnavailable, Message: "peer is offline", Retryable: true, At: start.Add(2 * time.Second)}
	waiting, err := store.TransitionSource(queued.Manifest.HandoffID, preparing.Revision, SourceRetryWait, Transition{
		At: start.Add(2 * time.Second), NextRetry: &retryAt, Failure: &failure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.State != SourceRetryWait {
		t.Fatalf("waiting state = %s", waiting.State)
	}
	if due, err := store.DueSource(retryAt.Add(-time.Nanosecond)); err != nil || len(due) != 0 {
		t.Fatalf("early due=%+v err=%v", due, err)
	}
	if due, err := store.DueSource(retryAt); err != nil || len(due) != 1 {
		t.Fatalf("due=%+v err=%v", due, err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.GetSource(queued.Manifest.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != SourceRetryWait || persisted.Attempts != 1 || persisted.NextRetry == nil || !persisted.NextRetry.Equal(retryAt) {
		t.Fatalf("persisted retry = %+v", persisted)
	}
	retried, err := reopened.TransitionSource(persisted.Manifest.HandoffID, persisted.Revision, SourcePreparingRemote, Transition{At: retryAt})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Attempts != 2 || retried.NextRetry != nil || retried.Failure != nil {
		t.Fatalf("retried source = %+v", retried)
	}
}

func TestTargetWaitingAssetsIsDueAndPersists(t *testing.T) {
	store, root := openTestStore(t)
	start := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	received, _, err := store.ReceiveTarget(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	validating, err := store.TransitionTarget(received.Manifest.HandoffID, received.Revision, TargetValidating, Transition{At: start.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	retryAt := start.Add(time.Minute)
	failure := Failure{Code: FailureMissingAsset, Message: "history unavailable", Retryable: true, At: start.Add(2 * time.Second)}
	waiting, err := store.TransitionTarget(received.Manifest.HandoffID, validating.Revision, TargetWaitingAssets, Transition{
		At: start.Add(2 * time.Second), NextRetry: &retryAt, Failure: &failure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.State != TargetWaitingAssets {
		t.Fatalf("waiting state = %s", waiting.State)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if due, err := reopened.DueTarget(retryAt); err != nil || len(due) != 1 || due[0].Manifest.HandoffID != waiting.Manifest.HandoffID {
		t.Fatalf("target due=%+v err=%v", due, err)
	}
}

func TestListRecordsIsDeterministic(t *testing.T) {
	store, _ := openTestStore(t)
	for _, id := range []string{"handoff-z", "handoff-a", "handoff-m"} {
		manifest := testManifest()
		manifest.HandoffID = id
		if _, _, err := store.QueueSource(manifest); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.ListSource()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"handoff-a", "handoff-m", "handoff-z"} {
		if records[i].Manifest.HandoffID != want {
			t.Fatalf("record %d = %q, want %q", i, records[i].Manifest.HandoffID, want)
		}
	}
}

func TestRunnableSourceAfterRestart(t *testing.T) {
	store, root := openTestStore(t)
	start := time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	dueAt := start.Add(time.Hour)
	for _, state := range []SourceState{
		SourceQueued, SourcePreparingRemote, SourceRemotePrepared, SourceLaunchingRemote,
		SourceAccepted, SourceRetryWait, SourceFailed,
	} {
		id := "source-" + string(state)
		retryAt := dueAt.Add(-time.Minute)
		createSourceInState(t, store, id, state, start, retryAt)
	}
	createSourceInState(t, store, "source-retry-future", SourceRetryWait, start, dueAt.Add(time.Minute))

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := reopened.RunnableSource(dueAt)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"source-launching_remote", "source-preparing_remote", "source-queued", "source-retry_wait"}
	if got := sourceIDs(records); !reflect.DeepEqual(got, want) {
		t.Fatalf("runnable source ids = %v, want %v", got, want)
	}
}

func TestRecoverableTargetAfterRestart(t *testing.T) {
	store, root := openTestStore(t)
	start := time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	dueAt := start.Add(time.Hour)
	for _, state := range []TargetState{
		TargetReceived, TargetValidating, TargetPrepared, TargetLaunching,
		TargetAccepted, TargetWaitingAssets, TargetRejected,
	} {
		id := "target-" + string(state)
		retryAt := dueAt.Add(-time.Minute)
		createTargetInState(t, store, id, state, start, retryAt)
	}
	createTargetInState(t, store, "target-waiting-future", TargetWaitingAssets, start, dueAt.Add(time.Minute))

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := reopened.RecoverableTarget(dueAt)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"target-launching", "target-received", "target-validating", "target-waiting_assets"}
	if got := targetIDs(records); !reflect.DeepEqual(got, want) {
		t.Fatalf("recoverable target ids = %v, want %v", got, want)
	}
}

func createSourceInState(t *testing.T, store *Store, id string, state SourceState, start, retryAt time.Time) SourceRecord {
	t.Helper()
	manifest := testManifest()
	manifest.HandoffID = id
	record, _, err := store.QueueSource(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if state == SourceQueued {
		return record
	}
	if state == SourceFailed {
		failure := Failure{Code: FailureInternal, Message: "failed", At: start.Add(time.Second)}
		record, err = store.TransitionSource(id, record.Revision, state, Transition{At: start.Add(time.Second), Failure: &failure})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record, err = store.TransitionSource(id, record.Revision, SourcePreparingRemote, Transition{At: start.Add(time.Second)})
	if err != nil || state == SourcePreparingRemote {
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	if state == SourceRetryWait {
		failure := Failure{Code: FailureUnavailable, Message: "offline", Retryable: true, At: start.Add(2 * time.Second)}
		record, err = store.TransitionSource(id, record.Revision, state, Transition{At: start.Add(2 * time.Second), NextRetry: &retryAt, Failure: &failure})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record, err = store.TransitionSource(id, record.Revision, SourceRemotePrepared, Transition{At: start.Add(2 * time.Second)})
	if err != nil || state == SourceRemotePrepared {
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record, err = store.TransitionSource(id, record.Revision, SourceLaunchingRemote, Transition{At: start.Add(3 * time.Second)})
	if err != nil || state == SourceLaunchingRemote {
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	locator := testTargetLocator()
	record, err = store.TransitionSource(id, record.Revision, SourceAccepted, Transition{At: start.Add(4 * time.Second), TargetLocator: &locator})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func createTargetInState(t *testing.T, store *Store, id string, state TargetState, start, retryAt time.Time) TargetRecord {
	t.Helper()
	manifest := testManifest()
	manifest.HandoffID = id
	record, _, err := store.ReceiveTarget(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if state == TargetReceived {
		return record
	}
	if state == TargetRejected {
		failure := Failure{Code: FailureInternal, Message: "rejected", At: start.Add(time.Second)}
		record, err = store.TransitionTarget(id, record.Revision, state, Transition{At: start.Add(time.Second), Failure: &failure})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record, err = store.TransitionTarget(id, record.Revision, TargetValidating, Transition{At: start.Add(time.Second)})
	if err != nil || state == TargetValidating {
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	if state == TargetWaitingAssets {
		failure := Failure{Code: FailureMissingAsset, Message: "waiting", Retryable: true, At: start.Add(2 * time.Second)}
		record, err = store.TransitionTarget(id, record.Revision, state, Transition{At: start.Add(2 * time.Second), NextRetry: &retryAt, Failure: &failure})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record, err = store.TransitionTarget(id, record.Revision, TargetPrepared, Transition{At: start.Add(2 * time.Second)})
	if err != nil || state == TargetPrepared {
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record, err = store.TransitionTarget(id, record.Revision, TargetLaunching, Transition{At: start.Add(3 * time.Second)})
	if err != nil || state == TargetLaunching {
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	locator := testTargetLocator()
	record, err = store.TransitionTarget(id, record.Revision, TargetAccepted, Transition{At: start.Add(4 * time.Second), TargetLocator: &locator})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func sourceIDs(records []SourceRecord) []string {
	ids := make([]string, len(records))
	for i, record := range records {
		ids[i] = record.Manifest.HandoffID
	}
	return ids
}

func targetIDs(records []TargetRecord) []string {
	ids := make([]string, len(records))
	for i, record := range records {
		ids[i] = record.Manifest.HandoffID
	}
	return ids
}

func testTargetLocator() TargetLocator {
	return TargetLocator{DeviceID: "devbox", ProfileScope: "root", SessionID: "target-session-1"}
}
