// Package meshstate owns the durable, local projection of state learned from
// authenticated arcmux mesh peers. It deliberately stores metadata and stable
// locators, not terminal output or credentials.
package meshstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = 1

const (
	maxMetadataBytes = 32 << 10
	maxTitleRunes    = 512
	maxStateRunes    = 128
	maxPathRunes     = 1024
	maxDesiredTopics = 16
)

var (
	ErrNotFound       = errors.New("mesh state not found")
	ErrConflict       = errors.New("mesh state conflict")
	ErrStaleSnapshot  = errors.New("stale mesh inventory snapshot")
	ErrSnapshotClosed = errors.New("mesh inventory snapshot is closed")

	safeID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	profileSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$`)
	uuidRE      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	repoSlugRE  = regexp.MustCompile(`^[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$`)
	commitRE    = regexp.MustCompile(`(?i)^[0-9a-f]{7,64}$`)
)

// ProfileScope identifies the machine root daemon or one named profile
// daemon. It is a string so it remains pleasant in JSON and on disk.
type ProfileScope string

const RootProfileScope ProfileScope = "root"

func NamedProfileScope(slug string) ProfileScope { return ProfileScope("profile:" + slug) }

func (s ProfileScope) Validate() error {
	if s == RootProfileScope {
		return nil
	}
	value := string(s)
	if !strings.HasPrefix(value, "profile:") || !profileSlug.MatchString(strings.TrimPrefix(value, "profile:")) {
		return fmt.Errorf("invalid profile scope %q", s)
	}
	return nil
}

// RemoteSessionLocator is the stable identity of one remote session.
// TransportBindingID is an optional observation about how that session is
// surfaced. It is deliberately excluded from identity equality.
type RemoteSessionLocator struct {
	SchemaVersion      int          `json:"schema_version"`
	DeviceID           string       `json:"device_id"`
	ProfileScope       ProfileScope `json:"profile_scope"`
	SessionID          string       `json:"session_id"`
	TransportBindingID string       `json:"transport_binding_id,omitempty"`
}

func (l RemoteSessionLocator) Validate() error {
	if l.SchemaVersion != SchemaVersion {
		return fmt.Errorf("locator schema version %d is unsupported", l.SchemaVersion)
	}
	if err := validateID("device_id", l.DeviceID); err != nil {
		return err
	}
	if err := l.ProfileScope.Validate(); err != nil {
		return err
	}
	if err := validateID("session_id", l.SessionID); err != nil {
		return err
	}
	if l.TransportBindingID != "" {
		if err := validateID("transport_binding_id", l.TransportBindingID); err != nil {
			return err
		}
	}
	return nil
}

// EqualIdentity ignores TransportBindingID by design.
func (l RemoteSessionLocator) EqualIdentity(other RemoteSessionLocator) bool {
	return l.SchemaVersion == other.SchemaVersion &&
		l.DeviceID == other.DeviceID &&
		l.ProfileScope == other.ProfileScope &&
		l.SessionID == other.SessionID
}

func (l RemoteSessionLocator) identityKey() string {
	return l.DeviceID + "/" + string(l.ProfileScope) + "/" + l.SessionID
}

type Freshness string

const (
	FreshnessSyncing Freshness = "syncing"
	FreshnessFresh   Freshness = "fresh"
	FreshnessStale   Freshness = "stale"
	FreshnessGone    Freshness = "gone"
)

func (f Freshness) Validate() error {
	switch f {
	case FreshnessSyncing, FreshnessFresh, FreshnessStale, FreshnessGone:
		return nil
	default:
		return fmt.Errorf("invalid freshness %q", f)
	}
}

// RemoteSessionProjection is locally received state. ReceivedAt and
// FreshnessChangedAt are local timestamps; no freshness decision depends on a
// remote clock or activity timestamp embedded in Metadata.
type RemoteSessionProjection struct {
	SchemaVersion      int                  `json:"schema_version"`
	Locator            RemoteSessionLocator `json:"locator"`
	Metadata           json.RawMessage      `json:"metadata"`
	ReceivedAt         time.Time            `json:"received_at"`
	FreshnessChangedAt time.Time            `json:"freshness_changed_at"`
	SourceEpoch        string               `json:"source_epoch"`
	SourceRevision     uint64               `json:"source_revision"`
	Freshness          Freshness            `json:"freshness"`
}

// ResolvedSurfaceBinding is an exact binding lookup. Projection is nil when
// the target has never been observed locally; EffectiveFreshness still tells a
// consumer whether the peer is syncing, stale, or has authoritatively reported
// the target gone. Resolution never searches by title, cwd, or session name.
type ResolvedSurfaceBinding struct {
	Binding            SurfaceBinding           `json:"binding"`
	Projection         *RemoteSessionProjection `json:"projection,omitempty"`
	Peer               *PeerState               `json:"peer,omitempty"`
	PeerFreshness      Freshness                `json:"peer_freshness"`
	EffectiveFreshness Freshness                `json:"effective_freshness"`
}

func (p RemoteSessionProjection) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("projection schema version %d is unsupported", p.SchemaVersion)
	}
	if err := p.Locator.Validate(); err != nil {
		return fmt.Errorf("projection locator: %w", err)
	}
	if err := validateMetadata(p.Metadata); err != nil {
		return err
	}
	if p.ReceivedAt.IsZero() || p.FreshnessChangedAt.IsZero() {
		return errors.New("projection local timestamps are required")
	}
	if err := validateID("source_epoch", p.SourceEpoch); err != nil {
		return err
	}
	if p.SourceRevision == 0 {
		return errors.New("source_revision must be positive")
	}
	return p.Freshness.Validate()
}

// SurfaceBinding binds one stable cmux surface UUID to exactly one remote
// session locator. The store never derives or retargets this from names.
type SurfaceBinding struct {
	SchemaVersion int                  `json:"schema_version"`
	BindingID     string               `json:"binding_id"`
	LocalDeviceID string               `json:"local_device_id"`
	Mux           string               `json:"mux"`
	SurfaceID     string               `json:"surface_id"`
	WorkspaceID   string               `json:"workspace_id"`
	Locator       RemoteSessionLocator `json:"locator"`
	Source        string               `json:"source"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

func (b SurfaceBinding) Validate() error {
	if b.SchemaVersion != SchemaVersion {
		return fmt.Errorf("binding schema version %d is unsupported", b.SchemaVersion)
	}
	if err := validateID("binding_id", b.BindingID); err != nil {
		return err
	}
	if err := validateID("local_device_id", b.LocalDeviceID); err != nil {
		return err
	}
	if b.Mux != "cmux" {
		return fmt.Errorf("binding mux %q is unsupported", b.Mux)
	}
	if !uuidRE.MatchString(b.SurfaceID) {
		return fmt.Errorf("surface_id %q is not a UUID", b.SurfaceID)
	}
	if !uuidRE.MatchString(b.WorkspaceID) {
		return fmt.Errorf("workspace_id %q is not a UUID", b.WorkspaceID)
	}
	if err := b.Locator.Validate(); err != nil {
		return fmt.Errorf("binding locator: %w", err)
	}
	if err := validateBounded("source", b.Source, maxStateRunes, true); err != nil {
		return err
	}
	if b.CreatedAt.IsZero() || b.UpdatedAt.IsZero() {
		return errors.New("binding timestamps are required")
	}
	return nil
}

func (b SurfaceBinding) sameTarget(other SurfaceBinding) bool {
	return b.BindingID == other.BindingID && b.LocalDeviceID == other.LocalDeviceID &&
		b.Mux == other.Mux && b.SurfaceID == other.SurfaceID &&
		b.WorkspaceID == other.WorkspaceID && b.Locator.EqualIdentity(other.Locator)
}

type ArtifactKind string

const (
	ArtifactGoal           ArtifactKind = "goal"
	ArtifactSessionHistory ArtifactKind = "session_history"
	ArtifactDocument       ArtifactKind = "document"
	ArtifactBranch         ArtifactKind = "branch"
	ArtifactCommit         ArtifactKind = "commit"
	ArtifactPullRequest    ArtifactKind = "pull_request"
)

func (k ArtifactKind) Validate() error {
	switch k {
	case ArtifactGoal, ArtifactSessionHistory, ArtifactDocument, ArtifactBranch, ArtifactCommit, ArtifactPullRequest:
		return nil
	default:
		return fmt.Errorf("invalid artifact kind %q", k)
	}
}

// RepoRef is a metadata-only source-control locator. Repo is a stable slug
// such as "lin-labs/arcmux"; Ref and Commit are optional by artifact kind.
type RepoRef struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
}

func (r RepoRef) Validate() error {
	if !repoSlugRE.MatchString(r.Repo) || strings.Contains(r.Repo, "..") {
		return fmt.Errorf("invalid repo locator %q", r.Repo)
	}
	if err := validateBounded("repo ref", r.Ref, 512, false); err != nil {
		return err
	}
	if strings.ContainsAny(r.Ref, "\x00\r\n") {
		return errors.New("repo ref contains control characters")
	}
	if r.Commit != "" && !commitRE.MatchString(r.Commit) {
		return fmt.Errorf("invalid commit locator %q", r.Commit)
	}
	return nil
}

// ArtifactEnvelope stores references only. Content transfer belongs to a
// separate, explicit protocol.
type ArtifactEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	// OriginDeviceID is empty for a locally-authored artifact. Received
	// artifacts are stored under this authenticated peer identity; callers may
	// not set it through PutArtifact.
	OriginDeviceID string `json:"origin_device_id,omitempty"`
	// SourceID preserves the artifact's identifier on OriginDeviceID. Local
	// artifacts normalize SourceID to ID.
	SourceID           string                `json:"source_id,omitempty"`
	Kind               ArtifactKind          `json:"kind"`
	Title              string                `json:"title,omitempty"`
	State              string                `json:"state,omitempty"`
	URL                string                `json:"url,omitempty"`
	PathHint           string                `json:"path_hint,omitempty"`
	Repo               *RepoRef              `json:"repo,omitempty"`
	Session            *RemoteSessionLocator `json:"session,omitempty"`
	Provenance         string                `json:"provenance"`
	Revision           string                `json:"revision,omitempty"`
	SourceEpoch        string                `json:"source_epoch,omitempty"`
	SourceRevision     uint64                `json:"source_revision,omitempty"`
	RemoteObservedAt   *time.Time            `json:"remote_observed_at,omitempty"`
	ReceivedAt         time.Time             `json:"received_at"`
	FreshnessChangedAt time.Time             `json:"freshness_changed_at"`
	Freshness          Freshness             `json:"freshness"`
}

func (a ArtifactEnvelope) Validate() error {
	if a.SchemaVersion != SchemaVersion {
		return fmt.Errorf("artifact schema version %d is unsupported", a.SchemaVersion)
	}
	if err := validateID("artifact id", a.ID); err != nil {
		return err
	}
	if a.OriginDeviceID == "" {
		if a.SourceID != "" && a.SourceID != a.ID {
			return errors.New("local artifact source_id must equal id")
		}
		if a.SourceEpoch != "" || a.SourceRevision != 0 {
			return errors.New("local artifact must not carry a remote source cursor")
		}
	} else {
		if err := validateID("artifact origin_device_id", a.OriginDeviceID); err != nil {
			return err
		}
		if err := validateID("artifact source_id", a.SourceID); err != nil {
			return err
		}
		if err := validateID("artifact source_epoch", a.SourceEpoch); err != nil {
			return err
		}
		if a.SourceRevision == 0 {
			return errors.New("remote artifact source_revision must be positive")
		}
	}
	if err := a.Kind.Validate(); err != nil {
		return err
	}
	if err := validateBounded("title", a.Title, maxTitleRunes, false); err != nil {
		return err
	}
	if err := validateBounded("state", a.State, maxStateRunes, false); err != nil {
		return err
	}
	if err := validateHTTPSURL(a.URL); err != nil {
		return err
	}
	if err := validatePathHint(a.PathHint); err != nil {
		return err
	}
	if a.Repo != nil {
		if err := a.Repo.Validate(); err != nil {
			return err
		}
	}
	if a.Session != nil {
		if err := a.Session.Validate(); err != nil {
			return fmt.Errorf("artifact session: %w", err)
		}
	}
	if err := validateBounded("provenance", a.Provenance, 512, true); err != nil {
		return err
	}
	if err := validateBounded("revision", a.Revision, 512, false); err != nil {
		return err
	}
	if a.ReceivedAt.IsZero() || a.FreshnessChangedAt.IsZero() {
		return errors.New("artifact local timestamps are required")
	}
	if a.RemoteObservedAt != nil && a.RemoteObservedAt.IsZero() {
		return errors.New("artifact remote_observed_at must be non-zero when present")
	}
	return a.Freshness.Validate()
}

type InventoryCursor struct {
	SourceEpoch    string    `json:"source_epoch"`
	SourceRevision uint64    `json:"source_revision"`
	CommittedAt    time.Time `json:"committed_at"`
}

func (c InventoryCursor) Validate(name string) error {
	if err := validateID(name+" source_epoch", c.SourceEpoch); err != nil {
		return err
	}
	if c.SourceRevision == 0 || c.CommittedAt.IsZero() {
		return fmt.Errorf("%s has invalid cursor", name)
	}
	return nil
}

type PeerState struct {
	SchemaVersion              int                        `json:"schema_version"`
	DeviceID                   string                     `json:"device_id"`
	Freshness                  Freshness                  `json:"freshness"`
	SourceEpoch                string                     `json:"source_epoch,omitempty"`
	UpdatedAt                  time.Time                  `json:"updated_at"`
	SessionFreshness           Freshness                  `json:"session_freshness,omitempty"`
	SessionFreshnessChangedAt  time.Time                  `json:"session_freshness_changed_at,omitempty"`
	ArtifactFreshness          Freshness                  `json:"artifact_freshness,omitempty"`
	ArtifactFreshnessChangedAt time.Time                  `json:"artifact_freshness_changed_at,omitempty"`
	DesiredTopics              []string                   `json:"desired_topics,omitempty"`
	Inventories                map[string]InventoryCursor `json:"inventories,omitempty"`
	// SessionInventory is the atomic visibility marker for a complete
	// device-wide root+profiles snapshot. Inventories remains for compatibility
	// and per-scope diagnostics.
	SessionInventory  *InventoryCursor `json:"session_inventory,omitempty"`
	ArtifactInventory *InventoryCursor `json:"artifact_inventory,omitempty"`
}

func (p PeerState) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("peer schema version %d is unsupported", p.SchemaVersion)
	}
	if err := validateID("device_id", p.DeviceID); err != nil {
		return err
	}
	if err := p.Freshness.Validate(); err != nil {
		return err
	}
	if p.SourceEpoch != "" {
		if err := validateID("source_epoch", p.SourceEpoch); err != nil {
			return err
		}
	}
	if p.UpdatedAt.IsZero() {
		return errors.New("peer updated_at is required")
	}
	if err := validateInventoryFreshness("session", p.SessionFreshness, p.SessionFreshnessChangedAt); err != nil {
		return err
	}
	if err := validateInventoryFreshness("artifact", p.ArtifactFreshness, p.ArtifactFreshnessChangedAt); err != nil {
		return err
	}
	if len(p.DesiredTopics) > maxDesiredTopics {
		return fmt.Errorf("desired topics exceeds %d entries", maxDesiredTopics)
	}
	for index, topic := range p.DesiredTopics {
		if err := validateID("desired topic", topic); err != nil {
			return err
		}
		if index > 0 && p.DesiredTopics[index-1] >= topic {
			return errors.New("desired topics must be sorted and unique")
		}
	}
	for scope, cursor := range p.Inventories {
		if err := ProfileScope(scope).Validate(); err != nil {
			return err
		}
		if err := cursor.Validate("inventory " + scope); err != nil {
			return err
		}
	}
	if p.SessionInventory != nil {
		if err := p.SessionInventory.Validate("session inventory"); err != nil {
			return err
		}
	}
	if p.ArtifactInventory != nil {
		if err := p.ArtifactInventory.Validate("artifact inventory"); err != nil {
			return err
		}
	}
	return nil
}

func validateInventoryFreshness(name string, freshness Freshness, changedAt time.Time) error {
	if freshness == "" {
		if !changedAt.IsZero() {
			return fmt.Errorf("%s freshness timestamp requires freshness", name)
		}
		return nil
	}
	if freshness == FreshnessGone {
		return fmt.Errorf("%s inventory cannot be gone", name)
	}
	if err := freshness.Validate(); err != nil {
		return err
	}
	if changedAt.IsZero() {
		return fmt.Errorf("%s freshness timestamp is required", name)
	}
	return nil
}

type ChangeType string

const (
	ChangeUpsert    ChangeType = "upsert"
	ChangeDelete    ChangeType = "delete"
	ChangeFreshness ChangeType = "freshness"
	ChangeSnapshot  ChangeType = "snapshot"
	ChangeGap       ChangeType = "gap"
)

type EntityType string

const (
	EntityRemoteSession  EntityType = "remote_session"
	EntitySurfaceBinding EntityType = "surface_binding"
	EntityArtifact       EntityType = "artifact"
	EntityPeer           EntityType = "peer"
	EntityInventory      EntityType = "inventory"
)

// Change is an in-process notification. A gap means one or more prior events
// were dropped and the subscriber must list state again before trusting
// incrementals.
type Change struct {
	Sequence uint64     `json:"sequence"`
	Type     ChangeType `json:"type"`
	Entity   EntityType `json:"entity,omitempty"`
	Key      string     `json:"key,omitempty"`
	At       time.Time  `json:"at"`
}

func validateID(field, value string) error {
	if !safeID.MatchString(value) || value == "." || value == ".." {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	return nil
}

func validateNoControls(field, value string) error {
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return fmt.Errorf("%s contains control character U+%04X", field, r)
		}
	}
	return nil
}

func validateBounded(field, value string, max int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len([]rune(value)) > max {
		return fmt.Errorf("%s exceeds %d runes", field, max)
	}
	return validateNoControls(field, value)
}

func validateMetadata(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("projection metadata is required")
	}
	if len(raw) > maxMetadataBytes {
		return fmt.Errorf("projection metadata exceeds %d bytes", maxMetadataBytes)
	}
	if !json.Valid(raw) {
		return errors.New("projection metadata is not one complete JSON value")
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("invalid projection metadata: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("projection metadata must be a JSON object")
	}
	if err := rejectSensitiveMetadata(value); err != nil {
		return err
	}
	return nil
}

func rejectSensitiveMetadata(value any) error {
	forbidden := map[string]bool{
		"authorization": true, "api_key": true, "token": true,
		"secret": true, "password": true, "env": true,
		"environment": true, "screen": true, "screen_output": true,
		"output": true, "tool_output": true, "tmux_target": true, "backend_session_id": true,
		"pid": true,
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbidden[strings.ToLower(key)] {
				return fmt.Errorf("projection metadata field %q is not safe", key)
			}
			if err := rejectSensitiveMetadata(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectSensitiveMetadata(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateHTTPSURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 2048 {
		return errors.New("artifact URL is too long")
	}
	if err := validateNoControls("artifact URL", raw); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("artifact URL must be absolute HTTPS: %q", raw)
	}
	if u.User != nil {
		return errors.New("artifact URL must not contain userinfo")
	}
	for key := range u.Query() {
		lower := strings.ToLower(key)
		for _, secret := range []string{"token", "secret", "password", "signature", "credential", "api_key", "apikey", "auth", "code"} {
			if strings.Contains(lower, secret) {
				return fmt.Errorf("artifact URL contains secret-bearing query key %q", key)
			}
		}
	}
	return nil
}

func validatePathHint(value string) error {
	if value == "" {
		return nil
	}
	if len([]rune(value)) > maxPathRunes {
		return errors.New("artifact path_hint is too long")
	}
	if err := validateNoControls("artifact path_hint", value); err != nil {
		return err
	}
	if !strings.HasPrefix(value, "~/") || strings.Contains(value, "\\") {
		return fmt.Errorf("path_hint must be home-relative (~/...), got %q", value)
	}
	rel := strings.TrimPrefix(value, "~/")
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel {
		return fmt.Errorf("invalid path_hint %q", value)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path_hint %q", value)
		}
	}
	return nil
}
