package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

type sourceOutboxFixture struct {
	outbox       *sourceHandoffOutbox
	store        *handoff.Store
	detail       sessionview.Detail
	remote       func(context.Context, string, meshHandoffPrepareRequest) (meshHandoffStatus, error)
	launchRemote func(context.Context, string, meshHandoffLaunchRequest) (meshHandoffStatus, error)
	inspectedCWD string
	manifest     handoff.Manifest
}

func newSourceOutboxFixture(t *testing.T) *sourceOutboxFixture {
	t.Helper()
	root := t.TempDir()
	store, err := handoff.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	projectsPath := filepath.Join(root, "projects.yaml")
	if err := os.WriteFile(projectsPath, []byte("projects:\n  - project: demo\n    path: /registered/demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locator, err := sessionview.NewLocator(sessionview.RootProfileScope, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sourceOutboxFixture{store: store}
	fixture.detail = sessionview.Detail{Summary: sessionview.Summary{
		Locator: locator, Agent: "codex", LaunchCWD: "/actual/source/worktree",
		History: &sessionview.HistoryReference{Basename: "session-history.md"},
	}}
	ids := []string{"handoff-test-1", "trace-test-1"}
	fixture.outbox = &sourceHandoffOutbox{
		store: store, deviceID: "ref", historyRoot: filepath.Join(root, "histories"), projectsPath: projectsPath,
		now: time.Now,
		lookupSession: func(scope sessionview.ProfileScope, id string) (sessionview.Detail, bool) {
			if scope != fixture.detail.Summary.Locator.ProfileScope || id != fixture.detail.Summary.Locator.SessionID {
				return sessionview.Detail{}, false
			}
			return fixture.detail, true
		},
		loadProjects: project.LoadConsolidated,
		inspectRepository: func(_ context.Context, cwd string, resolved project.ResolvedProject) (handoff.RepositorySnapshot, error) {
			fixture.inspectedCWD = cwd
			if resolved.Slug != "demo" {
				t.Fatalf("resolved project = %#v", resolved)
			}
			return handoff.RepositorySnapshot{
				ProjectSlug: "demo", RepoSlug: "lin-labs/demo", Branch: "feature/handoff",
				SourceHead: strings.Repeat("a", 40), BaseCommit: strings.Repeat("a", 40), TreeDigest: strings.Repeat("b", 40),
				Cleanliness: handoff.RepositoryClean, Transfer: handoff.TransferRemoteBranch,
			}, nil
		},
		publishHistory: func(root, basename, conversation string) (handoff.HistoryRef, error) {
			if root != fixture.outbox.historyRoot || basename != "session-history.md" || conversation != "conversation-1" {
				t.Fatalf("history inspection root=%q basename=%q conversation=%q", root, basename, conversation)
			}
			return handoff.HistoryRef{
				ArtifactID: "history-" + strings.Repeat("c", 64), Basename: "arcmux-handoff-sha256-" + strings.Repeat("c", 64) + ".md",
				SHA256: strings.Repeat("c", 64), SizeBytes: 128, ConversationID: conversation,
			}, nil
		},
		callPrepare: func(ctx context.Context, peer string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
			fixture.manifest = request.Manifest
			return fixture.remote(ctx, peer, request)
		},
		callLaunch: func(ctx context.Context, peer string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
			return fixture.launchRemote(ctx, peer, request)
		},
		callVerify: func(_ context.Context, _ string, request meshHandoffVerifyRequest) (meshHandoffVerification, error) {
			return meshHandoffVerification{
				HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator,
				VerificationState: "pending",
			}, nil
		},
		closeSource: func(context.Context, handoff.SourceSession, time.Duration) error { return nil },
		newID: func(_ string) (string, error) {
			if len(ids) == 0 {
				return "", errors.New("no id")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
	}
	fixture.remote = func(_ context.Context, peer string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		if peer != "devbox" {
			t.Fatalf("peer=%q", peer)
		}
		queued, err := fixture.store.GetSource(request.Manifest.HandoffID)
		if err != nil || queued.State != handoff.SourcePreparingRemote {
			t.Fatalf("remote called before durable queue: state=%s err=%v", queued.State, err)
		}
		digest, err := request.Manifest.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetPrepared}, nil
	}
	fixture.launchRemote = func(_ context.Context, peer string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
		if peer != "devbox" {
			t.Fatalf("launch peer=%q", peer)
		}
		return meshHandoffStatus{
			HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, State: handoff.TargetAccepted,
			TargetLocator: &handoff.TargetLocator{DeviceID: "devbox", ProfileScope: "root", SessionID: "target-session"},
		}, nil
	}
	return fixture
}

func sourcePrepareRequest() sourceHandoffPrepareRequest {
	return sourceHandoffPrepareRequest{
		ProfileScope: sessionview.RootProfileScope, SessionID: "session-1", TargetPeer: "devbox",
		TargetAgent: "codex", Project: "demo", Goal: "Continue the verified handoff slice.",
		ConversationID: "conversation-1", Validation: handoff.ValidationPassed,
	}
}

func TestSourceHandoffPrepareDerivesImmutableManifestAndPrepares(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	dto, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	if dto.State != handoff.SourceRemotePrepared || dto.Attempts != 1 || dto.TargetDevice != "devbox" || dto.TargetProfile != "codex" || dto.Project != "demo" {
		t.Fatalf("dto = %#v", dto)
	}
	manifest := fixture.manifest
	if manifest.Source.DeviceID != "ref" || manifest.SourceAgent != "codex" || manifest.Source.ProfileScope != "root" ||
		manifest.Source.SessionID != "session-1" || fixture.inspectedCWD != "/actual/source/worktree" {
		t.Fatalf("source derivation manifest=%#v cwd=%q", manifest, fixture.inspectedCWD)
	}
	if manifest.Goal.Provenance != "explicit_operator" || !strings.HasPrefix(manifest.History.Basename, "arcmux-handoff-sha256-") || manifest.Artifacts == nil || len(manifest.Artifacts) != 0 {
		t.Fatalf("derived manifest = %#v", manifest)
	}
	if manifest.Validation.RepositoryRevision != manifest.Repository.SourceHead || manifest.Validation.CompletedAt == nil {
		t.Fatalf("validation not bound to source head: %#v", manifest.Validation)
	}
	stored, err := fixture.store.GetSource(dto.HandoffID)
	if err != nil || stored.State != handoff.SourceRemotePrepared || stored.Digest != dto.ManifestDigest {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
}

func TestSourceHandoffLaunchIsExplicitAndPersistsAcceptedLocator(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	var launchCalls atomic.Int32
	fixture.launchRemote = func(_ context.Context, peer string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
		launchCalls.Add(1)
		return meshHandoffStatus{
			HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, State: handoff.TargetAccepted,
			TargetLocator: &handoff.TargetLocator{DeviceID: peer, ProfileScope: "root", SessionID: "target-session"},
		}, nil
	}
	prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil || prepared.State != handoff.SourceRemotePrepared || launchCalls.Load() != 0 {
		t.Fatalf("prepare=%+v launch_calls=%d err=%v", prepared, launchCalls.Load(), err)
	}
	accepted, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
	if err != nil || accepted.State != handoff.SourceAccepted || accepted.TargetLocator == nil || launchCalls.Load() != 1 {
		t.Fatalf("launch=%+v launch_calls=%d err=%v", accepted, launchCalls.Load(), err)
	}
	replay, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
	if err != nil || replay.TargetLocator == nil || *replay.TargetLocator != *accepted.TargetLocator || launchCalls.Load() != 1 {
		t.Fatalf("replay=%+v launch_calls=%d err=%v", replay, launchCalls.Load(), err)
	}
}

func TestSourceHandoffVerifyRequiresExactRemoteAcknowledgementAndKeepsSource(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	fixture.outbox.closeSource = func(context.Context, handoff.SourceSession, time.Duration) error {
		closed++
		return nil
	}
	fixture.outbox.callVerify = func(context.Context, string, meshHandoffVerifyRequest) (meshHandoffVerification, error) {
		return meshHandoffVerification{}, errors.New("offline")
	}
	offline, err := fixture.outbox.verify(context.Background(), accepted.HandoffID)
	if err != nil || offline.VerificationState != "unavailable" || offline.ContextLoaded || closed != 0 {
		t.Fatalf("offline verification=%+v closed=%d err=%v", offline, closed, err)
	}
	stored, _ := fixture.store.GetSource(accepted.HandoffID)
	if stored.ContextLoaded != nil || stored.Retirement != nil {
		t.Fatalf("offline verification mutated source: %+v", stored)
	}

	fixture.outbox.callVerify = func(_ context.Context, _ string, request meshHandoffVerifyRequest) (meshHandoffVerification, error) {
		wrong := request.TargetLocator
		wrong.SessionID = "wrong-target"
		return meshHandoffVerification{HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, TargetLocator: wrong, VerificationState: "context_loaded", ContextLoaded: true}, nil
	}
	mismatch, err := fixture.outbox.verify(context.Background(), accepted.HandoffID)
	if err != nil || mismatch.VerificationState != "mismatch" || mismatch.ContextLoaded || closed != 0 {
		t.Fatalf("mismatch verification=%+v closed=%d err=%v", mismatch, closed, err)
	}

	ackAt := time.Now().UTC()
	fixture.outbox.callVerify = func(_ context.Context, _ string, request meshHandoffVerifyRequest) (meshHandoffVerification, error) {
		ack := handoff.ContextAcknowledgement{
			Phase: handoff.ContextLoadedPhase, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator, AcknowledgedAt: ackAt,
		}
		return meshHandoffVerification{
			HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator,
			VerificationState: "context_loaded", ContextLoaded: true, Acknowledgement: &ack,
		}, nil
	}
	verified, err := fixture.outbox.verify(context.Background(), accepted.HandoffID)
	if err != nil || !verified.ContextLoaded || verified.VerificationState != "context_loaded" || closed != 0 {
		t.Fatalf("verified=%+v closed=%d err=%v", verified, closed, err)
	}
	stored, _ = fixture.store.GetSource(accepted.HandoffID)
	if stored.ContextLoaded == nil || stored.ContextLoaded.TargetLocator != *accepted.TargetLocator {
		t.Fatalf("stored proof = %+v", stored.ContextLoaded)
	}
}

func TestSourceHandoffRetireClosesOnlyImmutableSourceAfterVerification(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.outbox.retire(context.Background(), accepted.HandoffID, handoff.RetirementImmediate, 10*time.Second); sourceHandoffErrorKindOf(err) != sourceHandoffConflict {
		t.Fatalf("retire before ack err=%v", err)
	}
	ackAt := time.Now().UTC()
	fixture.outbox.callVerify = func(_ context.Context, _ string, request meshHandoffVerifyRequest) (meshHandoffVerification, error) {
		ack := handoff.ContextAcknowledgement{Phase: handoff.ContextLoadedPhase, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator, AcknowledgedAt: ackAt}
		return meshHandoffVerification{HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator, VerificationState: "context_loaded", ContextLoaded: true, Acknowledgement: &ack}, nil
	}
	if _, err := fixture.outbox.verify(context.Background(), accepted.HandoffID); err != nil {
		t.Fatal(err)
	}
	var closed []handoff.SourceSession
	fixture.outbox.closeSource = func(_ context.Context, source handoff.SourceSession, timeout time.Duration) error {
		if timeout != 10*time.Second {
			t.Fatalf("close timeout=%s", timeout)
		}
		closed = append(closed, source)
		return nil
	}
	retired, err := fixture.outbox.retire(context.Background(), accepted.HandoffID, handoff.RetirementImmediate, 10*time.Second)
	if err != nil || retired.RetirementState != "retired" || len(closed) != 1 || closed[0] != fixture.manifest.Source {
		t.Fatalf("retired=%+v closed=%+v err=%v", retired, closed, err)
	}
	replay, err := fixture.outbox.retire(context.Background(), accepted.HandoffID, handoff.RetirementImmediate, 10*time.Second)
	if err != nil || replay.RetirementState != "retired" || len(closed) != 1 {
		t.Fatalf("retire replay=%+v closed=%+v err=%v", replay, closed, err)
	}
}

func TestSourceHandoffAfterTurnRetirementWaitsForNewDurableTurnEnd(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	ackAt := time.Now().UTC()
	fixture.outbox.callVerify = func(_ context.Context, _ string, request meshHandoffVerifyRequest) (meshHandoffVerification, error) {
		ack := handoff.ContextAcknowledgement{Phase: handoff.ContextLoadedPhase, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator, AcknowledgedAt: ackAt}
		return meshHandoffVerification{HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, TargetLocator: request.TargetLocator, VerificationState: "context_loaded", ContextLoaded: true, Acknowledgement: &ack}, nil
	}
	if _, err := fixture.outbox.verify(context.Background(), accepted.HandoffID); err != nil {
		t.Fatal(err)
	}
	baseline := time.Now().UTC()
	fixture.detail.Summary.State = "idle"
	fixture.detail.Turn = &sessionview.TurnActivity{TurnCount: 3, LastTurnEndAt: &baseline}
	requestedAt := baseline.Add(time.Second)
	fixture.outbox.now = func() time.Time { return requestedAt }
	closed := 0
	fixture.outbox.closeSource = func(context.Context, handoff.SourceSession, time.Duration) error {
		closed++
		return nil
	}
	pending, err := fixture.outbox.retire(context.Background(), accepted.HandoffID, handoff.RetirementAfterTurnEnd, 10*time.Second)
	if err != nil || pending.RetirementState != "pending" || closed != 0 {
		t.Fatalf("pending=%+v closed=%d err=%v", pending, closed, err)
	}
	if err := fixture.outbox.reconcile(context.Background(), requestedAt.Add(time.Second)); err != nil || closed != 0 {
		t.Fatalf("idle-only reconciliation closed=%d err=%v", closed, err)
	}
	newEnd := requestedAt.Add(2 * time.Second)
	fixture.detail.Turn = &sessionview.TurnActivity{TurnCount: 4, LastTurnEndAt: &newEnd}
	fixture.outbox.now = func() time.Time { return newEnd }
	if err := fixture.outbox.reconcile(context.Background(), newEnd); err != nil || closed != 1 {
		t.Fatalf("turn-end reconciliation closed=%d err=%v", closed, err)
	}
	stored, _ := fixture.store.GetSource(accepted.HandoffID)
	if stored.Retirement == nil || stored.Retirement.State != handoff.RetirementRetired {
		t.Fatalf("retirement after reconcile=%+v", stored.Retirement)
	}
}

func TestSourceHandoffOperatorAttemptsUseConfiguredTimeout(t *testing.T) {
	t.Run("prepare", func(t *testing.T) {
		fixture := newSourceOutboxFixture(t)
		fixture.outbox.attemptTimeout = 10 * time.Millisecond
		fixture.remote = func(ctx context.Context, _ string, _ meshHandoffPrepareRequest) (meshHandoffStatus, error) {
			<-ctx.Done()
			return meshHandoffStatus{}, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		started := time.Now()
		status, err := fixture.outbox.prepare(ctx, sourcePrepareRequest())
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("prepare after %s: %v", elapsed, err)
		}
		if elapsed >= 100*time.Millisecond {
			t.Fatalf("prepare took %s, want configured attempt timeout", elapsed)
		}
		if status.State != handoff.SourceRetryWait || status.Failure == nil || !status.Failure.Retryable {
			t.Fatalf("prepare status = %+v", status)
		}
	})

	t.Run("launch", func(t *testing.T) {
		fixture := newSourceOutboxFixture(t)
		prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
		if err != nil {
			t.Fatal(err)
		}
		fixture.outbox.attemptTimeout = 10 * time.Millisecond
		fixture.launchRemote = func(ctx context.Context, _ string, _ meshHandoffLaunchRequest) (meshHandoffStatus, error) {
			<-ctx.Done()
			return meshHandoffStatus{}, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		started := time.Now()
		status, err := fixture.outbox.launch(ctx, prepared.HandoffID)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("launch after %s: %v", elapsed, err)
		}
		if elapsed >= 100*time.Millisecond {
			t.Fatalf("launch took %s, want configured attempt timeout", elapsed)
		}
		if status.State != handoff.SourceLaunchRetryWait || status.Failure == nil || !status.Failure.Retryable {
			t.Fatalf("launch status = %+v", status)
		}
	})
}

func TestSourceHandoffLaunchMissingGrantRetriesSamePreparedHandoff(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.launchRemote = func(context.Context, string, meshHandoffLaunchRequest) (meshHandoffStatus, error) {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorPermissionDenied, Message: "private remote text"}
	}
	waiting, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
	if err != nil || waiting.State != handoff.SourceLaunchRetryWait || waiting.Failure == nil ||
		waiting.Failure.Code != handoff.FailureUnauthorized || !waiting.Failure.Retryable {
		t.Fatalf("missing grant=%+v err=%v", waiting, err)
	}
	fixture.launchRemote = func(_ context.Context, peer string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
		return meshHandoffStatus{
			HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, State: handoff.TargetAccepted,
			TargetLocator: &handoff.TargetLocator{DeviceID: peer, ProfileScope: "root", SessionID: "target-session"},
		}, nil
	}
	accepted, err := fixture.outbox.retry(context.Background(), prepared.HandoffID)
	if err != nil || accepted.HandoffID != prepared.HandoffID || accepted.State != handoff.SourceAccepted {
		t.Fatalf("grant retry=%+v err=%v", accepted, err)
	}
}

func TestSourceHandoffLaunchRejectsUnsafeAcceptedLocator(t *testing.T) {
	for _, locator := range []*handoff.TargetLocator{
		nil,
		{DeviceID: "devbox", ProfileScope: "profile:codex", SessionID: "target-session"},
		{DeviceID: "devbox", ProfileScope: "root", SessionID: "../unsafe"},
	} {
		fixture := newSourceOutboxFixture(t)
		prepared, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
		if err != nil {
			t.Fatal(err)
		}
		fixture.launchRemote = func(_ context.Context, _ string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
			return meshHandoffStatus{HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, State: handoff.TargetAccepted, TargetLocator: locator}, nil
		}
		failed, err := fixture.outbox.launch(context.Background(), prepared.HandoffID)
		if err != nil || failed.State != handoff.SourceFailed || failed.Failure == nil || failed.Failure.Code != handoff.FailureConflict {
			t.Fatalf("locator=%+v failed=%+v err=%v", locator, failed, err)
		}
	}
}

func TestSourceLaunchReconcileNeverMutatesSourceSession(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	_, manifest := newHandoffTestApplication(t, true)
	manifest.HandoffID = "source-launch-preserves-session"
	manifest.Target.DeviceID = "devbox"
	seedSourceHandoffState(t, fixture.store, manifest, handoff.SourceLaunchingRemote, time.Time{})
	fixture.outbox.callLaunch = func(_ context.Context, peer string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
		return meshHandoffStatus{
			HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, State: handoff.TargetAccepted,
			TargetLocator: &handoff.TargetLocator{DeviceID: peer, ProfileScope: "root", SessionID: "target-session"},
		}, nil
	}
	d := newMeshApplicationTestDaemon(t, "ref")
	secretCWD := filepath.Join(t.TempDir(), "source-worktree")
	source := session.NewSession(manifest.Source.SessionID, "source remains live", manifest.SourceAgent, secretCWD)
	source.SetCurrentCommand("source is still processing this exact command")
	source.SetState(session.StateWorking)
	d.mu.Lock()
	d.sessions[source.Snapshot().ID] = source
	d.mu.Unlock()
	before := source.Snapshot()
	if err := fixture.outbox.reconcile(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	accepted, err := fixture.store.GetSource(manifest.HandoffID)
	if err != nil || accepted.State != handoff.SourceAccepted {
		t.Fatalf("source record=%+v err=%v", accepted, err)
	}
	kept, ok := d.GetSession(source.Snapshot().ID)
	after := kept.Snapshot()
	if !ok || kept != source || after.State != before.State || after.CurrentCommand != before.CurrentCommand || after.CWD != before.CWD {
		t.Fatalf("source session mutated before=%+v after=%+v same=%t ok=%t", before, after, kept == source, ok)
	}
}

func TestSourceHandoffPreparePublishesHistoryBeforeQueueAndSurvivesLaterTurns(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	if err := os.Mkdir(fixture.outbox.historyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("# Session\n\nStable handoff point.\n")
	originalPath := filepath.Join(fixture.outbox.historyRoot, "session-history.md")
	if err := os.WriteFile(originalPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.outbox.publishHistory = handoff.PublishSourceHistory
	fixture.remote = func(_ context.Context, _ string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		if request.Manifest.History.Basename == "session-history.md" || !strings.HasPrefix(request.Manifest.History.Basename, "arcmux-handoff-sha256-") {
			t.Fatalf("remote received mutable history ref: %#v", request.Manifest.History)
		}
		if _, err := os.Lstat(filepath.Join(fixture.outbox.historyRoot, request.Manifest.History.Basename)); err != nil {
			t.Fatalf("remote called before history publication: %v", err)
		}
		queued, err := fixture.store.GetSource(request.Manifest.HandoffID)
		if err != nil || queued.Manifest.History.Basename != request.Manifest.History.Basename {
			t.Fatalf("remote called before durable manifest queue: queued=%#v err=%v", queued, err)
		}
		digest, _ := request.Manifest.Digest()
		return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetPrepared}, nil
	}
	dto, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(originalPath, []byte("# Session\n\nCompletely rewritten later turn.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.store.GetSource(dto.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := handoff.SnapshotHistory(fixture.outbox.historyRoot, private, record.Manifest.HandoffID, record.Manifest.History)
	if err != nil {
		t.Fatalf("target snapshot after later source turn: %v", err)
	}
	got, err := os.ReadFile(snapshot)
	if err != nil || string(got) != string(original) {
		t.Fatalf("target snapshot=%q err=%v, want %q", got, err, original)
	}
}

func TestSourceHandoffOfflineQueuesAndExplicitRetryUsesSameManifest(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	calls := 0
	fixture.remote = func(_ context.Context, _ string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		calls++
		if calls == 1 {
			return meshHandoffStatus{}, arcmuxmesh.ErrPeerDisconnected
		}
		digest, _ := request.Manifest.Digest()
		return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetPrepared}, nil
	}
	queued, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != handoff.SourceRetryWait || queued.Failure == nil || !queued.Failure.Retryable || queued.Attempts != 1 {
		t.Fatalf("offline state = %#v", queued)
	}
	firstManifest := fixture.manifest
	prepared, err := fixture.outbox.retry(context.Background(), queued.HandoffID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State != handoff.SourceRemotePrepared || prepared.Attempts != 2 || fixture.manifest.HandoffID != firstManifest.HandoffID {
		t.Fatalf("retry state=%#v first=%s second=%s", prepared, firstManifest.HandoffID, fixture.manifest.HandoffID)
	}
	firstDigest, _ := firstManifest.Digest()
	secondDigest, _ := fixture.manifest.Digest()
	if firstDigest != secondDigest {
		t.Fatal("retry changed immutable manifest")
	}
}

func TestSourceHandoffRemoteOutcomesAreDeterministic(t *testing.T) {
	t.Run("permission fails", func(t *testing.T) {
		fixture := newSourceOutboxFixture(t)
		fixture.remote = func(context.Context, string, meshHandoffPrepareRequest) (meshHandoffStatus, error) {
			return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorPermissionDenied, Message: "sensitive remote text"}
		}
		dto, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
		if err != nil {
			t.Fatal(err)
		}
		if dto.State != handoff.SourceFailed || dto.Failure == nil || dto.Failure.Code != handoff.FailureUnauthorized || strings.Contains(dto.Failure.Message, "sensitive") {
			t.Fatalf("permission outcome = %#v", dto)
		}
	})

	t.Run("target waits", func(t *testing.T) {
		fixture := newSourceOutboxFixture(t)
		fixture.remote = func(_ context.Context, _ string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
			digest, _ := request.Manifest.Digest()
			return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetWaitingAssets}, nil
		}
		dto, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
		if err != nil {
			t.Fatal(err)
		}
		if dto.State != handoff.SourceRetryWait || dto.Failure == nil || dto.Failure.Code != handoff.FailureMissingAsset {
			t.Fatalf("waiting outcome = %#v", dto)
		}
	})
}

func TestSourceHandoffRejectsUnknownNonlocalUnsafeAndSelf(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*sourceOutboxFixture, *sourceHandoffPrepareRequest)
	}{
		{"unknown session", func(_ *sourceOutboxFixture, request *sourceHandoffPrepareRequest) { request.SessionID = "absent" }},
		{"unknown project", func(_ *sourceOutboxFixture, request *sourceHandoffPrepareRequest) { request.Project = "absent" }},
		{"self target", func(_ *sourceOutboxFixture, request *sourceHandoffPrepareRequest) { request.TargetPeer = "ref" }},
		{"secret goal", func(_ *sourceOutboxFixture, request *sourceHandoffPrepareRequest) {
			request.Goal = "API_KEY=sk_supersecretvalue"
		}},
		{"missing history", func(f *sourceOutboxFixture, _ *sourceHandoffPrepareRequest) { f.detail.Summary.History = nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSourceOutboxFixture(t)
			request := sourcePrepareRequest()
			test.mutate(fixture, &request)
			if _, err := fixture.outbox.prepare(context.Background(), request); sourceHandoffErrorKindOf(err) != sourceHandoffInvalid {
				t.Fatalf("error=%v kind=%s", err, sourceHandoffErrorKindOf(err))
			}
			records, err := fixture.store.ListSource()
			if err != nil || len(records) != 0 {
				t.Fatalf("invalid request queued records=%d err=%v", len(records), err)
			}
		})
	}
}

func TestSourceHandoffDTORedactsManifest(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	dto, err := fixture.outbox.prepare(context.Background(), sourcePrepareRequest())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	for _, forbidden := range []string{"Continue the verified", "session-history.md", "feature/handoff", "/actual/source", "conversation-1", `"goal"`, `"history"`, `"repository"`} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("redacted DTO contains %q: %s", forbidden, encoded)
		}
	}
}

func TestSourceHandoffReconcileResumesPreparedWorkAndAuthorizedLaunches(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	_, base := newHandoffTestApplication(t, true)
	scanAt := time.Now().UTC().Add(time.Minute)
	fixture.outbox.now = func() time.Time { return scanAt }

	states := []struct {
		id        string
		state     handoff.SourceState
		nextRetry time.Time
	}{
		{id: "reconcile-queued", state: handoff.SourceQueued},
		{id: "reconcile-preparing", state: handoff.SourcePreparingRemote},
		{id: "reconcile-due", state: handoff.SourceRetryWait, nextRetry: scanAt.Add(-time.Second)},
		{id: "reconcile-future", state: handoff.SourceRetryWait, nextRetry: scanAt.Add(time.Hour)},
		{id: "reconcile-prepared", state: handoff.SourceRemotePrepared},
		{id: "reconcile-launching", state: handoff.SourceLaunchingRemote},
		{id: "reconcile-launch-retry", state: handoff.SourceLaunchRetryWait, nextRetry: scanAt.Add(-time.Second)},
		{id: "reconcile-accepted", state: handoff.SourceAccepted},
		{id: "reconcile-failed", state: handoff.SourceFailed},
	}
	for _, item := range states {
		manifest := base
		manifest.HandoffID = item.id
		seedSourceHandoffState(t, fixture.store, manifest, item.state, item.nextRetry)
	}

	var mu sync.Mutex
	calls := make(map[string]int)
	launchCalls := make(map[string]int)
	fixture.outbox.callPrepare = func(_ context.Context, _ string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		mu.Lock()
		calls[request.Manifest.HandoffID]++
		mu.Unlock()
		digest, err := request.Manifest.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetPrepared}, nil
	}
	fixture.outbox.callLaunch = func(_ context.Context, _ string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
		mu.Lock()
		launchCalls[request.HandoffID]++
		mu.Unlock()
		return meshHandoffStatus{
			HandoffID: request.HandoffID, ManifestDigest: request.ManifestDigest, State: handoff.TargetAccepted,
			TargetLocator: &handoff.TargetLocator{DeviceID: "server", ProfileScope: "root", SessionID: "target-session"},
		}, nil
	}
	if err := fixture.outbox.reconcile(context.Background(), scanAt); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"reconcile-queued", "reconcile-preparing", "reconcile-due"} {
		record, err := fixture.store.GetSource(id)
		if err != nil || record.State != handoff.SourceRemotePrepared || calls[id] != 1 {
			t.Fatalf("resumed %s record=%+v calls=%d err=%v", id, record, calls[id], err)
		}
	}
	for _, id := range []string{"reconcile-launching", "reconcile-launch-retry"} {
		record, err := fixture.store.GetSource(id)
		if err != nil || record.State != handoff.SourceAccepted || launchCalls[id] != 1 || record.TargetLocator == nil {
			t.Fatalf("resumed launch %s record=%+v calls=%d err=%v", id, record, launchCalls[id], err)
		}
	}
	for _, item := range []struct {
		id    string
		state handoff.SourceState
	}{
		{id: "reconcile-future", state: handoff.SourceRetryWait},
		{id: "reconcile-prepared", state: handoff.SourceRemotePrepared},
		{id: "reconcile-accepted", state: handoff.SourceAccepted},
		{id: "reconcile-failed", state: handoff.SourceFailed},
	} {
		record, err := fixture.store.GetSource(item.id)
		if err != nil || record.State != item.state || calls[item.id] != 0 || launchCalls[item.id] != 0 {
			t.Fatalf("inert %s record=%+v prepare_calls=%d launch_calls=%d err=%v", item.id, record, calls[item.id], launchCalls[item.id], err)
		}
	}
}

func TestSourceHandoffReconcileSerializesConcurrentPassesPerID(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	_, manifest := newHandoffTestApplication(t, true)
	manifest.HandoffID = "reconcile-concurrent"
	seedSourceHandoffState(t, fixture.store, manifest, handoff.SourceQueued, time.Time{})
	scanAt := time.Now().UTC().Add(time.Minute)
	fixture.outbox.now = func() time.Time { return scanAt }

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fixture.outbox.callPrepare = func(_ context.Context, _ string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		digest, err := request.Manifest.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetPrepared}, nil
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- fixture.outbox.reconcile(context.Background(), scanAt)
		}()
	}
	close(start)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first recovery RPC did not start")
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("remote calls = %d, want 1", got)
	}
	record, err := fixture.store.GetSource(manifest.HandoffID)
	if err != nil || record.State != handoff.SourceRemotePrepared {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestSourceHandoffReconcileTimesOutBlockedPeerAndDrainsNextID(t *testing.T) {
	fixture := newSourceOutboxFixture(t)
	_, base := newHandoffTestApplication(t, true)
	blocked := base
	blocked.HandoffID = "reconcile-timeout-a"
	next := base
	next.HandoffID = "reconcile-timeout-b"
	seedSourceHandoffState(t, fixture.store, blocked, handoff.SourceQueued, time.Time{})
	seedSourceHandoffState(t, fixture.store, next, handoff.SourceQueued, time.Time{})
	scanAt := time.Now().UTC().Add(time.Minute)
	fixture.outbox.now = func() time.Time { return scanAt }
	fixture.outbox.attemptTimeout = 25 * time.Millisecond

	var calls atomic.Int32
	fixture.outbox.callPrepare = func(ctx context.Context, _ string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
		calls.Add(1)
		if request.Manifest.HandoffID == blocked.HandoffID {
			<-ctx.Done()
			return meshHandoffStatus{}, ctx.Err()
		}
		digest, err := request.Manifest.Digest()
		if err != nil {
			return meshHandoffStatus{}, err
		}
		return meshHandoffStatus{HandoffID: request.Manifest.HandoffID, ManifestDigest: digest, State: handoff.TargetPrepared}, nil
	}
	started := time.Now()
	if err := fixture.outbox.reconcile(context.Background(), scanAt); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked recovery took %s", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("remote calls = %d, want 2", got)
	}
	blockedRecord, err := fixture.store.GetSource(blocked.HandoffID)
	if err != nil || blockedRecord.State != handoff.SourceRetryWait || blockedRecord.Failure == nil || !blockedRecord.Failure.Retryable {
		t.Fatalf("blocked record=%+v err=%v", blockedRecord, err)
	}
	nextRecord, err := fixture.store.GetSource(next.HandoffID)
	if err != nil || nextRecord.State != handoff.SourceRemotePrepared {
		t.Fatalf("next record=%+v err=%v", nextRecord, err)
	}
}

func seedSourceHandoffState(t *testing.T, store *handoff.Store, manifest handoff.Manifest, state handoff.SourceState, nextRetry time.Time) handoff.SourceRecord {
	t.Helper()
	record, replay, err := store.QueueSource(manifest)
	if err != nil || replay {
		t.Fatalf("queue %s replay=%t err=%v", manifest.HandoffID, replay, err)
	}
	if state == handoff.SourceQueued {
		return record
	}
	transition := func(next handoff.SourceState, detail handoff.Transition) {
		var transitionErr error
		record, transitionErr = store.TransitionSource(manifest.HandoffID, record.Revision, next, detail)
		if transitionErr != nil {
			t.Fatalf("transition %s to %s: %v", manifest.HandoffID, next, transitionErr)
		}
	}
	if state == handoff.SourceFailed {
		failure := &handoff.Failure{Code: handoff.FailureVerification, Message: "test failure", At: record.Updated}
		transition(handoff.SourceFailed, handoff.Transition{At: record.Updated, Failure: failure})
		return record
	}
	transition(handoff.SourcePreparingRemote, handoff.Transition{At: record.Updated})
	if state == handoff.SourcePreparingRemote {
		return record
	}
	if state == handoff.SourceRetryWait {
		failure := &handoff.Failure{Code: handoff.FailureUnavailable, Message: "test retry", Retryable: true, At: record.Updated}
		transition(handoff.SourceRetryWait, handoff.Transition{At: record.Updated, NextRetry: &nextRetry, Failure: failure})
		return record
	}
	transition(handoff.SourceRemotePrepared, handoff.Transition{At: record.Updated})
	if state == handoff.SourceRemotePrepared {
		return record
	}
	transition(handoff.SourceLaunchingRemote, handoff.Transition{At: record.Updated})
	if state == handoff.SourceLaunchingRemote {
		return record
	}
	if state == handoff.SourceLaunchRetryWait {
		failure := &handoff.Failure{Code: handoff.FailureUnavailable, Message: "test launch retry", Retryable: true, At: record.Updated}
		transition(handoff.SourceLaunchRetryWait, handoff.Transition{At: record.Updated, NextRetry: &nextRetry, Failure: failure})
		return record
	}
	if state == handoff.SourceAccepted {
		locator := &handoff.TargetLocator{DeviceID: manifest.Target.DeviceID, ProfileScope: "profile:codex", SessionID: "target-session"}
		transition(handoff.SourceAccepted, handoff.Transition{At: record.Updated, TargetLocator: locator})
		return record
	}
	t.Fatalf("unsupported source state %s", state)
	return handoff.SourceRecord{}
}
