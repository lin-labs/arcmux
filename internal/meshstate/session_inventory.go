package meshstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SessionInventorySnapshot accumulates one complete device-wide inventory
// across root and every currently existing named profile. Its final PeerState
// cursor is the atomic visibility marker: projection files written before that
// cursor is durable resolve as syncing, never as a partially fresh inventory.
type SessionInventorySnapshot struct {
	store    *Store
	deviceID string
	epoch    string
	revision uint64
	scopes   map[ProfileScope]bool

	mu      sync.Mutex
	closed  bool
	pending map[string]pendingProjection
}

// BeginSessionInventory starts a complete root+profiles snapshot. scopes must
// enumerate every scope currently exposed by the source, including empty
// profiles. Omitted previously known scopes become gone only after Commit.
func (s *Store) BeginSessionInventory(deviceID string, scopes []ProfileScope, epoch string, revision uint64) (*SessionInventorySnapshot, error) {
	if err := validateID("device_id", deviceID); err != nil {
		return nil, err
	}
	if err := validateID("source_epoch", epoch); err != nil {
		return nil, err
	}
	if revision == 0 {
		return nil, errors.New("source_revision must be positive")
	}
	if len(scopes) == 0 {
		return nil, errors.New("session inventory must declare at least one profile scope")
	}
	scopeSet := make(map[ProfileScope]bool, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return nil, err
		}
		if scopeSet[scope] {
			return nil, fmt.Errorf("profile scope %q is duplicated", scope)
		}
		scopeSet[scope] = true
	}

	s.mu.Lock()
	if err := s.validateSessionInventoryCursorLocked(deviceID, epoch, revision); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	// Persist syncing before any new projection can be written. That peer marker
	// is what makes an interrupted multi-file commit fail closed after restart.
	err := s.markSessionInventorySyncingLocked(deviceID, epoch)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &SessionInventorySnapshot{
		store: s, deviceID: deviceID, epoch: epoch, revision: revision,
		scopes: scopeSet, pending: make(map[string]pendingProjection),
	}, nil
}

func (s *Store) validateSessionInventoryCursorLocked(deviceID, epoch string, revision uint64) error {
	peer, err := s.getPeerLocked(deviceID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if peer.SessionInventory != nil && peer.SessionInventory.SourceEpoch == epoch && revision <= peer.SessionInventory.SourceRevision {
		return fmt.Errorf("%w: %s session inventory epoch %s revision %d <= %d", ErrStaleSnapshot, deviceID, epoch, revision, peer.SessionInventory.SourceRevision)
	}
	if peer.SessionInventory == nil {
		for _, cursor := range peer.Inventories {
			if cursor.SourceEpoch == epoch && revision <= cursor.SourceRevision {
				return fmt.Errorf("%w: %s legacy session inventory epoch %s revision %d <= %d", ErrStaleSnapshot, deviceID, epoch, revision, cursor.SourceRevision)
			}
		}
	}
	return nil
}

// Upsert adds one safe session projection. The authenticated device and a
// declared profile scope must match the transaction exactly.
func (x *SessionInventorySnapshot) Upsert(locator RemoteSessionLocator, metadata json.RawMessage) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return ErrSnapshotClosed
	}
	if err := locator.Validate(); err != nil {
		return err
	}
	if locator.DeviceID != x.deviceID || !x.scopes[locator.ProfileScope] {
		return fmt.Errorf("locator %s is outside device inventory", locator.identityKey())
	}
	if err := validateMetadata(metadata); err != nil {
		return err
	}
	key := string(locator.ProfileScope) + "\x00" + locator.SessionID
	x.pending[key] = pendingProjection{locator: locator, metadata: append(json.RawMessage(nil), metadata...)}
	return nil
}

func (x *SessionInventorySnapshot) Commit() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return ErrSnapshotClosed
	}
	if err := x.store.commitSessionInventory(x); err != nil {
		return err
	}
	x.closed = true
	return nil
}

// Abort leaves the durable peer marker syncing. Existing committed gone
// records remain effectively gone; any records written by an interrupted
// commit resolve syncing because they do not match the final group cursor.
func (x *SessionInventorySnapshot) Abort() {
	x.mu.Lock()
	x.closed = true
	x.pending = nil
	x.mu.Unlock()
}

func (s *Store) commitSessionInventory(x *SessionInventorySnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateSessionInventoryCursorLocked(x.deviceID, x.epoch, x.revision); err != nil {
		return err
	}
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
	existing, err := s.listRemoteSessionsLocked(x.deviceID, "")
	if err != nil {
		return err
	}
	now := s.now().UTC()
	keys := make([]string, 0, len(x.pending))
	for key := range x.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	present := make(map[string]bool, len(keys))
	for _, key := range keys {
		item := x.pending[key]
		projection := RemoteSessionProjection{
			SchemaVersion: SchemaVersion, Locator: item.locator,
			Metadata:   append(json.RawMessage(nil), item.metadata...),
			ReceivedAt: now, FreshnessChangedAt: now,
			SourceEpoch: x.epoch, SourceRevision: x.revision, Freshness: FreshnessFresh,
		}
		if err := s.writeRemoteSessionLocked(projection); err != nil {
			return err
		}
		present[projection.Locator.identityKey()] = true
		s.publishLocked(ChangeUpsert, EntityRemoteSession, projection.Locator.identityKey())
	}
	allScopes := make(map[ProfileScope]bool, len(x.scopes)+len(peer.Inventories))
	for scope := range x.scopes {
		allScopes[scope] = true
	}
	for scope := range peer.Inventories {
		if parsed := ProfileScope(scope); parsed.Validate() == nil {
			allScopes[parsed] = true
		}
	}
	for _, projection := range existing {
		allScopes[projection.Locator.ProfileScope] = true
		if present[projection.Locator.identityKey()] {
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
	cursor := InventoryCursor{SourceEpoch: x.epoch, SourceRevision: x.revision, CommittedAt: now}
	for scope := range allScopes {
		peer.Inventories[string(scope)] = cursor
	}
	peer.SchemaVersion = SchemaVersion
	peer.DeviceID = x.deviceID
	peer.SourceEpoch = x.epoch
	peer.UpdatedAt = now
	peer.SessionFreshness = FreshnessFresh
	peer.SessionFreshnessChangedAt = now
	recomputePeerFreshness(&peer)
	peer.SessionInventory = &cursor
	// This single final write is the visibility commit for all scopes.
	if err := s.writePeerLocked(peer); err != nil {
		return err
	}
	s.publishLocked(ChangeSnapshot, EntityInventory, x.deviceID+"/sessions")
	s.publishLocked(ChangeFreshness, EntityPeer, x.deviceID)
	return nil
}

func sameCursor(cursor *InventoryCursor, epoch string, revision uint64) bool {
	return cursor != nil && cursor.SourceEpoch == epoch && cursor.SourceRevision == revision
}

func effectiveSessionProjection(projection RemoteSessionProjection, peer *PeerState) RemoteSessionProjection {
	if peer == nil {
		projection.Freshness = FreshnessStale
		return projection
	}
	committed := false
	if peer.SessionInventory != nil {
		committed = sameCursor(peer.SessionInventory, projection.SourceEpoch, projection.SourceRevision)
	} else if cursor, ok := peer.Inventories[string(projection.Locator.ProfileScope)]; ok {
		committed = cursor.SourceEpoch == projection.SourceEpoch && cursor.SourceRevision == projection.SourceRevision
	}
	inventoryFreshness, changedAt := sessionInventoryFreshness(peer)
	if !committed {
		if inventoryFreshness == FreshnessStale {
			setEffectiveSessionFreshness(&projection, FreshnessStale, changedAt)
		} else {
			setEffectiveSessionFreshness(&projection, FreshnessSyncing, changedAt)
		}
		return projection
	}
	if projection.Freshness == FreshnessGone {
		return projection
	}
	switch inventoryFreshness {
	case FreshnessStale:
		setEffectiveSessionFreshness(&projection, FreshnessStale, changedAt)
	case FreshnessSyncing:
		setEffectiveSessionFreshness(&projection, FreshnessSyncing, changedAt)
	}
	return projection
}

func sessionInventoryFreshness(peer *PeerState) (Freshness, time.Time) {
	if peer.SessionFreshness != "" {
		return peer.SessionFreshness, peer.SessionFreshnessChangedAt
	}
	if peer.SessionInventory != nil || len(peer.Inventories) > 0 {
		return peer.Freshness, peer.UpdatedAt
	}
	return FreshnessStale, peer.UpdatedAt
}

func artifactInventoryFreshness(peer *PeerState) (Freshness, time.Time) {
	if peer.ArtifactFreshness != "" {
		return peer.ArtifactFreshness, peer.ArtifactFreshnessChangedAt
	}
	if peer.ArtifactInventory != nil {
		return peer.Freshness, peer.UpdatedAt
	}
	return FreshnessStale, peer.UpdatedAt
}

func recomputePeerFreshness(peer *PeerState) {
	states := []Freshness{peer.SessionFreshness, peer.ArtifactFreshness}
	hasFresh := false
	hasStale := false
	for _, state := range states {
		switch state {
		case FreshnessSyncing:
			peer.Freshness = FreshnessSyncing
			return
		case FreshnessStale:
			hasStale = true
		case FreshnessFresh:
			hasFresh = true
		}
	}
	if hasStale {
		peer.Freshness = FreshnessStale
	} else if hasFresh {
		peer.Freshness = FreshnessFresh
	}
}

func setEffectiveSessionFreshness(projection *RemoteSessionProjection, freshness Freshness, changedAt time.Time) {
	if projection.Freshness != freshness {
		projection.Freshness = freshness
		projection.FreshnessChangedAt = changedAt
	}
}
