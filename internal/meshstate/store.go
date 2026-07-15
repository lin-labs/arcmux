package meshstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store persists mesh projections as small atomic JSON files. All public
// operations are safe for concurrent use.
type Store struct {
	root string

	mu            sync.RWMutex
	now           func() time.Time
	watchers      map[uint64]chan Change
	nextWatcherID uint64
	changeSeq     uint64
}

func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("mesh state root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve mesh state root: %w", err)
	}
	if err := ensurePrivateRoot(abs); err != nil {
		return nil, err
	}
	return &Store{
		root:     filepath.Clean(abs),
		now:      time.Now,
		watchers: make(map[uint64]chan Change),
	}, nil
}

func (s *Store) Root() string { return s.root }

// Watch subscribes to bounded in-process change events. When a subscriber
// falls behind, queued incrementals are replaced by an explicit ChangeGap;
// the consumer must list durable state again. Cancel is idempotent.
func (s *Store) Watch(buffer int) (<-chan Change, func()) {
	if buffer < 1 {
		buffer = 1
	}
	s.mu.Lock()
	s.nextWatcherID++
	id := s.nextWatcherID
	ch := make(chan Change, buffer)
	s.watchers[id] = ch
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if current, ok := s.watchers[id]; ok {
				delete(s.watchers, id)
				close(current)
			}
			s.mu.Unlock()
		})
	}
	return ch, cancel
}

func (s *Store) publishLocked(kind ChangeType, entity EntityType, key string) {
	s.changeSeq++
	event := Change{Sequence: s.changeSeq, Type: kind, Entity: entity, Key: key, At: s.now().UTC()}
	for _, ch := range s.watchers {
		select {
		case ch <- event:
		default:
			// Throw away the subscriber's stale incrementals and leave one
			// durable gap marker. If the buffer is still full due to a racing
			// receiver, retry the non-blocking send once after draining.
			drainChannel(ch)
			gap := Change{Sequence: event.Sequence, Type: ChangeGap, At: s.now().UTC()}
			select {
			case ch <- gap:
			default:
			}
		}
	}
}

func drainChannel(ch chan Change) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Snapshot accumulates one complete peer/profile inventory in memory. Commit
// writes every present session before marking omitted sessions gone.
type Snapshot struct {
	store    *Store
	deviceID string
	scope    ProfileScope
	epoch    string
	revision uint64

	mu      sync.Mutex
	closed  bool
	pending map[string]pendingProjection
}

type pendingProjection struct {
	locator  RemoteSessionLocator
	metadata json.RawMessage
}

func (s *Store) BeginSnapshot(deviceID string, scope ProfileScope, epoch string, revision uint64) (*Snapshot, error) {
	if err := validateID("device_id", deviceID); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := validateID("source_epoch", epoch); err != nil {
		return nil, err
	}
	if revision == 0 {
		return nil, errors.New("source_revision must be positive")
	}

	s.mu.Lock()
	if err := s.validateSnapshotCursorLocked(deviceID, scope, epoch, revision); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	err := s.markPeerSyncingLocked(deviceID, epoch, &scope)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		store: s, deviceID: deviceID, scope: scope, epoch: epoch, revision: revision,
		pending: make(map[string]pendingProjection),
	}, nil
}

func (s *Store) validateSnapshotCursorLocked(deviceID string, scope ProfileScope, epoch string, revision uint64) error {
	peer, err := s.getPeerLocked(deviceID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cursor, ok := peer.Inventories[string(scope)]; ok && cursor.SourceEpoch == epoch && revision <= cursor.SourceRevision {
		return fmt.Errorf("%w: %s/%s epoch %s revision %d <= %d", ErrStaleSnapshot, deviceID, scope, epoch, revision, cursor.SourceRevision)
	}
	return nil
}

// Upsert adds one safe session projection to the pending inventory.
func (x *Snapshot) Upsert(locator RemoteSessionLocator, metadata json.RawMessage) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return ErrSnapshotClosed
	}
	if err := locator.Validate(); err != nil {
		return err
	}
	if locator.DeviceID != x.deviceID || locator.ProfileScope != x.scope {
		return fmt.Errorf("locator %s is outside snapshot %s/%s", locator.identityKey(), x.deviceID, x.scope)
	}
	if err := validateMetadata(metadata); err != nil {
		return err
	}
	x.pending[locator.SessionID] = pendingProjection{
		locator:  locator,
		metadata: append(json.RawMessage(nil), metadata...),
	}
	return nil
}

func (x *Snapshot) Commit() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return ErrSnapshotClosed
	}
	if err := x.store.commitSnapshot(x); err != nil {
		return err
	}
	x.closed = true
	return nil
}

// Abort closes the snapshot without writing inventory contents. Begin may
// already have marked prior projections syncing, but never gone.
func (x *Snapshot) Abort() {
	x.mu.Lock()
	x.closed = true
	x.pending = nil
	x.mu.Unlock()
}

func (s *Store) commitSnapshot(x *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	peer, err := s.getPeerLocked(x.deviceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		peer = PeerState{SchemaVersion: SchemaVersion, DeviceID: x.deviceID, Inventories: map[string]InventoryCursor{}}
	}
	if peer.Inventories == nil {
		peer.Inventories = map[string]InventoryCursor{}
	}
	if cursor, ok := peer.Inventories[string(x.scope)]; ok && cursor.SourceEpoch == x.epoch && x.revision <= cursor.SourceRevision {
		return fmt.Errorf("%w: %s/%s epoch %s revision %d <= %d", ErrStaleSnapshot, x.deviceID, x.scope, x.epoch, x.revision, cursor.SourceRevision)
	}

	existing, err := s.listRemoteSessionsLocked(x.deviceID, x.scope)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	ids := make([]string, 0, len(x.pending))
	for id := range x.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	present := make(map[string]bool, len(ids))

	// First make every session in the completed inventory durable and fresh.
	for _, id := range ids {
		item := x.pending[id]
		projection := RemoteSessionProjection{
			SchemaVersion:      SchemaVersion,
			Locator:            item.locator,
			Metadata:           append(json.RawMessage(nil), item.metadata...),
			ReceivedAt:         now,
			FreshnessChangedAt: now,
			SourceEpoch:        x.epoch,
			SourceRevision:     x.revision,
			Freshness:          FreshnessFresh,
		}
		if err := s.writeRemoteSessionLocked(projection); err != nil {
			return err
		}
		present[id] = true
		s.publishLocked(ChangeUpsert, EntityRemoteSession, projection.Locator.identityKey())
	}

	// Only after all present records are durable may omission mean gone.
	for _, projection := range existing {
		if present[projection.Locator.SessionID] {
			continue
		}
		projection.Freshness = FreshnessGone
		projection.FreshnessChangedAt = now
		projection.ReceivedAt = now
		projection.SourceEpoch = x.epoch
		projection.SourceRevision = x.revision
		if err := s.writeRemoteSessionLocked(projection); err != nil {
			return err
		}
		s.publishLocked(ChangeFreshness, EntityRemoteSession, projection.Locator.identityKey())
	}

	peer.SchemaVersion = SchemaVersion
	peer.DeviceID = x.deviceID
	peer.SourceEpoch = x.epoch
	peer.UpdatedAt = now
	peer.SessionFreshness = FreshnessFresh
	peer.SessionFreshnessChangedAt = now
	recomputePeerFreshness(&peer)
	peer.Inventories[string(x.scope)] = InventoryCursor{
		SourceEpoch: x.epoch, SourceRevision: x.revision, CommittedAt: now,
	}
	// BeginSnapshot is the compatibility, per-scope transaction. Clear the
	// device-wide visibility marker so reads use this scope's committed cursor.
	peer.SessionInventory = nil
	if err := s.writePeerLocked(peer); err != nil {
		return err
	}
	s.publishLocked(ChangeSnapshot, EntityInventory, x.deviceID+"/"+string(x.scope))
	s.publishLocked(ChangeFreshness, EntityPeer, x.deviceID)
	return nil
}

func (s *Store) MarkPeerSyncing(deviceID, sourceEpoch string) error {
	if err := validateSyncingPeer(deviceID, sourceEpoch); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markPeerSyncingLocked(deviceID, sourceEpoch, nil)
}

// MarkSessionSyncing starts or resumes only the peer's session inventory.
// Cached artifacts retain their own freshness and remain safe for consumers.
func (s *Store) MarkSessionSyncing(deviceID, sourceEpoch string) error {
	if err := validateSyncingPeer(deviceID, sourceEpoch); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markSessionInventorySyncingLocked(deviceID, sourceEpoch)
}

// MarkArtifactSyncing starts or resumes only the peer's artifact inventory.
// Cached sessions and resolved surface bindings retain session freshness.
func (s *Store) MarkArtifactSyncing(deviceID, sourceEpoch string) error {
	if err := validateSyncingPeer(deviceID, sourceEpoch); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markArtifactInventorySyncingLocked(deviceID, sourceEpoch)
}

func validateSyncingPeer(deviceID, sourceEpoch string) error {
	if err := validateID("device_id", deviceID); err != nil {
		return err
	}
	if err := validateID("source_epoch", sourceEpoch); err != nil {
		return err
	}
	return nil
}

func (s *Store) markPeerSyncingLocked(deviceID, sourceEpoch string, onlyScope *ProfileScope) error {
	return s.markPeerInventoriesSyncingLocked(deviceID, sourceEpoch, onlyScope, true, onlyScope == nil)
}

func (s *Store) markSessionInventorySyncingLocked(deviceID, sourceEpoch string) error {
	return s.markPeerInventoriesSyncingLocked(deviceID, sourceEpoch, nil, true, false)
}

func (s *Store) markArtifactInventorySyncingLocked(deviceID, sourceEpoch string) error {
	return s.markPeerInventoriesSyncingLocked(deviceID, sourceEpoch, nil, false, true)
}

func (s *Store) markPeerInventoriesSyncingLocked(deviceID, sourceEpoch string, onlyScope *ProfileScope, includeSessions, includeArtifacts bool) error {
	now := s.now().UTC()
	if includeSessions {
		projections, err := s.listRemoteSessionsLocked(deviceID, "")
		if err != nil {
			return err
		}
		for _, projection := range projections {
			if onlyScope != nil && projection.Locator.ProfileScope != *onlyScope {
				continue
			}
			if projection.Freshness == FreshnessGone || projection.Freshness == FreshnessSyncing {
				continue
			}
			projection.Freshness = FreshnessSyncing
			projection.FreshnessChangedAt = now
			if err := s.writeRemoteSessionLocked(projection); err != nil {
				return err
			}
			s.publishLocked(ChangeFreshness, EntityRemoteSession, projection.Locator.identityKey())
		}
	}
	if includeArtifacts {
		artifacts, err := s.listRemoteArtifactsRawLocked(deviceID, "")
		if err != nil {
			return err
		}
		for _, artifact := range artifacts {
			if artifact.Freshness == FreshnessGone || artifact.Freshness == FreshnessSyncing {
				continue
			}
			artifact.Freshness = FreshnessSyncing
			artifact.FreshnessChangedAt = now
			if err := s.writeRemoteArtifactLocked(artifact); err != nil {
				return err
			}
			s.publishLocked(ChangeFreshness, EntityArtifact, artifactRemoteIdentity(artifact))
		}
	}
	peer, err := s.getPeerLocked(deviceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		peer = PeerState{SchemaVersion: SchemaVersion, DeviceID: deviceID, Inventories: map[string]InventoryCursor{}}
	}
	initializeLegacyTopicFreshness(&peer)
	peer.SourceEpoch = sourceEpoch
	peer.UpdatedAt = now
	if includeSessions {
		peer.SessionFreshness = FreshnessSyncing
		peer.SessionFreshnessChangedAt = now
	}
	if includeArtifacts {
		peer.ArtifactFreshness = FreshnessSyncing
		peer.ArtifactFreshnessChangedAt = now
	}
	recomputePeerFreshness(&peer)
	if peer.Inventories == nil {
		peer.Inventories = map[string]InventoryCursor{}
	}
	if err := s.writePeerLocked(peer); err != nil {
		return err
	}
	s.publishLocked(ChangeFreshness, EntityPeer, deviceID)
	return nil
}

func initializeLegacyTopicFreshness(peer *PeerState) {
	legacy := peer.Freshness
	if legacy == "" || legacy == FreshnessGone {
		legacy = FreshnessStale
	}
	changedAt := peer.UpdatedAt
	if changedAt.IsZero() {
		changedAt = time.Unix(1, 0).UTC()
	}
	if peer.SessionFreshness == "" && (peer.SessionInventory != nil || len(peer.Inventories) > 0) {
		peer.SessionFreshness = legacy
		peer.SessionFreshnessChangedAt = changedAt
	}
	if peer.ArtifactFreshness == "" && peer.ArtifactInventory != nil {
		peer.ArtifactFreshness = legacy
		peer.ArtifactFreshnessChangedAt = changedAt
	}
}

func (s *Store) MarkPeerDisconnected(deviceID string) error {
	if err := validateID("device_id", deviceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	projections, err := s.listRemoteSessionsLocked(deviceID, "")
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, projection := range projections {
		if projection.Freshness == FreshnessGone || projection.Freshness == FreshnessStale {
			continue
		}
		projection.Freshness = FreshnessStale
		projection.FreshnessChangedAt = now
		if err := s.writeRemoteSessionLocked(projection); err != nil {
			return err
		}
		s.publishLocked(ChangeFreshness, EntityRemoteSession, projection.Locator.identityKey())
	}
	artifacts, err := s.listRemoteArtifactsRawLocked(deviceID, "")
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if artifact.Freshness == FreshnessGone || artifact.Freshness == FreshnessStale {
			continue
		}
		artifact.Freshness = FreshnessStale
		artifact.FreshnessChangedAt = now
		if err := s.writeRemoteArtifactLocked(artifact); err != nil {
			return err
		}
		s.publishLocked(ChangeFreshness, EntityArtifact, artifactRemoteIdentity(artifact))
	}
	peer, err := s.getPeerLocked(deviceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		peer = PeerState{SchemaVersion: SchemaVersion, DeviceID: deviceID, Inventories: map[string]InventoryCursor{}}
	}
	peer.UpdatedAt = now
	peer.SessionFreshness = FreshnessStale
	peer.SessionFreshnessChangedAt = now
	peer.ArtifactFreshness = FreshnessStale
	peer.ArtifactFreshnessChangedAt = now
	recomputePeerFreshness(&peer)
	if peer.Inventories == nil {
		peer.Inventories = map[string]InventoryCursor{}
	}
	if err := s.writePeerLocked(peer); err != nil {
		return err
	}
	s.publishLocked(ChangeFreshness, EntityPeer, deviceID)
	return nil
}

func (s *Store) GetRemoteSession(locator RemoteSessionLocator) (RemoteSessionProjection, error) {
	if err := locator.Validate(); err != nil {
		return RemoteSessionProjection{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getRemoteSessionLocked(locator)
}

func (s *Store) getRemoteSessionLocked(locator RemoteSessionLocator) (RemoteSessionProjection, error) {
	file, err := s.remoteSessionPath(locator)
	if err != nil {
		return RemoteSessionProjection{}, err
	}
	var projection RemoteSessionProjection
	if err := readJSONFile(file, &projection); err != nil {
		return RemoteSessionProjection{}, err
	}
	if err := projection.Validate(); err != nil {
		return RemoteSessionProjection{}, err
	}
	if !projection.Locator.EqualIdentity(locator) {
		return RemoteSessionProjection{}, errors.New("stored projection locator does not match path")
	}
	peer, err := s.getPeerLocked(locator.DeviceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return RemoteSessionProjection{}, err
	}
	return effectiveSessionProjection(projection, peerPointer(peer)), nil
}

// ListRemoteSessions returns deterministic locator order. Empty filters mean
// all devices/scopes; non-empty filters are validated before touching disk.
func (s *Store) ListRemoteSessions(deviceID string, scope ProfileScope) ([]RemoteSessionProjection, error) {
	if deviceID != "" {
		if err := validateID("device_id", deviceID); err != nil {
			return nil, err
		}
	}
	if scope != "" {
		if err := scope.Validate(); err != nil {
			return nil, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	projections, err := s.listRemoteSessionsLocked(deviceID, scope)
	if err != nil {
		return nil, err
	}
	peers := make(map[string]*PeerState)
	for index := range projections {
		peerID := projections[index].Locator.DeviceID
		peer, ok := peers[peerID]
		if !ok {
			loaded, loadErr := s.getPeerLocked(peerID)
			if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
				return nil, loadErr
			}
			if loadErr == nil {
				peer = &loaded
			}
			peers[peerID] = peer
		}
		projections[index] = effectiveSessionProjection(projections[index], peer)
	}
	return projections, nil
}

func (s *Store) listRemoteSessionsLocked(deviceID string, scope ProfileScope) ([]RemoteSessionProjection, error) {
	base, err := s.path("remote-sessions")
	if err != nil {
		return nil, err
	}
	files, err := collectJSONFiles(base, 3)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteSessionProjection, 0, len(files))
	for _, file := range files {
		var projection RemoteSessionProjection
		if err := readJSONFile(file, &projection); err != nil {
			return nil, err
		}
		if err := projection.Validate(); err != nil {
			return nil, fmt.Errorf("validate %s: %w", file, err)
		}
		expected, err := s.remoteSessionPath(projection.Locator)
		if err != nil || filepath.Clean(expected) != filepath.Clean(file) {
			return nil, fmt.Errorf("projection locator/path mismatch at %s", file)
		}
		if deviceID != "" && projection.Locator.DeviceID != deviceID {
			continue
		}
		if scope != "" && projection.Locator.ProfileScope != scope {
			continue
		}
		out = append(out, projection)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Locator.identityKey() < out[j].Locator.identityKey()
	})
	return out, nil
}

func (s *Store) writeRemoteSessionLocked(projection RemoteSessionProjection) error {
	if err := projection.Validate(); err != nil {
		return err
	}
	file, err := s.remoteSessionPath(projection.Locator)
	if err != nil {
		return err
	}
	return s.writeJSONLocked(file, projection)
}

func (s *Store) PutSurfaceBinding(binding SurfaceBinding, replace bool) error {
	if binding.SchemaVersion == 0 {
		binding.SchemaVersion = SchemaVersion
	}
	now := s.now().UTC()
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	if err := binding.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putSurfaceBindingLocked(binding, replace)
}

func (s *Store) putSurfaceBindingLocked(binding SurfaceBinding, replace bool) error {
	file, err := s.surfaceBindingPath(binding.SurfaceID)
	if err != nil {
		return err
	}
	var existing SurfaceBinding
	if err := readJSONFile(file, &existing); err == nil {
		if !existing.sameTarget(binding) && !replace {
			return fmt.Errorf("%w: surface %s is already bound to %s", ErrConflict, binding.SurfaceID, existing.Locator.identityKey())
		}
		if existing.sameTarget(binding) {
			binding.CreatedAt = existing.CreatedAt
			// Source is creation provenance, not target identity. Preserve the
			// original writer when another owner path idempotently asserts the
			// same exact binding (for example surface bind followed by mesh bind).
			binding.Source = existing.Source
			// TransportBindingID is mutable attachment metadata, not session
			// identity. A new non-empty observation updates it; an omitted one
			// preserves the last known attachment instead of erasing it.
			if binding.Locator.TransportBindingID == "" {
				binding.Locator.TransportBindingID = existing.Locator.TransportBindingID
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := s.writeJSONLocked(file, binding); err != nil {
		return err
	}
	s.publishLocked(ChangeUpsert, EntitySurfaceBinding, binding.SurfaceID)
	return nil
}

// ValidateAndPutSurfaceBinding atomically validates an exact cached session is
// not gone and writes the surface binding under the same owner-local lock. This
// closes the check/write race that would otherwise allow an authoritative
// completed inventory to mark the target gone between the two operations.
func (s *Store) ValidateAndPutSurfaceBinding(binding SurfaceBinding, replace bool) (ResolvedSurfaceBinding, error) {
	if binding.SchemaVersion == 0 {
		binding.SchemaVersion = SchemaVersion
	}
	now := s.now().UTC()
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	if err := binding.Validate(); err != nil {
		return ResolvedSurfaceBinding{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	projection, err := s.getRemoteSessionLocked(binding.Locator)
	if err != nil {
		return ResolvedSurfaceBinding{}, err
	}
	if !projection.Locator.EqualIdentity(binding.Locator) {
		return ResolvedSurfaceBinding{}, errors.New("cached remote session locator does not match binding target")
	}
	if projection.Freshness == FreshnessGone {
		return ResolvedSurfaceBinding{}, errors.New("cached remote session is gone")
	}
	if err := s.putSurfaceBindingLocked(binding, replace); err != nil {
		return ResolvedSurfaceBinding{}, err
	}
	stored, err := s.getSurfaceBindingLocked(binding.SurfaceID)
	if err != nil {
		return ResolvedSurfaceBinding{}, err
	}
	resolved := ResolvedSurfaceBinding{
		Binding: stored, Projection: &projection,
		PeerFreshness: FreshnessStale, EffectiveFreshness: projection.Freshness,
	}
	if peer, err := s.getPeerLocked(binding.Locator.DeviceID); err == nil {
		resolved.Peer = &peer
		resolved.PeerFreshness = peer.Freshness
	} else if !errors.Is(err, ErrNotFound) {
		return ResolvedSurfaceBinding{}, err
	}
	return resolved, nil
}

func (s *Store) GetSurfaceBinding(surfaceID string) (SurfaceBinding, error) {
	if !uuidRE.MatchString(surfaceID) {
		return SurfaceBinding{}, fmt.Errorf("surface_id %q is not a UUID", surfaceID)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getSurfaceBindingLocked(surfaceID)
}

func (s *Store) getSurfaceBindingLocked(surfaceID string) (SurfaceBinding, error) {
	file, err := s.surfaceBindingPath(surfaceID)
	if err != nil {
		return SurfaceBinding{}, err
	}
	var binding SurfaceBinding
	if err := readJSONFile(file, &binding); err != nil {
		return SurfaceBinding{}, err
	}
	if err := binding.Validate(); err != nil {
		return SurfaceBinding{}, err
	}
	if binding.SurfaceID != surfaceID {
		return SurfaceBinding{}, errors.New("stored surface binding does not match path")
	}
	return binding, nil
}

// ResolveSurfaceBinding reads the durable binding and its exact target under a
// single store lock. It never guesses by title, cwd, session name, or transport
// provenance; a missing projection is represented explicitly with freshness.
func (s *Store) ResolveSurfaceBinding(surfaceID string) (ResolvedSurfaceBinding, error) {
	if !uuidRE.MatchString(surfaceID) {
		return ResolvedSurfaceBinding{}, fmt.Errorf("surface_id %q is not a UUID", surfaceID)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	binding, err := s.getSurfaceBindingLocked(surfaceID)
	if err != nil {
		return ResolvedSurfaceBinding{}, err
	}
	resolved := ResolvedSurfaceBinding{
		Binding:            binding,
		PeerFreshness:      FreshnessStale,
		EffectiveFreshness: FreshnessStale,
	}
	peer, peerErr := s.getPeerLocked(binding.Locator.DeviceID)
	if peerErr != nil && !errors.Is(peerErr, ErrNotFound) {
		return ResolvedSurfaceBinding{}, peerErr
	}
	var peerPtr *PeerState
	if peerErr == nil {
		peerPtr = &peer
		resolved.Peer = peerPtr
		resolved.PeerFreshness = peer.Freshness
		resolved.EffectiveFreshness, _ = sessionInventoryFreshness(peerPtr)
	}
	file, err := s.remoteSessionPath(binding.Locator)
	if err != nil {
		return ResolvedSurfaceBinding{}, err
	}
	var projection RemoteSessionProjection
	if err := readJSONFile(file, &projection); err != nil {
		if errors.Is(err, ErrNotFound) {
			if peerErr == nil && resolved.EffectiveFreshness == FreshnessFresh {
				resolved.EffectiveFreshness = FreshnessGone
			}
			return resolved, nil
		}
		return ResolvedSurfaceBinding{}, err
	}
	if err := projection.Validate(); err != nil {
		return ResolvedSurfaceBinding{}, err
	}
	if !projection.Locator.EqualIdentity(binding.Locator) {
		return ResolvedSurfaceBinding{}, errors.New("stored projection locator does not match binding target")
	}
	projection = effectiveSessionProjection(projection, peerPtr)
	resolved.Projection = &projection
	resolved.EffectiveFreshness = projection.Freshness
	return resolved, nil
}

func (s *Store) ListSurfaceBindings() ([]SurfaceBinding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	base, err := s.path("surface-bindings", "cmux")
	if err != nil {
		return nil, err
	}
	files, err := collectJSONFiles(base, 1)
	if err != nil {
		return nil, err
	}
	out := make([]SurfaceBinding, 0, len(files))
	for _, file := range files {
		var binding SurfaceBinding
		if err := readJSONFile(file, &binding); err != nil {
			return nil, err
		}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		expected, _ := s.surfaceBindingPath(binding.SurfaceID)
		if filepath.Clean(expected) != filepath.Clean(file) {
			return nil, fmt.Errorf("surface binding/path mismatch at %s", file)
		}
		out = append(out, binding)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SurfaceID < out[j].SurfaceID })
	return out, nil
}

func (s *Store) DeleteSurfaceBinding(surfaceID string) error {
	if !uuidRE.MatchString(surfaceID) {
		return fmt.Errorf("surface_id %q is not a UUID", surfaceID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.surfaceBindingPath(surfaceID)
	if err != nil {
		return err
	}
	if err := removeRegularFile(file); err != nil {
		return err
	}
	s.publishLocked(ChangeDelete, EntitySurfaceBinding, surfaceID)
	return nil
}

func (s *Store) PutArtifact(artifact ArtifactEnvelope) error {
	if artifact.OriginDeviceID != "" {
		return errors.New("PutArtifact only accepts locally-authored artifacts")
	}
	if artifact.SourceEpoch != "" || artifact.SourceRevision != 0 {
		return errors.New("local artifact must not carry a remote source cursor")
	}
	if artifact.SchemaVersion == 0 {
		artifact.SchemaVersion = SchemaVersion
	}
	if artifact.SourceID != "" && artifact.SourceID != artifact.ID {
		return errors.New("local artifact source_id must equal id")
	}
	artifact.SourceID = artifact.ID
	now := s.now().UTC()
	artifact.ReceivedAt = now
	artifact.FreshnessChangedAt = now
	artifact.Freshness = FreshnessFresh
	if err := artifact.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.artifactPath(artifact.Kind, artifact.ID)
	if err != nil {
		return err
	}
	if err := s.writeJSONLocked(file, artifact); err != nil {
		return err
	}
	s.publishLocked(ChangeUpsert, EntityArtifact, string(artifact.Kind)+"/"+artifact.ID)
	return nil
}

func (s *Store) GetArtifact(kind ArtifactKind, id string) (ArtifactEnvelope, error) {
	if err := kind.Validate(); err != nil {
		return ArtifactEnvelope{}, err
	}
	if err := validateID("artifact id", id); err != nil {
		return ArtifactEnvelope{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	file, err := s.artifactPath(kind, id)
	if err != nil {
		return ArtifactEnvelope{}, err
	}
	artifact, err := readArtifactFile(file)
	if err != nil {
		return ArtifactEnvelope{}, err
	}
	if artifact.OriginDeviceID != "" || artifact.Kind != kind || artifact.ID != id {
		return ArtifactEnvelope{}, errors.New("stored artifact does not match path")
	}
	return artifact, nil
}

// ListArtifacts returns deterministic kind/id order. Empty kind means all.
func (s *Store) ListArtifacts(kind ArtifactKind) ([]ArtifactEnvelope, error) {
	if kind != "" {
		if err := kind.Validate(); err != nil {
			return nil, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocalArtifactsLocked(kind)
}

func (s *Store) listLocalArtifactsLocked(kind ArtifactKind) ([]ArtifactEnvelope, error) {
	base, err := s.path("artifacts")
	if err != nil {
		return nil, err
	}
	files, err := collectJSONFiles(base, 2)
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactEnvelope, 0, len(files))
	for _, file := range files {
		artifact, err := readArtifactFile(file)
		if err != nil {
			return nil, err
		}
		if artifact.OriginDeviceID != "" {
			return nil, fmt.Errorf("local artifact has remote origin at %s", file)
		}
		expected, _ := s.artifactPath(artifact.Kind, artifact.ID)
		if filepath.Clean(expected) != filepath.Clean(file) {
			return nil, fmt.Errorf("artifact/path mismatch at %s", file)
		}
		if kind == "" || artifact.Kind == kind {
			out = append(out, artifact)
		}
	}
	sortArtifacts(out)
	return out, nil
}

func readArtifactFile(file string) (ArtifactEnvelope, error) {
	var artifact ArtifactEnvelope
	if err := readJSONFile(file, &artifact); err != nil {
		return ArtifactEnvelope{}, err
	}
	// Schema v1 artifacts written before peer inventories did not have explicit
	// source identity or freshness. Normalize them in memory; the next ordinary
	// PutArtifact rewrites the canonical shape without a startup migration.
	if artifact.OriginDeviceID == "" {
		if artifact.SourceID == "" {
			artifact.SourceID = artifact.ID
		}
		if artifact.Freshness == "" {
			artifact.Freshness = FreshnessFresh
		}
		if artifact.FreshnessChangedAt.IsZero() {
			artifact.FreshnessChangedAt = artifact.ReceivedAt
		}
	}
	if err := artifact.Validate(); err != nil {
		return ArtifactEnvelope{}, err
	}
	return artifact, nil
}

func (s *Store) GetPeer(deviceID string) (PeerState, error) {
	if err := validateID("device_id", deviceID); err != nil {
		return PeerState{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getPeerLocked(deviceID)
}

// SetDesiredTopics persists the local subscription intent for one peer. The
// daemon owns the protocol allowlist; the store enforces bounded safe strings
// and canonical set ordering without changing remotely observed freshness.
func (s *Store) SetDesiredTopics(deviceID string, topics []string) error {
	if err := validateID("device_id", deviceID); err != nil {
		return err
	}
	if len(topics) > maxDesiredTopics {
		return fmt.Errorf("desired topics exceeds %d entries", maxDesiredTopics)
	}
	unique := make(map[string]bool, len(topics))
	canonical := make([]string, 0, len(topics))
	for _, topic := range topics {
		if err := validateID("desired topic", topic); err != nil {
			return err
		}
		if !unique[topic] {
			unique[topic] = true
			canonical = append(canonical, topic)
		}
	}
	sort.Strings(canonical)

	s.mu.Lock()
	defer s.mu.Unlock()
	peer, err := s.getPeerLocked(deviceID)
	if errors.Is(err, ErrNotFound) {
		if len(canonical) == 0 {
			return nil
		}
		now := s.now().UTC()
		peer = PeerState{
			SchemaVersion: SchemaVersion,
			DeviceID:      deviceID,
			Freshness:     FreshnessStale,
			UpdatedAt:     now,
			Inventories:   map[string]InventoryCursor{},
		}
	} else if err != nil {
		return err
	}
	if equalStrings(peer.DesiredTopics, canonical) {
		return nil
	}
	peer.DesiredTopics = append([]string(nil), canonical...)
	if err := s.writePeerLocked(peer); err != nil {
		return err
	}
	s.publishLocked(ChangeUpsert, EntityPeer, deviceID)
	return nil
}

func (s *Store) DesiredTopics(deviceID string) ([]string, error) {
	if err := validateID("device_id", deviceID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	peer, err := s.getPeerLocked(deviceID)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), peer.DesiredTopics...), nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Store) ListPeers() ([]PeerState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	base, err := s.path("peers")
	if err != nil {
		return nil, err
	}
	files, err := collectJSONFiles(base, 1)
	if err != nil {
		return nil, err
	}
	out := make([]PeerState, 0, len(files))
	for _, file := range files {
		var peer PeerState
		if err := readJSONFile(file, &peer); err != nil {
			return nil, err
		}
		if err := peer.Validate(); err != nil {
			return nil, err
		}
		expected, _ := s.peerPath(peer.DeviceID)
		if filepath.Clean(expected) != filepath.Clean(file) {
			return nil, fmt.Errorf("peer/path mismatch at %s", file)
		}
		out = append(out, peer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out, nil
}

func (s *Store) getPeerLocked(deviceID string) (PeerState, error) {
	file, err := s.peerPath(deviceID)
	if err != nil {
		return PeerState{}, err
	}
	var peer PeerState
	if err := readJSONFile(file, &peer); err != nil {
		return PeerState{}, err
	}
	if err := peer.Validate(); err != nil {
		return PeerState{}, err
	}
	if peer.DeviceID != deviceID {
		return PeerState{}, errors.New("stored peer does not match path")
	}
	return peer, nil
}

func (s *Store) writePeerLocked(peer PeerState) error {
	if err := peer.Validate(); err != nil {
		return err
	}
	file, err := s.peerPath(peer.DeviceID)
	if err != nil {
		return err
	}
	return s.writeJSONLocked(file, peer)
}

func (s *Store) remoteSessionPath(locator RemoteSessionLocator) (string, error) {
	return s.path("remote-sessions", locator.DeviceID, string(locator.ProfileScope), locator.SessionID+".json")
}

func (s *Store) surfaceBindingPath(surfaceID string) (string, error) {
	return s.path("surface-bindings", "cmux", surfaceID+".json")
}

func (s *Store) artifactPath(kind ArtifactKind, id string) (string, error) {
	return s.path("artifacts", string(kind), id+".json")
}

func (s *Store) remoteArtifactPath(originDeviceID string, kind ArtifactKind, sourceID string) (string, error) {
	return s.path("remote-artifacts", originDeviceID, string(kind), sourceID+".json")
}

func (s *Store) peerPath(deviceID string) (string, error) {
	return s.path("peers", deviceID+".json")
}

func (s *Store) path(segments ...string) (string, error) {
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || filepath.Base(segment) != segment || strings.ContainsAny(segment, "/\\\x00") {
			return "", fmt.Errorf("unsafe mesh state path segment %q", segment)
		}
	}
	joined := filepath.Join(append([]string{s.root}, segments...)...)
	rel, err := filepath.Rel(s.root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("mesh state path escapes root")
	}
	return joined, nil
}

func (s *Store) writeJSONLocked(file string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := ensurePrivateDir(s.root, filepath.Dir(file)); err != nil {
		return err
	}
	if info, err := os.Lstat(file); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to replace non-regular mesh state file %s", file)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(file)
	tmp, err := os.CreateTemp(dir, ".meshstate-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, file); err != nil {
		return err
	}
	_ = os.Chmod(file, 0o600)
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func ensurePrivateRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create mesh state root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("mesh state root must be a real directory: %s", root)
	}
	return os.Chmod(root, 0o700)
}

func ensurePrivateDir(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("mesh state directory escapes root")
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, segment := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, segment)
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("mesh state directory component is not a real directory: %s", current)
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func readJSONFile(file string, value any) error {
	info, err := os.Lstat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("mesh state file is not a regular file: %s", file)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("mesh state file permissions %04o are too open: %s", info.Mode().Perm(), file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", file, err)
	}
	return nil
}

func removeRegularFile(file string) error {
	info, err := os.Lstat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("mesh state file is not a regular file: %s", file)
	}
	return os.Remove(file)
}

func collectJSONFiles(base string, depth int) ([]string, error) {
	info, err := os.Lstat(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("mesh state collection root is not a real directory: %s", base)
	}
	files := make([]string, 0)
	var walk func(string, int) error
	walk = func(dir string, remaining int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("symlink is not allowed in mesh state: %s", filepath.Join(dir, entry.Name()))
			}
			full := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if remaining <= 1 {
					return fmt.Errorf("unexpected directory depth in mesh state: %s", full)
				}
				if err := walk(full, remaining-1); err != nil {
					return err
				}
				continue
			}
			if remaining != 1 || filepath.Ext(entry.Name()) != ".json" {
				return fmt.Errorf("unexpected mesh state file: %s", full)
			}
			files = append(files, full)
		}
		return nil
	}
	if err := walk(base, depth); err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
