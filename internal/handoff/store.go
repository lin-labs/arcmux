package handoff

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

var (
	ErrNotFound          = errors.New("handoff record not found")
	ErrManifestConflict  = errors.New("handoff manifest conflict")
	ErrCASConflict       = errors.New("handoff record revision conflict")
	ErrIllegalTransition = errors.New("illegal handoff state transition")
)

// Store persists source and target handoff records beneath a caller-owned
// state root. Public operations are safe for concurrent use.
type Store struct {
	root string
	mu   sync.RWMutex
	now  func() time.Time
}

func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("handoff state root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve handoff state root: %w", err)
	}
	store := &Store{root: filepath.Clean(abs), now: time.Now}
	if err := ensurePrivateRoot(store.root); err != nil {
		return nil, err
	}
	for _, side := range []string{"source", "target"} {
		dir, err := store.path("handoffs", side)
		if err != nil {
			return nil, err
		}
		if err := ensurePrivateDir(store.root, dir); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Root() string { return s.root }

// QueueSource creates a source record. replay is true only when an existing
// record has the same immutable manifest digest.
func (s *Store) QueueSource(manifest Manifest) (record SourceRecord, replay bool, err error) {
	digest, err := manifest.Digest()
	if err != nil {
		return SourceRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.recordPath("source", manifest.HandoffID)
	if err != nil {
		return SourceRecord{}, false, err
	}
	var existing SourceRecord
	if err := readJSONFile(file, &existing); err == nil {
		if err := existing.validate(); err != nil {
			return SourceRecord{}, false, err
		}
		if existing.Manifest.HandoffID != manifest.HandoffID || existing.Digest != digest {
			return SourceRecord{}, false, ErrManifestConflict
		}
		return existing, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return SourceRecord{}, false, err
	}
	now := s.now().UTC()
	record = SourceRecord{
		Version: RecordVersion, Manifest: cloneManifest(manifest), Digest: digest,
		State: SourceQueued, Revision: 1, Updated: now,
	}
	if err := record.validate(); err != nil {
		return SourceRecord{}, false, err
	}
	if err := writeJSONFile(s.root, file, record); err != nil {
		return SourceRecord{}, false, err
	}
	return record, false, nil
}

// ReceiveTarget creates the target-side record for an authenticated service
// layer to process. This method does not trust or compare Source.DeviceID.
func (s *Store) ReceiveTarget(manifest Manifest) (record TargetRecord, replay bool, err error) {
	digest, err := manifest.Digest()
	if err != nil {
		return TargetRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.recordPath("target", manifest.HandoffID)
	if err != nil {
		return TargetRecord{}, false, err
	}
	var existing TargetRecord
	if err := readJSONFile(file, &existing); err == nil {
		if err := existing.validate(); err != nil {
			return TargetRecord{}, false, err
		}
		if existing.Manifest.HandoffID != manifest.HandoffID || existing.Digest != digest {
			return TargetRecord{}, false, ErrManifestConflict
		}
		return existing, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return TargetRecord{}, false, err
	}
	now := s.now().UTC()
	record = TargetRecord{
		Version: RecordVersion, Manifest: cloneManifest(manifest), Digest: digest,
		State: TargetReceived, Revision: 1, Updated: now,
	}
	if err := record.validate(); err != nil {
		return TargetRecord{}, false, err
	}
	if err := writeJSONFile(s.root, file, record); err != nil {
		return TargetRecord{}, false, err
	}
	return record, false, nil
}

func (s *Store) GetSource(id string) (SourceRecord, error) {
	if err := validateID("handoff id", id); err != nil {
		return SourceRecord{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getSourceLocked(id)
}

func (s *Store) GetTarget(id string) (TargetRecord, error) {
	if err := validateID("handoff id", id); err != nil {
		return TargetRecord{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getTargetLocked(id)
}

func (s *Store) ListSource() ([]SourceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files, err := s.recordFiles("source")
	if err != nil {
		return nil, err
	}
	out := make([]SourceRecord, 0, len(files))
	for _, file := range files {
		var record SourceRecord
		if err := readJSONFile(file, &record); err != nil {
			return nil, err
		}
		if err := record.validatePath(file, s, "source"); err != nil {
			return nil, err
		}
		out = append(out, cloneSourceRecord(record))
	}
	return out, nil
}

func (s *Store) ListTarget() ([]TargetRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	files, err := s.recordFiles("target")
	if err != nil {
		return nil, err
	}
	out := make([]TargetRecord, 0, len(files))
	for _, file := range files {
		var record TargetRecord
		if err := readJSONFile(file, &record); err != nil {
			return nil, err
		}
		if err := record.validatePath(file, s, "target"); err != nil {
			return nil, err
		}
		out = append(out, cloneTargetRecord(record))
	}
	return out, nil
}

func (s *Store) DueSource(at time.Time) ([]SourceRecord, error) {
	if at.IsZero() {
		return nil, errors.New("due time is required")
	}
	records, err := s.ListSource()
	if err != nil {
		return nil, err
	}
	out := make([]SourceRecord, 0)
	for _, record := range records {
		if record.State == SourceRetryWait && record.NextRetry != nil && !record.NextRetry.After(at) {
			out = append(out, record)
		}
	}
	return out, nil
}

func (s *Store) DueTarget(at time.Time) ([]TargetRecord, error) {
	if at.IsZero() {
		return nil, errors.New("due time is required")
	}
	records, err := s.ListTarget()
	if err != nil {
		return nil, err
	}
	out := make([]TargetRecord, 0)
	for _, record := range records {
		if record.State == TargetWaitingAssets && record.NextRetry != nil && !record.NextRetry.After(at) {
			out = append(out, record)
		}
	}
	return out, nil
}

func (s *Store) TransitionSource(id string, expectedRevision uint64, next SourceState, transition Transition) (SourceRecord, error) {
	if !validSourceState(next) {
		return SourceRecord{}, fmt.Errorf("invalid source state %q", next)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.getSourceLocked(id)
	if err != nil {
		return SourceRecord{}, err
	}
	if record.Revision != expectedRevision {
		return SourceRecord{}, ErrCASConflict
	}
	if !legalSourceTransition(record.State, next) {
		return SourceRecord{}, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, record.State, next)
	}
	at := transition.At
	if at.IsZero() {
		at = s.now().UTC()
	}
	if at.Location() != time.UTC || at.Before(record.Updated) {
		return SourceRecord{}, errors.New("transition time must be UTC and not precede the record")
	}
	record.State = next
	record.Revision++
	record.Updated = at
	applyTransition(&record.NextRetry, &record.Failure, &record.TargetLocator, transition, next == SourceRetryWait, next == SourceFailed, next == SourceAccepted)
	if next == SourcePreparingRemote {
		record.Attempts++
	}
	if err := record.validate(); err != nil {
		return SourceRecord{}, err
	}
	file, _ := s.recordPath("source", id)
	if err := writeJSONFile(s.root, file, record); err != nil {
		return SourceRecord{}, err
	}
	return cloneSourceRecord(record), nil
}

func (s *Store) TransitionTarget(id string, expectedRevision uint64, next TargetState, transition Transition) (TargetRecord, error) {
	if !validTargetState(next) {
		return TargetRecord{}, fmt.Errorf("invalid target state %q", next)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.getTargetLocked(id)
	if err != nil {
		return TargetRecord{}, err
	}
	if record.Revision != expectedRevision {
		return TargetRecord{}, ErrCASConflict
	}
	if !legalTargetTransition(record.State, next) {
		return TargetRecord{}, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, record.State, next)
	}
	at := transition.At
	if at.IsZero() {
		at = s.now().UTC()
	}
	if at.Location() != time.UTC || at.Before(record.Updated) {
		return TargetRecord{}, errors.New("transition time must be UTC and not precede the record")
	}
	record.State = next
	record.Revision++
	record.Updated = at
	applyTransition(&record.NextRetry, &record.Failure, &record.TargetLocator, transition, next == TargetWaitingAssets, next == TargetRejected, next == TargetAccepted)
	if next == TargetValidating {
		record.Attempts++
	}
	if err := record.validate(); err != nil {
		return TargetRecord{}, err
	}
	file, _ := s.recordPath("target", id)
	if err := writeJSONFile(s.root, file, record); err != nil {
		return TargetRecord{}, err
	}
	return cloneTargetRecord(record), nil
}

func applyTransition(nextRetry **time.Time, failure **Failure, locator **TargetLocator, transition Transition, retry, terminalFailure, accepted bool) {
	*nextRetry = nil
	*failure = nil
	if retry {
		*nextRetry = cloneTimePtr(transition.NextRetry)
		*failure = cloneFailure(transition.Failure)
	} else if terminalFailure {
		*failure = cloneFailure(transition.Failure)
	}
	if transition.TargetLocator != nil {
		*locator = cloneLocator(transition.TargetLocator)
	}
	if accepted && transition.TargetLocator == nil {
		// Preserve a locator learned during an earlier prepared/launching state.
		return
	}
}

func (s *Store) getSourceLocked(id string) (SourceRecord, error) {
	file, err := s.recordPath("source", id)
	if err != nil {
		return SourceRecord{}, err
	}
	if err := verifyPrivateDir(s.root, filepath.Dir(file)); err != nil {
		return SourceRecord{}, err
	}
	var record SourceRecord
	if err := readJSONFile(file, &record); err != nil {
		return SourceRecord{}, err
	}
	if err := record.validatePath(file, s, "source"); err != nil {
		return SourceRecord{}, err
	}
	return cloneSourceRecord(record), nil
}

func (s *Store) getTargetLocked(id string) (TargetRecord, error) {
	file, err := s.recordPath("target", id)
	if err != nil {
		return TargetRecord{}, err
	}
	if err := verifyPrivateDir(s.root, filepath.Dir(file)); err != nil {
		return TargetRecord{}, err
	}
	var record TargetRecord
	if err := readJSONFile(file, &record); err != nil {
		return TargetRecord{}, err
	}
	if err := record.validatePath(file, s, "target"); err != nil {
		return TargetRecord{}, err
	}
	return cloneTargetRecord(record), nil
}

func (r SourceRecord) validatePath(file string, store *Store, side string) error {
	if err := r.validate(); err != nil {
		return err
	}
	expected, _ := store.recordPath(side, r.Manifest.HandoffID)
	if filepath.Clean(file) != filepath.Clean(expected) {
		return fmt.Errorf("source record/path mismatch at %s", file)
	}
	return nil
}

func (r TargetRecord) validatePath(file string, store *Store, side string) error {
	if err := r.validate(); err != nil {
		return err
	}
	expected, _ := store.recordPath(side, r.Manifest.HandoffID)
	if filepath.Clean(file) != filepath.Clean(expected) {
		return fmt.Errorf("target record/path mismatch at %s", file)
	}
	return nil
}

func (s *Store) recordPath(side, id string) (string, error) {
	if side != "source" && side != "target" {
		return "", errors.New("invalid handoff side")
	}
	if err := validateID("handoff id", id); err != nil {
		return "", err
	}
	return s.path("handoffs", side, id+".json")
}

func (s *Store) recordFiles(side string) ([]string, error) {
	base, err := s.path("handoffs", side)
	if err != nil {
		return nil, err
	}
	if err := verifyPrivateDir(s.root, base); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&fs.ModeSymlink != 0 || entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, fmt.Errorf("unexpected handoff state entry %s", filepath.Join(base, entry.Name()))
		}
		files = append(files, filepath.Join(base, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func (s *Store) path(segments ...string) (string, error) {
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || filepath.Base(segment) != segment || strings.ContainsAny(segment, "/\\\x00") {
			return "", fmt.Errorf("unsafe handoff state path segment %q", segment)
		}
	}
	joined := filepath.Join(append([]string{s.root}, segments...)...)
	rel, err := filepath.Rel(s.root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("handoff state path escapes root")
	}
	return joined, nil
}

func writeJSONFile(root, file string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(file)
	if err := ensurePrivateDir(root, dir); err != nil {
		return err
	}
	if info, err := os.Lstat(file); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to replace non-regular handoff state file %s", file)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".handoff-*.tmp")
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
	if err := os.Chmod(file, 0o600); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
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
		return fmt.Errorf("handoff state file is not regular: %s", file)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("handoff state file permissions %04o are too open: %s", info.Mode().Perm(), file)
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

func ensurePrivateRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("handoff state root must be a real directory: %s", root)
	}
	return os.Chmod(root, 0o700)
}

func ensurePrivateDir(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("handoff state directory escapes root")
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
			return fmt.Errorf("handoff state directory is not real: %s", current)
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func verifyPrivateDir(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("handoff state directory escapes root")
	}
	current := root
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for _, segment := range append([]string{""}, parts...) {
		if segment != "" {
			current = filepath.Join(current, segment)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("handoff state directory is not real: %s", current)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("handoff state directory permissions %04o are too open: %s", info.Mode().Perm(), current)
		}
	}
	return nil
}

func cloneManifest(manifest Manifest) Manifest {
	clone := manifest
	clone.Artifacts = append([]ArtifactRef(nil), manifest.Artifacts...)
	for i := range clone.Artifacts {
		if manifest.Artifacts[i].Repo != nil {
			repo := *manifest.Artifacts[i].Repo
			clone.Artifacts[i].Repo = &repo
		}
		if manifest.Artifacts[i].Session != nil {
			session := *manifest.Artifacts[i].Session
			clone.Artifacts[i].Session = &session
		}
	}
	if manifest.Repository.Patch != nil {
		patch := *manifest.Repository.Patch
		clone.Repository.Patch = &patch
	}
	if manifest.Validation.CompletedAt != nil {
		completed := *manifest.Validation.CompletedAt
		clone.Validation.CompletedAt = &completed
	}
	return clone
}

func cloneSourceRecord(record SourceRecord) SourceRecord {
	record.Manifest = cloneManifest(record.Manifest)
	record.NextRetry = cloneTimePtr(record.NextRetry)
	record.Failure = cloneFailure(record.Failure)
	record.TargetLocator = cloneLocator(record.TargetLocator)
	return record
}

func cloneTargetRecord(record TargetRecord) TargetRecord {
	record.Manifest = cloneManifest(record.Manifest)
	record.NextRetry = cloneTimePtr(record.NextRetry)
	record.Failure = cloneFailure(record.Failure)
	record.TargetLocator = cloneLocator(record.TargetLocator)
	return record
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneFailure(value *Failure) *Failure {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneLocator(value *TargetLocator) *TargetLocator {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
