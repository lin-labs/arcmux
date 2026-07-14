package meshstate

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ArtifactInventorySnapshot is a complete metadata-reference inventory from
// one authenticated peer. OriginDeviceID and source cursors are store-owned;
// callers cannot inject them through Upsert.
type ArtifactInventorySnapshot struct {
	store    *Store
	deviceID string
	epoch    string
	revision uint64

	mu      sync.Mutex
	closed  bool
	pending map[string]ArtifactEnvelope
}

func (s *Store) BeginArtifactInventory(deviceID, epoch string, revision uint64) (*ArtifactInventorySnapshot, error) {
	if err := validateID("device_id", deviceID); err != nil {
		return nil, err
	}
	if err := validateID("source_epoch", epoch); err != nil {
		return nil, err
	}
	if revision == 0 {
		return nil, errors.New("source_revision must be positive")
	}
	s.mu.Lock()
	if err := s.validateArtifactInventoryCursorLocked(deviceID, epoch, revision); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	err := s.markArtifactInventorySyncingLocked(deviceID, epoch)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &ArtifactInventorySnapshot{
		store: s, deviceID: deviceID, epoch: epoch, revision: revision,
		pending: make(map[string]ArtifactEnvelope),
	}, nil
}

func (s *Store) validateArtifactInventoryCursorLocked(deviceID, epoch string, revision uint64) error {
	peer, err := s.getPeerLocked(deviceID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if peer.ArtifactInventory != nil && peer.ArtifactInventory.SourceEpoch == epoch && revision <= peer.ArtifactInventory.SourceRevision {
		return fmt.Errorf("%w: %s artifact inventory epoch %s revision %d <= %d", ErrStaleSnapshot, deviceID, epoch, revision, peer.ArtifactInventory.SourceRevision)
	}
	return nil
}

func (x *ArtifactInventorySnapshot) Upsert(artifact ArtifactEnvelope) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return ErrSnapshotClosed
	}
	if artifact.OriginDeviceID != "" {
		return errors.New("artifact origin_device_id is store-owned")
	}
	if artifact.SourceEpoch != "" || artifact.SourceRevision != 0 {
		return errors.New("artifact source cursor is store-owned")
	}
	if artifact.SchemaVersion == 0 {
		artifact.SchemaVersion = SchemaVersion
	}
	sourceID := artifact.SourceID
	if sourceID == "" {
		sourceID = artifact.ID
	}
	if err := validateID("artifact source_id", sourceID); err != nil {
		return err
	}
	if artifact.ID != sourceID {
		return errors.New("incoming artifact source_id must equal id")
	}
	// Validate the allowlisted metadata as a local envelope first. Receipt and
	// source cursor fields are overwritten below and never trusted.
	validationTime := time.Unix(1, 0).UTC()
	artifact.SourceID = artifact.ID
	artifact.ReceivedAt = validationTime
	artifact.FreshnessChangedAt = validationTime
	artifact.Freshness = FreshnessFresh
	if err := artifact.Validate(); err != nil {
		return err
	}
	artifact.OriginDeviceID = x.deviceID
	artifact.SourceID = sourceID
	artifact.SourceEpoch = x.epoch
	artifact.SourceRevision = x.revision
	key := string(artifact.Kind) + "\x00" + sourceID
	x.pending[key] = artifact
	return nil
}

func (x *ArtifactInventorySnapshot) Commit() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return ErrSnapshotClosed
	}
	if err := x.store.commitArtifactInventory(x); err != nil {
		return err
	}
	x.closed = true
	return nil
}

func (x *ArtifactInventorySnapshot) Abort() {
	x.mu.Lock()
	x.closed = true
	x.pending = nil
	x.mu.Unlock()
}

func (s *Store) commitArtifactInventory(x *ArtifactInventorySnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateArtifactInventoryCursorLocked(x.deviceID, x.epoch, x.revision); err != nil {
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
	existing, err := s.listRemoteArtifactsRawLocked(x.deviceID, "")
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
		artifact := x.pending[key]
		artifact.ReceivedAt = now
		artifact.FreshnessChangedAt = now
		artifact.Freshness = FreshnessFresh
		if err := s.writeRemoteArtifactLocked(artifact); err != nil {
			return err
		}
		present[artifactRemoteIdentity(artifact)] = true
		s.publishLocked(ChangeUpsert, EntityArtifact, artifactRemoteIdentity(artifact))
	}
	for _, artifact := range existing {
		if present[artifactRemoteIdentity(artifact)] {
			continue
		}
		artifact.SourceEpoch = x.epoch
		artifact.SourceRevision = x.revision
		artifact.ReceivedAt = now
		artifact.FreshnessChangedAt = now
		artifact.Freshness = FreshnessGone
		if err := s.writeRemoteArtifactLocked(artifact); err != nil {
			return err
		}
		s.publishLocked(ChangeFreshness, EntityArtifact, artifactRemoteIdentity(artifact))
	}
	cursor := InventoryCursor{SourceEpoch: x.epoch, SourceRevision: x.revision, CommittedAt: now}
	peer.SchemaVersion = SchemaVersion
	peer.DeviceID = x.deviceID
	peer.SourceEpoch = x.epoch
	peer.UpdatedAt = now
	peer.ArtifactFreshness = FreshnessFresh
	peer.ArtifactFreshnessChangedAt = now
	recomputePeerFreshness(&peer)
	peer.ArtifactInventory = &cursor
	if err := s.writePeerLocked(peer); err != nil {
		return err
	}
	s.publishLocked(ChangeSnapshot, EntityInventory, x.deviceID+"/artifacts")
	s.publishLocked(ChangeFreshness, EntityPeer, x.deviceID)
	return nil
}

func artifactRemoteIdentity(artifact ArtifactEnvelope) string {
	return artifact.OriginDeviceID + "/" + string(artifact.Kind) + "/" + artifact.SourceID
}

func effectiveArtifact(artifact ArtifactEnvelope, peer *PeerState) ArtifactEnvelope {
	if artifact.OriginDeviceID == "" {
		return artifact
	}
	if peer == nil {
		artifact.Freshness = FreshnessStale
		return artifact
	}
	committed := sameCursor(peer.ArtifactInventory, artifact.SourceEpoch, artifact.SourceRevision)
	inventoryFreshness, changedAt := artifactInventoryFreshness(peer)
	if !committed {
		if inventoryFreshness == FreshnessStale {
			setEffectiveArtifactFreshness(&artifact, FreshnessStale, changedAt)
		} else {
			setEffectiveArtifactFreshness(&artifact, FreshnessSyncing, changedAt)
		}
		return artifact
	}
	if artifact.Freshness == FreshnessGone {
		return artifact
	}
	switch inventoryFreshness {
	case FreshnessStale:
		setEffectiveArtifactFreshness(&artifact, FreshnessStale, changedAt)
	case FreshnessSyncing:
		setEffectiveArtifactFreshness(&artifact, FreshnessSyncing, changedAt)
	}
	return artifact
}

func setEffectiveArtifactFreshness(artifact *ArtifactEnvelope, freshness Freshness, changedAt time.Time) {
	if artifact.Freshness != freshness {
		artifact.Freshness = freshness
		artifact.FreshnessChangedAt = changedAt
	}
}

func (s *Store) writeRemoteArtifactLocked(artifact ArtifactEnvelope) error {
	if artifact.OriginDeviceID == "" {
		return errors.New("remote artifact origin_device_id is required")
	}
	if err := artifact.Validate(); err != nil {
		return err
	}
	file, err := s.remoteArtifactPath(artifact.OriginDeviceID, artifact.Kind, artifact.SourceID)
	if err != nil {
		return err
	}
	return s.writeJSONLocked(file, artifact)
}

func (s *Store) GetRemoteArtifact(originDeviceID string, kind ArtifactKind, sourceID string) (ArtifactEnvelope, error) {
	if err := validateID("origin_device_id", originDeviceID); err != nil {
		return ArtifactEnvelope{}, err
	}
	if err := kind.Validate(); err != nil {
		return ArtifactEnvelope{}, err
	}
	if err := validateID("source_id", sourceID); err != nil {
		return ArtifactEnvelope{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	file, err := s.remoteArtifactPath(originDeviceID, kind, sourceID)
	if err != nil {
		return ArtifactEnvelope{}, err
	}
	artifact, err := readArtifactFile(file)
	if err != nil {
		return ArtifactEnvelope{}, err
	}
	if artifact.OriginDeviceID != originDeviceID || artifact.Kind != kind || artifact.SourceID != sourceID {
		return ArtifactEnvelope{}, errors.New("stored remote artifact does not match path")
	}
	peer, err := s.getPeerLocked(originDeviceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ArtifactEnvelope{}, err
	}
	return effectiveArtifact(artifact, peerPointer(peer)), nil
}

// ListArtifactsForOrigin returns local artifacts when originDeviceID is empty,
// otherwise only artifacts received from that authenticated peer.
func (s *Store) ListArtifactsForOrigin(originDeviceID string, kind ArtifactKind) ([]ArtifactEnvelope, error) {
	if originDeviceID == "" {
		return s.ListArtifacts(kind)
	}
	if err := validateID("origin_device_id", originDeviceID); err != nil {
		return nil, err
	}
	if kind != "" {
		if err := kind.Validate(); err != nil {
			return nil, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listRemoteArtifactsLocked(originDeviceID, kind)
}

// ListAllArtifacts returns local and all received records in deterministic
// origin/kind/source order. ListArtifacts intentionally remains local-only so
// outbound mesh handlers cannot accidentally echo another peer's projection.
func (s *Store) ListAllArtifacts(kind ArtifactKind) ([]ArtifactEnvelope, error) {
	if kind != "" {
		if err := kind.Validate(); err != nil {
			return nil, err
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	local, err := s.listLocalArtifactsLocked(kind)
	if err != nil {
		return nil, err
	}
	remote, err := s.listRemoteArtifactsLocked("", kind)
	if err != nil {
		return nil, err
	}
	out := append(local, remote...)
	sortArtifacts(out)
	return out, nil
}

func (s *Store) listRemoteArtifactsLocked(originDeviceID string, kind ArtifactKind) ([]ArtifactEnvelope, error) {
	out, err := s.listRemoteArtifactsRawLocked(originDeviceID, kind)
	if err != nil {
		return nil, err
	}
	peers := make(map[string]*PeerState)
	for index := range out {
		origin := out[index].OriginDeviceID
		peer, ok := peers[origin]
		if !ok {
			loaded, loadErr := s.getPeerLocked(origin)
			if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
				return nil, loadErr
			}
			if loadErr == nil {
				peer = &loaded
			}
			peers[origin] = peer
		}
		out[index] = effectiveArtifact(out[index], peer)
	}
	return out, nil
}

func (s *Store) listRemoteArtifactsRawLocked(originDeviceID string, kind ArtifactKind) ([]ArtifactEnvelope, error) {
	base, err := s.path("remote-artifacts")
	if err != nil {
		return nil, err
	}
	files, err := collectJSONFiles(base, 3)
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactEnvelope, 0, len(files))
	for _, file := range files {
		artifact, err := readArtifactFile(file)
		if err != nil {
			return nil, err
		}
		expected, err := s.remoteArtifactPath(artifact.OriginDeviceID, artifact.Kind, artifact.SourceID)
		if err != nil || filepath.Clean(expected) != filepath.Clean(file) {
			return nil, fmt.Errorf("remote artifact/path mismatch at %s", file)
		}
		if originDeviceID != "" && artifact.OriginDeviceID != originDeviceID {
			continue
		}
		if kind != "" && artifact.Kind != kind {
			continue
		}
		out = append(out, artifact)
	}
	sortArtifacts(out)
	return out, nil
}

func sortArtifacts(artifacts []ArtifactEnvelope) {
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].OriginDeviceID != artifacts[j].OriginDeviceID {
			return artifacts[i].OriginDeviceID < artifacts[j].OriginDeviceID
		}
		if artifacts[i].Kind != artifacts[j].Kind {
			return artifacts[i].Kind < artifacts[j].Kind
		}
		left, right := artifacts[i].SourceID, artifacts[j].SourceID
		if left == "" {
			left = artifacts[i].ID
		}
		if right == "" {
			right = artifacts[j].ID
		}
		return left < right
	})
}

func peerPointer(peer PeerState) *PeerState {
	if peer.DeviceID == "" {
		return nil
	}
	return &peer
}
