package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/project"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

const (
	sourceHandoffRetryDelay     = 15 * time.Second
	sourceHandoffAttemptTimeout = 10 * time.Second
)

// sourceHandoffPrepareRequest is deliberately an operator-intent request, not
// a partially trusted manifest. Device identity, source agent/CWD, repository
// revision, history integrity, timestamps, and identifiers are all derived by
// the daemon.
type sourceHandoffPrepareRequest struct {
	ProfileScope    sessionview.ProfileScope `json:"profile_scope"`
	SessionID       string                   `json:"session_id"`
	TargetPeer      string                   `json:"target_peer"`
	TargetAgent     string                   `json:"target_agent"`
	Project         string                   `json:"project"`
	Goal            string                   `json:"goal"`
	History         string                   `json:"history,omitempty"`
	ConversationID  string                   `json:"conversation_id,omitempty"`
	ParentHandoffID string                   `json:"parent_handoff_id,omitempty"`
	Validation      handoff.ValidationState  `json:"validation,omitempty"`
}

// sourceHandoffDTO is the complete local operator response allowlist. The
// immutable manifest remains private state because it contains the goal,
// history locator, repository branch, and source-session identity.
type sourceHandoffDTO struct {
	HandoffID      string                 `json:"handoff_id"`
	ManifestDigest string                 `json:"manifest_digest"`
	State          handoff.SourceState    `json:"state"`
	Attempts       uint32                 `json:"attempts"`
	TargetDevice   string                 `json:"target_device"`
	Project        string                 `json:"project"`
	Failure        *meshHandoffFailureDTO `json:"failure,omitempty"`
}

type sourceHandoffErrorKind string

const (
	sourceHandoffInvalid     sourceHandoffErrorKind = "invalid"
	sourceHandoffNotFound    sourceHandoffErrorKind = "not_found"
	sourceHandoffConflict    sourceHandoffErrorKind = "conflict"
	sourceHandoffUnavailable sourceHandoffErrorKind = "unavailable"
)

type sourceHandoffError struct {
	kind    sourceHandoffErrorKind
	message string
}

func (e *sourceHandoffError) Error() string { return e.message }

type sourceSessionLookup func(sessionview.ProfileScope, string) (sessionview.Detail, bool)
type sourceProjectLoader func(string) (*project.ConsolidatedRegistry, error)
type sourceRepositoryInspector func(context.Context, string, project.ResolvedProject) (handoff.RepositorySnapshot, error)
type sourceHistoryPublisher func(string, string, string) (handoff.HistoryRef, error)
type sourceHandoffPrepareCaller func(context.Context, string, meshHandoffPrepareRequest) (meshHandoffStatus, error)
type safeIDGenerator func(string) (string, error)

type sourceHandoffOutbox struct {
	store             *handoff.Store
	deviceID          string
	historyRoot       string
	projectsPath      string
	now               func() time.Time
	lookupSession     sourceSessionLookup
	loadProjects      sourceProjectLoader
	inspectRepository sourceRepositoryInspector
	publishHistory    sourceHistoryPublisher
	callPrepare       sourceHandoffPrepareCaller
	newID             safeIDGenerator
	attemptTimeout    time.Duration
}

func (d *Daemon) sourceHandoffOutbox() (*sourceHandoffOutbox, error) {
	app, err := d.handoffApplication()
	if err != nil {
		return nil, err
	}
	deviceID := d.meshDeviceID()
	if deviceID == "" {
		return nil, errors.New("local mesh device identity is unavailable")
	}
	return &sourceHandoffOutbox{
		store:        app.store,
		deviceID:     deviceID,
		historyRoot:  app.historyRoot,
		projectsPath: app.projectsPath,
		now:          time.Now,
		lookupSession: func(scope sessionview.ProfileScope, id string) (sessionview.Detail, bool) {
			locator, locatorErr := sessionview.NewLocator(scope, id)
			if locatorErr != nil {
				return sessionview.Detail{}, false
			}
			return d.SessionCatalog().Get(locator)
		},
		loadProjects:      project.LoadConsolidated,
		inspectRepository: handoff.InspectSourceRepository,
		publishHistory:    handoff.PublishSourceHistory,
		callPrepare:       d.callRemoteHandoffPrepare,
		newID:             newHandoffSafeID,
		attemptTimeout:    sourceHandoffAttemptTimeout,
	}, nil
}

func (d *Daemon) callRemoteHandoffPrepare(ctx context.Context, peer string, request meshHandoffPrepareRequest) (meshHandoffStatus, error) {
	manager, err := d.currentMeshManager()
	if err != nil {
		return meshHandoffStatus{}, err
	}
	var response meshHandoffStatus
	if err := manager.Call(ctx, peer, meshMethodHandoffsPrepare, request, &response); err != nil {
		return meshHandoffStatus{}, err
	}
	return response, nil
}

func newHandoffSafeID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func (o *sourceHandoffOutbox) prepare(ctx context.Context, request sourceHandoffPrepareRequest) (sourceHandoffDTO, error) {
	if o == nil || o.store == nil || o.lookupSession == nil || o.loadProjects == nil || o.inspectRepository == nil ||
		o.publishHistory == nil || o.callPrepare == nil || o.newID == nil || o.now == nil {
		return sourceHandoffDTO{}, sourceOutboxUnavailable("handoff preparation is unavailable")
	}
	if request.Validation == "" {
		request.Validation = handoff.ValidationNotRun
	}
	if request.TargetPeer == "" || request.TargetAgent == "" || request.Project == "" || request.SessionID == "" || request.ProfileScope == "" {
		return sourceHandoffDTO{}, sourceOutboxInvalid("profile_scope, session_id, target_peer, target_agent, and project are required")
	}
	if request.TargetPeer == o.deviceID {
		return sourceHandoffDTO{}, sourceOutboxInvalid("target peer must differ from the local device")
	}

	detail, ok := o.lookupSession(request.ProfileScope, request.SessionID)
	if !ok || detail.Summary.Locator.ProfileScope != request.ProfileScope || detail.Summary.Locator.SessionID != request.SessionID {
		return sourceHandoffDTO{}, sourceOutboxInvalid("source session is not a supervised local session")
	}
	if detail.Summary.Agent == "" || detail.Summary.LaunchCWD == "" {
		return sourceHandoffDTO{}, sourceOutboxInvalid("source session is missing its agent or launch directory")
	}

	registry, err := o.loadProjects(o.projectsPath)
	if err != nil {
		return sourceHandoffDTO{}, sourceOutboxUnavailable("project registry is unavailable")
	}
	resolved, ok := registry.ResolveProject(request.Project)
	if !ok {
		return sourceHandoffDTO{}, sourceOutboxInvalid("project is not registered locally")
	}
	repository, err := o.inspectRepository(ctx, detail.Summary.LaunchCWD, resolved)
	if err != nil {
		if code, classified := handoff.RepositoryErrorCodeOf(err); classified && code == handoff.RepositoryErrorRetryable {
			return sourceHandoffDTO{}, sourceOutboxUnavailable(err.Error())
		}
		return sourceHandoffDTO{}, sourceOutboxInvalid(err.Error())
	}

	historyName := request.History
	if historyName == "" && detail.Summary.History != nil {
		historyName = detail.Summary.History.Basename
	}
	if historyName == "" {
		return sourceHandoffDTO{}, sourceOutboxInvalid("source session has no synced history; provide --history after sync completes")
	}
	history, err := o.publishHistory(o.historyRoot, historyName, request.ConversationID)
	if err != nil {
		if code, classified := handoff.HistoryErrorCodeOf(err); classified && code == handoff.HistoryErrorRetryable {
			return sourceHandoffDTO{}, sourceOutboxUnavailable(err.Error())
		}
		return sourceHandoffDTO{}, sourceOutboxInvalid(err.Error())
	}

	handoffID, err := o.newID("handoff-")
	if err != nil {
		return sourceHandoffDTO{}, sourceOutboxUnavailable("unable to allocate handoff identity")
	}
	traceID, err := o.newID("trace-")
	if err != nil {
		return sourceHandoffDTO{}, sourceOutboxUnavailable("unable to allocate handoff trace identity")
	}
	at := o.now().UTC()
	validation := handoff.ValidationEvidence{State: request.Validation}
	if validation.State == handoff.ValidationPassed || validation.State == handoff.ValidationFailed {
		validation.RepositoryRevision = repository.SourceHead
		completed := at
		validation.CompletedAt = &completed
	}
	manifest := handoff.Manifest{
		SchemaVersion:   handoff.ManifestVersion,
		HandoffID:       handoffID,
		TraceID:         traceID,
		ParentHandoffID: request.ParentHandoffID,
		Source: handoff.SourceSession{
			DeviceID: o.deviceID, ProfileScope: string(request.ProfileScope), SessionID: request.SessionID,
		},
		SourceAgent: detail.Summary.Agent,
		Target:      handoff.TargetAgent{DeviceID: request.TargetPeer, Profile: request.TargetAgent},
		Goal:        handoff.GoalSummary{Text: request.Goal, Provenance: "explicit_operator", UpdatedAt: at},
		History:     history,
		Repository:  repository,
		Artifacts:   []handoff.ArtifactRef{},
		Validation:  validation,
		CreatedAt:   at,
	}
	if err := manifest.Validate(); err != nil {
		return sourceHandoffDTO{}, sourceOutboxInvalid("handoff request contains invalid or unsafe operator input")
	}
	record, replay, err := o.store.QueueSource(manifest)
	if err != nil {
		if errors.Is(err, handoff.ErrManifestConflict) {
			return sourceHandoffDTO{}, sourceOutboxConflict("handoff identity conflicts with existing state")
		}
		return sourceHandoffDTO{}, sourceOutboxUnavailable("unable to queue handoff")
	}
	if replay {
		return sourceHandoffDTO{}, sourceOutboxConflict("generated handoff identity already exists; retry preparation")
	}
	return o.attempt(ctx, record.Manifest.HandoffID, false)
}

func (o *sourceHandoffOutbox) get(id string) (sourceHandoffDTO, error) {
	record, err := o.store.GetSource(id)
	if err != nil {
		if errors.Is(err, handoff.ErrNotFound) {
			return sourceHandoffDTO{}, sourceOutboxNotFound("handoff not found")
		}
		return sourceHandoffDTO{}, sourceOutboxInvalid("invalid handoff id")
	}
	return sourceHandoffRecordDTO(record), nil
}

func (o *sourceHandoffOutbox) list() ([]sourceHandoffDTO, error) {
	records, err := o.store.ListSource()
	if err != nil {
		return nil, sourceOutboxUnavailable("unable to read handoff outbox")
	}
	dtos := make([]sourceHandoffDTO, 0, len(records))
	for _, record := range records {
		dtos = append(dtos, sourceHandoffRecordDTO(record))
	}
	return dtos, nil
}

func (o *sourceHandoffOutbox) retry(ctx context.Context, id string) (sourceHandoffDTO, error) {
	record, err := o.store.GetSource(id)
	if err != nil {
		if errors.Is(err, handoff.ErrNotFound) {
			return sourceHandoffDTO{}, sourceOutboxNotFound("handoff not found")
		}
		return sourceHandoffDTO{}, sourceOutboxInvalid("invalid handoff id")
	}
	if record.State != handoff.SourceRetryWait {
		return sourceHandoffDTO{}, sourceOutboxConflict("only a prepare retry_wait handoff can be retried")
	}
	return o.attempt(ctx, id, true)
}

// reconcile resumes only source-side prepare work. RunnableSource also
// exposes launch recovery states for the future launch driver, so filter them
// here rather than treating restart or connectivity as launch authorization.
func (o *sourceHandoffOutbox) reconcile(ctx context.Context, at time.Time) error {
	if o == nil || o.store == nil || o.now == nil {
		return errors.New("source handoff recovery is unavailable")
	}
	records, err := o.store.RunnableSource(at)
	if err != nil {
		return err
	}
	attemptTimeout := o.attemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = sourceHandoffAttemptTimeout
	}
	var firstErr error
	for _, candidate := range records {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch candidate.State {
		case handoff.SourceQueued, handoff.SourcePreparingRemote:
		case handoff.SourceRetryWait:
			if candidate.NextRetry == nil || candidate.NextRetry.After(at) {
				continue
			}
		default:
			continue
		}

		// attempt reloads under the shared per-store/per-ID lock. Concurrent
		// operator retries and coalesced runtime passes therefore make one RPC.
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		_, attemptErr := o.attempt(attemptCtx, candidate.Manifest.HandoffID, false)
		cancel()
		if attemptErr != nil && ctx.Err() == nil {
			// A concurrent state transition is already durable and will be
			// considered by a later pass; continue draining unrelated IDs.
			if firstErr == nil {
				firstErr = attemptErr
			}
			continue
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return firstErr
}

func (d *Daemon) reconcileSourceHandoffs(ctx context.Context, at time.Time) {
	outbox, err := d.sourceHandoffOutbox()
	if err != nil {
		return
	}
	// Use the scan timestamp for due checks and state transitions so a pass is
	// internally consistent. Runtime callers always provide the current UTC
	// time; tests can exercise the deadline boundary deterministically.
	outbox.now = func() time.Time { return at }
	if err := outbox.reconcile(ctx, at); err != nil && ctx.Err() == nil {
		d.logger.Warn("source handoff recovery scan failed")
	}
}

var sourceAttemptLocks = newSourceHandoffLocks()

type sourceHandoffLocks struct {
	mu    sync.Mutex
	locks map[string]*sourceHandoffLock
}

type sourceHandoffLock struct {
	mu   sync.Mutex
	refs int
}

func newSourceHandoffLocks() *sourceHandoffLocks {
	return &sourceHandoffLocks{locks: make(map[string]*sourceHandoffLock)}
}

func (l *sourceHandoffLocks) lock(key string) func() {
	l.mu.Lock()
	entry := l.locks[key]
	if entry == nil {
		entry = &sourceHandoffLock{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

func (o *sourceHandoffOutbox) attempt(ctx context.Context, id string, force bool) (sourceHandoffDTO, error) {
	release := sourceAttemptLocks.lock(o.store.Root() + "\x00" + id)
	defer release()

	record, err := o.store.GetSource(id)
	if err != nil {
		return sourceHandoffDTO{}, sourceOutboxNotFound("handoff not found")
	}
	switch record.State {
	case handoff.SourceQueued:
		// Initial attempt is always immediate.
	case handoff.SourceRetryWait:
		if !force && record.NextRetry != nil && record.NextRetry.After(o.now().UTC()) {
			return sourceHandoffRecordDTO(record), nil
		}
	case handoff.SourcePreparingRemote:
		// Resume the exact persisted manifest after a process interruption.
	case handoff.SourceRemotePrepared, handoff.SourceFailed:
		return sourceHandoffRecordDTO(record), nil
	default:
		// Launch states are deliberately inert in the prepare-only outbox.
		return sourceHandoffDTO{}, sourceOutboxConflict("handoff is outside the prepare phase")
	}

	if record.State != handoff.SourcePreparingRemote {
		at := sourceTransitionTime(o.now, record.Updated)
		record, err = o.store.TransitionSource(id, record.Revision, handoff.SourcePreparingRemote, handoff.Transition{At: at})
		if err != nil {
			return sourceHandoffDTO{}, sourceOutboxConflict("handoff state changed concurrently")
		}
	}

	remote, callErr := o.callPrepare(ctx, record.Manifest.Target.DeviceID, meshHandoffPrepareRequest{Manifest: record.Manifest})
	if callErr != nil {
		failure, retryable := sourceCallFailure(callErr, sourceTransitionTime(o.now, record.Updated))
		if retryable {
			return o.transitionRetry(record, failure)
		}
		return o.transitionFailed(record, failure)
	}
	if remote.HandoffID != record.Manifest.HandoffID || remote.ManifestDigest != record.Digest {
		return o.transitionFailed(record, handoff.Failure{
			Code: handoff.FailureConflict, Message: "target returned conflicting handoff identity", Retryable: false,
			At: sourceTransitionTime(o.now, record.Updated),
		})
	}

	switch remote.State {
	case handoff.TargetPrepared:
		at := sourceTransitionTime(o.now, record.Updated)
		updated, err := o.store.TransitionSource(id, record.Revision, handoff.SourceRemotePrepared, handoff.Transition{At: at})
		if err != nil {
			return sourceHandoffDTO{}, sourceOutboxConflict("handoff state changed concurrently")
		}
		return sourceHandoffRecordDTO(updated), nil
	case handoff.TargetReceived, handoff.TargetValidating:
		return o.transitionRetry(record, handoff.Failure{
			Code: handoff.FailureUnavailable, Message: "target handoff preparation is still in progress", Retryable: true,
			At: sourceTransitionTime(o.now, record.Updated),
		})
	case handoff.TargetWaitingAssets:
		return o.transitionRetry(record, handoff.Failure{
			Code: handoff.FailureMissingAsset, Message: "target is waiting for synchronized handoff assets", Retryable: true,
			At: sourceTransitionTime(o.now, record.Updated),
		})
	case handoff.TargetRejected:
		code := handoff.FailureVerification
		if remote.Failure != nil && validSourceFailureCode(remote.Failure.Code) {
			code = remote.Failure.Code
		}
		return o.transitionFailed(record, handoff.Failure{
			Code: code, Message: "target rejected handoff preparation", Retryable: false,
			At: sourceTransitionTime(o.now, record.Updated),
		})
	default:
		return o.transitionFailed(record, handoff.Failure{
			Code: handoff.FailureConflict, Message: "target returned a state outside handoff preparation", Retryable: false,
			At: sourceTransitionTime(o.now, record.Updated),
		})
	}
}

func (o *sourceHandoffOutbox) transitionRetry(record handoff.SourceRecord, failure handoff.Failure) (sourceHandoffDTO, error) {
	at := sourceTransitionTime(o.now, record.Updated)
	failure.At = at
	next := at.Add(sourceHandoffRetryDelay)
	updated, err := o.store.TransitionSource(record.Manifest.HandoffID, record.Revision, handoff.SourceRetryWait, handoff.Transition{
		At: at, NextRetry: &next, Failure: &failure,
	})
	if err != nil {
		return sourceHandoffDTO{}, sourceOutboxConflict("handoff state changed concurrently")
	}
	return sourceHandoffRecordDTO(updated), nil
}

func (o *sourceHandoffOutbox) transitionFailed(record handoff.SourceRecord, failure handoff.Failure) (sourceHandoffDTO, error) {
	at := sourceTransitionTime(o.now, record.Updated)
	failure.At = at
	failure.Retryable = false
	updated, err := o.store.TransitionSource(record.Manifest.HandoffID, record.Revision, handoff.SourceFailed, handoff.Transition{
		At: at, Failure: &failure,
	})
	if err != nil {
		return sourceHandoffDTO{}, sourceOutboxConflict("handoff state changed concurrently")
	}
	return sourceHandoffRecordDTO(updated), nil
}

func sourceTransitionTime(now func() time.Time, notBefore time.Time) time.Time {
	at := now().UTC()
	if at.Before(notBefore) {
		return notBefore
	}
	return at
}

func sourceCallFailure(err error, at time.Time) (handoff.Failure, bool) {
	failure := handoff.Failure{
		Code: handoff.FailureUnavailable, Message: "target peer is temporarily unavailable", Retryable: true, At: at,
	}
	var rpcErr *arcmuxmesh.RPCError
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case arcmuxmesh.ErrorPermissionDenied:
			return handoff.Failure{Code: handoff.FailureUnauthorized, Message: "target denied handoff preparation", At: at}, false
		case arcmuxmesh.ErrorInvalidRequest, arcmuxmesh.ErrorPayloadTooLarge, arcmuxmesh.ErrorUnsupportedMethod:
			return handoff.Failure{Code: handoff.FailureInvalidManifest, Message: "target rejected the handoff request", At: at}, false
		case arcmuxmesh.ErrorCapabilityRequired, arcmuxmesh.ErrorBackpressure, arcmuxmesh.ErrorInternal:
			return failure, true
		}
	}
	if errors.Is(err, arcmuxmesh.ErrMethodNotRegistered) {
		return handoff.Failure{Code: handoff.FailureInternal, Message: "local handoff protocol is unavailable", At: at}, false
	}
	return failure, true
}

func validSourceFailureCode(code handoff.FailureCode) bool {
	switch code {
	case handoff.FailureUnavailable, handoff.FailureUnauthorized, handoff.FailureInvalidManifest, handoff.FailureConflict,
		handoff.FailureMissingAsset, handoff.FailureVerification, handoff.FailureLaunch, handoff.FailureInternal:
		return true
	default:
		return false
	}
}

func sourceHandoffRecordDTO(record handoff.SourceRecord) sourceHandoffDTO {
	dto := sourceHandoffDTO{
		HandoffID: record.Manifest.HandoffID, ManifestDigest: record.Digest, State: record.State,
		Attempts: record.Attempts, TargetDevice: record.Manifest.Target.DeviceID, Project: record.Manifest.Repository.ProjectSlug,
	}
	if record.Failure != nil {
		dto.Failure = &meshHandoffFailureDTO{
			Code: record.Failure.Code, Message: record.Failure.Message, Retryable: record.Failure.Retryable,
		}
	}
	return dto
}

func sourceOutboxInvalid(message string) error {
	return &sourceHandoffError{kind: sourceHandoffInvalid, message: message}
}

func sourceOutboxNotFound(message string) error {
	return &sourceHandoffError{kind: sourceHandoffNotFound, message: message}
}

func sourceOutboxConflict(message string) error {
	return &sourceHandoffError{kind: sourceHandoffConflict, message: message}
}

func sourceOutboxUnavailable(message string) error {
	return &sourceHandoffError{kind: sourceHandoffUnavailable, message: message}
}

func sourceHandoffErrorKindOf(err error) sourceHandoffErrorKind {
	var sourceErr *sourceHandoffError
	if errors.As(err, &sourceErr) {
		return sourceErr.kind
	}
	return sourceHandoffUnavailable
}
