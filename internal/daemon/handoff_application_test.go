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
	"sync/atomic"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/project"
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
		Artifacts: []handoff.ArtifactRef{{
			Kind: handoff.ArtifactSessionHistory, ID: "history-1",
			Session: &handoff.ArtifactSessionRef{ProfileScope: "profile:codex", SessionID: "session-1"},
		}},
		Validation: handoff.ValidationEvidence{State: handoff.ValidationNotRun},
		CreatedAt:  now,
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("test manifest: %v", err)
	}
	app := newHandoffApplication(store)
	app.historyRoot = historyRoot
	app.projectsPath = projectsPath
	app.prepareRepository = func(context.Context, handoff.Manifest, project.ResolvedProject) error { return nil }
	return app, manifest
}
