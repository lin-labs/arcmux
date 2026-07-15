// Package handoff defines the durable, transport-independent contract for
// handing an arcmux-supervised agent session to another device.
package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const ManifestVersion = 1

const (
	maxArtifacts    = 64
	maxGoalRunes    = 2048
	maxHistoryBytes = 64 << 20
	maxPatchBytes   = 256 << 20
	maxRefRunes     = 256
)

var (
	safeID            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	profileScope      = regexp.MustCompile(`^(?:root|profile:[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?)$`)
	repoSlug          = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	objectID          = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	sha256Hex         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	secretAssignment  = regexp.MustCompile(`(?i)(api[_ -]?key|access[_ -]?token|auth[_ -]?token|authorization|password|secret)\s*[:=]\s*\S+`)
	bearerCredential  = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	privateKey        = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	keyLikeCredential = regexp.MustCompile(`(?i)\b(?:ghp[_-]|github_pat[_-]|xox[baprs]-|xai[_-]|sk[_-])[A-Za-z0-9_-]{8,}`)
)

// Manifest is immutable once queued or received. Other than the explicitly
// bounded operator-authored goal summary, it contains structured identifiers
// and integrity metadata only. Fetch locations, filesystem paths, URLs,
// environment, credentials, and artifact content belong outside this payload.
//
// Source identifies the sender's claim. It is not an authorization decision:
// the service layer must bind it to the authenticated peer before accepting a
// manifest.
type Manifest struct {
	SchemaVersion   int                `json:"schema_version"`
	HandoffID       string             `json:"handoff_id"`
	TraceID         string             `json:"trace_id"`
	ParentHandoffID string             `json:"parent_handoff_id,omitempty"`
	Source          SourceSession      `json:"source"`
	SourceAgent     string             `json:"source_agent"`
	Target          TargetAgent        `json:"target"`
	Goal            GoalSummary        `json:"goal"`
	History         HistoryRef         `json:"history"`
	Repository      RepositorySnapshot `json:"repository"`
	Artifacts       []ArtifactRef      `json:"artifacts"`
	Validation      ValidationEvidence `json:"validation"`
	CreatedAt       time.Time          `json:"created_at"`
}

type SourceSession struct {
	DeviceID     string `json:"device_id"`
	ProfileScope string `json:"profile_scope"`
	SessionID    string `json:"session_id"`
}

// TargetAgent selects a device and its configured agent profile. The target
// session locator is produced only after supervised launch and lives in the
// durable state record, not in the immutable manifest.
type TargetAgent struct {
	DeviceID string `json:"device_id"`
	Profile  string `json:"profile"`
}

type GoalSummary struct {
	Text       string    `json:"text"`
	Provenance string    `json:"provenance"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type HistoryRef struct {
	ArtifactID     string `json:"artifact_id"`
	Basename       string `json:"basename"`
	SHA256         string `json:"sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type TransferMode string

const (
	TransferRemoteBranch TransferMode = "remote_branch"
	TransferStoredPatch  TransferMode = "stored_patch"
)

type RepositoryCleanliness string

const (
	RepositoryClean RepositoryCleanliness = "clean"
	RepositoryDirty RepositoryCleanliness = "dirty"
)

type RepositorySnapshot struct {
	ProjectSlug string                `json:"project_slug"`
	RepoSlug    string                `json:"repo_slug"`
	Branch      string                `json:"branch"`
	SourceHead  string                `json:"source_head"`
	BaseCommit  string                `json:"base_commit"`
	TreeDigest  string                `json:"tree_digest"`
	Cleanliness RepositoryCleanliness `json:"cleanliness"`
	Transfer    TransferMode          `json:"transfer"`
	Patch       *StoredPatchRef       `json:"patch,omitempty"`
}

type StoredPatchRef struct {
	ArtifactID string `json:"artifact_id"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	ResultTree string `json:"result_tree"`
}

// ArtifactKind intentionally matches the phase-2 mesh artifact allowlist.
type ArtifactKind string

const (
	ArtifactGoal           ArtifactKind = "goal"
	ArtifactSessionHistory ArtifactKind = "session_history"
	ArtifactDocument       ArtifactKind = "document"
	ArtifactBranch         ArtifactKind = "branch"
	ArtifactCommit         ArtifactKind = "commit"
	ArtifactPullRequest    ArtifactKind = "pull_request"
)

// ArtifactRef intentionally matches the phase-2 mesh wire projection: stable
// identity plus an optional structured repository or session locator.
type ArtifactRef struct {
	Kind    ArtifactKind        `json:"kind"`
	ID      string              `json:"id"`
	Repo    *ArtifactRepoRef    `json:"repo,omitempty"`
	Session *ArtifactSessionRef `json:"session,omitempty"`
}

type ArtifactRepoRef struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit,omitempty"`
}

type ArtifactSessionRef struct {
	ProfileScope string `json:"profile_scope"`
	SessionID    string `json:"session_id"`
}

type ValidationState string

const (
	ValidationNotRun ValidationState = "not_run"
	ValidationPassed ValidationState = "passed"
	ValidationFailed ValidationState = "failed"
)

type ValidationEvidence struct {
	State              ValidationState `json:"state"`
	RepositoryRevision string          `json:"repository_revision"`
	CompletedAt        *time.Time      `json:"completed_at"`
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestVersion {
		return fmt.Errorf("manifest schema version %d is unsupported", m.SchemaVersion)
	}
	if err := validateID("handoff_id", m.HandoffID); err != nil {
		return err
	}
	if err := validateID("trace_id", m.TraceID); err != nil {
		return err
	}
	if m.ParentHandoffID != "" {
		if err := validateID("parent_handoff_id", m.ParentHandoffID); err != nil {
			return err
		}
		if m.ParentHandoffID == m.HandoffID {
			return errors.New("parent_handoff_id must differ from handoff_id")
		}
	}
	if err := m.Source.validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := validateID("source_agent", m.SourceAgent); err != nil {
		return err
	}
	if err := m.Target.validate(); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if m.Source.DeviceID == m.Target.DeviceID {
		return errors.New("source and target devices must differ")
	}
	if err := m.Goal.validate(); err != nil {
		return fmt.Errorf("goal: %w", err)
	}
	if err := m.History.Validate(); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if err := m.Repository.validate(); err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	if m.Artifacts == nil || len(m.Artifacts) > maxArtifacts {
		return fmt.Errorf("artifacts must be present and contain at most %d entries", maxArtifacts)
	}
	for i, artifact := range m.Artifacts {
		if err := artifact.validate(); err != nil {
			return fmt.Errorf("artifact %d: %w", i, err)
		}
		for j := 0; j < i; j++ {
			if m.Artifacts[j].Kind == artifact.Kind && m.Artifacts[j].ID == artifact.ID {
				return fmt.Errorf("duplicate artifact %s/%s", artifact.Kind, artifact.ID)
			}
		}
	}
	if err := m.Validation.validate(m.Repository); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	if err := validateUTCTime("created_at", m.CreatedAt); err != nil {
		return err
	}
	return nil
}

// Digest returns the lowercase SHA-256 of json.Marshal(m). Manifest contains
// no maps, so struct field order and slice order make this representation
// deterministic without a second canonicalization format.
func (m Manifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (s SourceSession) validate() error {
	if err := validateID("device_id", s.DeviceID); err != nil {
		return err
	}
	if !profileScope.MatchString(s.ProfileScope) {
		return fmt.Errorf("invalid profile_scope %q", s.ProfileScope)
	}
	return validateID("session_id", s.SessionID)
}

func (t TargetAgent) validate() error {
	if err := validateID("device_id", t.DeviceID); err != nil {
		return err
	}
	return validateID("profile", t.Profile)
}

func (g GoalSummary) validate() error {
	if err := validateOperatorText("text", g.Text, maxGoalRunes, true); err != nil {
		return err
	}
	if g.Provenance != "explicit_operator" {
		return errors.New("provenance must be explicit_operator")
	}
	return validateUTCTime("updated_at", g.UpdatedAt)
}

func (h HistoryRef) Validate() error {
	if err := validateID("artifact_id", h.ArtifactID); err != nil {
		return err
	}
	if h.Basename == "" || len([]rune(h.Basename)) > 255 || filepath.Base(h.Basename) != h.Basename ||
		h.Basename == "." || h.Basename == ".." || strings.ContainsAny(h.Basename, "/\\\x00\r\n") {
		return fmt.Errorf("invalid history basename %q", h.Basename)
	}
	if !sha256Hex.MatchString(h.SHA256) {
		return errors.New("history sha256 must be 64 lowercase hexadecimal characters")
	}
	if h.SizeBytes <= 0 || h.SizeBytes > maxHistoryBytes {
		return fmt.Errorf("history size_bytes must be 1..%d", maxHistoryBytes)
	}
	if h.ConversationID != "" {
		if err := validateID("conversation_id", h.ConversationID); err != nil {
			return err
		}
	}
	return nil
}

func (r RepositorySnapshot) validate() error {
	if err := validateID("project_slug", r.ProjectSlug); err != nil {
		return err
	}
	if !repoSlug.MatchString(r.RepoSlug) || strings.Contains(r.RepoSlug, "..") {
		return fmt.Errorf("invalid repo_slug %q", r.RepoSlug)
	}
	if err := validateGitRef(r.Branch); err != nil {
		return err
	}
	for _, item := range []struct {
		name  string
		value string
	}{{"source_head", r.SourceHead}, {"base_commit", r.BaseCommit}, {"tree_digest", r.TreeDigest}} {
		if !objectID.MatchString(item.value) {
			return fmt.Errorf("%s must be a 40..64 character lowercase hexadecimal object id", item.name)
		}
	}
	switch r.Cleanliness {
	case RepositoryClean, RepositoryDirty:
	default:
		return fmt.Errorf("invalid cleanliness %q", r.Cleanliness)
	}
	switch r.Transfer {
	case TransferRemoteBranch:
		if r.Patch != nil {
			return errors.New("remote_branch transfer must not contain a patch")
		}
		if r.Cleanliness != RepositoryClean {
			return errors.New("remote_branch transfer requires a clean repository")
		}
	case TransferStoredPatch:
		if r.Patch == nil {
			return errors.New("stored_patch transfer requires a patch")
		}
		if err := r.Patch.validate(); err != nil {
			return err
		}
		if r.Patch.ResultTree != r.TreeDigest {
			return errors.New("stored patch result_tree must equal repository tree_digest")
		}
	default:
		return fmt.Errorf("invalid transfer %q", r.Transfer)
	}
	return nil
}

func (p StoredPatchRef) validate() error {
	if err := validateID("patch artifact_id", p.ArtifactID); err != nil {
		return err
	}
	if !sha256Hex.MatchString(p.SHA256) {
		return errors.New("patch sha256 must be 64 lowercase hexadecimal characters")
	}
	if p.SizeBytes <= 0 || p.SizeBytes > maxPatchBytes {
		return fmt.Errorf("patch size_bytes must be 1..%d", maxPatchBytes)
	}
	if !objectID.MatchString(p.ResultTree) {
		return errors.New("patch result_tree must be a lowercase object id")
	}
	return nil
}

func (a ArtifactRef) validate() error {
	switch a.Kind {
	case ArtifactGoal, ArtifactSessionHistory, ArtifactDocument, ArtifactBranch, ArtifactCommit, ArtifactPullRequest:
	default:
		return fmt.Errorf("invalid kind %q", a.Kind)
	}
	if err := validateID("id", a.ID); err != nil {
		return err
	}
	if a.Repo != nil {
		if !repoSlug.MatchString(a.Repo.Repo) || strings.Contains(a.Repo.Repo, "..") {
			return fmt.Errorf("invalid artifact repo %q", a.Repo.Repo)
		}
		if a.Repo.Commit != "" && !objectID.MatchString(a.Repo.Commit) {
			return errors.New("artifact repo commit must be a lowercase object id")
		}
	}
	if a.Session != nil {
		if !profileScope.MatchString(a.Session.ProfileScope) {
			return fmt.Errorf("invalid artifact profile_scope %q", a.Session.ProfileScope)
		}
		if err := validateID("artifact session_id", a.Session.SessionID); err != nil {
			return err
		}
	}
	return nil
}

func (v ValidationEvidence) validate(repository RepositorySnapshot) error {
	switch v.State {
	case ValidationNotRun:
		if v.RepositoryRevision != "" || v.CompletedAt != nil {
			return errors.New("not_run validation must not have repository_revision or completed_at")
		}
	case ValidationPassed, ValidationFailed:
		if err := validateOpaqueRevision(v.RepositoryRevision); err != nil {
			return err
		}
		expected := repository.SourceHead
		if repository.Transfer == TransferStoredPatch {
			if repository.Patch == nil {
				return errors.New("stored_patch validation requires repository patch")
			}
			expected = repository.Patch.ResultTree
			if expected != repository.TreeDigest {
				return errors.New("stored_patch validation revision does not match repository tree_digest")
			}
		}
		if v.RepositoryRevision != expected {
			return errors.New("validation repository_revision does not match repository state")
		}
		if v.CompletedAt == nil {
			return errors.New("completed validation requires completed_at")
		}
		if err := validateUTCTime("completed_at", *v.CompletedAt); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid state %q", v.State)
	}
	return nil
}

func validateID(name, value string) error {
	if !safeID.MatchString(value) {
		return fmt.Errorf("invalid %s %q", name, value)
	}
	return nil
}

func validateOperatorText(name, value string, maxRunes int, required bool) error {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len([]rune(value)) > maxRunes {
		return fmt.Errorf("%s exceeds %d runes", name, maxRunes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	if privateKey.MatchString(value) || secretAssignment.MatchString(value) || bearerCredential.MatchString(value) || keyLikeCredential.MatchString(value) {
		return fmt.Errorf("%s appears to contain credential material", name)
	}
	return nil
}

func validateOpaqueRevision(value string) error {
	if value == "" || len([]rune(value)) > 128 {
		return errors.New("validation revision must be 1..128 runes")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune("/\\:=", r) {
			return errors.New("validation revision contains unsafe characters")
		}
	}
	return nil
}

func validateUTCTime(name string, value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return fmt.Errorf("%s must be a non-zero UTC timestamp", name)
	}
	return nil
}

func validateGitRef(value string) error {
	if value == "" || len([]rune(value)) > maxRefRunes || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") ||
		strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.Contains(value, "\\") ||
		strings.HasSuffix(value, ".lock") {
		return fmt.Errorf("invalid repository branch %q", value)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("invalid repository branch %q", value)
		}
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[", r) {
			return fmt.Errorf("invalid repository branch %q", value)
		}
	}
	return nil
}
