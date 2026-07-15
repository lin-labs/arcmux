package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lin-labs/arcmux/internal/handoff"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/meshstate"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

const (
	meshTopicSessions  = "sessions"
	meshTopicArtifacts = "artifacts"
	meshPageItemMax    = 256
	meshPageTTL        = 30 * time.Second
	meshPageCacheMax   = 64
	// Leave room for the RPC response wrapper and future protocol metadata.
	// Every encoded application result is checked against this smaller budget.
	meshPageResultBudget = arcmuxmesh.MaxApplicationPayload - 1024

	meshMethodSessionsList      = "sessions.list"
	meshMethodSessionsGet       = "sessions.get"
	meshMethodArtifactsList     = "artifacts.list"
	meshMethodArtifactsGet      = "artifacts.get"
	meshMethodEventsSubscribe   = "events.subscribe"
	meshMethodEventsUnsubscribe = "events.unsubscribe"
	meshMethodHandoffsPrepare   = "handoffs.prepare"
	meshMethodHandoffsStatus    = "handoffs.status"
	meshMethodHandoffsLaunch    = "handoffs.launch"
)

var meshMethodSpecs = []arcmuxmesh.MethodSpec{
	{Name: meshMethodSessionsList, Capability: arcmuxmesh.CapabilitySessionsReadV1, RequiredScope: arcmuxmesh.ScopeSessionsRead},
	{Name: meshMethodSessionsGet, Capability: arcmuxmesh.CapabilitySessionsReadV1, RequiredScope: arcmuxmesh.ScopeSessionsRead},
	{Name: meshMethodArtifactsList, Capability: arcmuxmesh.CapabilityArtifactsReadV1, RequiredScope: arcmuxmesh.ScopeArtifactsRead},
	{Name: meshMethodArtifactsGet, Capability: arcmuxmesh.CapabilityArtifactsReadV1, RequiredScope: arcmuxmesh.ScopeArtifactsRead},
	{Name: meshMethodEventsSubscribe, Capability: arcmuxmesh.CapabilityEventsV1, RequiredScope: arcmuxmesh.ScopeEventsRead},
	{Name: meshMethodEventsUnsubscribe, Capability: arcmuxmesh.CapabilityEventsV1, RequiredScope: arcmuxmesh.ScopeEventsRead},
	{Name: meshMethodHandoffsPrepare, Capability: arcmuxmesh.CapabilityHandoffsV1, RequiredScope: arcmuxmesh.ScopeHandoffsPrepare},
	{Name: meshMethodHandoffsStatus, Capability: arcmuxmesh.CapabilityHandoffsV1, RequiredScope: arcmuxmesh.ScopeHandoffsPrepare},
	{Name: meshMethodHandoffsLaunch, Capability: arcmuxmesh.CapabilityHandoffsV1, RequiredScope: arcmuxmesh.ScopeHandoffsLaunch},
}

type meshPeerSubscription struct {
	connectedAt time.Time
	topics      map[string]bool
	needsGap    bool
}

// meshSyncState coalesces bursts without losing causality. dirty means an
// event/gap arrived after the current inventory began, so one more complete
// pass is required. resubscribe is sticky across an in-flight pass so a
// reconnect cannot lose its restore intent behind ordinary event work.
type meshSyncState struct {
	dirty       bool
	resubscribe bool
}

type meshPageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type cachedSessionInventory struct {
	created  time.Time
	response meshSessionsListResponse
}

type cachedArtifactInventory struct {
	created  time.Time
	kind     meshstate.ArtifactKind
	response meshArtifactsListResponse
}

type meshApplication struct {
	store    *meshstate.Store
	handoffs *handoffApplication
	epoch    string
	deviceID string
	revision atomic.Uint64

	subsMu   sync.RWMutex
	subs     map[string]meshPeerSubscription // inbound: peers subscribed to us
	outbound map[string]meshPeerSubscription // desired outbound subscriptions

	pagesMu       sync.Mutex
	sessionPages  map[string]cachedSessionInventory
	artifactPages map[string]cachedArtifactInventory

	runtimeMu         sync.Mutex
	cancel            context.CancelFunc
	runtimeCtx        context.Context
	handoffWake       chan struct{}
	sourceHandoffWake chan struct{}
	wg                sync.WaitGroup

	syncMu            sync.Mutex
	syncing           map[string]*meshSyncState
	reconcileInterval time.Duration
	// beforeSync is nil in production. Tests use it to stop a pass at a
	// deterministic boundary and prove reconnect/event dirty-rerun behavior.
	beforeSync func(string)
}

func (d *Daemon) setMeshDeviceID(deviceID string) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.runtimeMu.Lock()
	app.deviceID = deviceID
	app.runtimeMu.Unlock()
}

func (d *Daemon) meshDeviceID() string {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return ""
	}
	app.runtimeMu.Lock()
	defer app.runtimeMu.Unlock()
	return app.deviceID
}

type meshSessionsListResponse struct {
	SourceEpoch    string                     `json:"source_epoch"`
	SourceRevision uint64                     `json:"source_revision"`
	ProfileScopes  []sessionview.ProfileScope `json:"profile_scopes"`
	Sessions       []sessionview.Summary      `json:"sessions"`
	NextCursor     string                     `json:"next_cursor,omitempty"`
}

type meshSessionGetRequest struct {
	ProfileScope sessionview.ProfileScope `json:"profile_scope"`
	SessionID    string                   `json:"session_id"`
}

type meshSessionGetResponse struct {
	SourceEpoch    string             `json:"source_epoch"`
	SourceRevision uint64             `json:"source_revision"`
	Session        sessionview.Detail `json:"session"`
}

type meshArtifactsListRequest struct {
	Kind meshstate.ArtifactKind `json:"kind,omitempty"`
	meshPageRequest
}

type meshArtifactsListResponse struct {
	SourceEpoch    string                  `json:"source_epoch"`
	SourceRevision uint64                  `json:"source_revision"`
	Artifacts      []meshArtifactReference `json:"artifacts"`
	NextCursor     string                  `json:"next_cursor,omitempty"`
}

type meshArtifactGetRequest struct {
	Kind meshstate.ArtifactKind `json:"kind"`
	ID   string                 `json:"id"`
}

type meshArtifactGetResponse struct {
	SourceEpoch    string                `json:"source_epoch"`
	SourceRevision uint64                `json:"source_revision"`
	Artifact       meshArtifactReference `json:"artifact"`
}

// meshArtifactReference is the complete artifact wire allowlist. It carries
// stable structured references only: no URL, title, state, path, branch ref,
// provenance, content, or remote-controlled received timestamp crosses the
// mesh. URLs are caller-controlled free text even after syntactic validation
// and may embed credentials in paths or ordinary query values.
type meshArtifactReference struct {
	ID      string                  `json:"id"`
	Kind    meshstate.ArtifactKind  `json:"kind"`
	Repo    *meshArtifactRepoRef    `json:"repo,omitempty"`
	Session *meshArtifactSessionRef `json:"session,omitempty"`
}

type meshArtifactRepoRef struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit,omitempty"`
}

type meshArtifactSessionRef struct {
	ProfileScope sessionview.ProfileScope `json:"profile_scope"`
	SessionID    string                   `json:"session_id"`
}

type meshPageTooLargeError struct{}

func (meshPageTooLargeError) Error() string {
	return "one inventory item exceeds the safe mesh page budget"
}

type meshSubscriptionRequest struct {
	Topics []string `json:"topics,omitempty"`
}

type meshSubscriptionResponse struct {
	Topics []string `json:"topics"`
}

type meshSessionEvent struct {
	Kind           string                   `json:"kind"`
	SourceEpoch    string                   `json:"source_epoch"`
	SourceRevision uint64                   `json:"source_revision"`
	ProfileScope   sessionview.ProfileScope `json:"profile_scope"`
	SessionID      string                   `json:"session_id"`
	Session        *sessionview.Detail      `json:"session,omitempty"`
}

func (d *Daemon) initMeshApplication() error {
	if d.cfg.Daemon.ProfileName != "" {
		return nil
	}
	root, err := d.meshProtocolRoot()
	if err != nil {
		return err
	}
	store, err := meshstate.Open(root)
	if err != nil {
		return err
	}
	handoffStore, err := handoff.Open(root)
	if err != nil {
		return fmt.Errorf("open handoff state: %w", err)
	}
	outbound := make(map[string]meshPeerSubscription)
	peers, err := store.ListPeers()
	if err != nil {
		return fmt.Errorf("load durable mesh subscription intent: %w", err)
	}
	for _, peer := range peers {
		topics, err := validateMeshTopics(peer.DesiredTopics)
		if err != nil {
			return fmt.Errorf("load durable mesh topics for %s: %w", peer.DeviceID, err)
		}
		if len(topics) == 0 {
			continue
		}
		set := make(map[string]bool, len(topics))
		for _, topic := range topics {
			set[topic] = true
		}
		outbound[peer.DeviceID] = meshPeerSubscription{topics: set}
	}
	handoffs := newHandoffApplication(handoffStore, d.profiles)
	handoffs.configureLaunchRuntime(d)
	d.meshMu.Lock()
	if d.meshApp == nil {
		d.meshApp = &meshApplication{
			store: store, handoffs: handoffs, epoch: fmt.Sprintf("boot-%d", time.Now().UTC().UnixNano()),
			subs: make(map[string]meshPeerSubscription), outbound: outbound,
			sessionPages: make(map[string]cachedSessionInventory), artifactPages: make(map[string]cachedArtifactInventory),
			syncing: make(map[string]*meshSyncState), reconcileInterval: 15 * time.Second,
		}
	}
	d.meshMu.Unlock()
	return nil
}

func (d *Daemon) meshProtocolRoot() (string, error) {
	return d.cfg.ProtocolStateRoot()
}

func (d *Daemon) meshStateStore() (*meshstate.Store, error) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || app.store == nil {
		return nil, errors.New("mesh state is unavailable")
	}
	return app.store, nil
}

func (d *Daemon) registerMeshApplication(manager *arcmuxmesh.Manager) error {
	if manager == nil {
		return errors.New("mesh manager is nil")
	}
	if _, err := d.meshStateStore(); err != nil {
		if initErr := d.initMeshApplication(); initErr != nil {
			return initErr
		}
	}
	handlers := map[string]arcmuxmesh.RequestHandler{
		meshMethodSessionsList: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshPageRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(err)
			}
			response, err := d.localMeshSessionsPage(principal.PeerID, request)
			if err != nil {
				return nil, meshRequestError(err)
			}
			return response, nil
		},
		meshMethodSessionsGet: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshSessionGetRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(err)
			}
			locator, err := sessionview.NewLocator(request.ProfileScope, request.SessionID)
			if err != nil {
				return nil, meshInvalidRequest(err)
			}
			detail, ok := d.SessionCatalog().Get(locator)
			if !ok {
				return nil, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInvalidRequest, Message: "session not found"}
			}
			detail, err = d.meshSafeDetail(detail)
			if err != nil {
				return nil, meshInvalidRequest(err)
			}
			_ = principal
			return meshSessionGetResponse{SourceEpoch: d.meshEpoch(), SourceRevision: d.nextMeshRevision(), Session: detail}, nil
		},
		meshMethodArtifactsList: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshArtifactsListRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(err)
			}
			response, err := d.localMeshArtifactsPage(principal.PeerID, request)
			if err != nil {
				return nil, meshRequestError(err)
			}
			return response, nil
		},
		meshMethodArtifactsGet: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshArtifactGetRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(err)
			}
			store, err := d.meshStateStore()
			if err != nil {
				return nil, err
			}
			artifact, err := store.GetArtifact(request.Kind, request.ID)
			if err != nil {
				if errors.Is(err, meshstate.ErrNotFound) {
					return nil, &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInvalidRequest, Message: "artifact not found"}
				}
				return nil, meshInvalidRequest(err)
			}
			reference, err := meshArtifactReferenceFromEnvelope(artifact)
			if err != nil {
				return nil, meshInvalidRequest(err)
			}
			_ = principal
			return meshArtifactGetResponse{
				SourceEpoch: d.meshEpoch(), SourceRevision: d.nextMeshRevision(), Artifact: reference,
			}, nil
		},
		meshMethodEventsSubscribe: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshSubscriptionRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(err)
			}
			topics, err := validateMeshTopics(request.Topics)
			if err != nil {
				return nil, meshInvalidRequest(err)
			}
			if err := authorizeMeshTopics(principal, topics); err != nil {
				return nil, err
			}
			if err := d.replacePeerSubscription(manager, principal.PeerID, topics); err != nil {
				return nil, err
			}
			return meshSubscriptionResponse{Topics: topics}, nil
		},
		meshMethodEventsUnsubscribe: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshSubscriptionRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(err)
			}
			d.clearPeerSubscription(principal.PeerID)
			return meshSubscriptionResponse{Topics: []string{}}, nil
		},
		meshMethodHandoffsPrepare: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshHandoffPrepareRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(errors.New("invalid handoff prepare request"))
			}
			app, err := d.handoffApplication()
			if err != nil {
				return nil, err
			}
			resumeCtx, cancel := app.resumeContext(ctx)
			defer cancel()
			return app.prepare(resumeCtx, principal, d.meshDeviceID(), request)
		},
		meshMethodHandoffsStatus: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshHandoffStatusRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(errors.New("invalid handoff status request"))
			}
			app, err := d.handoffApplication()
			if err != nil {
				return nil, err
			}
			return app.status(ctx, principal, request)
		},
		meshMethodHandoffsLaunch: func(ctx context.Context, principal arcmuxmesh.Principal, raw json.RawMessage) (any, error) {
			var request meshHandoffLaunchRequest
			if err := decodeMeshParams(raw, &request); err != nil {
				return nil, meshInvalidRequest(errors.New("invalid handoff launch request"))
			}
			app, err := d.handoffApplication()
			if err != nil {
				return nil, err
			}
			resumeCtx, cancel := app.resumeContext(ctx)
			defer cancel()
			return app.launch(resumeCtx, principal, d.meshDeviceID(), request)
		},
	}
	for _, spec := range meshMethodSpecs {
		if err := manager.RegisterHandler(spec, handlers[spec.Name]); err != nil {
			return err
		}
	}
	return nil
}

func decodeMeshParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request contains multiple JSON values")
	}
	return nil
}

func meshInvalidRequest(err error) *arcmuxmesh.RPCError {
	message := "invalid request"
	if err != nil {
		message = err.Error()
		if len(message) > 200 {
			message = message[:200]
		}
	}
	return &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorInvalidRequest, Message: message}
}

func meshRequestError(err error) *arcmuxmesh.RPCError {
	var tooLarge meshPageTooLargeError
	if errors.As(err, &tooLarge) {
		return &arcmuxmesh.RPCError{Code: arcmuxmesh.ErrorPayloadTooLarge, Message: tooLarge.Error()}
	}
	return meshInvalidRequest(err)
}

func (d *Daemon) meshEpoch() string {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || app.epoch == "" {
		return "boot-unavailable"
	}
	return app.epoch
}

func (d *Daemon) nextMeshRevision() uint64 {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return 1
	}
	return app.revision.Add(1)
}

func (d *Daemon) localMeshSessions() meshSessionsListResponse {
	list := d.SessionCatalog().List()
	safe := make([]sessionview.Summary, 0, len(list.Sessions))
	for _, summary := range list.Sessions {
		clean, err := d.meshSafeSummary(summary)
		if err != nil {
			d.logger.Warn("omit unsafe session from mesh inventory", "profile", summary.Locator.ProfileScope, "session", summary.Locator.SessionID, "error", err)
			continue
		}
		safe = append(safe, clean)
	}
	return meshSessionsListResponse{
		SourceEpoch: d.meshEpoch(), SourceRevision: d.nextMeshRevision(),
		ProfileScopes: d.localMeshProfileScopes(), Sessions: safe,
	}
}

func (d *Daemon) localMeshSessionsPage(peer string, request meshPageRequest) (meshSessionsListResponse, error) {
	limit, err := meshPageLimit(request.Limit, meshPageItemMax)
	if err != nil {
		return meshSessionsListResponse{}, err
	}
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return meshSessionsListResponse{}, errors.New("mesh state is unavailable")
	}
	if request.Cursor == "" {
		full := d.localMeshSessions()
		return app.firstSessionPage(peer, full, limit)
	}
	revision, offset, err := parseMeshCursor("s", request.Cursor)
	if err != nil {
		return meshSessionsListResponse{}, err
	}
	return app.nextSessionPage(peer, revision, offset, limit)
}

func (d *Daemon) localMeshArtifactsPage(peer string, request meshArtifactsListRequest) (meshArtifactsListResponse, error) {
	limit, err := meshPageLimit(request.Limit, meshPageItemMax)
	if err != nil {
		return meshArtifactsListResponse{}, err
	}
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return meshArtifactsListResponse{}, errors.New("mesh state is unavailable")
	}
	if request.Cursor != "" {
		revision, offset, err := parseMeshCursor("a", request.Cursor)
		if err != nil {
			return meshArtifactsListResponse{}, err
		}
		return app.nextArtifactPage(peer, request.Kind, revision, offset, limit)
	}
	store, err := d.meshStateStore()
	if err != nil {
		return meshArtifactsListResponse{}, err
	}
	artifacts, err := store.ListArtifacts(request.Kind)
	if err != nil {
		return meshArtifactsListResponse{}, err
	}
	local := make([]meshArtifactReference, 0, len(artifacts))
	for _, artifact := range artifacts {
		reference, err := meshArtifactReferenceFromEnvelope(artifact)
		if err != nil {
			return meshArtifactsListResponse{}, err
		}
		local = append(local, reference)
	}
	full := meshArtifactsListResponse{
		SourceEpoch: d.meshEpoch(), SourceRevision: d.nextMeshRevision(), Artifacts: local,
	}
	return app.firstArtifactPage(peer, request.Kind, full, limit)
}

func meshPageLimit(requested, maximum int) (int, error) {
	if requested == 0 {
		return maximum, nil
	}
	if requested < 1 || requested > maximum {
		return 0, fmt.Errorf("limit must be between 1 and %d", maximum)
	}
	return requested, nil
}

func meshCursor(prefix string, revision uint64, offset int) string {
	return fmt.Sprintf("%s.%d.%d", prefix, revision, offset)
}

func parseMeshCursor(prefix, cursor string) (uint64, int, error) {
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 || parts[0] != prefix {
		return 0, 0, errors.New("invalid cursor")
	}
	revision, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || revision == 0 {
		return 0, 0, errors.New("invalid cursor revision")
	}
	offset64, err := strconv.ParseUint(parts[2], 10, 31)
	if err != nil || offset64 == 0 {
		return 0, 0, errors.New("invalid cursor offset")
	}
	return revision, int(offset64), nil
}

func meshPageKey(peer string, revision uint64) string {
	return peer + "/" + strconv.FormatUint(revision, 10)
}

func (app *meshApplication) prunePagesLocked(now time.Time) {
	for key, page := range app.sessionPages {
		if now.Sub(page.created) > meshPageTTL {
			delete(app.sessionPages, key)
		}
	}
	for key, page := range app.artifactPages {
		if now.Sub(page.created) > meshPageTTL {
			delete(app.artifactPages, key)
		}
	}
}

func (app *meshApplication) boundSessionPagesLocked() {
	for len(app.sessionPages) >= meshPageCacheMax {
		oldestKey := ""
		var oldest time.Time
		for key, page := range app.sessionPages {
			if oldestKey == "" || page.created.Before(oldest) {
				oldestKey, oldest = key, page.created
			}
		}
		delete(app.sessionPages, oldestKey)
	}
}

func (app *meshApplication) boundArtifactPagesLocked() {
	for len(app.artifactPages) >= meshPageCacheMax {
		oldestKey := ""
		var oldest time.Time
		for key, page := range app.artifactPages {
			if oldestKey == "" || page.created.Before(oldest) {
				oldestKey, oldest = key, page.created
			}
		}
		delete(app.artifactPages, oldestKey)
	}
}

func (app *meshApplication) firstSessionPage(peer string, full meshSessionsListResponse, limit int) (meshSessionsListResponse, error) {
	page, end, err := buildSessionPage(full, 0, limit)
	if err != nil {
		return meshSessionsListResponse{}, err
	}
	if end < len(full.Sessions) {
		app.pagesMu.Lock()
		app.prunePagesLocked(time.Now())
		app.boundSessionPagesLocked()
		app.sessionPages[meshPageKey(peer, full.SourceRevision)] = cachedSessionInventory{created: time.Now(), response: full}
		app.pagesMu.Unlock()
	}
	return page, nil
}

func (app *meshApplication) nextSessionPage(peer string, revision uint64, offset, limit int) (meshSessionsListResponse, error) {
	key := meshPageKey(peer, revision)
	app.pagesMu.Lock()
	defer app.pagesMu.Unlock()
	app.prunePagesLocked(time.Now())
	cached, ok := app.sessionPages[key]
	if !ok || offset >= len(cached.response.Sessions) {
		return meshSessionsListResponse{}, errors.New("cursor is expired or out of range")
	}
	page, end, err := buildSessionPage(cached.response, offset, limit)
	if err != nil {
		return meshSessionsListResponse{}, err
	}
	if end == len(cached.response.Sessions) {
		delete(app.sessionPages, key)
	}
	return page, nil
}

func buildSessionPage(full meshSessionsListResponse, offset, limit int) (meshSessionsListResponse, int, error) {
	page := full
	page.Sessions = make([]sessionview.Summary, 0)
	page.NextCursor = ""
	if offset == len(full.Sessions) {
		if !meshResultFits(page) {
			return meshSessionsListResponse{}, offset, meshPageTooLargeError{}
		}
		return page, offset, nil
	}
	end := offset
	for end < len(full.Sessions) && end-offset < limit {
		candidate := page
		candidate.Sessions = append(append([]sessionview.Summary(nil), page.Sessions...), full.Sessions[end])
		candidate.NextCursor = ""
		if end+1 < len(full.Sessions) {
			candidate.NextCursor = meshCursor("s", full.SourceRevision, end+1)
		}
		if !meshResultFits(candidate) {
			break
		}
		page = candidate
		end++
	}
	if end == offset {
		return meshSessionsListResponse{}, offset, meshPageTooLargeError{}
	}
	return page, end, nil
}

func (app *meshApplication) firstArtifactPage(peer string, kind meshstate.ArtifactKind, full meshArtifactsListResponse, limit int) (meshArtifactsListResponse, error) {
	page, end, err := buildArtifactPage(full, 0, limit)
	if err != nil {
		return meshArtifactsListResponse{}, err
	}
	if end < len(full.Artifacts) {
		app.pagesMu.Lock()
		app.prunePagesLocked(time.Now())
		app.boundArtifactPagesLocked()
		app.artifactPages[meshPageKey(peer, full.SourceRevision)] = cachedArtifactInventory{created: time.Now(), kind: kind, response: full}
		app.pagesMu.Unlock()
	}
	return page, nil
}

func (app *meshApplication) nextArtifactPage(peer string, kind meshstate.ArtifactKind, revision uint64, offset, limit int) (meshArtifactsListResponse, error) {
	key := meshPageKey(peer, revision)
	app.pagesMu.Lock()
	defer app.pagesMu.Unlock()
	app.prunePagesLocked(time.Now())
	cached, ok := app.artifactPages[key]
	if !ok || cached.kind != kind || offset >= len(cached.response.Artifacts) {
		return meshArtifactsListResponse{}, errors.New("cursor is expired or out of range")
	}
	page, end, err := buildArtifactPage(cached.response, offset, limit)
	if err != nil {
		return meshArtifactsListResponse{}, err
	}
	if end == len(cached.response.Artifacts) {
		delete(app.artifactPages, key)
	}
	return page, nil
}

func buildArtifactPage(full meshArtifactsListResponse, offset, limit int) (meshArtifactsListResponse, int, error) {
	page := full
	page.Artifacts = make([]meshArtifactReference, 0)
	page.NextCursor = ""
	if offset == len(full.Artifacts) {
		if !meshResultFits(page) {
			return meshArtifactsListResponse{}, offset, meshPageTooLargeError{}
		}
		return page, offset, nil
	}
	end := offset
	for end < len(full.Artifacts) && end-offset < limit {
		candidate := page
		candidate.Artifacts = append(append([]meshArtifactReference(nil), page.Artifacts...), full.Artifacts[end])
		candidate.NextCursor = ""
		if end+1 < len(full.Artifacts) {
			candidate.NextCursor = meshCursor("a", full.SourceRevision, end+1)
		}
		if !meshResultFits(candidate) {
			break
		}
		page = candidate
		end++
	}
	if end == offset {
		return meshArtifactsListResponse{}, offset, meshPageTooLargeError{}
	}
	return page, end, nil
}

func meshResultFits(value any) bool {
	result, err := json.Marshal(value)
	if err != nil {
		return false
	}
	payload, err := json.Marshal(struct {
		Result json.RawMessage `json:"result,omitempty"`
	}{Result: result})
	return err == nil && len(payload) <= meshPageResultBudget
}

func (d *Daemon) localMeshProfileScopes() []sessionview.ProfileScope {
	if d.cfg.Daemon.ProfileName != "" {
		scope, err := sessionview.NamedProfileScope(d.cfg.Daemon.ProfileName)
		if err != nil {
			return nil
		}
		return []sessionview.ProfileScope{scope}
	}
	scopes := []sessionview.ProfileScope{sessionview.RootProfileScope}
	if d.profileManager != nil {
		for name := range d.profileManager.SnapshotDaemons() {
			scope, err := sessionview.NamedProfileScope(name)
			if err == nil {
				scopes = append(scopes, scope)
			}
		}
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i] < scopes[j] })
	return scopes
}

func (d *Daemon) meshSafeSummary(summary sessionview.Summary) (sessionview.Summary, error) {
	if summary.Private {
		// A handoff continuation's exact target-local checkout is private
		// protocol state, not a mesh inventory hint.
		summary.LaunchCWD = ""
	} else {
		summary.LaunchCWD = meshPathHint(summary.LaunchCWD)
	}
	// Hook Goal/OverallGoal currently lack field-level provenance proving they
	// were machine-summarized rather than copied or seeded from a raw prompt.
	// Regex redaction cannot make arbitrary prompt text safe, so phase two
	// omits Work at the transport boundary until that provenance exists.
	summary.Work = nil
	summary.History = nil
	return meshSanitizeSummary(summary)
}

func (d *Daemon) meshSafeDetail(detail sessionview.Detail) (sessionview.Detail, error) {
	var err error
	detail.Summary, err = d.meshSafeSummary(detail.Summary)
	if err != nil {
		return sessionview.Detail{}, err
	}
	return meshSanitizeDetail(detail)
}

func meshAcceptSummary(summary sessionview.Summary) (sessionview.Summary, error) {
	// Treat every remote payload as untrusted even when the current server-side
	// implementation already omits turn-contract text. A stale or malicious
	// peer must not be able to seed raw prompt-derived Work into projections.
	summary.Work = nil
	summary.History = nil
	var err error
	summary, err = meshSanitizeSummary(summary)
	if err != nil {
		return sessionview.Summary{}, err
	}
	if summary.LaunchCWD == "~" {
		return summary, nil
	}
	if !strings.HasPrefix(summary.LaunchCWD, "~/") {
		summary.LaunchCWD = ""
		return summary, nil
	}
	rel := filepath.Clean(strings.TrimPrefix(summary.LaunchCWD, "~/"))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		summary.LaunchCWD = ""
		return summary, nil
	}
	summary.LaunchCWD = filepath.Join("~", rel)
	return summary, nil
}

func meshAcceptDetail(detail sessionview.Detail) (sessionview.Detail, error) {
	var err error
	detail.Summary, err = meshAcceptSummary(detail.Summary)
	if err != nil {
		return sessionview.Detail{}, err
	}
	return meshSanitizeDetail(detail)
}

const (
	meshSessionNameRunes  = 128
	meshSessionFieldRunes = 128
	meshSessionCWDRunes   = 4096
	meshSessionOwnerRunes = 256
)

func meshSanitizeSummary(summary sessionview.Summary) (sessionview.Summary, error) {
	if err := summary.Locator.Validate(); err != nil {
		return sessionview.Summary{}, err
	}
	summary.Name = meshCleanText(summary.Name, meshSessionNameRunes)
	summary.Agent = meshCleanText(summary.Agent, meshSessionFieldRunes)
	summary.Transport = meshCleanText(summary.Transport, meshSessionFieldRunes)
	summary.LaunchCWD = meshCleanText(summary.LaunchCWD, meshSessionCWDRunes)
	summary.OwnerID = meshCleanText(summary.OwnerID, meshSessionOwnerRunes)
	summary.State = meshCleanText(summary.State, meshSessionFieldRunes)
	summary.Health = meshCleanText(summary.Health, meshSessionFieldRunes)
	currentWork, err := sessionview.NormalizeCurrentWork(summary.CurrentWork)
	if err != nil {
		return sessionview.Summary{}, err
	}
	summary.CurrentWork = currentWork
	if strings.TrimSpace(summary.Agent) == "" || strings.TrimSpace(summary.State) == "" {
		return sessionview.Summary{}, errors.New("session agent and state are required")
	}
	now := time.Now().UTC()
	for field, value := range map[string]time.Time{
		"started_at": summary.StartedAt, "last_activity_at": summary.LastActivityAt,
		"freshness.observed_at":       summary.Freshness.ObservedAt,
		"freshness.source_updated_at": summary.Freshness.SourceUpdatedAt,
	} {
		if err := validateMeshTimestamp(field, value, now); err != nil {
			return sessionview.Summary{}, err
		}
	}
	if summary.CurrentWork != nil {
		if err := validateMeshTimestamp("current_work.updated_at", summary.CurrentWork.UpdatedAt, now); err != nil {
			return sessionview.Summary{}, err
		}
	}
	for field, value := range map[string]*time.Time{"idle_since": summary.IdleSince, "working_since": summary.WorkingSince} {
		if value == nil {
			continue
		}
		if err := validateMeshTimestamp(field, *value, now); err != nil {
			return sessionview.Summary{}, err
		}
		clean := value.UTC()
		if field == "idle_since" {
			summary.IdleSince = &clean
		} else {
			summary.WorkingSince = &clean
		}
	}
	summary.StartedAt = summary.StartedAt.UTC()
	summary.LastActivityAt = summary.LastActivityAt.UTC()
	summary.Freshness.ObservedAt = summary.Freshness.ObservedAt.UTC()
	summary.Freshness.SourceUpdatedAt = summary.Freshness.SourceUpdatedAt.UTC()
	summary.Work = nil
	summary.History = nil
	return summary, nil
}

func meshSanitizeDetail(detail sessionview.Detail) (sessionview.Detail, error) {
	if detail.NudgeCount < 0 {
		return sessionview.Detail{}, errors.New("session nudge count cannot be negative")
	}
	if detail.Turn == nil {
		return detail, nil
	}
	if detail.Turn.TurnCount < 0 || detail.Turn.EventsSeen < 0 {
		return sessionview.Detail{}, errors.New("session turn counters cannot be negative")
	}
	now := time.Now().UTC()
	for field, value := range map[string]*time.Time{
		"last_prompt_submit_at": detail.Turn.LastPromptSubmitAt,
		"last_turn_end_at":      detail.Turn.LastTurnEndAt,
	} {
		if value == nil {
			continue
		}
		if err := validateMeshTimestamp("turn."+field, *value, now); err != nil {
			return sessionview.Detail{}, err
		}
		clean := value.UTC()
		if field == "last_prompt_submit_at" {
			detail.Turn.LastPromptSubmitAt = &clean
		} else {
			detail.Turn.LastTurnEndAt = &clean
		}
	}
	return detail, nil
}

func validateMeshTimestamp(field string, value, now time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", field)
	}
	value = value.UTC()
	if value.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || value.After(now.Add(24*time.Hour)) {
		return fmt.Errorf("%s is outside the accepted range", field)
	}
	return nil
}

func meshCleanText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, value)
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	return string([]rune(value)[:maxRunes])
}

func meshPathHint(value string) string {
	if value == "" {
		return ""
	}
	if !filepath.IsAbs(value) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(home, value)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if rel == "." {
		return "~"
	}
	return filepath.Join("~", rel)
}

func validateMeshTopics(topics []string) ([]string, error) {
	seen := make(map[string]bool, len(topics))
	out := make([]string, 0, len(topics))
	for _, topic := range topics {
		if topic != meshTopicSessions && topic != meshTopicArtifacts {
			return nil, fmt.Errorf("unsupported event topic %q", topic)
		}
		if !seen[topic] {
			seen[topic] = true
			out = append(out, topic)
		}
	}
	sort.Strings(out)
	return out, nil
}

// authorizeMeshTopics prevents events.read from becoming a side door around
// the data-specific read grants. The connection-derived Principal is the only
// authority; requested topics carry no identity or grant information.
func authorizeMeshTopics(principal arcmuxmesh.Principal, topics []string) error {
	for _, topic := range topics {
		requiredScope := ""
		switch topic {
		case meshTopicSessions:
			requiredScope = arcmuxmesh.ScopeSessionsRead
		case meshTopicArtifacts:
			requiredScope = arcmuxmesh.ScopeArtifactsRead
		default:
			return meshInvalidRequest(fmt.Errorf("unsupported event topic %q", topic))
		}
		if !principal.HasScope(requiredScope) {
			return &arcmuxmesh.RPCError{
				Code:    arcmuxmesh.ErrorPermissionDenied,
				Message: fmt.Sprintf("topic %q requires %s", topic, requiredScope),
			}
		}
	}
	return nil
}

func (d *Daemon) replacePeerSubscription(manager *arcmuxmesh.Manager, peer string, topics []string) error {
	connectedAt, ok := meshConnectedAt(manager, peer)
	if !ok {
		return arcmuxmesh.ErrPeerDisconnected
	}
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return errors.New("mesh state is unavailable")
	}
	set := make(map[string]bool, len(topics))
	for _, topic := range topics {
		set[topic] = true
	}
	app.subsMu.Lock()
	if len(set) == 0 {
		delete(app.subs, peer)
	} else {
		app.subs[peer] = meshPeerSubscription{connectedAt: connectedAt, topics: set}
	}
	app.subsMu.Unlock()
	return nil
}

func (d *Daemon) clearPeerSubscription(peer string) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.subsMu.Lock()
	delete(app.subs, peer)
	app.subsMu.Unlock()
}

func meshConnectedAt(manager *arcmuxmesh.Manager, peer string) (time.Time, bool) {
	if manager == nil {
		return time.Time{}, false
	}
	for _, status := range manager.Status() {
		if status.PeerID == peer && status.State == "connected" && status.ConnectedAt != nil {
			return *status.ConnectedAt, true
		}
	}
	return time.Time{}, false
}

func (d *Daemon) currentMeshManager() (*arcmuxmesh.Manager, error) {
	d.meshMu.RLock()
	manager := d.mesh
	d.meshMu.RUnlock()
	if manager == nil {
		return nil, errors.New("mesh is disabled")
	}
	return manager, nil
}

func (d *Daemon) detachMeshTransport() {
	d.meshMu.Lock()
	manager := d.mesh
	d.mesh = nil
	app := d.meshApp
	d.meshMu.Unlock()
	if app != nil {
		app.stopRuntime()
	}
	if manager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		manager.Stop(ctx)
		cancel()
	}
}

func (d *Daemon) stopMeshTransport() {
	d.meshReloadMu.Lock()
	d.detachMeshTransport()
	d.meshReloadMu.Unlock()
}

func (app *meshApplication) stopRuntime() {
	app.runtimeMu.Lock()
	cancel := app.cancel
	app.cancel = nil
	app.runtimeCtx = nil
	app.handoffWake = nil
	app.sourceHandoffWake = nil
	app.runtimeMu.Unlock()
	if cancel != nil {
		cancel()
		app.wg.Wait()
	}
	app.subsMu.Lock()
	app.subs = make(map[string]meshPeerSubscription)
	app.subsMu.Unlock()
}

func (d *Daemon) startMeshApplicationRuntime(manager *arcmuxmesh.Manager) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || manager == nil {
		return
	}
	app.stopRuntime()
	if err := d.restoreDesiredMeshTopics(); err != nil {
		d.logger.Warn("restore durable mesh subscriptions failed", "error", err)
	}
	ctx, cancel := context.WithCancel(d.ctx)
	remoteEvents, cancelRemoteEvents := manager.SubscribeEvents(64)
	storeEvents, cancelStoreEvents := app.store.Watch(64)
	var sessionEvents <-chan Event
	sessionSubID := -1
	if d.eventBus != nil {
		sessionEvents, sessionSubID = d.Subscribe("")
	}
	app.runtimeMu.Lock()
	app.cancel = cancel
	app.runtimeCtx = ctx
	app.handoffWake = make(chan struct{}, 1)
	handoffWake := app.handoffWake
	app.sourceHandoffWake = make(chan struct{}, 1)
	sourceHandoffWake := app.sourceHandoffWake
	app.wg.Add(3)
	app.runtimeMu.Unlock()
	go func() {
		defer app.wg.Done()
		defer cancelRemoteEvents()
		defer cancelStoreEvents()
		if sessionSubID >= 0 {
			defer d.Unsubscribe(sessionSubID)
		}
		statusTicker := time.NewTicker(250 * time.Millisecond)
		defer statusTicker.Stop()
		app.runtimeMu.Lock()
		reconcileInterval := app.reconcileInterval
		app.runtimeMu.Unlock()
		if reconcileInterval <= 0 {
			reconcileInterval = 15 * time.Second
		}
		reconcileTicker := time.NewTicker(reconcileInterval)
		defer reconcileTicker.Stop()
		knownConnections := make(map[string]time.Time)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-sessionEvents:
				if !ok {
					return
				}
				d.forwardMeshSessionEvent(sessionview.RootProfileScope, event)
			case change, ok := <-storeEvents:
				if !ok {
					return
				}
				d.forwardMeshStoreChange(change)
			case event, ok := <-remoteEvents:
				if !ok {
					return
				}
				d.handleRemoteMeshEvent(event)
			case <-statusTicker.C:
				d.reconcileMeshStatuses(manager, knownConnections)
				d.retryMeshGaps(manager)
			case <-reconcileTicker.C:
				d.scheduleTargetHandoffReconcile()
				d.scheduleSourceHandoffReconcile()
				for _, peer := range d.connectedDesiredMeshPeers(manager) {
					d.scheduleMeshSync(peer, d.meshSubscriptionNeedsRestore(manager, peer))
				}
			}
		}
	}()
	go func() {
		defer app.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-handoffWake:
				d.reconcileTargetHandoffs(ctx, time.Now().UTC())
			}
		}
	}()
	go func() {
		defer app.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sourceHandoffWake:
				d.reconcileSourceHandoffs(ctx, time.Now().UTC())
			}
		}
	}()
	d.scheduleTargetHandoffReconcile()
	d.scheduleSourceHandoffReconcile()
}

func (d *Daemon) reconcileMeshStatuses(manager *arcmuxmesh.Manager, known map[string]time.Time) {
	store, err := d.meshStateStore()
	if err != nil {
		return
	}
	seen := make(map[string]bool)
	for _, status := range manager.Status() {
		seen[status.PeerID] = true
		if status.State == "connected" && status.ConnectedAt != nil {
			if err := d.restoreDesiredMeshPeer(status.PeerID); err != nil {
				d.logger.Warn("restore durable mesh peer subscription failed", "peer", status.PeerID, "error", err)
			}
			previous, existed := known[status.PeerID]
			if !existed || !previous.Equal(*status.ConnectedAt) {
				known[status.PeerID] = *status.ConnectedAt
				d.clearSubscriptionUnlessConnection(status.PeerID, *status.ConnectedAt)
				d.scheduleMeshSync(status.PeerID, true)
				d.scheduleTargetHandoffReconcile()
				d.scheduleSourceHandoffReconcile()
			}
			continue
		}
		previous, tracked := known[status.PeerID]
		if !tracked || !previous.IsZero() {
			known[status.PeerID] = time.Time{}
			d.clearPeerSubscription(status.PeerID)
			_ = store.MarkPeerDisconnected(status.PeerID)
		}
	}
	for peer, previous := range known {
		if !seen[peer] {
			delete(known, peer)
			d.clearPeerSubscription(peer)
			if !previous.IsZero() {
				_ = store.MarkPeerDisconnected(peer)
			}
		}
	}
}

func (d *Daemon) clearSubscriptionUnlessConnection(peer string, connectedAt time.Time) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.subsMu.Lock()
	if current, ok := app.subs[peer]; ok && !current.connectedAt.Equal(connectedAt) {
		delete(app.subs, peer)
	}
	app.subsMu.Unlock()
}

func (d *Daemon) forwardMeshSessionEvent(scope sessionview.ProfileScope, event Event) {
	if event.Type != "state_changed" || event.SessionID == "" {
		return
	}
	kind := "upsert"
	var detail *sessionview.Detail
	if event.State == string(session.StateExited) || event.State == string(session.StateFailed) {
		kind = "remove"
	} else {
		locator, err := sessionview.NewLocator(scope, event.SessionID)
		if err != nil {
			return
		}
		found, ok := d.SessionCatalog().Get(locator)
		if !ok {
			return
		}
		safe, err := d.meshSafeDetail(found)
		if err != nil {
			d.logger.Warn("omit unsafe session event from mesh", "profile", scope, "session", event.SessionID, "error", err)
			return
		}
		detail = &safe
	}
	payload := meshSessionEvent{
		Kind: kind, SourceEpoch: d.meshEpoch(), SourceRevision: d.nextMeshRevision(),
		ProfileScope: scope, SessionID: event.SessionID, Session: detail,
	}
	d.sendMeshEvent(meshTopicSessions, "sessions.changed", payload)
}

func (d *Daemon) forwardProfileSessionEvents(ctx context.Context, profileName string, child *Daemon) {
	scope, err := sessionview.NamedProfileScope(profileName)
	if err != nil || child == nil {
		return
	}
	events, subscriptionID := child.Subscribe("")
	go func() {
		defer child.Unsubscribe(subscriptionID)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				d.forwardMeshSessionEvent(scope, event)
			}
		}
	}()
}

func (d *Daemon) forwardMeshStoreChange(change meshstate.Change) {
	if change.Type == meshstate.ChangeGap {
		d.sendMeshGap(change)
		return
	}
	if change.Entity != meshstate.EntityArtifact || change.Type != meshstate.ChangeUpsert {
		return
	}
	parts := strings.SplitN(change.Key, "/", 2)
	if len(parts) != 2 {
		return
	}
	store, err := d.meshStateStore()
	if err != nil {
		return
	}
	artifact, err := store.GetArtifact(meshstate.ArtifactKind(parts[0]), parts[1])
	if err != nil {
		return
	}
	reference, err := meshArtifactReferenceFromEnvelope(artifact)
	if err != nil {
		d.logger.Warn("omit unsafe artifact event from mesh", "kind", parts[0], "id", parts[1], "error", err)
		return
	}
	d.sendMeshEvent(meshTopicArtifacts, "artifacts.changed", struct {
		Change   meshstate.Change      `json:"change"`
		Artifact meshArtifactReference `json:"artifact"`
	}{Change: change, Artifact: reference})
}

func (d *Daemon) sendMeshGap(change meshstate.Change) {
	d.sendMeshEventToTopics([]string{meshTopicSessions, meshTopicArtifacts}, "events.gap", change)
}

func (d *Daemon) sendMeshEvent(topic, name string, data any) {
	d.sendMeshEventToTopics([]string{topic}, name, data)
}

func (d *Daemon) sendMeshEventToTopics(topics []string, name string, data any) {
	manager, err := d.currentMeshManager()
	if err != nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.subsMu.RLock()
	subscriptions := make(map[string]meshPeerSubscription, len(app.subs))
	for peer, subscription := range app.subs {
		subscriptions[peer] = subscription
	}
	app.subsMu.RUnlock()
	for peer, subscription := range subscriptions {
		connectedAt, connected := meshConnectedAt(manager, peer)
		if !connected || !connectedAt.Equal(subscription.connectedAt) || !subscribedToAny(subscription, topics) {
			continue
		}
		if err := manager.SendEvent(peer, arcmuxmesh.Event{Name: name, Data: payload}); err != nil {
			if errors.Is(err, arcmuxmesh.ErrBackpressure) {
				d.markMeshGap(peer, subscription.connectedAt)
				d.logger.Warn("mesh event queue full; gap marked for retry and periodic reconciliation", "peer", peer, "event", name)
				continue
			}
			d.logger.Debug("mesh event not delivered", "peer", peer, "event", name, "error", err)
		}
	}
}

func (d *Daemon) markMeshGap(peer string, connectedAt time.Time) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.subsMu.Lock()
	if subscription, ok := app.subs[peer]; ok && subscription.connectedAt.Equal(connectedAt) {
		subscription.needsGap = true
		app.subs[peer] = subscription
	}
	app.subsMu.Unlock()
}

func (d *Daemon) retryMeshGaps(manager *arcmuxmesh.Manager) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || manager == nil {
		return
	}
	app.subsMu.RLock()
	pending := make(map[string]time.Time)
	for peer, subscription := range app.subs {
		if subscription.needsGap {
			pending[peer] = subscription.connectedAt
		}
	}
	app.subsMu.RUnlock()
	payload := json.RawMessage(`{"reason":"sender_backpressure"}`)
	for peer, expectedConnection := range pending {
		connectedAt, connected := meshConnectedAt(manager, peer)
		if !connected || !connectedAt.Equal(expectedConnection) {
			continue
		}
		if err := manager.SendEvent(peer, arcmuxmesh.Event{Name: "events.gap", Data: payload}); err != nil {
			if !errors.Is(err, arcmuxmesh.ErrBackpressure) {
				d.logger.Debug("mesh gap retry not delivered", "peer", peer, "error", err)
			}
			continue
		}
		app.subsMu.Lock()
		if subscription, ok := app.subs[peer]; ok && subscription.connectedAt.Equal(expectedConnection) {
			subscription.needsGap = false
			app.subs[peer] = subscription
		}
		app.subsMu.Unlock()
	}
}

func subscribedToAny(subscription meshPeerSubscription, topics []string) bool {
	for _, topic := range topics {
		if subscription.topics[topic] {
			return true
		}
	}
	return false
}

func (d *Daemon) handleRemoteMeshEvent(event arcmuxmesh.PeerEvent) {
	switch event.Event.Name {
	case "sessions.changed", "artifacts.changed", "events.gap":
		// scheduleMeshSync marks an already-running pass dirty. In particular,
		// an explicit gap can never be swallowed by coalescing.
		d.scheduleMeshSync(event.PeerID, false)
	}
}

func (d *Daemon) scheduleMeshSync(peer string, resubscribe bool) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	// Register a new worker while holding runtimeMu. stopRuntime clears
	// runtimeCtx under the same lock before waiting, so no worker can be added
	// after teardown begins or outlive the projection store it may update.
	app.runtimeMu.Lock()
	if app.runtimeCtx == nil {
		app.runtimeMu.Unlock()
		return
	}
	app.syncMu.Lock()
	if state, running := app.syncing[peer]; running {
		state.dirty = true
		state.resubscribe = state.resubscribe || resubscribe
		app.syncMu.Unlock()
		app.runtimeMu.Unlock()
		return
	}
	app.syncing[peer] = &meshSyncState{resubscribe: resubscribe}
	app.wg.Add(1)
	app.syncMu.Unlock()
	app.runtimeMu.Unlock()
	go func() {
		defer app.wg.Done()
		for {
			app.runtimeMu.Lock()
			runtimeCtx := app.runtimeCtx
			app.runtimeMu.Unlock()
			if runtimeCtx == nil {
				app.finishMeshSync(peer)
				return
			}
			ctx, cancel := context.WithTimeout(runtimeCtx, 15*time.Second)
			topics := d.desiredMeshTopics(peer)
			if len(topics) == 0 {
				cancel()
				app.finishMeshSync(peer)
				return
			}
			_, syncErr := d.syncMeshPeerTopics(ctx, peer, topics)
			cancel()
			if syncErr != nil {
				d.logger.Debug("mesh event reconciliation failed; periodic retry remains armed", "peer", peer, "error", syncErr)
				if app.retryDirtyMeshSync(peer, false) {
					continue
				}
				return
			}

			app.syncMu.Lock()
			state := app.syncing[peer]
			resubscribeNow := state != nil && state.resubscribe
			if state != nil {
				state.resubscribe = false
			}
			app.syncMu.Unlock()
			if resubscribeNow {
				ctx, cancel := context.WithTimeout(runtimeCtx, 15*time.Second)
				_, err := d.SubscribeMeshPeer(ctx, peer, topics)
				cancel()
				if err != nil {
					d.logger.Debug("mesh subscription restore failed; periodic retry remains armed", "peer", peer, "error", err)
					if app.retryDirtyMeshSync(peer, true) {
						continue
					}
					return
				}
			}

			app.syncMu.Lock()
			state = app.syncing[peer]
			if state != nil && state.dirty {
				state.dirty = false
				app.syncMu.Unlock()
				continue
			}
			delete(app.syncing, peer)
			app.syncMu.Unlock()
			return
		}
	}()
}

func (app *meshApplication) finishMeshSync(peer string) {
	app.syncMu.Lock()
	delete(app.syncing, peer)
	app.syncMu.Unlock()
}

// retryDirtyMeshSync consumes one coalesced dirty edge and keeps the worker
// alive. If no edge arrived, it releases the slot so the bounded periodic pass
// can retry later without spinning on a persistent transport failure.
func (app *meshApplication) retryDirtyMeshSync(peer string, preserveResubscribe bool) bool {
	app.syncMu.Lock()
	defer app.syncMu.Unlock()
	state := app.syncing[peer]
	if state == nil {
		return false
	}
	state.resubscribe = state.resubscribe || preserveResubscribe
	if state.dirty {
		state.dirty = false
		return true
	}
	delete(app.syncing, peer)
	return false
}

func (d *Daemon) desiredMeshTopics(peer string) []string {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return nil
	}
	app.subsMu.RLock()
	subscription, ok := app.outbound[peer]
	app.subsMu.RUnlock()
	if !ok {
		return nil
	}
	topics := make([]string, 0, len(subscription.topics))
	for topic := range subscription.topics {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	return topics
}

func (d *Daemon) connectedDesiredMeshPeers(manager *arcmuxmesh.Manager) []string {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return nil
	}
	app.subsMu.RLock()
	peers := make([]string, 0, len(app.outbound))
	for peer := range app.outbound {
		if _, ok := meshConnectedAt(manager, peer); ok {
			peers = append(peers, peer)
		}
	}
	app.subsMu.RUnlock()
	sort.Strings(peers)
	return peers
}

func (d *Daemon) meshSubscriptionNeedsRestore(manager *arcmuxmesh.Manager, peer string) bool {
	connectedAt, ok := meshConnectedAt(manager, peer)
	if !ok {
		return false
	}
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return false
	}
	app.subsMu.RLock()
	subscription, desired := app.outbound[peer]
	app.subsMu.RUnlock()
	return desired && !subscription.connectedAt.Equal(connectedAt)
}

// RemoteSessionsList retrieves a full remote inventory and atomically updates
// every returned profile scope in the local projection store.
func (d *Daemon) RemoteSessionsList(ctx context.Context, peer string) ([]meshstate.RemoteSessionProjection, error) {
	manager, err := d.currentMeshManager()
	if err != nil {
		return nil, err
	}
	var response meshSessionsListResponse
	cursor := ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= 4096 {
			return nil, errors.New("remote session inventory exceeded page limit")
		}
		var page meshSessionsListResponse
		if err := manager.Call(ctx, peer, meshMethodSessionsList, meshPageRequest{Cursor: cursor}, &page); err != nil {
			return nil, err
		}
		if pageNumber == 0 {
			response = page
			response.Sessions = nil
		} else if page.SourceEpoch != response.SourceEpoch || page.SourceRevision != response.SourceRevision || !sameProfileScopes(page.ProfileScopes, response.ProfileScopes) {
			return nil, errors.New("remote session inventory cursor changed between pages")
		}
		if len(page.Sessions) > meshPageItemMax {
			return nil, errors.New("remote session page exceeds item bound")
		}
		response.Sessions = append(response.Sessions, page.Sessions...)
		cursor = page.NextCursor
		if cursor != "" {
			if len(page.Sessions) == 0 {
				return nil, errors.New("remote session inventory returned an empty continuation page")
			}
			revision, offset, err := parseMeshCursor("s", cursor)
			if err != nil || revision != response.SourceRevision || offset != len(response.Sessions) {
				return nil, errors.New("remote session inventory returned a non-contiguous cursor")
			}
		}
		if cursor == "" {
			response.NextCursor = ""
			break
		}
	}
	if err := d.commitRemoteSessions(peer, response); err != nil {
		return nil, err
	}
	store, err := d.meshStateStore()
	if err != nil {
		return nil, err
	}
	return store.ListRemoteSessions(peer, "")
}

func sameProfileScopes(left, right []sessionview.ProfileScope) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (d *Daemon) commitRemoteSessions(peer string, response meshSessionsListResponse) error {
	if response.SourceEpoch == "" || response.SourceRevision == 0 {
		return errors.New("remote session inventory has no source cursor")
	}
	store, err := d.meshStateStore()
	if err != nil {
		return err
	}
	byScope := make(map[sessionview.ProfileScope]bool, len(response.ProfileScopes))
	stateScopes := make([]meshstate.ProfileScope, 0, len(response.ProfileScopes))
	for _, scope := range response.ProfileScopes {
		if err := scope.Validate(); err != nil {
			return err
		}
		if byScope[scope] {
			return fmt.Errorf("remote session scope %q is duplicated", scope)
		}
		byScope[scope] = true
		stateScopes = append(stateScopes, meshstate.ProfileScope(scope))
	}
	if len(byScope) == 0 {
		return errors.New("remote session inventory declares no profile scopes")
	}
	type acceptedSession struct {
		summary  sessionview.Summary
		metadata json.RawMessage
	}
	accepted := make([]acceptedSession, 0, len(response.Sessions))
	for _, summary := range response.Sessions {
		if err := summary.Locator.Validate(); err != nil {
			return err
		}
		if !byScope[summary.Locator.ProfileScope] {
			return fmt.Errorf("remote session scope %q is not declared", summary.Locator.ProfileScope)
		}
		clean, err := meshAcceptSummary(summary)
		if err != nil {
			return err
		}
		metadata, err := json.Marshal(clean)
		if err != nil {
			return err
		}
		accepted = append(accepted, acceptedSession{summary: clean, metadata: metadata})
	}
	snapshot, err := store.BeginSessionInventory(peer, stateScopes, response.SourceEpoch, response.SourceRevision)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			snapshot.Abort()
		}
	}()
	for _, item := range accepted {
		locator := meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion, DeviceID: peer,
			ProfileScope: meshstate.ProfileScope(item.summary.Locator.ProfileScope),
			SessionID:    item.summary.Locator.SessionID,
		}
		if err := snapshot.Upsert(locator, item.metadata); err != nil {
			return err
		}
	}
	if err := snapshot.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// RemoteSessionGet retrieves safe detail without accepting a device identity
// from the remote payload; the authenticated peer argument remains authority.
func (d *Daemon) RemoteSessionGet(ctx context.Context, peer string, scope sessionview.ProfileScope, sessionID string) (sessionview.Detail, error) {
	manager, err := d.currentMeshManager()
	if err != nil {
		return sessionview.Detail{}, err
	}
	request := meshSessionGetRequest{ProfileScope: scope, SessionID: sessionID}
	var response meshSessionGetResponse
	if err := manager.Call(ctx, peer, meshMethodSessionsGet, request, &response); err != nil {
		return sessionview.Detail{}, err
	}
	if response.Session.Summary.Locator.ProfileScope != scope || response.Session.Summary.Locator.SessionID != sessionID {
		return sessionview.Detail{}, errors.New("remote session response locator mismatch")
	}
	return meshAcceptDetail(response.Session)
}

func (d *Daemon) RemoteArtifactsList(ctx context.Context, peer string, kind meshstate.ArtifactKind) ([]meshstate.ArtifactEnvelope, error) {
	manager, err := d.currentMeshManager()
	if err != nil {
		return nil, err
	}
	var response meshArtifactsListResponse
	cursor := ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= 4096 {
			return nil, errors.New("remote artifact inventory exceeded page limit")
		}
		var page meshArtifactsListResponse
		// Always retrieve the peer's complete artifact inventory. A filtered
		// snapshot cannot safely assign gone freshness to omitted kinds.
		request := meshArtifactsListRequest{meshPageRequest: meshPageRequest{Cursor: cursor}}
		if err := manager.Call(ctx, peer, meshMethodArtifactsList, request, &page); err != nil {
			return nil, err
		}
		if pageNumber == 0 {
			response = page
			response.Artifacts = nil
		} else if page.SourceEpoch != response.SourceEpoch || page.SourceRevision != response.SourceRevision {
			return nil, errors.New("remote artifact inventory cursor changed between pages")
		}
		if len(page.Artifacts) > meshPageItemMax {
			return nil, errors.New("remote artifact page exceeds item bound")
		}
		response.Artifacts = append(response.Artifacts, page.Artifacts...)
		cursor = page.NextCursor
		if cursor != "" {
			if len(page.Artifacts) == 0 {
				return nil, errors.New("remote artifact inventory returned an empty continuation page")
			}
			revision, offset, err := parseMeshCursor("a", cursor)
			if err != nil || revision != response.SourceRevision || offset != len(response.Artifacts) {
				return nil, errors.New("remote artifact inventory returned a non-contiguous cursor")
			}
		}
		if cursor == "" {
			response.NextCursor = ""
			break
		}
	}
	store, err := d.meshStateStore()
	if err != nil {
		return nil, err
	}
	snapshot, err := store.BeginArtifactInventory(peer, response.SourceEpoch, response.SourceRevision)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			snapshot.Abort()
		}
	}()
	for i := range response.Artifacts {
		artifact, err := meshArtifactEnvelopeFromReference(peer, response.Artifacts[i])
		if err != nil {
			return nil, err
		}
		if err := snapshot.Upsert(artifact); err != nil {
			return nil, err
		}
	}
	if err := snapshot.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return store.ListArtifactsForOrigin(peer, kind)
}

func (d *Daemon) RemoteArtifactGet(ctx context.Context, peer string, kind meshstate.ArtifactKind, id string) (meshstate.ArtifactEnvelope, error) {
	manager, err := d.currentMeshManager()
	if err != nil {
		return meshstate.ArtifactEnvelope{}, err
	}
	var response meshArtifactGetResponse
	if err := manager.Call(ctx, peer, meshMethodArtifactsGet, meshArtifactGetRequest{Kind: kind, ID: id}, &response); err != nil {
		return meshstate.ArtifactEnvelope{}, err
	}
	if response.Artifact.Kind != kind || response.Artifact.ID != id {
		return meshstate.ArtifactEnvelope{}, errors.New("remote artifact response locator mismatch")
	}
	artifact, err := meshArtifactEnvelopeFromReference(peer, response.Artifact)
	if err != nil {
		return meshstate.ArtifactEnvelope{}, err
	}
	if response.SourceEpoch == "" || response.SourceRevision == 0 {
		return meshstate.ArtifactEnvelope{}, errors.New("remote artifact detail has no source cursor")
	}
	now := time.Now().UTC()
	artifact.OriginDeviceID = peer
	artifact.SourceID = artifact.ID
	artifact.SourceEpoch = response.SourceEpoch
	artifact.SourceRevision = response.SourceRevision
	artifact.ReceivedAt = now
	artifact.FreshnessChangedAt = now
	artifact.Freshness = meshstate.FreshnessFresh
	if err := artifact.Validate(); err != nil {
		return meshstate.ArtifactEnvelope{}, err
	}
	return artifact, nil
}

func meshArtifactReferenceFromEnvelope(artifact meshstate.ArtifactEnvelope) (meshArtifactReference, error) {
	if err := artifact.Validate(); err != nil {
		return meshArtifactReference{}, err
	}
	reference := meshArtifactReference{ID: artifact.ID, Kind: artifact.Kind}
	if artifact.Repo != nil {
		reference.Repo = &meshArtifactRepoRef{Repo: artifact.Repo.Repo, Commit: artifact.Repo.Commit}
	}
	if artifact.Session != nil {
		scope := sessionview.ProfileScope(artifact.Session.ProfileScope)
		locator, err := sessionview.NewLocator(scope, artifact.Session.SessionID)
		if err != nil {
			return meshArtifactReference{}, err
		}
		reference.Session = &meshArtifactSessionRef{ProfileScope: locator.ProfileScope, SessionID: locator.SessionID}
	}
	return reference, nil
}

func meshArtifactEnvelopeFromReference(peer string, reference meshArtifactReference) (meshstate.ArtifactEnvelope, error) {
	now := time.Now().UTC()
	artifact := meshstate.ArtifactEnvelope{
		SchemaVersion:      meshstate.SchemaVersion,
		ID:                 reference.ID,
		Kind:               reference.Kind,
		Provenance:         "mesh.reference.v1",
		ReceivedAt:         now,
		FreshnessChangedAt: now,
		Freshness:          meshstate.FreshnessFresh,
	}
	if reference.Repo != nil {
		artifact.Repo = &meshstate.RepoRef{Repo: reference.Repo.Repo, Commit: reference.Repo.Commit}
	}
	if reference.Session != nil {
		locator, err := sessionview.NewLocator(reference.Session.ProfileScope, reference.Session.SessionID)
		if err != nil {
			return meshstate.ArtifactEnvelope{}, err
		}
		artifact.Session = &meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion,
			DeviceID:      peer,
			ProfileScope:  meshstate.ProfileScope(locator.ProfileScope),
			SessionID:     locator.SessionID,
		}
	}
	if err := artifact.Validate(); err != nil {
		return meshstate.ArtifactEnvelope{}, fmt.Errorf("unsafe remote artifact reference: %w", err)
	}
	return artifact, nil
}

func (d *Daemon) SubscribeMeshPeer(ctx context.Context, peer string, topics []string) ([]string, error) {
	topics, err := validateMeshTopics(topics)
	if err != nil {
		return nil, err
	}
	// Record operator intent before transport work. A transient disconnect or
	// failed subscribe must not erase the desired topics; the periodic connected
	// peer pass will retry them. connectedAt is activation state, not intent.
	if err := d.setDesiredMeshTopics(peer, topics, time.Time{}); err != nil {
		return nil, err
	}
	manager, err := d.currentMeshManager()
	if err != nil {
		return nil, err
	}
	var response meshSubscriptionResponse
	if err := manager.Call(ctx, peer, meshMethodEventsSubscribe, meshSubscriptionRequest{Topics: topics}, &response); err != nil {
		return nil, err
	}
	accepted, err := validateMeshTopics(response.Topics)
	if err != nil {
		return nil, err
	}
	if !sameStrings(topics, accepted) {
		return nil, errors.New("remote subscription response changed requested topics")
	}
	connectedAt, ok := meshConnectedAt(manager, peer)
	if !ok {
		return nil, arcmuxmesh.ErrPeerDisconnected
	}
	if err := d.setDesiredMeshTopics(peer, accepted, connectedAt); err != nil {
		return nil, err
	}
	return accepted, nil
}

func (d *Daemon) setDesiredMeshTopics(peer string, topics []string, connectedAt time.Time) error {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return errors.New("mesh state is unavailable")
	}
	if err := app.store.SetDesiredTopics(peer, topics); err != nil {
		return err
	}
	set := make(map[string]bool, len(topics))
	for _, topic := range topics {
		set[topic] = true
	}
	app.subsMu.Lock()
	if len(set) == 0 {
		delete(app.outbound, peer)
	} else {
		app.outbound[peer] = meshPeerSubscription{connectedAt: connectedAt, topics: set}
	}
	app.subsMu.Unlock()
	return nil
}

func (d *Daemon) restoreDesiredMeshTopics() error {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || app.store == nil {
		return errors.New("mesh state is unavailable")
	}
	peers, err := app.store.ListPeers()
	if err != nil {
		return err
	}
	restored := make(map[string]meshPeerSubscription)
	for _, peer := range peers {
		topics, err := validateMeshTopics(peer.DesiredTopics)
		if err != nil {
			return err
		}
		if len(topics) == 0 {
			continue
		}
		set := make(map[string]bool, len(topics))
		for _, topic := range topics {
			set[topic] = true
		}
		restored[peer.DeviceID] = meshPeerSubscription{topics: set}
	}
	app.subsMu.Lock()
	app.outbound = restored
	app.subsMu.Unlock()
	return nil
}

func (d *Daemon) restoreDesiredMeshPeer(peer string) error {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil || app.store == nil {
		return errors.New("mesh state is unavailable")
	}
	topics, err := app.store.DesiredTopics(peer)
	if errors.Is(err, meshstate.ErrNotFound) {
		topics = nil
	} else if err != nil {
		return err
	}
	topics, err = validateMeshTopics(topics)
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(topics))
	for _, topic := range topics {
		set[topic] = true
	}
	app.subsMu.Lock()
	if len(set) == 0 {
		delete(app.outbound, peer)
	} else {
		connectedAt := time.Time{}
		if active, ok := app.outbound[peer]; ok && sameTopicSet(active.topics, set) {
			connectedAt = active.connectedAt
		}
		app.outbound[peer] = meshPeerSubscription{connectedAt: connectedAt, topics: set}
	}
	app.subsMu.Unlock()
	return nil
}

func sameTopicSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for topic := range left {
		if !right[topic] {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (d *Daemon) UnsubscribeMeshPeer(ctx context.Context, peer string) error {
	// Clearing local intent is authoritative even if the best-effort remote RPC
	// fails. A replaced connection starts default-off, and no periodic worker
	// may resurrect an operator-cancelled subscription.
	if err := d.setDesiredMeshTopics(peer, nil, time.Time{}); err != nil {
		return err
	}
	manager, err := d.currentMeshManager()
	if err != nil {
		return err
	}
	var response meshSubscriptionResponse
	if err := manager.Call(ctx, peer, meshMethodEventsUnsubscribe, meshSubscriptionRequest{}, &response); err != nil {
		return err
	}
	return nil
}

// SyncMeshSessions synchronizes only the session capability. It does not
// require artifacts.read and is the path used by /mesh/sessions/sync.
func (d *Daemon) SyncMeshSessions(ctx context.Context, peer string) ([]meshstate.RemoteSessionProjection, error) {
	return d.syncMeshSessions(ctx, peer, true)
}

// SyncMeshArtifacts synchronizes only artifact metadata.
func (d *Daemon) SyncMeshArtifacts(ctx context.Context, peer string) ([]meshstate.ArtifactEnvelope, error) {
	return d.syncMeshArtifacts(ctx, peer, true)
}

func (d *Daemon) syncMeshSessions(ctx context.Context, peer string, markSyncing bool) ([]meshstate.RemoteSessionProjection, error) {
	if markSyncing {
		store, err := d.meshStateStore()
		if err != nil {
			return nil, err
		}
		if err := store.MarkSessionSyncing(peer, "pending-"+d.meshEpoch()); err != nil {
			return nil, err
		}
	}
	d.beforeMeshTopicFetch(peer)
	return d.RemoteSessionsList(ctx, peer)
}

func (d *Daemon) syncMeshArtifacts(ctx context.Context, peer string, markSyncing bool) ([]meshstate.ArtifactEnvelope, error) {
	if markSyncing {
		store, err := d.meshStateStore()
		if err != nil {
			return nil, err
		}
		if err := store.MarkArtifactSyncing(peer, "pending-"+d.meshEpoch()); err != nil {
			return nil, err
		}
	}
	d.beforeMeshTopicFetch(peer)
	return d.RemoteArtifactsList(ctx, peer, "")
}

func (d *Daemon) beforeMeshTopicFetch(peer string) {
	d.meshMu.RLock()
	app := d.meshApp
	d.meshMu.RUnlock()
	if app == nil {
		return
	}
	app.runtimeMu.Lock()
	beforeSync := app.beforeSync
	app.runtimeMu.Unlock()
	if beforeSync != nil {
		beforeSync(peer)
	}
}

func (d *Daemon) syncMeshPeerTopics(ctx context.Context, peer string, topics []string) ([]meshstate.RemoteSessionProjection, error) {
	topics, err := validateMeshTopics(topics)
	if err != nil {
		return nil, err
	}
	markEach := true
	if len(topics) > 1 {
		store, err := d.meshStateStore()
		if err != nil {
			return nil, err
		}
		// Both inventories become syncing as one visibility transition before
		// either commit. Otherwise the first completed topic could transiently
		// make the aggregate peer look fresh while the second is still old.
		if err := store.MarkPeerSyncing(peer, "pending-"+d.meshEpoch()); err != nil {
			return nil, err
		}
		markEach = false
	}
	var projections []meshstate.RemoteSessionProjection
	for _, topic := range topics {
		switch topic {
		case meshTopicSessions:
			projections, err = d.syncMeshSessions(ctx, peer, markEach)
			if err != nil {
				return nil, err
			}
		case meshTopicArtifacts:
			if _, err := d.syncMeshArtifacts(ctx, peer, markEach); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported sync topic %q", topic)
		}
	}
	return projections, nil
}

// SyncMeshPeer performs an explicit full synchronization across both read
// capabilities. Topic-specific callers should use the split methods above.
func (d *Daemon) SyncMeshPeer(ctx context.Context, peer string) ([]meshstate.RemoteSessionProjection, error) {
	return d.syncMeshPeerTopics(ctx, peer, []string{meshTopicSessions, meshTopicArtifacts})
}
