package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

type sourceOutboxFixture struct {
	outbox       *sourceHandoffOutbox
	store        *handoff.Store
	detail       sessionview.Detail
	remote       func(context.Context, string, meshHandoffPrepareRequest) (meshHandoffStatus, error)
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
		inspectHistory: func(root, basename, conversation string) (handoff.HistoryRef, error) {
			if root != fixture.outbox.historyRoot || basename != "session-history.md" || conversation != "conversation-1" {
				t.Fatalf("history inspection root=%q basename=%q conversation=%q", root, basename, conversation)
			}
			return handoff.HistoryRef{
				ArtifactID: "history-" + strings.Repeat("c", 64), Basename: basename,
				SHA256: strings.Repeat("c", 64), SizeBytes: 128, ConversationID: conversation,
			}, nil
		},
		callPrepare: func(ctx context.Context, peer string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
			fixture.manifest = request.Manifest
			return fixture.remote(ctx, peer, request)
		},
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
	if dto.State != handoff.SourceRemotePrepared || dto.Attempts != 1 || dto.TargetDevice != "devbox" || dto.Project != "demo" {
		t.Fatalf("dto = %#v", dto)
	}
	manifest := fixture.manifest
	if manifest.Source.DeviceID != "ref" || manifest.SourceAgent != "codex" || manifest.Source.ProfileScope != "root" ||
		manifest.Source.SessionID != "session-1" || fixture.inspectedCWD != "/actual/source/worktree" {
		t.Fatalf("source derivation manifest=%#v cwd=%q", manifest, fixture.inspectedCWD)
	}
	if manifest.Goal.Provenance != "explicit_operator" || manifest.History.Basename != "session-history.md" || manifest.Artifacts == nil || len(manifest.Artifacts) != 0 {
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
