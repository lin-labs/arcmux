package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

const (
	handoffAssetRetryDelay     = 15 * time.Second
	targetHandoffResumeTimeout = 45 * time.Second
	handoffLaunchPollInterval  = 25 * time.Millisecond
	handoffSafePreambleRunes   = 200
)

type meshHandoffPrepareRequest struct {
	Manifest handoff.Manifest `json:"manifest"`
}

type meshHandoffStatusRequest struct {
	HandoffID string `json:"handoff_id"`
}

type meshHandoffLaunchRequest struct {
	HandoffID      string `json:"handoff_id"`
	ManifestDigest string `json:"manifest_digest"`
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
	TargetLocator  *handoff.TargetLocator `json:"target_locator,omitempty"`
}

type meshHandoffFailureDTO struct {
	Code      handoff.FailureCode `json:"code"`
	Message   string              `json:"message"`
	Retryable bool                `json:"retryable"`
}

type repositoryPreparer func(context.Context, handoff.Manifest, project.ResolvedProject) error
type launchRepositoryPreparer func(context.Context, handoff.Manifest, project.ResolvedProject) (handoff.RepositoryPreparation, error)
type targetProfileValidator func(string) error
type targetSessionCreator func(context.Context, CreateSessionRequest) (*session.Session, bool, error)
type targetSessionLookup func(string) (*session.Session, bool)
type targetPromptSender func(context.Context, string, string, bool, bool) error
type targetLocatorRecorder func(string, uint64, handoff.TargetLocator, time.Time) (handoff.TargetRecord, error)

type handoffPrepareLock struct {
	token chan struct{}
	refs  int
}

type handoffApplication struct {
	store                *handoff.Store
	historyRoot          string
	projectsPath         string
	launchRendezvousRoot string
	now                  func() time.Time
	snapshotHistory      func(string, string, string, handoff.HistoryRef) (string, error)
	loadProjects         func(string) (*project.ConsolidatedRegistry, error)
	prepareRepository    repositoryPreparer
	prepareLaunchRepo    launchRepositoryPreparer
	publishLaunchFile    func(string, string, string, handoff.Manifest, handoff.RepositoryPreparation) (string, error)
	validateProfile      targetProfileValidator
	createSession        targetSessionCreator
	lookupSession        targetSessionLookup
	sendPrompt           targetPromptSender
	persistSessions      func() error
	recordLocator        targetLocatorRecorder
	resumeTimeout        time.Duration
	launchPoll           time.Duration
	prepareMu            sync.Mutex
	prepareLocks         map[string]*handoffPrepareLock
}

func newHandoffApplication(store *handoff.Store, profiles map[string]profile.Profile) *handoffApplication {
	app := &handoffApplication{
		store:                store,
		historyRoot:          defaultHandoffHistoryRoot(),
		projectsPath:         project.DefaultConsolidatedPath(),
		launchRendezvousRoot: handoff.DefaultLaunchRendezvousRoot(),
		now:                  time.Now,
		snapshotHistory:      handoff.SnapshotHistory,
		loadProjects:         project.LoadConsolidated,
		prepareRepository:    prepareHandoffRepository,
		prepareLaunchRepo:    handoff.PrepareRepository,
		publishLaunchFile:    publishHandoffLaunchInstructions,
		resumeTimeout:        targetHandoffResumeTimeout,
		launchPoll:           handoffLaunchPollInterval,
		validateProfile: func(name string) error {
			return validateHandoffTargetProfile(profiles, name)
		},
		prepareLocks: make(map[string]*handoffPrepareLock),
	}
	app.recordLocator = store.RecordTargetLaunchLocator
	return app
}

func (a *handoffApplication) configureLaunchRuntime(d *Daemon) {
	if a == nil || d == nil {
		return
	}
	a.createSession = d.createSessionWithIdempotency
	a.lookupSession = d.GetSession
	a.sendPrompt = d.SendPrompt
	a.persistSessions = d.persistSessionsChecked
}

func (a *handoffApplication) resumeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.resumeTimeout
	if timeout <= 0 {
		timeout = targetHandoffResumeTimeout
	}
	return context.WithTimeout(ctx, timeout)
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

// reconcileTargetHandoffs resumes both preparation and already-authorized
// launches. TargetPrepared is absent from RecoverableTarget, so a restart can
// never infer launch authority from preparation alone.
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
	for _, candidate := range records {
		if ctx.Err() != nil {
			return
		}
		switch candidate.State {
		case handoff.TargetReceived, handoff.TargetValidating, handoff.TargetWaitingAssets,
			handoff.TargetLaunching, handoff.TargetLaunchWaitingAssets:
		default:
			continue
		}

		resumeCtx, cancel := app.resumeContext(ctx)
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
		if (current.State == handoff.TargetWaitingAssets || current.State == handoff.TargetLaunchWaitingAssets) &&
			(current.NextRetry == nil || current.NextRetry.After(at)) {
			release()
			cancel()
			continue
		}
		switch current.State {
		case handoff.TargetReceived, handoff.TargetValidating, handoff.TargetWaitingAssets,
			handoff.TargetLaunching, handoff.TargetLaunchWaitingAssets:
			isLaunch := current.State == handoff.TargetLaunching || current.State == handoff.TargetLaunchWaitingAssets
			result := make(chan error, 1)
			go func() {
				var resumeErr error
				if isLaunch {
					_, resumeErr = app.resumeLaunch(resumeCtx, current, d.meshDeviceID())
				} else {
					_, resumeErr = app.resumeTarget(resumeCtx, current, d.meshDeviceID())
				}
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

// launch is the independently authorized target-side control operation. A
// successful prepare grant can create TargetPrepared, but only this method's
// separate handoffs.launch grant may durably enter TargetLaunching.
func (a *handoffApplication) launch(ctx context.Context, principal arcmuxmesh.Principal, localDeviceID string, request meshHandoffLaunchRequest) (meshHandoffStatus, error) {
	if request.HandoffID == "" || request.ManifestDigest == "" {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff_id and manifest_digest are required"))
	}
	release, err := a.lockPrepareContext(ctx, request.HandoffID)
	if err != nil {
		return meshHandoffStatus{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return meshHandoffStatus{}, err
	}
	record, err := a.store.GetTarget(request.HandoffID)
	if err != nil {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff not found"))
	}
	if principal.PeerID == "" || record.Manifest.Source.DeviceID != principal.PeerID {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorPermissionDenied, Message: "handoff belongs to a different authenticated peer"}
	}
	if localDeviceID == "" || record.Manifest.Target.DeviceID != localDeviceID {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff target does not match this device"))
	}
	if request.ManifestDigest != record.Digest {
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff manifest digest does not match prepared state"))
	}

	switch record.State {
	case handoff.TargetPrepared, handoff.TargetLaunchWaitingAssets:
		at := targetTransitionTime(a.now, record.Updated)
		record, err = a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetLaunching, handoff.Transition{At: at})
		if err != nil {
			return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to authorize target launch"}
		}
	case handoff.TargetLaunching:
		// Resume the exact durable authorization after an interrupted call.
	case handoff.TargetAccepted, handoff.TargetRejected:
		return handoffStatusDTO(record), nil
	default:
		return meshHandoffStatus{}, meshInvalidRequest(errors.New("handoff is not prepared for launch"))
	}
	return a.resumeLaunch(ctx, record, localDeviceID)
}

// resumeLaunch revalidates every prepared input before selecting or creating
// the deterministic supervised continuation. The caller must hold the per-ID
// lock. TargetLaunching itself is the durable authorization used by restart
// reconciliation; TargetPrepared is intentionally never resumed here.
func (a *handoffApplication) resumeLaunch(ctx context.Context, record handoff.TargetRecord, localDeviceID string) (meshHandoffStatus, error) {
	if record.State == handoff.TargetLaunchWaitingAssets {
		at := targetTransitionTime(a.now, record.Updated)
		var err error
		record, err = a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetLaunching, handoff.Transition{At: at})
		if err != nil {
			return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to resume target launch"}
		}
	}
	if record.State != handoff.TargetLaunching {
		return handoffStatusDTO(record), nil
	}
	if err := ctx.Err(); err != nil {
		return meshHandoffStatus{}, err
	}
	if localDeviceID == "" || record.Manifest.Target.DeviceID != localDeviceID {
		return a.reject(record, handoff.FailureVerification, "target device binding no longer matches this runtime")
	}
	if len(record.Manifest.Artifacts) != 0 {
		return a.reject(record, handoff.FailureInvalidManifest, "handoff artifacts are not supported")
	}
	if err := a.validateProfile(record.Manifest.Target.Profile); err != nil {
		return a.reject(record, handoff.FailureVerification, "target agent profile is not available for supervised launch")
	}
	if a.prepareLaunchRepo == nil || a.createSession == nil || a.lookupSession == nil || a.sendPrompt == nil || a.persistSessions == nil || a.recordLocator == nil {
		return a.reject(record, handoff.FailureInternal, "target launch runtime is unavailable")
	}

	registry, err := a.loadProjects(a.projectsPath)
	if err != nil {
		return a.reject(record, handoff.FailureVerification, "target project registry is invalid")
	}
	resolvedProject, ok := registry.ResolveProject(record.Manifest.Repository.ProjectSlug)
	if !ok {
		return a.reject(record, handoff.FailureVerification, "target project is not registered")
	}
	historyPath, err := a.snapshotHistory(a.historyRoot, a.store.Root(), record.Manifest.HandoffID, record.Manifest.History)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return meshHandoffStatus{}, ctxErr
	}
	if err != nil {
		if code, ok := handoff.HistoryErrorCodeOf(err); ok && code == handoff.HistoryErrorRetryable {
			return a.waitForLaunch(record, handoff.FailureMissingAsset, "launch is waiting for synchronized history")
		}
		return a.reject(record, handoff.FailureVerification, "launch history did not match prepared state")
	}
	prepared, err := a.prepareLaunchRepo(ctx, record.Manifest, resolvedProject)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return meshHandoffStatus{}, ctxErr
	}
	if err != nil {
		if code, ok := handoff.RepositoryErrorCodeOf(err); ok && code == handoff.RepositoryErrorRetryable {
			return a.waitForLaunch(record, handoff.FailureMissingAsset, "launch is waiting for the prepared repository")
		}
		return a.reject(record, handoff.FailureVerification, "launch repository did not match prepared state")
	}

	if a.publishLaunchFile == nil {
		return a.reject(record, handoff.FailureInternal, "target launch runtime is unavailable")
	}
	instructionsPath, err := a.publishLaunchFile(a.store.Root(), record.Manifest.HandoffID, historyPath, record.Manifest, prepared)
	if err != nil {
		return a.waitForLaunch(record, handoff.FailureInternal, "private target instructions are not ready")
	}
	if err := handoff.PublishLaunchRendezvous(
		a.launchRendezvousRoot,
		handoff.LaunchMarker(record.Manifest.HandoffID, record.Digest),
		a.store.Root(),
	); err != nil {
		return a.waitForLaunch(record, handoff.FailureInternal, "private target instructions are not ready")
	}
	sess, current, alreadyDelivered, err := a.targetLaunchSession(ctx, record, prepared, instructionsPath)
	if err != nil {
		if errors.Is(err, errHandoffLaunchConflict) {
			return a.reject(current, handoff.FailureLaunch, "target continuation conflicts with durable launch state")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return meshHandoffStatus{}, ctxErr
		}
		return a.waitForLaunch(current, handoff.FailureLaunch, "target continuation is not ready")
	}
	if !alreadyDelivered {
		prompt := handoffLaunchPrompt(current)
		if err := a.sendPrompt(ctx, sess.Snapshot().ID, prompt, true, false); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return meshHandoffStatus{}, ctxErr
			}
			return a.waitForLaunch(current, handoff.FailureLaunch, "confirmed target prompt delivery is not ready")
		}
	}
	if sess.Snapshot().CurrentCommand != handoffLaunchCurrentCommand(current) {
		return a.reject(current, handoff.FailureLaunch, "target continuation did not persist launch delivery evidence")
	}
	if err := a.persistSessions(); err != nil {
		return a.waitForLaunch(current, handoff.FailureInternal, "target session persistence is not ready")
	}
	locator := current.TargetLocator
	if locator == nil {
		return a.reject(current, handoff.FailureInternal, "target continuation locator is unavailable")
	}
	at := targetTransitionTime(a.now, current.Updated)
	accepted, err := a.store.TransitionTarget(current.Manifest.HandoffID, current.Revision, handoff.TargetAccepted, handoff.Transition{
		At: at, TargetLocator: locator,
	})
	if err != nil {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to accept target launch"}
	}
	return handoffStatusDTO(accepted), nil
}

var errHandoffLaunchConflict = errors.New("handoff launch conflict")

func (a *handoffApplication) targetLaunchSession(ctx context.Context, record handoff.TargetRecord, prepared handoff.RepositoryPreparation, instructionsPath string) (*session.Session, handoff.TargetRecord, bool, error) {
	name, owner := handoffLaunchSessionIdentity(record)
	env := map[string]string{"ARCMUX_HANDOFF_INSTRUCTIONS": instructionsPath}
	var sess *session.Session
	if record.TargetLocator != nil {
		if record.TargetLocator.DeviceID != record.Manifest.Target.DeviceID || record.TargetLocator.ProfileScope != string(sessionview.RootProfileScope) {
			return nil, record, false, errHandoffLaunchConflict
		}
		var ok bool
		sess, ok = a.lookupSession(record.TargetLocator.SessionID)
		if !ok {
			return nil, record, false, errHandoffLaunchConflict
		}
	} else {
		var err error
		sess, _, err = a.createSession(ctx, CreateSessionRequest{
			Agent: record.Manifest.Target.Profile, CWD: prepared.WorktreePath, Prompt: "",
			Name: name, OwnerID: owner, Env: env, private: true,
		})
		if err != nil {
			return nil, record, false, err
		}
		snap := sess.Snapshot()
		if !snap.Private || snap.Name != name || snap.OwnerID != owner || snap.Agent != record.Manifest.Target.Profile || snap.CWD != prepared.WorktreePath ||
			snap.Env["ARCMUX_HANDOFF_INSTRUCTIONS"] != instructionsPath {
			return nil, record, false, errHandoffLaunchConflict
		}
		// CreateSession's ordinary persistence is best-effort. Establish a
		// checked, fsync-durable inventory entry before the target record can
		// point at this session across a daemon restart.
		if err := a.persistSessions(); err != nil {
			return nil, record, false, err
		}
		locator := handoff.TargetLocator{
			DeviceID: record.Manifest.Target.DeviceID, ProfileScope: string(sessionview.RootProfileScope), SessionID: snap.ID,
		}
		at := targetTransitionTime(a.now, record.Updated)
		record, err = a.recordLocator(record.Manifest.HandoffID, record.Revision, locator, at)
		if err != nil {
			if errors.Is(err, handoff.ErrCASConflict) || errors.Is(err, handoff.ErrManifestConflict) || errors.Is(err, handoff.ErrIllegalTransition) {
				return nil, record, false, errHandoffLaunchConflict
			}
			return nil, record, false, err
		}
	}

	interval := a.launchPoll
	if interval <= 0 {
		interval = handoffLaunchPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snap := sess.Snapshot()
		if !snap.Private || snap.Name != name || snap.OwnerID != owner || snap.Agent != record.Manifest.Target.Profile || snap.CWD != prepared.WorktreePath ||
			record.TargetLocator == nil || snap.ID != record.TargetLocator.SessionID || snap.Env["ARCMUX_HANDOFF_INSTRUCTIONS"] != instructionsPath {
			return nil, record, false, errHandoffLaunchConflict
		}
		if snap.CurrentCommand == handoffLaunchCurrentCommand(record) {
			return sess, record, true, nil
		}
		if snap.CurrentCommand != "" {
			return nil, record, false, errHandoffLaunchConflict
		}
		switch snap.State {
		case session.StateIdle:
			return sess, record, false, nil
		case session.StateExited, session.StateFailed, session.StateWorking:
			return nil, record, false, errHandoffLaunchConflict
		}
		select {
		case <-ctx.Done():
			return nil, record, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func handoffLaunchSessionIdentity(record handoff.TargetRecord) (string, string) {
	digest := sha256.Sum256([]byte("arcmux-handoff-session-v1\x00" + record.Manifest.HandoffID + "\x00" + record.Digest))
	suffix := hex.EncodeToString(digest[:12])
	return "handoff-" + suffix, "arcmux-handoff:" + record.Manifest.HandoffID
}

func handoffLaunchMarker(record handoff.TargetRecord) string {
	return handoff.LaunchMarker(record.Manifest.HandoffID, record.Digest)
}

func handoffLaunchSafePreamble(record handoff.TargetRecord) string {
	marker := handoffLaunchMarker(record)
	return marker + strings.Repeat("-", handoffSafePreambleRunes-len(marker))
}

func handoffLaunchCurrentCommand(record handoff.TargetRecord) string {
	return handoffLaunchSafePreamble(record) + "…"
}

func handoffLaunchPrompt(record handoff.TargetRecord) string {
	marker := handoffLaunchMarker(record)
	return handoffLaunchSafePreamble(record) + "\n\n" +
		"Resume this explicitly authorized handoff. Run `arcmux handoff receive " + marker +
		"` to read the owner-local instructions before acting; the exact continuation checkout is already active."
}

type handoffLaunchLineage struct {
	HandoffID       string                `json:"handoff_id"`
	TraceID         string                `json:"trace_id"`
	ParentHandoffID string                `json:"parent_handoff_id,omitempty"`
	Source          handoff.SourceSession `json:"source"`
	ConversationID  string                `json:"conversation_id,omitempty"`
	TargetProfile   string                `json:"target_profile"`
}

func publishHandoffLaunchInstructions(privateRoot, handoffID, historyPath string, manifest handoff.Manifest, prepared handoff.RepositoryPreparation) (string, error) {
	if handoffID == "" || filepath.Base(handoffID) != handoffID || strings.ContainsAny(handoffID, "/\\\x00") {
		return "", errors.New("invalid handoff identity")
	}
	root, err := filepath.Abs(privateRoot)
	if err != nil {
		return "", errors.New("resolve private handoff state")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("resolve private handoff state")
	}
	dir := filepath.Join(root, "handoff-"+handoffID)
	expectedHistory := filepath.Join(dir, "history.md")
	historyAbsolute, err := filepath.Abs(historyPath)
	if err != nil {
		return "", errors.New("resolve private history snapshot")
	}
	canonicalHistory, err := filepath.EvalSymlinks(historyAbsolute)
	if err != nil || filepath.Clean(canonicalHistory) != expectedHistory {
		return "", errors.New("history snapshot is outside private handoff state")
	}
	if !filepath.IsAbs(prepared.WorktreePath) || prepared.Head != manifest.Repository.SourceHead ||
		prepared.SourceBranch != manifest.Repository.Branch || prepared.LocalBranch == "" {
		return "", errors.New("repository preparation does not match immutable handoff state")
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("private handoff directory is unavailable")
	}
	data, err := json.Marshal(struct {
		Lineage      handoffLaunchLineage `json:"lineage"`
		Goal         string               `json:"goal"`
		History      string               `json:"history"`
		Worktree     string               `json:"worktree"`
		SourceBranch string               `json:"source_branch"`
		LocalBranch  string               `json:"local_branch"`
		ExpectedHead string               `json:"expected_head"`
	}{
		Lineage: handoffLaunchLineage{
			HandoffID: manifest.HandoffID, TraceID: manifest.TraceID, ParentHandoffID: manifest.ParentHandoffID,
			Source: manifest.Source, ConversationID: manifest.History.ConversationID, TargetProfile: manifest.Target.Profile,
		},
		Goal: manifest.Goal.Text, History: canonicalHistory, Worktree: prepared.WorktreePath,
		SourceBranch: prepared.SourceBranch, LocalBranch: prepared.LocalBranch, ExpectedHead: prepared.Head,
	})
	if err != nil {
		return "", errors.New("encode private handoff instructions")
	}
	data = append(data, '\n')
	path := filepath.Join(dir, "launch-instructions.json")
	if existing, statErr := os.Lstat(path); statErr == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return "", errors.New("private handoff instructions are unsafe")
		}
	} else if !os.IsNotExist(statErr) {
		return "", errors.New("inspect private handoff instructions")
	}
	tmp, err := os.CreateTemp(dir, ".launch-instructions-*.tmp")
	if err != nil {
		return "", errors.New("create private handoff instructions")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", errors.New("secure private handoff instructions")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", errors.New("write private handoff instructions")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", errors.New("sync private handoff instructions")
	}
	if err := tmp.Close(); err != nil {
		return "", errors.New("close private handoff instructions")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", errors.New("publish private handoff instructions")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", errors.New("secure published handoff instructions")
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", errors.New("open private handoff directory")
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return "", errors.New("sync private handoff directory")
	}
	if err := directory.Close(); err != nil {
		return "", errors.New("close private handoff directory")
	}
	return path, nil
}

func (a *handoffApplication) waitForLaunch(record handoff.TargetRecord, code handoff.FailureCode, message string) (meshHandoffStatus, error) {
	now := targetTransitionTime(a.now, record.Updated)
	next := now.Add(handoffAssetRetryDelay)
	failure := &handoff.Failure{Code: code, Message: message, Retryable: true, At: now}
	waiting, err := a.store.TransitionTarget(record.Manifest.HandoffID, record.Revision, handoff.TargetLaunchWaitingAssets, handoff.Transition{
		At: now, NextRetry: &next, Failure: failure,
	})
	if err != nil {
		return meshHandoffStatus{}, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInternal, Message: "unable to record waiting target launch"}
	}
	return handoffStatusDTO(waiting), nil
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
	if record.State == handoff.TargetAccepted && record.TargetLocator != nil {
		locator := *record.TargetLocator
		status.TargetLocator = &locator
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
