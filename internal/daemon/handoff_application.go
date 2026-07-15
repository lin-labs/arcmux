package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/project"
)

const handoffAssetRetryDelay = 15 * time.Second

type meshHandoffPrepareRequest struct {
	Manifest handoff.Manifest `json:"manifest"`
}

type meshHandoffStatusRequest struct {
	HandoffID string `json:"handoff_id"`
}

// meshHandoffStatus is the complete handoff response allowlist. In particular,
// it never includes the manifest, goal, history locator, repository locator,
// target-local path, or remote URL.
type meshHandoffStatus struct {
	HandoffID      string                 `json:"handoff_id"`
	ManifestDigest string                 `json:"manifest_digest"`
	State          handoff.TargetState    `json:"state"`
	Attempts       uint32                 `json:"attempts"`
	Failure        *meshHandoffFailureDTO `json:"failure,omitempty"`
}

type meshHandoffFailureDTO struct {
	Code      handoff.FailureCode `json:"code"`
	Message   string              `json:"message"`
	Retryable bool                `json:"retryable"`
}

type repositoryPreparer func(context.Context, handoff.Manifest, project.ResolvedProject) error

type handoffPrepareLock struct {
	mu   sync.Mutex
	refs int
}

type handoffApplication struct {
	store             *handoff.Store
	historyRoot       string
	projectsPath      string
	now               func() time.Time
	snapshotHistory   func(string, string, string, handoff.HistoryRef) (string, error)
	loadProjects      func(string) (*project.ConsolidatedRegistry, error)
	prepareRepository repositoryPreparer
	prepareMu         sync.Mutex
	prepareLocks      map[string]*handoffPrepareLock
}

func newHandoffApplication(store *handoff.Store) *handoffApplication {
	return &handoffApplication{
		store:             store,
		historyRoot:       defaultHandoffHistoryRoot(),
		projectsPath:      project.DefaultConsolidatedPath(),
		now:               time.Now,
		snapshotHistory:   handoff.SnapshotHistory,
		loadProjects:      project.LoadConsolidated,
		prepareRepository: prepareHandoffRepository,
		prepareLocks:      make(map[string]*handoffPrepareLock),
	}
}

func defaultHandoffHistoryRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("~", "agents", "histories")
	}
	return filepath.Join(home, "agents", "histories")
}

func (d *Daemon) handoffApplication() (*handoffApplication, error) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || app.handoffs == nil || app.handoffs.store == nil {
		return nil, errors.New("handoff state is unavailable")
	}
	return app.handoffs, nil
}

func (a *handoffApplication) prepare(ctx context.Context, principal arcmuxmesh.Principal, localDeviceID string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
	manifest := request.Manifest
	if err := manifest.Validate(); err != nil {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("invalid handoff manifest"))
	}
	if principal.PeerID == "" || principal.PeerID != manifest.Source.DeviceID {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorPermissionDenied, Message: "handoff source does not match authenticated peer"}
	}
	if localDeviceID == "" || manifest.Target.DeviceID != localDeviceID {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff target does not match this device"))
	}
	release := a.lockPrepare(manifest.HandoffID)
	defer release()

	record, replay, err := a.store.ReceiveTarget(manifest)
	if err != nil {
		if errors.Is(err, handoff.ErrManifestConflict) {
			return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff id conflicts with an existing manifest"))
		}
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to persist handoff"}
	}
	if replay {
		switch record.State {
		case handoff.TargetReceived, handoff.TargetWaitingAssets:
			// An explicit duplicate prepare accelerates a retry. This also
			// resumes a record persisted immediately before a process restart.
		case handoff.TargetValidating:
			// Resume validation after a process restart without an illegal
			// self-transition or a second attempt increment.
		case handoff.TargetPrepared, handoff.TargetRejected, handoff.TargetLaunching, handoff.TargetAccepted:
			return handoffStatusDTO(record), nil
		default:
			return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "handoff has an unsupported state"}
		}
	}

	if record.State == handoff.TargetReceived || record.State == handoff.TargetWaitingAssets {
		record, err = a.store.TransitionTarget(manifest.HandoffID, record.Revision, handoff.TargetValidating, handoff.Transition{})
		if err != nil {
			return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to validate handoff"}
		}
	}

	registry, err := a.loadProjects(a.projectsPath)
	if err != nil {
		return a.reject(record, handoff.FailureVerification, "target project registry is invalid")
	}
	resolvedProject, ok := registry.ResolveProject(manifest.Repository.ProjectSlug)
	if !ok {
		return a.reject(record, handoff.FailureVerification, "target project is not registered")
	}

	if _, err := a.snapshotHistory(a.historyRoot, a.store.Root(), manifest.HandoffID, manifest.History); err != nil {
		if code, ok := handoff.HistoryErrorCodeOf(err); ok && code == handoff.HistoryErrorRetryable {
			return a.waitForAssets(record, "synced session history is not available")
		}
		return a.reject(record, handoff.FailureInvalidManifest, "session history reference is invalid")
	}

	if err := a.prepareRepository(ctx, manifest, resolvedProject); err != nil {
		return a.classifyRepositoryFailure(record, err)
	}

	prepared, err := a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetPrepared, handoff.Transition{})
	if err != nil {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to prepare handoff"}
	}
	return handoffStatusDTO(prepared), nil
}

func (a *handoffApplication) lockPrepare(id string) func() {
	a.prepareMu.Lock()
	entry := a.prepareLocks[id]
	if entry == nil {
		entry = &handoffPrepareLock{}
		a.prepareLocks[id] = entry
	}
	entry.refs++
	a.prepareMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		a.prepareMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(a.prepareLocks, id)
		}
		a.prepareMu.Unlock()
	}
}

func (a *handoffApplication) status(_ context.Context, principal arcmuxmesh.Principal, request meshHandoffStatusRequest) (meshHandoffStatus, error) {
	if request.HandoffID == "" {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff_id is required"))
	}
	record, err := a.store.GetTarget(request.HandoffID)
	if err != nil {
		if errors.Is(err, handoff.ErrNotFound) {
			return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff not found"))
		}
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("invalid handoff_id"))
	}
	if principal.PeerID == "" || record.Manifest.Source.DeviceID != principal.PeerID {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorPermissionDenied, Message: "handoff belongs to a different authenticated peer"}
	}
	return handoffStatusDTO(record), nil
}

func (a *handoffApplication) waitForAssets(record handoff.TargetRecord, message string) (meshHandoffStatus, error) {
	now := a.now().UTC()
	next := now.Add(handoffAssetRetryDelay)
	failure := &handoff.Failure{Code: handoff.FailureMissingAsset, Message: message, Retryable: true, At: now}
	waiting, err := a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetWaitingAssets, handoff.Transition{
		At: now, NextRetry: &next, Failure: failure,
	})
	if err != nil {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to record waiting handoff"}
	}
	return handoffStatusDTO(waiting), nil
}

func (a *handoffApplication) reject(record handoff.TargetRecord, code handoff.FailureCode, message string) (meshHandoffStatus, error) {
	now := a.now().UTC()
	failure := &handoff.Failure{Code: code, Message: message, Retryable: false, At: now}
	rejected, err := a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetRejected, handoff.Transition{At: now, Failure: failure})
	if err != nil {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to reject handoff"}
	}
	return handoffStatusDTO(rejected), nil
}

func (a *handoffApplication) classifyRepositoryFailure(record handoff.TargetRecord, err error) (meshHandoffStatus, error) {
	if code, ok := handoff.RepositoryErrorCodeOf(err); ok && code == handoff.RepositoryErrorRetryable {
		return a.waitForAssets(record, "repository snapshot is not available")
	}
	return a.reject(record, handoff.FailureVerification, "repository snapshot did not match the target project")
}

func handoffStatusDTO(record handoff.TargetRecord) meshHandoffStatus {
	status := meshHandoffStatus{
		HandoffID: record.Manifest.HandoffID, ManifestDigest: record.Digest,
		State: record.State, Attempts: record.Attempts,
	}
	if record.Failure != nil {
		status.Failure = &meshHandoffFailureDTO{
			Code: record.Failure.Code, Message: record.Failure.Message, Retryable: record.Failure.Retryable,
		}
	}
	return status
}

func prepareHandoffRepository(ctx context.Context, manifest handoff.Manifest, resolved project.ResolvedProject) error {
	_, err := handoff.PrepareRepository(ctx, manifest, resolved)
	return err
}
