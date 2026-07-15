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
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/project"
)

const (
	handoffAssetRetryDelay     = 15 * time.Second
	targetHandoffResumeTimeout = 10 * time.Second
)

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
type targetProfileValidator func(string) error

type handoffPrepareLock struct {
	token chan struct{}
	refs  int
}

type handoffApplication struct {
	store             *handoff.Store
	historyRoot       string
	projectsPath      string
	now               func() time.Time
	snapshotHistory   func(string, string, string, handoff.HistoryRef) (string, error)
	loadProjects      func(string) (*project.ConsolidatedRegistry, error)
	prepareRepository repositoryPreparer
	validateProfile   targetProfileValidator
	resumeTimeout     time.Duration
	prepareMu         sync.Mutex
	prepareLocks      map[string]*handoffPrepareLock
}

func newHandoffApplication(store *handoff.Store, profiles map[string]profile.Profile) *handoffApplication {
	return &handoffApplication{
		store:             store,
		historyRoot:       defaultHandoffHistoryRoot(),
		projectsPath:      project.DefaultConsolidatedPath(),
		now:               time.Now,
		snapshotHistory:   handoff.SnapshotHistory,
		loadProjects:      project.LoadConsolidated,
		prepareRepository: prepareHandoffRepository,
		resumeTimeout:     targetHandoffResumeTimeout,
		validateProfile: func(name string) error {
			return validateHandoffTargetProfile(profiles, name)
		},
		prepareLocks: make(map[string]*handoffPrepareLock),
	}
}

func validateHandoffTargetProfile(profiles map[string]profile.Profile, name string) error {
	target, ok := profiles[name]
	if !ok {
		return errors.New("target agent profile is not configured")
	}
	if target.Transport != profile.TransportTmux || target.StartCommand == "" {
		return errors.New("target agent profile is not available for supervised launch")
	}
	return nil
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

func (d *Daemon) scheduleTargetHandoffReconcile() {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.runtimeMu.Lock()
	wake := app.handoffWake
	app.runtimeMu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
		// One serialized worker drains the complete durable recoverable set,
		// so additional wakeups can safely coalesce while a pass is pending.
	}
}

func (d *Daemon) scheduleSourceHandoffReconcile() {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.runtimeMu.Lock()
	wake := app.sourceHandoffWake
	app.runtimeMu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
		// One serialized source worker drains the durable prepare outbox.
	}
}

// reconcileTargetHandoffs resumes only prepare-phase target work. Launching
// and launch-retry states are deliberately reserved for the future launch
// driver even though the store exposes them in its broader recovery query.
func (d *Daemon) reconcileTargetHandoffs(ctx context.Context, at time.Time) {
	app, err := d.handoffApplication()
	if err != nil {
		return
	}
	records, err := app.store.RecoverableTarget(at)
	if err != nil {
		d.logger.Warn("target handoff recovery scan failed")
		return
	}
	resumeTimeout := app.resumeTimeout
	if resumeTimeout <= 0 {
		resumeTimeout = targetHandoffResumeTimeout
	}
	for _, candidate := range records {
		if ctx.Err() != nil {
			return
		}
		switch candidate.State {
		case handoff.TargetReceived, handoff.TargetValidating, handoff.TargetWaitingAssets:
		default:
			continue
		}

		resumeCtx, cancel := context.WithTimeout(ctx, resumeTimeout)
		release, lockErr := app.lockPrepareContext(resumeCtx, candidate.Manifest.HandoffID)
		if lockErr != nil {
			cancel()
			if ctx.Err() != nil {
				return
			}
			d.logger.Warn("target handoff recovery attempt timed out",
				"handoff_id", candidate.Manifest.HandoffID,
				"state", candidate.State)
			continue
		}
		current, getErr := app.store.GetTarget(candidate.Manifest.HandoffID)
		if getErr != nil {
			release()
			cancel()
			d.logger.Warn("target handoff recovery reload failed", "handoff_id", candidate.Manifest.HandoffID)
			continue
		}
		if current.State == handoff.TargetWaitingAssets && (current.NextRetry == nil || current.NextRetry.After(at)) {
			release()
			cancel()
			continue
		}
		switch current.State {
		case handoff.TargetReceived, handoff.TargetValidating, handoff.TargetWaitingAssets:
			result := make(chan error, 1)
			go func() {
				_, resumeErr := app.resumeTarget(resumeCtx, current, d.meshDeviceID())
				release()
				result <- resumeErr
			}()
			var resumeErr error
			select {
			case resumeErr = <-result:
			case <-resumeCtx.Done():
				resumeErr = resumeCtx.Err()
			}
			cancel()
			if resumeErr != nil && ctx.Err() == nil {
				d.logger.Warn("target handoff recovery attempt failed",
					"handoff_id", current.Manifest.HandoffID,
					"state", current.State,
					"error_code", safeMeshRPCErrorCode(resumeErr))
			}
		default:
			release()
			cancel()
		}
	}
}

func safeMeshRPCErrorCode(err error) string {
	var rpcErr *arcmuxmesh.RPCError
	if errors.As(err, &rpcErr) && rpcErr.Code != "" {
		return rpcErr.Code
	}
	return arcmuxmesh.ErrorInternal
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
	if len(manifest.Artifacts) != 0 {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff artifacts are not supported"))
	}
	release, err := a.lockPrepareContext(ctx, manifest.HandoffID)
	if err != nil {
		return meshHandoffStatus{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return meshHandoffStatus{}, err
	}

	record, _, err := a.store.ReceiveTarget(manifest)
	if err != nil {
		if errors.Is(err, handoff.ErrManifestConflict) {
			return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff id conflicts with an existing manifest"))
		}
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to persist handoff"}
	}
	return a.resumeTarget(ctx, record, localDeviceID)
}

// resumeTarget performs the target-local preparation shared by inbound RPCs
// and restart reconciliation. The caller must hold lockPrepare for this ID.
func (a *handoffApplication) resumeTarget(ctx context.Context, record handoff.TargetRecord, localDeviceID string) (meshHandoffStatus, error) {
	if err := ctx.Err(); err != nil {
		return meshHandoffStatus{}, err
	}
	switch record.State {
	case handoff.TargetReceived, handoff.TargetValidating, handoff.TargetWaitingAssets:
		if len(record.Manifest.Artifacts) != 0 {
			return a.reject(record, handoff.FailureInvalidManifest, "handoff artifacts are not supported")
		}
	}
	switch record.State {
	case handoff.TargetReceived, handoff.TargetWaitingAssets:
		var err error
		at := targetTransitionTime(a.now, record.Updated)
		record, err = a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetValidating, handoff.Transition{At: at})
		if err != nil {
			return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to validate handoff"}
		}
	case handoff.TargetValidating:
		// Resume validation after a process restart without an illegal
		// self-transition or a second attempt increment.
	case handoff.TargetPrepared, handoff.TargetRejected, handoff.TargetLaunching, handoff.TargetLaunchWaitingAssets, handoff.TargetAccepted:
		return handoffStatusDTO(record), nil
	default:
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "handoff has an unsupported state"}
	}

	if localDeviceID == "" || record.Manifest.Target.DeviceID != localDeviceID {
		return a.reject(record, handoff.FailureVerification, "target device binding no longer matches this runtime")
	}
	if err := a.validateProfile(record.Manifest.Target.Profile); err != nil {
		return a.reject(record, handoff.FailureVerification, "target agent profile is not available for supervised launch")
	}
	if err := ctx.Err(); err != nil {
		return meshHandoffStatus{}, err
	}

	manifest := record.Manifest
	registry, err := a.loadProjects(a.projectsPath)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return meshHandoffStatus{}, ctxErr
	}
	if err != nil {
		return a.reject(record, handoff.FailureVerification, "target project registry is invalid")
	}
	resolvedProject, ok := registry.ResolveProject(manifest.Repository.ProjectSlug)
	if !ok {
		return a.reject(record, handoff.FailureVerification, "target project is not registered")
	}

	_, err = a.snapshotHistory(a.historyRoot, a.store.Root(), manifest.HandoffID, manifest.History)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return meshHandoffStatus{}, ctxErr
	}
	if err != nil {
		if code, ok := handoff.HistoryErrorCodeOf(err); ok && code == handoff.HistoryErrorRetryable {
			return a.waitForAssets(record, "synced session history is not available")
		}
		return a.reject(record, handoff.FailureInvalidManifest, "session history reference is invalid")
	}

	if err := a.prepareRepository(ctx, manifest, resolvedProject); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return meshHandoffStatus{}, ctxErr
		}
		return a.classifyRepositoryFailure(record, err)
	}
	if err := ctx.Err(); err != nil {
		return meshHandoffStatus{}, err
	}

	at := targetTransitionTime(a.now, record.Updated)
	prepared, err := a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetPrepared, handoff.Transition{At: at})
	if err != nil {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to prepare handoff"}
	}
	return handoffStatusDTO(prepared), nil
}

func (a *handoffApplication) lockPrepare(id string) func() {
	release, _ := a.lockPrepareContext(context.Background(), id)
	return release
}

func (a *handoffApplication) lockPrepareContext(ctx context.Context, id string) (func(), error) {
	a.prepareMu.Lock()
	entry := a.prepareLocks[id]
	if entry == nil {
		entry = &handoffPrepareLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		a.prepareLocks[id] = entry
	}
	entry.refs++
	a.prepareMu.Unlock()

	select {
	case <-entry.token:
		return func() {
			entry.token <- struct{}{}
			a.releasePrepareLock(id, entry)
		}, nil
	case <-ctx.Done():
		a.releasePrepareLock(id, entry)
		return nil, ctx.Err()
	}
}

func (a *handoffApplication) releasePrepareLock(id string, entry *handoffPrepareLock) {
	a.prepareMu.Lock()
	defer a.prepareMu.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(a.prepareLocks, id)
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
	now := targetTransitionTime(a.now, record.Updated)
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
	now := targetTransitionTime(a.now, record.Updated)
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

func targetTransitionTime(now func() time.Time, notBefore time.Time) time.Time {
	at := now().UTC()
	if at.Before(notBefore) {
		return notBefore
	}
	return at
}
