package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestHandoffPrepareBindsAuthenticatedSourceAndLocalTargetBeforePersist(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)

	if _, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "impostor"}, "server", meshHandoffPrepareRequest{Manifest: manifest}); !isMeshPermissionDenied(err) {
		t.Fatalf("source mismatch error = %v, want permission_denied", err)
	}
	if _, err := app.store.GetTarget(manifest.HandoffID); !errors.Is(err, handoff.ErrNotFound) {
		t.Fatalf("source mismatch persisted target record: %v", err)
	}

	if _, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "other-server", meshHandoffPrepareRequest{Manifest: manifest}); !isMeshInvalidRequest(err) {
		t.Fatalf("target mismatch error = %v, want invalid_request", err)
	}
	if _, err := app.store.GetTarget(manifest.HandoffID); !errors.Is(err, handoff.ErrNotFound) {
		t.Fatalf("target mismatch persisted target record: %v", err)
	}
}

func TestHandoffPrepareRejectsArtifactsBeforePersistOrAssetSideEffects(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	manifest.Artifacts = []handoff.ArtifactRef{{
		Kind: handoff.ArtifactSessionHistory, ID: "history-1",
		Session: &handoff.ArtifactSessionRef{
			ProfileScope: "profile:codex",
			SessionID:    "session-1",
		},
	}}
	var historyCalls, repositoryCalls atomic.Int32
	app.snapshotHistory = func(string, string, string, handoff.HistoryRef) (string, error) {
		historyCalls.Add(1)
		return "", nil
	}
	app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
		repositoryCalls.Add(1)
		return nil
	}

	if _, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest}); !isMeshInvalidRequest(err) {
		t.Fatalf("prepare with artifacts error = %v, want invalid_request", err)
	}
	if _, err := app.store.GetTarget(manifest.HandoffID); !errors.Is(err, handoff.ErrNotFound) {
		t.Fatalf("unsupported artifacts persisted target record: %v", err)
	}
	if historyCalls.Load() != 0 || repositoryCalls.Load() != 0 {
		t.Fatalf("unsupported artifacts ran history=%d repository=%d side effects", historyCalls.Load(), repositoryCalls.Load())
	}
}

func TestHandoffResumeRejectsPersistedArtifactsBeforeAssetSideEffects(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	manifest.HandoffID = "persisted-unsupported-artifacts"
	manifest.Artifacts = []handoff.ArtifactRef{{
		Kind: handoff.ArtifactSessionHistory, ID: "history-1",
		Session: &handoff.ArtifactSessionRef{
			ProfileScope: "profile:codex",
			SessionID:    "session-1",
		},
	}}
	record, replay, err := app.store.ReceiveTarget(manifest)
	if err != nil || replay {
		t.Fatalf("receive replay=%t err=%v", replay, err)
	}
	var historyCalls, repositoryCalls atomic.Int32
	app.snapshotHistory = func(string, string, string, handoff.HistoryRef) (string, error) {
		historyCalls.Add(1)
		return "", nil
	}
	app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
		repositoryCalls.Add(1)
		return nil
	}

	release := app.lockPrepare(manifest.HandoffID)
	status, err := app.resumeTarget(context.Background(), record, "server")
	release()
	if err != nil || status.State != handoff.TargetRejected || status.Failure == nil ||
		status.Failure.Code != handoff.FailureInvalidManifest || status.Failure.Retryable {
		t.Fatalf("persisted artifact rejection status=%+v err=%v", status, err)
	}
	if historyCalls.Load() != 0 || repositoryCalls.Load() != 0 {
		t.Fatalf("persisted artifacts ran history=%d repository=%d side effects", historyCalls.Load(), repositoryCalls.Load())
	}
}

func TestHandoffPrepareRejectsUnavailableTargetProfileBeforeAssetSideEffects(t *testing.T) {
	for _, targetProfile := range []string{"unknown", "codex_exec"} {
		t.Run(targetProfile, func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			manifest.HandoffID = "handoff-profile-" + targetProfile
			manifest.Target.Profile = targetProfile
			var historyCalls, repositoryCalls atomic.Int32
			app.snapshotHistory = func(string, string, string, handoff.HistoryRef) (string, error) {
				historyCalls.Add(1)
				return "", nil
			}
			app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
				repositoryCalls.Add(1)
				return nil
			}

			status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if status.State != handoff.TargetRejected || status.Failure == nil ||
				status.Failure.Code != handoff.FailureVerification || status.Failure.Retryable {
				t.Fatalf("profile rejection = %+v", status)
			}
			if strings.Contains(status.Failure.Message, targetProfile) {
				t.Fatalf("safe failure leaked target profile: %+v", status.Failure)
			}
			if historyCalls.Load() != 0 || repositoryCalls.Load() != 0 {
				t.Fatalf("profile rejection ran history=%d repository=%d side effects", historyCalls.Load(), repositoryCalls.Load())
			}
		})
	}
}

func TestHandoffPrepareReplayConflictAndRedactedStatus(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	principal := arcmuxmesh.Principal{PeerID: "client"}

	first, err := app.prepare(context.Background(), principal, "server", meshHandoffPrepareRequest{Manifest: manifest})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if first.State != handoff.TargetPrepared || first.Attempts != 1 || first.ManifestDigest == "" {
		t.Fatalf("first status = %+v", first)
	}
	snapshot := filepath.Join(app.store.Root(), "handoff-"+manifest.HandoffID, "history.md")
	if info, err := os.Stat(snapshot); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("private history snapshot info=%v err=%v", info, err)
	}
	replay, err := app.prepare(context.Background(), principal, "server", meshHandoffPrepareRequest{Manifest: manifest})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay = %+v, want %+v", replay, first)
	}

	conflict := manifest
	conflict.Goal.Text = "A different handoff goal."
	conflict.Goal.UpdatedAt = conflict.Goal.UpdatedAt.Add(time.Second)
	if _, err := app.prepare(context.Background(), principal, "server", meshHandoffPrepareRequest{Manifest: conflict}); !isMeshInvalidRequest(err) {
		t.Fatalf("digest conflict error = %v, want invalid_request", err)
	}

	status, err := app.status(context.Background(), principal, meshHandoffStatusRequest{HandoffID: manifest.HandoffID})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"DO_NOT_LEAK_GOAL", manifest.History.Basename, manifest.Repository.ProjectSlug,
		manifest.Repository.RepoSlug, manifest.Repository.Branch, app.historyRoot, app.store.Root(),
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("redacted status leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestHandoffPrepareConcurrentDuplicateIsIdempotent(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	var repositoryCalls atomic.Int32
	app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
		repositoryCalls.Add(1)
		return nil
	}
	type result struct {
		status meshHandoffStatus
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
			results <- result{status: status, err: err}
		}()
	}
	close(start)
	for range 2 {
		got := <-results
		if got.err != nil || got.status.State != handoff.TargetPrepared || got.status.Attempts != 1 {
			t.Fatalf("concurrent result status=%+v err=%v", got.status, got.err)
		}
	}
	if got := repositoryCalls.Load(); got != 1 {
		t.Fatalf("repository preparations = %d, want 1", got)
	}
}

func TestHandoffPrepareStopsWaitingForPerIDLockWhenRequestIsCanceled(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	release := app.lockPrepare(manifest.HandoffID)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := app.prepare(ctx, arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked duplicate error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked duplicate took %s", elapsed)
	}
	if _, err := app.store.GetTarget(manifest.HandoffID); !errors.Is(err, handoff.ErrNotFound) {
		t.Fatalf("canceled lock waiter persisted target record: %v", err)
	}
}

func TestHandoffStatusIsOwnedByAuthenticatedSourcePeer(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	owner := arcmuxmesh.Principal{PeerID: "client"}
	if _, err := app.prepare(context.Background(), owner, "server", meshHandoffPrepareRequest{Manifest: manifest}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	request := meshHandoffStatusRequest{HandoffID: manifest.HandoffID}
	if _, err := app.status(context.Background(), owner, request); err != nil {
		t.Fatalf("owner status: %v", err)
	}
	otherPeer := arcmuxmesh.Principal{PeerID: "another-client"}
	if _, err := app.status(context.Background(), otherPeer, request); !isMeshPermissionDenied(err) {
		t.Fatalf("other peer status error = %v, want permission_denied", err)
	}
}

func TestHandoffPrepareMissingHistoryWaitsAndInvalidProjectRejects(t *testing.T) {
	t.Run("missing synced history", func(t *testing.T) {
		app, manifest := newHandoffTestApplication(t, false)
		calledRepository := false
		app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
			calledRepository = true
			return nil
		}
		status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if status.State != handoff.TargetWaitingAssets || status.Attempts != 1 || status.Failure == nil ||
			status.Failure.Code != handoff.FailureMissingAsset || !status.Failure.Retryable {
			t.Fatalf("waiting status = %+v", status)
		}
		if calledRepository {
			t.Fatal("repository preparation ran before history became available")
		}
		content := []byte("# Synced session history\n")
		if err := os.WriteFile(filepath.Join(app.historyRoot, manifest.History.Basename), content, 0o600); err != nil {
			t.Fatal(err)
		}
		status, err = app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
		if err != nil {
			t.Fatalf("retry prepare: %v", err)
		}
		if status.State != handoff.TargetPrepared || status.Attempts != 2 || !calledRepository {
			t.Fatalf("retried status = %+v repository_called=%t", status, calledRepository)
		}
	})

	t.Run("unregistered project", func(t *testing.T) {
		app, manifest := newHandoffTestApplication(t, true)
		manifest.Repository.ProjectSlug = "unknown-project"
		status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if status.State != handoff.TargetRejected || status.Failure == nil || status.Failure.Retryable ||
			status.Failure.Code != handoff.FailureVerification {
			t.Fatalf("rejected status = %+v", status)
		}
		replay, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
		if err != nil {
			t.Fatalf("rejected replay: %v", err)
		}
		if !reflect.DeepEqual(replay, status) {
			t.Fatalf("rejected replay = %+v, want terminal %+v", replay, status)
		}
	})
}

func TestHandoffPrepareResumesPersistedReceivedAndValidating(t *testing.T) {
	for _, state := range []handoff.TargetState{handoff.TargetReceived, handoff.TargetValidating} {
		t.Run(string(state), func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			record, replay, err := app.store.ReceiveTarget(manifest)
			if err != nil || replay {
				t.Fatalf("receive target record=%+v replay=%t err=%v", record, replay, err)
			}
			if state == handoff.TargetValidating {
				record, err = app.store.TransitionTarget(manifest.HandoffID, record.Revision, handoff.TargetValidating, handoff.Transition{})
				if err != nil {
					t.Fatalf("persist validating: %v", err)
				}
			}
			status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
			if err != nil {
				t.Fatalf("resume prepare: %v", err)
			}
			if status.State != handoff.TargetPrepared || status.Attempts != 1 {
				t.Fatalf("resumed status = %+v", status)
			}
		})
	}
}

func TestTargetHandoffReconcilerRecoversPrepareStatesAndHonorsRetryDueTime(t *testing.T) {
	seed, base := newHandoffTestApplication(t, true)
	seedRecord := func(id string, state handoff.TargetState, retryDelay time.Duration) handoff.TargetRecord {
		t.Helper()
		manifest := base
		manifest.HandoffID = id
		record, replay, err := seed.store.ReceiveTarget(manifest)
		if err != nil || replay {
			t.Fatalf("seed %s receive record=%+v replay=%t err=%v", id, record, replay, err)
		}
		if state == handoff.TargetReceived {
			return record
		}
		record, err = seed.store.TransitionTarget(id, record.Revision, handoff.TargetValidating, handoff.Transition{})
		if err != nil {
			t.Fatalf("seed %s validating: %v", id, err)
		}
		switch state {
		case handoff.TargetValidating:
			return record
		case handoff.TargetWaitingAssets:
			next := record.Updated.Add(retryDelay)
			failure := &handoff.Failure{
				Code: handoff.FailureMissingAsset, Message: "history not synced", Retryable: true, At: record.Updated,
			}
			record, err = seed.store.TransitionTarget(id, record.Revision, state, handoff.Transition{
				At: record.Updated, NextRetry: &next, Failure: failure,
			})
		case handoff.TargetRejected:
			failure := &handoff.Failure{Code: handoff.FailureVerification, Message: "terminal rejection", At: record.Updated}
			record, err = seed.store.TransitionTarget(id, record.Revision, state, handoff.Transition{At: record.Updated, Failure: failure})
		default:
			t.Fatalf("unsupported seed state %s", state)
		}
		if err != nil {
			t.Fatalf("seed %s %s: %v", id, state, err)
		}
		return record
	}

	received := seedRecord("recovery-received", handoff.TargetReceived, 0)
	validating := seedRecord("recovery-validating", handoff.TargetValidating, 0)
	due := seedRecord("recovery-due", handoff.TargetWaitingAssets, time.Second)
	future := seedRecord("recovery-future", handoff.TargetWaitingAssets, time.Hour)
	rejected := seedRecord("recovery-rejected", handoff.TargetRejected, 0)
	wrongDeviceManifest := base
	wrongDeviceManifest.HandoffID = "recovery-wrong-device"
	wrongDeviceManifest.Target.DeviceID = "other-server"
	wrongDevice, replay, err := seed.store.ReceiveTarget(wrongDeviceManifest)
	if err != nil || replay {
		t.Fatalf("seed wrong-device record=%+v replay=%t err=%v", wrongDevice, replay, err)
	}

	reopened, err := handoff.Open(seed.store.Root())
	if err != nil {
		t.Fatalf("reopen handoff store: %v", err)
	}
	recovered := newHandoffApplication(reopened, map[string]profile.Profile{
		"codex": {Transport: profile.TransportTmux, StartCommand: "codex"},
	})
	recovered.historyRoot = seed.historyRoot
	recovered.projectsPath = seed.projectsPath
	var repositoryCalls atomic.Int32
	recovered.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
		repositoryCalls.Add(1)
		return nil
	}
	d := newMeshApplicationTestDaemon(t, "server")
	d.meshMu.Lock()
	d.meshApp.handoffs = recovered
	d.meshMu.Unlock()

	firstPassAt := due.NextRetry.Add(time.Second)
	if !firstPassAt.Before(*future.NextRetry) {
		t.Fatal("test retry times do not separate due and future records")
	}
	d.reconcileTargetHandoffs(context.Background(), firstPassAt)
	for _, record := range []handoff.TargetRecord{received, validating, due} {
		got, err := reopened.GetTarget(record.Manifest.HandoffID)
		if err != nil || got.State != handoff.TargetPrepared {
			t.Fatalf("recovered %s = %+v err=%v", record.Manifest.HandoffID, got, err)
		}
	}
	gotFuture, err := reopened.GetTarget(future.Manifest.HandoffID)
	if err != nil || gotFuture.State != handoff.TargetWaitingAssets || gotFuture.Attempts != 1 {
		t.Fatalf("future retry changed early: %+v err=%v", gotFuture, err)
	}
	gotRejected, err := reopened.GetTarget(rejected.Manifest.HandoffID)
	if err != nil || gotRejected.State != handoff.TargetRejected {
		t.Fatalf("terminal rejection changed: %+v err=%v", gotRejected, err)
	}
	gotWrongDevice, err := reopened.GetTarget(wrongDevice.Manifest.HandoffID)
	if err != nil || gotWrongDevice.State != handoff.TargetRejected || gotWrongDevice.Failure == nil ||
		gotWrongDevice.Failure.Code != handoff.FailureVerification {
		t.Fatalf("wrong-device recovery = %+v err=%v", gotWrongDevice, err)
	}
	if got := repositoryCalls.Load(); got != 3 {
		t.Fatalf("first pass repository calls = %d, want 3", got)
	}

	d.reconcileTargetHandoffs(context.Background(), future.NextRetry.Add(time.Second))
	gotFuture, err = reopened.GetTarget(future.Manifest.HandoffID)
	if err != nil || gotFuture.State != handoff.TargetPrepared || gotFuture.Attempts != 2 {
		t.Fatalf("due retry did not prepare: %+v err=%v", gotFuture, err)
	}
	if got := repositoryCalls.Load(); got != 4 {
		t.Fatalf("second pass repository calls = %d, want 4", got)
	}

	// Prepared/rejected records are inert on later reconciliation passes.
	d.reconcileTargetHandoffs(context.Background(), future.NextRetry.Add(2*time.Second))
	if got := repositoryCalls.Load(); got != 4 {
		t.Fatalf("terminal pass repository calls = %d, want 4", got)
	}
}

func TestTargetHandoffReconcilerTimesOutBlockedRecordAndDrainsNext(t *testing.T) {
	app, base := newHandoffTestApplication(t, true)
	blocked := base
	blocked.HandoffID = "reconcile-timeout-a"
	next := base
	next.HandoffID = "reconcile-timeout-b"
	for _, manifest := range []handoff.Manifest{blocked, next} {
		if _, replay, err := app.store.ReceiveTarget(manifest); err != nil || replay {
			t.Fatalf("seed %s replay=%t err=%v", manifest.HandoffID, replay, err)
		}
	}
	app.resumeTimeout = 25 * time.Millisecond

	releaseBlocked := make(chan struct{})
	blockedStarted := make(chan struct{})
	var startOnce sync.Once
	app.snapshotHistory = func(_, _, id string, _ handoff.HistoryRef) (string, error) {
		if id == blocked.HandoffID {
			startOnce.Do(func() { close(blockedStarted) })
			<-releaseBlocked
		}
		return "snapshot", nil
	}
	var repositoryCalls atomic.Int32
	app.prepareRepository = func(_ context.Context, manifest handoff.Manifest, _ project.ResolvedProject) error {
		repositoryCalls.Add(1)
		if manifest.HandoffID == blocked.HandoffID {
			t.Error("timed-out record reached repository preparation")
		}
		return nil
	}

	d := newMeshApplicationTestDaemon(t, "server")
	d.meshMu.Lock()
	d.meshApp.handoffs = app
	d.meshMu.Unlock()
	done := make(chan struct{})
	go func() {
		d.reconcileTargetHandoffs(context.Background(), time.Now().UTC())
		close(done)
	}()
	select {
	case <-blockedStarted:
	case <-time.After(time.Second):
		close(releaseBlocked)
		<-done
		t.Fatal("blocked recovery did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		close(releaseBlocked)
		<-done
		t.Fatal("blocked recovery starved the next target")
	}
	close(releaseBlocked)
	// Wait for the timed-out goroutine to observe cancellation and release its
	// per-ID lock before test cleanup removes its store and history roots.
	release := app.lockPrepare(blocked.HandoffID)
	release()

	blockedRecord, err := app.store.GetTarget(blocked.HandoffID)
	if err != nil || blockedRecord.State != handoff.TargetValidating {
		t.Fatalf("blocked record=%+v err=%v, want validating", blockedRecord, err)
	}
	nextRecord, err := app.store.GetTarget(next.HandoffID)
	if err != nil || nextRecord.State != handoff.TargetPrepared {
		t.Fatalf("next record=%+v err=%v, want prepared", nextRecord, err)
	}
	if got := repositoryCalls.Load(); got != 1 {
		t.Fatalf("repository calls = %d, want 1", got)
	}
}

func TestTargetLaunchReconcilerTimesOutBlockedRecordAndDrainsNext(t *testing.T) {
	app, base := newHandoffTestApplication(t, true)
	blocked := base
	blocked.HandoffID = "launch-timeout-a"
	next := base
	next.HandoffID = "launch-timeout-b"
	for _, manifest := range []handoff.Manifest{blocked, next} {
		prepared := prepareTargetHandoffForLaunch(t, app, manifest)
		if _, err := app.store.TransitionTarget(manifest.HandoffID, prepared.Revision, handoff.TargetLaunching, handoff.Transition{}); err != nil {
			t.Fatal(err)
		}
	}
	// Leave enough budget for the next record's fsync-backed launch under the
	// race detector while keeping the first deliberately blocked record
	// bounded. The test observes the completed reconciliation pass, not the
	// earlier locator-persistence midpoint.
	app.resumeTimeout = 500 * time.Millisecond
	app.launchPoll = time.Millisecond

	releaseBlocked := make(chan struct{})
	blockedStarted := make(chan struct{})
	var startOnce sync.Once
	worktrees := t.TempDir()
	app.prepareLaunchRepo = func(_ context.Context, manifest handoff.Manifest, _ project.ResolvedProject) (handoff.RepositoryPreparation, error) {
		if manifest.HandoffID == blocked.HandoffID {
			startOnce.Do(func() { close(blockedStarted) })
			<-releaseBlocked
		}
		path := filepath.Join(worktrees, manifest.HandoffID)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return handoff.RepositoryPreparation{}, err
		}
		return handoff.RepositoryPreparation{WorktreePath: path, Head: manifest.Repository.SourceHead, LocalBranch: manifest.Repository.Branch, SourceBranch: manifest.Repository.Branch}, nil
	}
	sessions := make(map[string]*session.Session)
	var sessionsMu sync.Mutex
	app.createSession = func(_ context.Context, request CreateSessionRequest) (*session.Session, bool, error) {
		sess := session.NewSession("target-"+request.Name, request.Name, request.Agent, request.CWD)
		sess.SetOwnerID(request.OwnerID)
		sess.SetEnv(request.Env)
		if request.private {
			sess.MarkPrivate()
		}
		sess.SetState(session.StateIdle)
		sessionsMu.Lock()
		sessions[sess.Snapshot().ID] = sess
		sessionsMu.Unlock()
		return sess, true, nil
	}
	app.lookupSession = func(id string) (*session.Session, bool) {
		sessionsMu.Lock()
		defer sessionsMu.Unlock()
		sess, ok := sessions[id]
		return sess, ok
	}
	app.sendPrompt = func(_ context.Context, id, _ string, _, _ bool) error {
		sessionsMu.Lock()
		sess := sessions[id]
		sessionsMu.Unlock()
		for _, handoffID := range []string{blocked.HandoffID, next.HandoffID} {
			record, err := app.store.GetTarget(handoffID)
			if err == nil && record.TargetLocator != nil && record.TargetLocator.SessionID == id {
				sess.SetCurrentCommand(handoffLaunchCurrentCommand(record))
				sess.SetState(session.StateWorking)
				return nil
			}
		}
		return errors.New("session locator unavailable")
	}
	app.persistSessions = func() error { return nil }

	d := newMeshApplicationTestDaemon(t, "server")
	d.meshMu.Lock()
	d.meshApp.handoffs = app
	d.meshMu.Unlock()
	done := make(chan struct{})
	go func() {
		d.reconcileTargetHandoffs(context.Background(), time.Now().UTC())
		close(done)
	}()
	select {
	case <-blockedStarted:
	case <-time.After(3 * time.Second):
		close(releaseBlocked)
		<-done
		t.Fatal("blocked launch recovery did not start")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(releaseBlocked)
		<-done
		t.Fatal("blocked launch recovery starved the next target")
	}
	close(releaseBlocked)
	release := app.lockPrepare(blocked.HandoffID)
	release()
	blockedRecord, err := app.store.GetTarget(blocked.HandoffID)
	if err != nil || blockedRecord.State != handoff.TargetLaunching || blockedRecord.TargetLocator != nil {
		t.Fatalf("blocked launch record=%+v err=%v", blockedRecord, err)
	}
	nextRecord, err := app.store.GetTarget(next.HandoffID)
	if err != nil || nextRecord.State != handoff.TargetAccepted || nextRecord.TargetLocator == nil {
		t.Fatalf("next launch record=%+v err=%v", nextRecord, err)
	}
}

func TestTargetTransitionsClampBackwardWallClock(t *testing.T) {
	t.Run("received validating and waiting assets resume", func(t *testing.T) {
		for _, state := range []handoff.TargetState{
			handoff.TargetReceived,
			handoff.TargetValidating,
			handoff.TargetWaitingAssets,
		} {
			t.Run(string(state), func(t *testing.T) {
				app, manifest := newHandoffTestApplication(t, true)
				manifest.HandoffID = "clock-backward-" + string(state)
				record, replay, err := app.store.ReceiveTarget(manifest)
				if err != nil || replay {
					t.Fatalf("receive replay=%t err=%v", replay, err)
				}
				if state != handoff.TargetReceived {
					record, err = app.store.TransitionTarget(manifest.HandoffID, record.Revision, handoff.TargetValidating, handoff.Transition{At: record.Updated})
					if err != nil {
						t.Fatalf("seed validating: %v", err)
					}
				}
				if state == handoff.TargetWaitingAssets {
					nextRetry := record.Updated.Add(time.Second)
					failure := &handoff.Failure{
						Code: handoff.FailureMissingAsset, Message: "pending", Retryable: true, At: record.Updated,
					}
					record, err = app.store.TransitionTarget(manifest.HandoffID, record.Revision, state, handoff.Transition{
						At: record.Updated, NextRetry: &nextRetry, Failure: failure,
					})
					if err != nil {
						t.Fatalf("seed waiting: %v", err)
					}
				}
				notBefore := record.Updated
				app.now = func() time.Time { return notBefore.Add(-time.Hour) }
				status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
				if err != nil || status.State != handoff.TargetPrepared {
					t.Fatalf("resume status=%+v err=%v", status, err)
				}
				prepared, err := app.store.GetTarget(manifest.HandoffID)
				if err != nil || !prepared.Updated.Equal(notBefore) {
					t.Fatalf("prepared updated=%v err=%v, want %v", prepared.Updated, err, notBefore)
				}
			})
		}
	})

	t.Run("waiting and rejected failures", func(t *testing.T) {
		tests := []struct {
			name         string
			writeHistory bool
			mutate       func(*handoff.Manifest)
			wantState    handoff.TargetState
		}{
			{name: "waiting", wantState: handoff.TargetWaitingAssets},
			{
				name: "rejected", writeHistory: true, wantState: handoff.TargetRejected,
				mutate: func(manifest *handoff.Manifest) { manifest.Repository.ProjectSlug = "unknown-project" },
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				app, manifest := newHandoffTestApplication(t, test.writeHistory)
				manifest.HandoffID = "clock-backward-" + test.name
				if test.mutate != nil {
					test.mutate(&manifest)
				}
				received, replay, err := app.store.ReceiveTarget(manifest)
				if err != nil || replay {
					t.Fatalf("receive replay=%t err=%v", replay, err)
				}
				app.now = func() time.Time { return received.Updated.Add(-time.Hour) }
				status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
				if err != nil || status.State != test.wantState {
					t.Fatalf("prepare status=%+v err=%v", status, err)
				}
				updated, err := app.store.GetTarget(manifest.HandoffID)
				if err != nil || !updated.Updated.Equal(received.Updated) || updated.Failure == nil || !updated.Failure.At.Equal(received.Updated) {
					t.Fatalf("updated record=%+v err=%v, want transition at %v", updated, err, received.Updated)
				}
				if test.wantState == handoff.TargetWaitingAssets && (updated.NextRetry == nil || !updated.NextRetry.Equal(received.Updated.Add(handoffAssetRetryDelay))) {
					t.Fatalf("waiting next_retry=%v, want %v", updated.NextRetry, received.Updated.Add(handoffAssetRetryDelay))
				}
			})
		}
	})
}

func TestMeshRuntimeImmediatelyAndPeriodicallyReconcilesTargetHandoffs(t *testing.T) {
	app, base := newHandoffTestApplication(t, true)
	receivedManifest := base
	receivedManifest.HandoffID = "runtime-received"
	if _, _, err := app.store.ReceiveTarget(receivedManifest); err != nil {
		t.Fatalf("seed received: %v", err)
	}
	waitingManifest := base
	waitingManifest.HandoffID = "runtime-waiting"
	waiting, _, err := app.store.ReceiveTarget(waitingManifest)
	if err != nil {
		t.Fatalf("seed waiting receive: %v", err)
	}
	waiting, err = app.store.TransitionTarget(waitingManifest.HandoffID, waiting.Revision, handoff.TargetValidating, handoff.Transition{})
	if err != nil {
		t.Fatalf("seed waiting validating: %v", err)
	}
	nextRetry := time.Now().UTC().Add(250 * time.Millisecond)
	if !nextRetry.After(waiting.Updated) {
		nextRetry = waiting.Updated.Add(250 * time.Millisecond)
	}
	failure := &handoff.Failure{Code: handoff.FailureMissingAsset, Message: "asset pending", Retryable: true, At: waiting.Updated}
	if _, err := app.store.TransitionTarget(waitingManifest.HandoffID, waiting.Revision, handoff.TargetWaitingAssets, handoff.Transition{
		At: waiting.Updated, NextRetry: &nextRetry, Failure: failure,
	}); err != nil {
		t.Fatalf("seed waiting: %v", err)
	}

	d := newMeshApplicationTestDaemon(t, "server")
	d.meshMu.Lock()
	d.meshApp.handoffs = app
	d.meshApp.reconcileInterval = 25 * time.Millisecond
	d.meshMu.Unlock()
	manager := arcmuxmesh.New(meshApplicationTestConfig("127.0.0.1:0"), &arcmuxmesh.Registry{
		Version: arcmuxmesh.RegistryVersion, DeviceID: "server",
	}, testDiscardLogger())
	d.startMeshApplicationRuntime(manager)
	t.Cleanup(func() {
		d.meshMu.RLock()
		meshApp := d.meshApp
		d.meshMu.RUnlock()
		meshApp.stopRuntime()
	})

	waitForTargetState := func(id string, want handoff.TargetState) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			record, err := app.store.GetTarget(id)
			if err == nil && record.State == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		record, err := app.store.GetTarget(id)
		t.Fatalf("target %s state=%s err=%v, want %s", id, record.State, err, want)
	}
	waitForTargetState(receivedManifest.HandoffID, handoff.TargetPrepared)
	waitForTargetState(waitingManifest.HandoffID, handoff.TargetPrepared)
}

func TestMeshRuntimeResumesInterruptedSourcePreparationOnStartup(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	targetApp, manifest := newHandoffTestApplication(t, true)
	manifest.HandoffID = "source-runtime-restart"
	server.meshMu.Lock()
	server.meshApp.handoffs = targetApp
	server.meshMu.Unlock()
	client.meshMu.RLock()
	sourceStore := client.meshApp.handoffs.store
	client.meshMu.RUnlock()
	sourceSession := session.NewSession(manifest.Source.SessionID, "source stays live", manifest.SourceAgent, t.TempDir())
	sourceSession.SetState(session.StateIdle)
	client.mu.Lock()
	client.sessions[sourceSession.ID] = sourceSession
	client.mu.Unlock()
	seedSourceHandoffState(t, sourceStore, manifest, handoff.SourcePreparingRemote, time.Time{})

	startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsPrepare})
	waitForSourceHandoffState(t, sourceStore, manifest.HandoffID, handoff.SourceRemotePrepared)
	record, err := sourceStore.GetSource(manifest.HandoffID)
	if err != nil || record.Attempts != 1 {
		t.Fatalf("recovered record=%+v err=%v", record, err)
	}
	kept, ok := client.GetSession(sourceSession.ID)
	if !ok || kept != sourceSession {
		t.Fatalf("source session removed or replaced during handoff recovery: kept=%p ok=%t", kept, ok)
	}
	if snapshot := kept.Snapshot(); snapshot.State != session.StateIdle {
		t.Fatalf("source session changed during handoff recovery: %+v", snapshot)
	}
}

func TestMeshRuntimePeriodicallyReconcilesDueButNotFutureSourceRetries(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	targetApp, manifest := newHandoffTestApplication(t, true)
	server.meshMu.Lock()
	server.meshApp.handoffs = targetApp
	server.meshMu.Unlock()
	client.meshMu.Lock()
	client.meshApp.reconcileInterval = 20 * time.Millisecond
	sourceStore := client.meshApp.handoffs.store
	client.meshMu.Unlock()
	startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsPrepare})

	dueManifest := manifest
	dueManifest.HandoffID = "source-runtime-periodic-due"
	futureManifest := manifest
	futureManifest.HandoffID = "source-runtime-periodic-future"
	dueAt := time.Now().UTC().Add(100 * time.Millisecond)
	seedSourceHandoffState(t, sourceStore, dueManifest, handoff.SourceRetryWait, dueAt)
	seedSourceHandoffState(t, sourceStore, futureManifest, handoff.SourceRetryWait, dueAt.Add(time.Hour))

	waitForSourceHandoffState(t, sourceStore, dueManifest.HandoffID, handoff.SourceRemotePrepared)
	future, err := sourceStore.GetSource(futureManifest.HandoffID)
	if err != nil || future.State != handoff.SourceRetryWait || future.Attempts != 1 {
		t.Fatalf("future retry changed early: %+v err=%v", future, err)
	}
}

func TestMeshConnectionWakeHonorsThenAcceleratesDueSourceRetry(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	targetApp, manifest := newHandoffTestApplication(t, true)
	manifest.HandoffID = "source-runtime-reconnect"
	server.meshMu.Lock()
	server.meshApp.handoffs = targetApp
	server.meshMu.Unlock()
	client.meshMu.Lock()
	client.meshApp.reconcileInterval = time.Hour
	sourceStore := client.meshApp.handoffs.store
	client.meshMu.Unlock()

	_, clientManager := startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsPrepare})
	dueAt := time.Now().UTC().Add(150 * time.Millisecond)
	seedSourceHandoffState(t, sourceStore, manifest, handoff.SourceRetryWait, dueAt)

	// A newly observed connection wakes recovery, but the future deadline is
	// still authoritative.
	client.reconcileMeshStatuses(clientManager, make(map[string]time.Time))
	time.Sleep(40 * time.Millisecond)
	beforeDue, err := sourceStore.GetSource(manifest.HandoffID)
	if err != nil || beforeDue.State != handoff.SourceRetryWait || beforeDue.Attempts != 1 {
		t.Fatalf("connection bypassed future retry: %+v err=%v", beforeDue, err)
	}

	if wait := time.Until(dueAt); wait > 0 {
		time.Sleep(wait + 20*time.Millisecond)
	}
	// Treating the same connected transport as newly established models the
	// reconnect edge that reconcileMeshStatuses reports to the runtime.
	client.reconcileMeshStatuses(clientManager, make(map[string]time.Time))
	waitForSourceHandoffState(t, sourceStore, manifest.HandoffID, handoff.SourceRemotePrepared)
}

func TestMeshRuntimeBlockedSourcePeerDoesNotStarveTargetRecovery(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	serverTargetApp, sourceManifest := newHandoffTestApplication(t, true)
	sourceManifest.HandoffID = "source-runtime-blocked"
	clientApp, localTargetManifest := newHandoffTestApplication(t, true)
	localTargetManifest.HandoffID = "target-runtime-independent"
	localTargetManifest.Source.DeviceID = "server"
	localTargetManifest.Target.DeviceID = "client"
	if _, _, err := clientApp.store.ReceiveTarget(localTargetManifest); err != nil {
		t.Fatalf("seed local target recovery: %v", err)
	}
	seedSourceHandoffState(t, clientApp.store, sourceManifest, handoff.SourceQueued, time.Time{})

	sourceStarted := make(chan struct{})
	releaseSource := make(chan struct{})
	var startedOnce sync.Once
	releaseOnce := sync.OnceFunc(func() { close(releaseSource) })
	defer releaseOnce()
	serverTargetApp.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error {
		startedOnce.Do(func() { close(sourceStarted) })
		<-releaseSource
		return nil
	}
	server.meshMu.Lock()
	server.meshApp.handoffs = serverTargetApp
	server.meshMu.Unlock()
	client.meshMu.Lock()
	client.meshApp.handoffs = clientApp
	client.meshMu.Unlock()

	startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsPrepare})
	select {
	case <-sourceStarted:
	case <-time.After(time.Second):
		t.Fatal("source recovery RPC did not block")
	}
	waitForTargetHandoffState(t, clientApp.store, localTargetManifest.HandoffID, handoff.TargetPrepared)
	releaseOnce()
}

func waitForSourceHandoffState(t *testing.T, store *handoff.Store, id string, want handoff.SourceState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record, err := store.GetSource(id)
		if err == nil && record.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, err := store.GetSource(id)
	t.Fatalf("source %s state=%s err=%v, want %s", id, record.State, err, want)
}

func waitForTargetHandoffState(t *testing.T, store *handoff.Store, id string, want handoff.TargetState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		record, err := store.GetTarget(id)
		if err == nil && record.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, err := store.GetTarget(id)
	t.Fatalf("target %s state=%s err=%v, want %s", id, record.State, err, want)
}

func TestHandoffPrepareClassifiesRepositoryValidation(t *testing.T) {
	tests := []struct {
		name      string
		repoError error
		state     handoff.TargetState
		code      handoff.FailureCode
		retryable bool
	}{
		{
			name: "missing remote branch", repoError: &handoff.RepositoryError{Code: handoff.RepositoryErrorRetryable, Err: errors.New("branch missing")},
			state: handoff.TargetWaitingAssets, code: handoff.FailureMissingAsset, retryable: true,
		},
		{
			name: "repository mismatch", repoError: &handoff.RepositoryError{Code: handoff.RepositoryErrorDeterministic, Err: errors.New("wrong origin")},
			state: handoff.TargetRejected, code: handoff.FailureVerification,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error { return test.repoError }
			status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffPrepareRequest{Manifest: manifest})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if status.State != test.state || status.Failure == nil || status.Failure.Code != test.code || status.Failure.Retryable != test.retryable {
				t.Fatalf("status = %+v", status)
			}
		})
	}
}

func TestHandoffRPCParamsAreStrictAndReadOnlyGrantIsDenied(t *testing.T) {
	var statusRequest meshHandoffStatusRequest
	if err := decodeMeshParams(json.RawMessage(`{"handoff_id":"handoff-1","extra":true}`), &statusRequest); err == nil {
		t.Fatal("status accepted unknown field")
	}
	var prepareRequest meshHandoffPrepareRequest
	if err := decodeMeshParams(json.RawMessage(`{"manifest":{"schema_version":1,"unknown":true}}`), &prepareRequest); err == nil {
		t.Fatal("prepare accepted nested unknown field")
	}
	var launchRequest meshHandoffLaunchRequest
	if err := decodeMeshParams(json.RawMessage(`{"handoff_id":"handoff-1","manifest_digest":"abc","extra":true}`), &launchRequest); err == nil {
		t.Fatal("launch accepted unknown field")
	}
	var verifyRequest meshHandoffVerifyRequest
	if err := decodeMeshParams(json.RawMessage(`{"handoff_id":"handoff-1","manifest_digest":"abc","target_locator":{"device_id":"server","profile_scope":"root","session_id":"target"},"extra":true}`), &verifyRequest); err == nil {
		t.Fatal("verify accepted unknown field")
	}

	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	_, clientManager := startDaemonMeshPairWithScopes(t, server, client, []string{
		arcmuxmesh.ScopeSessionsRead, arcmuxmesh.ScopeArtifactsRead, arcmuxmesh.ScopeEventsRead,
	})
	for _, call := range []struct {
		method string
		params any
	}{
		{meshMethodHandoffsPrepare, meshHandoffPrepareRequest{}},
		{meshMethodHandoffsStatus, meshHandoffStatusRequest{HandoffID: "handoff-1"}},
		{meshMethodHandoffsLaunch, meshHandoffLaunchRequest{HandoffID: "handoff-1", ManifestDigest: strings.Repeat("a", 64)}},
		{meshMethodHandoffsVerify, meshHandoffVerifyRequest{HandoffID: "handoff-1", ManifestDigest: strings.Repeat("a", 64)}},
	} {
		if err := clientManager.Call(context.Background(), "server", call.method, call.params, &meshHandoffStatus{}); !isMeshPermissionDenied(err) {
			t.Fatalf("%s with read-only grants = %v, want permission_denied", call.method, err)
		}
	}
}

func TestHandoffPrepareRPCUsesConnectionPrincipalAndReturnsRedactedDTO(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	handoffApp, manifest := newHandoffTestApplication(t, true)
	server.meshMu.Lock()
	server.meshApp.handoffs = handoffApp
	server.meshMu.Unlock()
	_, clientManager := startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsPrepare})

	var status meshHandoffStatus
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsPrepare, meshHandoffPrepareRequest{Manifest: manifest}, &status); err != nil {
		t.Fatalf("prepare RPC: %v", err)
	}
	if status.State != handoff.TargetPrepared || status.HandoffID != manifest.HandoffID || status.ManifestDigest == "" {
		t.Fatalf("status = %+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), manifest.Goal.Text) || strings.Contains(string(encoded), manifest.History.Basename) ||
		strings.Contains(string(encoded), manifest.Repository.Branch) {
		t.Fatalf("prepare response leaked manifest fields: %s", encoded)
	}
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsLaunch, meshHandoffLaunchRequest{
		HandoffID: manifest.HandoffID, ManifestDigest: status.ManifestDigest,
	}, &meshHandoffStatus{}); !isMeshPermissionDenied(err) {
		t.Fatalf("prepare-only grant launched handoff: %v", err)
	}

	wrongSource := manifest
	wrongSource.HandoffID = "handoff-wrong-source"
	wrongSource.Source.DeviceID = "another-client"
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsPrepare, meshHandoffPrepareRequest{Manifest: wrongSource}, &meshHandoffStatus{}); !isMeshPermissionDenied(err) {
		t.Fatalf("spoofed source error = %v, want permission_denied", err)
	}
	if _, err := handoffApp.store.GetTarget(wrongSource.HandoffID); !errors.Is(err, handoff.ErrNotFound) {
		t.Fatalf("spoofed source persisted record: %v", err)
	}

	wrongTarget := manifest
	wrongTarget.HandoffID = "handoff-wrong-target"
	wrongTarget.Target.DeviceID = "another-server"
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsPrepare, meshHandoffPrepareRequest{Manifest: wrongTarget}, &meshHandoffStatus{}); !isMeshInvalidRequest(err) {
		t.Fatalf("wrong target error = %v, want invalid_request", err)
	}
	if _, err := handoffApp.store.GetTarget(wrongTarget.HandoffID); !errors.Is(err, handoff.ErrNotFound) {
		t.Fatalf("wrong target persisted record: %v", err)
	}

	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsPrepare, map[string]any{
		"manifest": manifest, "unexpected": true,
	}, &meshHandoffStatus{}); !isMeshInvalidRequest(err) {
		t.Fatalf("unknown prepare field error = %v, want invalid_request", err)
	}
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsStatus, map[string]any{
		"handoff_id": manifest.HandoffID, "unexpected": true,
	}, &meshHandoffStatus{}); !isMeshInvalidRequest(err) {
		t.Fatalf("unknown status field error = %v, want invalid_request", err)
	}
}

func TestHandoffMeshRPCUsesTargetResumeTimeout(t *testing.T) {
	t.Run("prepare", func(t *testing.T) {
		server := newMeshApplicationTestDaemon(t, "server")
		client := newMeshApplicationTestDaemon(t, "client")
		targetApp, manifest := newHandoffTestApplication(t, true)
		targetApp.resumeTimeout = 10 * time.Millisecond
		targetApp.prepareRepository = func(ctx context.Context, _ handoff.Manifest, _ project.ResolvedProject) error {
			<-ctx.Done()
			return ctx.Err()
		}
		server.meshMu.Lock()
		server.meshApp.handoffs = targetApp
		server.meshMu.Unlock()
		_, clientManager := startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsPrepare})

		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		started := time.Now()
		err := clientManager.Call(ctx, "server", meshMethodHandoffsPrepare, meshHandoffPrepareRequest{Manifest: manifest}, &meshHandoffStatus{})
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("blocked target prepare unexpectedly succeeded")
		}
		if elapsed >= 100*time.Millisecond {
			t.Fatalf("target prepare took %s, want resume timeout", elapsed)
		}
	})

	t.Run("launch", func(t *testing.T) {
		server := newMeshApplicationTestDaemon(t, "server")
		client := newMeshApplicationTestDaemon(t, "client")
		targetApp, manifest := newHandoffTestApplication(t, true)
		prepared := prepareTargetHandoffForLaunch(t, targetApp, manifest)
		targetApp.resumeTimeout = 10 * time.Millisecond
		targetApp.prepareLaunchRepo = func(ctx context.Context, _ handoff.Manifest, _ project.ResolvedProject) (handoff.RepositoryPreparation, error) {
			<-ctx.Done()
			return handoff.RepositoryPreparation{}, ctx.Err()
		}
		targetApp.createSession = func(context.Context, CreateSessionRequest) (*session.Session, bool, error) {
			return nil, false, errors.New("unexpected create")
		}
		targetApp.lookupSession = func(string) (*session.Session, bool) { return nil, false }
		targetApp.sendPrompt = func(context.Context, string, string, bool, bool) error {
			return errors.New("unexpected send")
		}
		targetApp.persistSessions = func() error { return nil }
		server.meshMu.Lock()
		server.meshApp.handoffs = targetApp
		server.meshMu.Unlock()
		_, clientManager := startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsLaunch})

		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		started := time.Now()
		err := clientManager.Call(ctx, "server", meshMethodHandoffsLaunch, meshHandoffLaunchRequest{
			HandoffID: manifest.HandoffID, ManifestDigest: prepared.Digest,
		}, &meshHandoffStatus{})
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("blocked target launch unexpectedly succeeded")
		}
		if elapsed >= 100*time.Millisecond {
			t.Fatalf("target launch took %s, want resume timeout", elapsed)
		}
	})
}

func TestHandoffLaunchGrantDoesNotAuthorizePrepare(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	_, clientManager := startDaemonMeshPairWithScopes(t, server, client, []string{arcmuxmesh.ScopeHandoffsLaunch})
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsPrepare, meshHandoffPrepareRequest{}, &meshHandoffStatus{}); !isMeshPermissionDenied(err) {
		t.Fatalf("launch-only grant prepared handoff: %v", err)
	}
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsLaunch, meshHandoffLaunchRequest{}, &meshHandoffStatus{}); !isMeshInvalidRequest(err) {
		t.Fatalf("launch grant did not reach launch handler: %v", err)
	}
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsLaunch, map[string]any{
		"handoff_id": "handoff-1", "manifest_digest": "abc", "unexpected": true,
	}, &meshHandoffStatus{}); !isMeshInvalidRequest(err) {
		t.Fatalf("launch RPC accepted unknown field: %v", err)
	}
	if err := clientManager.Call(context.Background(), "server", meshMethodHandoffsVerify, meshHandoffVerifyRequest{}, &meshHandoffVerification{}); !isMeshInvalidRequest(err) {
		t.Fatalf("launch grant did not reach verify handler: %v", err)
	}
}

func TestHandoffLaunchWaitsForHandshakeAndKeepsSensitiveContextOutOfPrompt(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	app.launchRendezvousRoot = filepath.Join(t.TempDir(), "handoff-receive")
	manifest.ParentHandoffID = "handoff-parent"
	prepared := prepareTargetHandoffForLaunch(t, app, manifest)
	worktree := filepath.Join(t.TempDir(), "private-worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	app.prepareLaunchRepo = func(context.Context, handoff.Manifest, project.ResolvedProject) (handoff.RepositoryPreparation, error) {
		return handoff.RepositoryPreparation{
			WorktreePath: worktree, Head: manifest.Repository.SourceHead,
			LocalBranch: manifest.Repository.Branch, SourceBranch: manifest.Repository.Branch,
		}, nil
	}
	var created *session.Session
	var createRequest CreateSessionRequest
	app.createSession = func(_ context.Context, request CreateSessionRequest) (*session.Session, bool, error) {
		createRequest = request
		created = session.NewSession("target-session", request.Name, request.Agent, request.CWD)
		created.SetOwnerID(request.OwnerID)
		created.SetEnv(request.Env)
		if request.private {
			created.MarkPrivate()
		}
		go func() {
			time.Sleep(40 * time.Millisecond)
			created.SetState(session.StateIdle)
		}()
		return created, true, nil
	}
	app.lookupSession = func(id string) (*session.Session, bool) {
		return created, created != nil && created.Snapshot().ID == id
	}
	var sentPrompt string
	var sendState session.State
	app.sendPrompt = func(_ context.Context, id, prompt string, confirm, waitIdle bool) error {
		if id != "target-session" || !confirm || waitIdle {
			t.Fatalf("send id=%q confirm=%t waitIdle=%t", id, confirm, waitIdle)
		}
		sendState = created.Snapshot().State
		sentPrompt = prompt
		current, err := app.store.GetTarget(manifest.HandoffID)
		if err != nil {
			t.Fatal(err)
		}
		created.SetCurrentCommand(handoffLaunchCurrentCommand(current))
		created.SetState(session.StateWorking)
		return nil
	}
	var persisted atomic.Int32
	app.persistSessions = func() error { persisted.Add(1); return nil }
	app.launchPoll = time.Millisecond

	status, err := app.launch(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffLaunchRequest{
		HandoffID: manifest.HandoffID, ManifestDigest: prepared.Digest,
	})
	if err != nil || status.State != handoff.TargetAccepted || status.TargetLocator == nil || persisted.Load() != 2 {
		t.Fatalf("launch status=%+v persisted=%d err=%v", status, persisted.Load(), err)
	}
	if sendState != session.StateIdle {
		t.Fatalf("prompt sent before delayed handshake reached idle: %s", sendState)
	}
	if createRequest.Prompt != "" || !createRequest.private || createRequest.Env["ARCMUX_HANDOFF_INSTRUCTIONS"] == "" {
		t.Fatalf("create request = %+v", createRequest)
	}
	for _, forbidden := range []string{
		manifest.HandoffID, manifest.TraceID, manifest.ParentHandoffID,
		manifest.Source.DeviceID, manifest.Source.ProfileScope, manifest.Source.SessionID,
		manifest.History.ConversationID, manifest.Target.Profile,
		manifest.Goal.Text, manifest.History.Basename, manifest.Repository.Branch,
		manifest.Repository.SourceHead, worktree, createRequest.Env["ARCMUX_HANDOFF_INSTRUCTIONS"],
	} {
		if strings.Contains(sentPrompt, forbidden) {
			t.Fatalf("confirmed prompt leaked %q: %q", forbidden, sentPrompt)
		}
	}
	receiveCommand := "arcmux handoff receive " + handoff.LaunchMarker(prepared.Manifest.HandoffID, prepared.Digest)
	if !strings.Contains(sentPrompt, receiveCommand) || strings.Contains(sentPrompt, "ARCMUX_HANDOFF_INSTRUCTIONS") || len([]rune(handoffLaunchSafePreamble(prepared))) != handoffSafePreambleRunes {
		t.Fatalf("safe prompt contract missing: %q", sentPrompt)
	}
	instructions := createRequest.Env["ARCMUX_HANDOFF_INSTRUCTIONS"]
	info, err := os.Lstat(instructions)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("instruction mode=%v err=%v", info, err)
	}
	content, err := os.ReadFile(instructions)
	if err != nil {
		t.Fatal(err)
	}
	var private struct {
		Lineage         handoffLaunchLineage `json:"lineage"`
		RequiredActions []string             `json:"required_actions"`
		Acknowledge     string               `json:"acknowledge"`
	}
	if err := json.Unmarshal(content, &private); err != nil {
		t.Fatalf("decode private instructions: %v", err)
	}
	wantLineage := handoffLaunchLineage{
		HandoffID: manifest.HandoffID, TraceID: manifest.TraceID, ParentHandoffID: manifest.ParentHandoffID,
		Source: manifest.Source, ConversationID: manifest.History.ConversationID, TargetProfile: manifest.Target.Profile,
	}
	if private.Lineage != wantLineage {
		t.Fatalf("private lineage = %+v, want %+v", private.Lineage, wantLineage)
	}
	acknowledgeCommand := "arcmux handoff acknowledge " + handoff.LaunchMarker(prepared.Manifest.HandoffID, prepared.Digest) + " --phase context-loaded"
	if private.Acknowledge != acknowledgeCommand || len(private.RequiredActions) != 4 || !strings.Contains(sentPrompt, acknowledgeCommand) {
		t.Fatalf("context-loaded contract actions=%v acknowledge=%q prompt=%q", private.RequiredActions, private.Acknowledge, sentPrompt)
	}
	received, err := handoff.ReceiveLaunchInstructions(app.launchRendezvousRoot, handoff.LaunchMarker(prepared.Manifest.HandoffID, prepared.Digest))
	if err != nil || string(received) != string(content) {
		t.Fatalf("owner-local rendezvous instructions=%q err=%v, want %q", received, err, content)
	}
	for _, required := range []string{manifest.Goal.Text, manifest.Repository.Branch, manifest.Repository.SourceHead, worktree, "history.md"} {
		if !strings.Contains(string(content), required) {
			t.Fatalf("private instructions missing %q: %s", required, content)
		}
	}
}

func TestPublishHandoffLaunchInstructionsCanonicalizesSymlinkedParent(t *testing.T) {
	_, manifest := newHandoffTestApplication(t, true)
	realParent := t.TempDir()
	realRoot := filepath.Join(realParent, "mesh")
	handoffDir := filepath.Join(realRoot, "handoff-"+manifest.HandoffID)
	if err := os.MkdirAll(handoffDir, 0o700); err != nil {
		t.Fatal(err)
	}
	historyContent := []byte("private history\n")
	if err := os.WriteFile(filepath.Join(handoffDir, "history.md"), historyContent, 0o600); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}
	canonicalHandoffDir, err := filepath.EvalSymlinks(handoffDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	instructions, err := publishHandoffLaunchInstructions(
		filepath.Join(linkParent, "mesh"),
		manifest.HandoffID,
		filepath.Join(linkParent, "mesh", "handoff-"+manifest.HandoffID, "history.md"),
		manifest,
		handoff.RepositoryPreparation{
			WorktreePath: worktree,
			Head:         manifest.Repository.SourceHead,
			SourceBranch: manifest.Repository.Branch,
			LocalBranch:  manifest.Repository.Branch,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if instructions != filepath.Join(canonicalHandoffDir, "launch-instructions.json") {
		t.Fatalf("instructions path = %q, want canonical root %q", instructions, canonicalHandoffDir)
	}
	content, err := os.ReadFile(instructions)
	if err != nil {
		t.Fatal(err)
	}
	var private struct {
		History string `json:"history"`
	}
	if err := json.Unmarshal(content, &private); err != nil {
		t.Fatal(err)
	}
	if private.History != filepath.Join(canonicalHandoffDir, "history.md") || strings.Contains(private.History, linkParent) {
		t.Fatalf("private history path was not canonicalized: %q", private.History)
	}
}

func TestHandoffLaunchRejectsWrongOwnerTargetOrDigestBeforeSideEffects(t *testing.T) {
	for _, test := range []struct {
		name        string
		peer        string
		localDevice string
		digest      string
		permission  bool
	}{
		{name: "wrong authenticated owner", peer: "impostor", localDevice: "server", permission: true},
		{name: "wrong local target", peer: "client", localDevice: "other-server"},
		{name: "wrong immutable digest", peer: "client", localDevice: "server", digest: strings.Repeat("f", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			prepared := prepareTargetHandoffForLaunch(t, app, manifest)
			digest := test.digest
			if digest == "" {
				digest = prepared.Digest
			}
			var sideEffects atomic.Int32
			app.createSession = func(context.Context, CreateSessionRequest) (*session.Session, bool, error) {
				sideEffects.Add(1)
				return nil, false, nil
			}
			app.sendPrompt = func(context.Context, string, string, bool, bool) error { sideEffects.Add(1); return nil }
			app.persistSessions = func() error { sideEffects.Add(1); return nil }
			_, err := app.launch(context.Background(), arcmuxmesh.Principal{PeerID: test.peer}, test.localDevice, meshHandoffLaunchRequest{
				HandoffID: manifest.HandoffID, ManifestDigest: digest,
			})
			if test.permission {
				if !isMeshPermissionDenied(err) {
					t.Fatalf("error=%v, want permission_denied", err)
				}
			} else if !isMeshInvalidRequest(err) {
				t.Fatalf("error=%v, want invalid_request", err)
			}
			stored, getErr := app.store.GetTarget(manifest.HandoffID)
			if getErr != nil || stored.State != handoff.TargetPrepared || sideEffects.Load() != 0 {
				t.Fatalf("stored=%+v side_effects=%d err=%v", stored, sideEffects.Load(), getErr)
			}
		})
	}
}

func TestHandoffLaunchRevalidatesPreparedAssetsBeforeSessionSideEffects(t *testing.T) {
	for _, test := range []struct {
		name       string
		historyErr error
		repoErr    error
		wantState  handoff.TargetState
		wantCode   handoff.FailureCode
	}{
		{
			name: "invalid history", historyErr: &handoff.HistoryError{Code: handoff.HistoryErrorInvalid, Err: errors.New("private path")},
			wantState: handoff.TargetRejected, wantCode: handoff.FailureVerification,
		},
		{
			name: "missing history", historyErr: &handoff.HistoryError{Code: handoff.HistoryErrorRetryable, Err: errors.New("private path")},
			wantState: handoff.TargetLaunchWaitingAssets, wantCode: handoff.FailureMissingAsset,
		},
		{
			name: "repository mismatch", repoErr: &handoff.RepositoryError{Code: handoff.RepositoryErrorDeterministic, Err: errors.New("private path")},
			wantState: handoff.TargetRejected, wantCode: handoff.FailureVerification,
		},
		{
			name: "repository unavailable", repoErr: &handoff.RepositoryError{Code: handoff.RepositoryErrorRetryable, Err: errors.New("private path")},
			wantState: handoff.TargetLaunchWaitingAssets, wantCode: handoff.FailureMissingAsset,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			prepared := prepareTargetHandoffForLaunch(t, app, manifest)
			if test.historyErr != nil {
				app.snapshotHistory = func(string, string, string, handoff.HistoryRef) (string, error) { return "", test.historyErr }
			}
			app.prepareLaunchRepo = func(context.Context, handoff.Manifest, project.ResolvedProject) (handoff.RepositoryPreparation, error) {
				if test.repoErr != nil {
					return handoff.RepositoryPreparation{}, test.repoErr
				}
				return handoff.RepositoryPreparation{}, errors.New("repository revalidation unexpectedly reached")
			}
			var sideEffects atomic.Int32
			app.createSession = func(context.Context, CreateSessionRequest) (*session.Session, bool, error) {
				sideEffects.Add(1)
				return nil, false, errors.New("unexpected create")
			}
			app.lookupSession = func(string) (*session.Session, bool) { sideEffects.Add(1); return nil, false }
			app.sendPrompt = func(context.Context, string, string, bool, bool) error { sideEffects.Add(1); return nil }
			app.persistSessions = func() error { sideEffects.Add(1); return nil }
			status, err := app.launch(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffLaunchRequest{
				HandoffID: manifest.HandoffID, ManifestDigest: prepared.Digest,
			})
			if err != nil || status.State != test.wantState || status.Failure == nil || status.Failure.Code != test.wantCode || status.TargetLocator != nil || sideEffects.Load() != 0 {
				t.Fatalf("status=%+v side_effects=%d err=%v", status, sideEffects.Load(), err)
			}
		})
	}
}

func TestHandoffLaunchRecoversSpawnBeforePromptAndPromptBeforeAccept(t *testing.T) {
	for _, test := range []struct {
		name                string
		markerBeforeResume  bool
		locatorBeforeResume bool
		wantSends           int32
	}{
		{name: "spawn before prompt", wantSends: 1},
		{name: "prompt before accept", markerBeforeResume: true, locatorBeforeResume: true, wantSends: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			prepared := prepareTargetHandoffForLaunch(t, app, manifest)
			launching, err := app.store.TransitionTarget(manifest.HandoffID, prepared.Revision, handoff.TargetLaunching, handoff.Transition{})
			if err != nil {
				t.Fatal(err)
			}
			worktree := filepath.Join(t.TempDir(), "private-worktree")
			if err := os.Mkdir(worktree, 0o700); err != nil {
				t.Fatal(err)
			}
			app.prepareLaunchRepo = func(context.Context, handoff.Manifest, project.ResolvedProject) (handoff.RepositoryPreparation, error) {
				return handoff.RepositoryPreparation{WorktreePath: worktree, Head: manifest.Repository.SourceHead, LocalBranch: manifest.Repository.Branch, SourceBranch: manifest.Repository.Branch}, nil
			}
			historyPath, err := app.snapshotHistory(app.historyRoot, app.store.Root(), manifest.HandoffID, manifest.History)
			if err != nil {
				t.Fatal(err)
			}
			instructions, err := publishHandoffLaunchInstructions(app.store.Root(), manifest.HandoffID, historyPath, manifest, handoff.RepositoryPreparation{
				WorktreePath: worktree, Head: manifest.Repository.SourceHead, LocalBranch: manifest.Repository.Branch, SourceBranch: manifest.Repository.Branch,
			})
			if err != nil {
				t.Fatal(err)
			}
			name, owner := handoffLaunchSessionIdentity(launching)
			sess := session.NewSession("target-session", name, manifest.Target.Profile, worktree)
			sess.SetOwnerID(owner)
			sess.MarkPrivate()
			sess.SetEnv(map[string]string{"ARCMUX_HANDOFF_INSTRUCTIONS": instructions})
			sess.SetState(session.StateIdle)
			if test.locatorBeforeResume {
				launching, err = app.store.RecordTargetLaunchLocator(manifest.HandoffID, launching.Revision, handoff.TargetLocator{
					DeviceID: "server", ProfileScope: "root", SessionID: "target-session",
				}, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.markerBeforeResume {
				sess.SetCurrentCommand(handoffLaunchCurrentCommand(launching))
				sess.SetState(session.StateWorking)
			}
			app.createSession = func(context.Context, CreateSessionRequest) (*session.Session, bool, error) { return sess, false, nil }
			app.lookupSession = func(id string) (*session.Session, bool) { return sess, id == sess.Snapshot().ID }
			var sends atomic.Int32
			app.sendPrompt = func(context.Context, string, string, bool, bool) error {
				sends.Add(1)
				current, _ := app.store.GetTarget(manifest.HandoffID)
				sess.SetCurrentCommand(handoffLaunchCurrentCommand(current))
				sess.SetState(session.StateWorking)
				return nil
			}
			app.persistSessions = func() error { return nil }
			status, err := app.resumeLaunch(context.Background(), launching, "server")
			if err != nil || status.State != handoff.TargetAccepted || status.TargetLocator == nil || status.TargetLocator.SessionID != "target-session" || sends.Load() != test.wantSends {
				t.Fatalf("status=%+v sends=%d err=%v", status, sends.Load(), err)
			}
			replay, err := app.launch(context.Background(), arcmuxmesh.Principal{PeerID: "client"}, "server", meshHandoffLaunchRequest{
				HandoffID: manifest.HandoffID, ManifestDigest: prepared.Digest,
			})
			if err != nil || replay.TargetLocator == nil || *replay.TargetLocator != *status.TargetLocator || sends.Load() != test.wantSends {
				t.Fatalf("replay=%+v sends=%d err=%v", replay, sends.Load(), err)
			}
		})
	}
}

func TestHandoffStatusExposesLocatorOnlyAfterAccepted(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	prepared := prepareTargetHandoffForLaunch(t, app, manifest)
	launching, err := app.store.TransitionTarget(manifest.HandoffID, prepared.Revision, handoff.TargetLaunching, handoff.Transition{})
	if err != nil {
		t.Fatal(err)
	}
	locator := handoff.TargetLocator{DeviceID: "server", ProfileScope: "root", SessionID: "target-session"}
	launching, err = app.store.RecordTargetLaunchLocator(manifest.HandoffID, launching.Revision, locator, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if status := handoffStatusDTO(launching); status.TargetLocator != nil {
		t.Fatalf("launching status leaked locator: %+v", status)
	}
	accepted, err := app.store.TransitionTarget(manifest.HandoffID, launching.Revision, handoff.TargetAccepted, handoff.Transition{TargetLocator: &locator})
	if err != nil {
		t.Fatal(err)
	}
	if status := handoffStatusDTO(accepted); status.TargetLocator == nil || *status.TargetLocator != locator {
		t.Fatalf("accepted status omitted locator: %+v", status)
	}
}

func TestHandoffVerifyRequiresAuthenticatedExactAcceptedTargetAndAcknowledgement(t *testing.T) {
	app, manifest := newHandoffTestApplication(t, true)
	prepared := prepareTargetHandoffForLaunch(t, app, manifest)
	launching, err := app.store.TransitionTarget(manifest.HandoffID, prepared.Revision, handoff.TargetLaunching, handoff.Transition{})
	if err != nil {
		t.Fatal(err)
	}
	locator := handoff.TargetLocator{DeviceID: manifest.Target.DeviceID, ProfileScope: "root", SessionID: "target-session"}
	launching, err = app.store.RecordTargetLaunchLocator(manifest.HandoffID, launching.Revision, locator, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := app.store.TransitionTarget(manifest.HandoffID, launching.Revision, handoff.TargetAccepted, handoff.Transition{TargetLocator: &locator})
	if err != nil {
		t.Fatal(err)
	}
	request := meshHandoffVerifyRequest{HandoffID: manifest.HandoffID, ManifestDigest: accepted.Digest, TargetLocator: locator}
	pending, err := app.verify(context.Background(), arcmuxmesh.Principal{PeerID: manifest.Source.DeviceID}, manifest.Target.DeviceID, request)
	if err != nil || pending.ContextLoaded || pending.VerificationState != "pending" || pending.Acknowledgement != nil {
		t.Fatalf("pending verification=%+v err=%v", pending, err)
	}
	wrong := request
	wrong.TargetLocator.SessionID = "wrong-target"
	if _, err := app.verify(context.Background(), arcmuxmesh.Principal{PeerID: manifest.Source.DeviceID}, manifest.Target.DeviceID, wrong); !isMeshInvalidRequest(err) {
		t.Fatalf("wrong target verification err=%v", err)
	}
	if _, err := app.verify(context.Background(), arcmuxmesh.Principal{PeerID: "other-source"}, manifest.Target.DeviceID, request); !isMeshPermissionDenied(err) {
		t.Fatalf("wrong source verification err=%v", err)
	}
	marker := handoff.LaunchMarker(manifest.HandoffID, accepted.Digest)
	acknowledged, replay, err := app.store.AcknowledgeTarget(marker, handoff.ContextLoadedPhase, time.Now().UTC())
	if err != nil || replay || acknowledged.ContextLoaded == nil {
		t.Fatalf("acknowledged=%+v replay=%t err=%v", acknowledged, replay, err)
	}
	verified, err := app.verify(context.Background(), arcmuxmesh.Principal{PeerID: manifest.Source.DeviceID}, manifest.Target.DeviceID, request)
	if err != nil || !verified.ContextLoaded || verified.VerificationState != "context_loaded" || verified.Acknowledgement == nil || verified.Acknowledgement.TargetLocator != locator {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
}

func TestHandoffLaunchPersistsSessionBeforeLocatorAndRetriesLocatorIO(t *testing.T) {
	for _, test := range []struct {
		name         string
		persistError bool
		locatorError bool
		wantPersist  int32
		wantRecord   int32
	}{
		{name: "session inventory failure", persistError: true, wantPersist: 1},
		{name: "locator persistence failure", locatorError: true, wantPersist: 1, wantRecord: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, manifest := newHandoffTestApplication(t, true)
			prepared := prepareTargetHandoffForLaunch(t, app, manifest)
			launching, err := app.store.TransitionTarget(manifest.HandoffID, prepared.Revision, handoff.TargetLaunching, handoff.Transition{})
			if err != nil {
				t.Fatal(err)
			}
			worktree := filepath.Join(t.TempDir(), "private-worktree")
			if err := os.Mkdir(worktree, 0o700); err != nil {
				t.Fatal(err)
			}
			preparation := handoff.RepositoryPreparation{WorktreePath: worktree, Head: manifest.Repository.SourceHead, LocalBranch: manifest.Repository.Branch, SourceBranch: manifest.Repository.Branch}
			historyPath, err := app.snapshotHistory(app.historyRoot, app.store.Root(), manifest.HandoffID, manifest.History)
			if err != nil {
				t.Fatal(err)
			}
			instructions, err := publishHandoffLaunchInstructions(app.store.Root(), manifest.HandoffID, historyPath, manifest, preparation)
			if err != nil {
				t.Fatal(err)
			}
			name, owner := handoffLaunchSessionIdentity(launching)
			sess := session.NewSession("target-session", name, manifest.Target.Profile, worktree)
			sess.SetOwnerID(owner)
			sess.MarkPrivate()
			sess.SetEnv(map[string]string{"ARCMUX_HANDOFF_INSTRUCTIONS": instructions})
			sess.SetState(session.StateIdle)
			app.createSession = func(context.Context, CreateSessionRequest) (*session.Session, bool, error) { return sess, false, nil }
			app.lookupSession = func(id string) (*session.Session, bool) { return sess, id == "target-session" }
			var persistCalls, recordCalls atomic.Int32
			app.persistSessions = func() error {
				persistCalls.Add(1)
				if test.persistError {
					return errors.New("private disk path")
				}
				return nil
			}
			originalRecorder := app.recordLocator
			app.recordLocator = func(id string, revision uint64, locator handoff.TargetLocator, at time.Time) (handoff.TargetRecord, error) {
				recordCalls.Add(1)
				if test.locatorError {
					return handoff.TargetRecord{}, errors.New("private record path")
				}
				return originalRecorder(id, revision, locator, at)
			}
			_, _, _, err = app.targetLaunchSession(context.Background(), launching, preparation, instructions)
			if err == nil || errors.Is(err, errHandoffLaunchConflict) || persistCalls.Load() != test.wantPersist || recordCalls.Load() != test.wantRecord {
				t.Fatalf("err=%v persist=%d record=%d", err, persistCalls.Load(), recordCalls.Load())
			}
			stored, getErr := app.store.GetTarget(manifest.HandoffID)
			if getErr != nil || stored.TargetLocator != nil || stored.State != handoff.TargetLaunching {
				t.Fatalf("failure published locator: record=%+v err=%v", stored, getErr)
			}

			app.persistSessions = func() error { return nil }
			app.recordLocator = originalRecorder
			_, recovered, delivered, err := app.targetLaunchSession(context.Background(), stored, preparation, instructions)
			if err != nil || delivered || recovered.TargetLocator == nil || recovered.TargetLocator.SessionID != "target-session" {
				t.Fatalf("retry recovery record=%+v delivered=%t err=%v", recovered, delivered, err)
			}
		})
	}
}

func prepareTargetHandoffForLaunch(t *testing.T, app *handoffApplication, manifest handoff.Manifest) handoff.TargetRecord {
	t.Helper()
	status, err := app.prepare(context.Background(), arcmuxmesh.Principal{PeerID: manifest.Source.DeviceID}, manifest.Target.DeviceID, meshHandoffPrepareRequest{Manifest: manifest})
	if err != nil || status.State != handoff.TargetPrepared {
		t.Fatalf("prepare target status=%+v err=%v", status, err)
	}
	record, err := app.store.GetTarget(manifest.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func newHandoffTestApplication(t *testing.T, writeHistory bool) (*handoffApplication, handoff.Manifest) {
	t.Helper()
	root := t.TempDir()
	store, err := handoff.Open(filepath.Join(root, "mesh"))
	if err != nil {
		t.Fatal(err)
	}
	historyRoot := filepath.Join(root, "histories")
	if err := os.Mkdir(historyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	historyContent := []byte("# Synced session history\n")
	historyDigest := sha256.Sum256(historyContent)
	historyName := "private-session-history.md"
	if writeHistory {
		if err := os.WriteFile(filepath.Join(historyRoot, historyName), historyContent, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repoPath := filepath.Join(root, "repo")
	worktreesPath := filepath.Join(root, "worktrees")
	if err := os.Mkdir(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreesPath, 0o700); err != nil {
		t.Fatal(err)
	}
	projectsPath := filepath.Join(root, "projects.yaml")
	projectsYAML := "projects:\n" +
		"  - project: arcmux\n" +
		"    repo: arcmux\n" +
		"    path: " + repoPath + "\n" +
		"    worktrees: " + worktreesPath + "\n"
	if err := os.WriteFile(projectsPath, []byte(projectsYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	manifest := handoff.Manifest{
		SchemaVersion: handoff.ManifestVersion, HandoffID: "handoff-1", TraceID: "trace-1",
		Source:      handoff.SourceSession{DeviceID: "client", ProfileScope: "profile:codex", SessionID: "session-1"},
		SourceAgent: "codex", Target: handoff.TargetAgent{DeviceID: "server", Profile: "codex"},
		Goal: handoff.GoalSummary{
			Text: "DO_NOT_LEAK_GOAL", Provenance: "explicit_operator",
			UpdatedAt: now,
		},
		History: handoff.HistoryRef{
			ArtifactID: "history-1", Basename: historyName, SHA256: hex.EncodeToString(historyDigest[:]),
			SizeBytes: int64(len(historyContent)), ConversationID: "conversation-1",
		},
		Repository: handoff.RepositorySnapshot{
			ProjectSlug: "arcmux", RepoSlug: "lin-labs/arcmux", Branch: "boyan/private-branch",
			SourceHead: strings.Repeat("a", 40), BaseCommit: strings.Repeat("b", 40),
			TreeDigest: strings.Repeat("c", 40), Cleanliness: handoff.RepositoryClean,
			Transfer: handoff.TransferRemoteBranch,
		},
		Artifacts:  []handoff.ArtifactRef{},
		Validation: handoff.ValidationEvidence{State: handoff.ValidationNotRun},
		CreatedAt:  now,
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("test manifest: %v", err)
	}
	app := newHandoffApplication(store, map[string]profile.Profile{
		"codex":      {Transport: profile.TransportTmux, StartCommand: "codex"},
		"codex_exec": {Transport: profile.TransportExec},
	})
	app.historyRoot = historyRoot
	app.projectsPath = projectsPath
	app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error { return nil }
	return app, manifest
}
