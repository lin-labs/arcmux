package handoff

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HistoryErrorCode separates a deterministic invalid handoff input from a
// retryable target-local availability or synchronization problem.
type HistoryErrorCode string

const (
	HistoryErrorInvalid   HistoryErrorCode = "invalid"
	HistoryErrorRetryable HistoryErrorCode = "retryable"
)

// HistoryError is returned by SnapshotHistory. Callers may use its Code to
// decide whether the handoff is rejected or remains blocked pending sync.
type HistoryError struct {
	Code HistoryErrorCode
	Err  error
}

func (e *HistoryError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("history %s: %v", e.Code, e.Err)
}

func (e *HistoryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// HistoryErrorCodeOf extracts the classification from an error returned by
// SnapshotHistory.
func HistoryErrorCodeOf(err error) (HistoryErrorCode, bool) {
	var historyErr *HistoryError
	if !errors.As(err, &historyErr) {
		return "", false
	}
	return historyErr.Code, true
}

// SnapshotHistory copies a verified synced history into private handoff state
// and returns that immutable target-local snapshot path. The source is read
// exactly once from an already-open descriptor; it is never reopened by path
// after verification. Replays accept only the same size and digest.
func SnapshotHistory(historyRoot, privateRoot, handoffID string, ref HistoryRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", historyError(HistoryErrorInvalid, "%v", err)
	}
	if err := validateID("handoff_id", handoffID); err != nil {
		return "", historyError(HistoryErrorInvalid, "invalid handoff id")
	}

	privatePath, private, err := openPrivateHistoryRoot(privateRoot)
	if err != nil {
		return "", err
	}
	defer private.Close()
	handoffDir := "handoff-" + handoffID
	if err := ensurePrivateDir(private, handoffDir); err != nil {
		return "", err
	}
	if err := syncDirectory(privatePath); err != nil {
		return "", historyError(HistoryErrorRetryable, "sync private history root: %v", err)
	}
	handoffPath := filepath.Join(privatePath, handoffDir)
	if !withinResolvedRoot(privatePath, handoffPath) {
		return "", historyError(HistoryErrorInvalid, "handoff state escapes private root")
	}
	handoff, err := private.OpenRoot(handoffDir)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "open private handoff state: %v", err)
	}
	defer handoff.Close()

	const snapshotName = "history.md"
	snapshotPath := filepath.Join(handoffPath, snapshotName)
	if exists, err := verifyHistorySnapshot(handoff, snapshotName, ref); err != nil {
		return "", err
	} else if exists {
		return snapshotPath, nil
	}

	lock, err := acquireHistorySnapshotLock(handoff)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = lock.Close()
		_ = handoff.Remove(".history.lock")
	}()
	// Another cooperating preparer may have finished immediately before this
	// process acquired the lock.
	if exists, err := verifyHistorySnapshot(handoff, snapshotName, ref); err != nil {
		return "", err
	} else if exists {
		return snapshotPath, nil
	}

	source, err := openHistorySource(historyRoot, ref)
	if err != nil {
		return "", err
	}
	defer source.Close()

	temp, tempName, err := createHistoryTemp(handoff)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = temp.Close()
		_ = handoff.Remove(tempName)
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", historyError(HistoryErrorRetryable, "secure private history snapshot: %v", err)
	}

	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), source.file)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "copy synced history: %v", err)
	}
	if written != ref.SizeBytes {
		return "", historyError(HistoryErrorRetryable, "history size changed while reading: got %d, want %d", written, ref.SizeBytes)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != ref.SHA256 {
		return "", historyError(HistoryErrorRetryable, "history sha256 mismatch")
	}
	if err := source.checkUnchanged(); err != nil {
		return "", err
	}
	if err := temp.Sync(); err != nil {
		return "", historyError(HistoryErrorRetryable, "sync private history snapshot: %v", err)
	}
	if err := temp.Close(); err != nil {
		return "", historyError(HistoryErrorRetryable, "close private history snapshot: %v", err)
	}
	if err := handoff.Rename(tempName, snapshotName); err != nil {
		return "", historyError(HistoryErrorRetryable, "publish private history snapshot: %v", err)
	}
	if err := syncDirectory(handoffPath); err != nil {
		return "", historyError(HistoryErrorRetryable, "sync private handoff state: %v", err)
	}
	if exists, err := verifyHistorySnapshot(handoff, snapshotName, ref); err != nil {
		return "", err
	} else if !exists {
		return "", historyError(HistoryErrorRetryable, "published history snapshot is unavailable")
	}
	return snapshotPath, nil
}

type openedHistorySource struct {
	file   *os.File
	root   *os.Root
	name   string
	before os.FileInfo
}

func openHistorySource(historyRoot string, ref HistoryRef) (*openedHistorySource, error) {
	resolvedRoot, err := resolveHistoryRoot(historyRoot)
	if err != nil {
		return nil, err
	}
	candidate := filepath.Join(resolvedRoot, ref.Basename)
	if !withinResolvedRoot(resolvedRoot, candidate) {
		return nil, historyError(HistoryErrorInvalid, "basename resolves outside configured root")
	}
	root, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return nil, historyError(HistoryErrorRetryable, "open configured root: %v", err)
	}
	before, err := root.Lstat(ref.Basename)
	if err != nil {
		root.Close()
		return nil, historyError(HistoryErrorRetryable, "history file unavailable: %v", err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		root.Close()
		return nil, historyError(HistoryErrorInvalid, "history file must not be a symlink")
	}
	if !before.Mode().IsRegular() {
		root.Close()
		return nil, historyError(HistoryErrorInvalid, "history file must be regular")
	}
	if before.Size() != ref.SizeBytes {
		root.Close()
		return nil, historyError(HistoryErrorRetryable, "history size mismatch: got %d, want %d", before.Size(), ref.SizeBytes)
	}
	file, err := root.Open(ref.Basename)
	if err != nil {
		root.Close()
		return nil, historyError(HistoryErrorRetryable, "open history file: %v", err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != ref.SizeBytes {
		file.Close()
		root.Close()
		return nil, historyError(HistoryErrorRetryable, "history file changed while opening")
	}
	return &openedHistorySource{file: file, root: root, name: ref.Basename, before: opened}, nil
}

func (s *openedHistorySource) Close() {
	_ = s.file.Close()
	_ = s.root.Close()
}

func (s *openedHistorySource) checkUnchanged() error {
	after, err := s.root.Lstat(s.name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(s.before, after) {
		return historyError(HistoryErrorRetryable, "history file changed while validating")
	}
	return nil
}

func openPrivateHistoryRoot(raw string) (string, *os.Root, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsRune(raw, '\x00') || !filepath.IsAbs(raw) {
		return "", nil, historyError(HistoryErrorInvalid, "private history root must be an absolute path")
	}
	info, err := os.Lstat(raw)
	if err != nil {
		return "", nil, historyError(HistoryErrorInvalid, "private history root is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", nil, historyError(HistoryErrorInvalid, "private history root must be a real 0700 directory")
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", nil, historyError(HistoryErrorRetryable, "resolve private history root: %v", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", nil, historyError(HistoryErrorInvalid, "make private history root absolute: %v", err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return "", nil, historyError(HistoryErrorRetryable, "open private history root: %v", err)
	}
	return filepath.Clean(resolved), root, nil
}

func ensurePrivateDir(root *os.Root, name string) error {
	if err := root.Mkdir(name, 0o700); err != nil && !os.IsExist(err) {
		return historyError(HistoryErrorRetryable, "create private handoff state: %v", err)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return historyError(HistoryErrorRetryable, "inspect private handoff state: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return historyError(HistoryErrorInvalid, "private handoff state must be a real 0700 directory")
	}
	return nil
}

func acquireHistorySnapshotLock(root *os.Root) (*os.File, error) {
	if info, err := root.Lstat(".history.lock"); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, historyError(HistoryErrorInvalid, "private history lock path is unsafe")
		}
		return nil, historyError(HistoryErrorRetryable, "private history snapshot is already in progress")
	} else if !os.IsNotExist(err) {
		return nil, historyError(HistoryErrorRetryable, "inspect private history lock failed")
	}
	lock, err := root.OpenFile(".history.lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, historyError(HistoryErrorRetryable, "acquire private history lock failed")
	}
	if err := lock.Chmod(0o600); err != nil {
		lock.Close()
		root.Remove(".history.lock")
		return nil, historyError(HistoryErrorRetryable, "secure private history lock failed")
	}
	return lock, nil
}

func createHistoryTemp(root *os.Root) (*os.File, string, error) {
	for range 16 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", historyError(HistoryErrorRetryable, "generate private history snapshot name failed")
		}
		name := ".history-" + hex.EncodeToString(random[:]) + ".tmp"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", historyError(HistoryErrorRetryable, "create private history snapshot failed")
		}
	}
	return nil, "", historyError(HistoryErrorRetryable, "allocate private history snapshot name failed")
}

func verifyHistorySnapshot(root *os.Root, name string, ref HistoryRef) (bool, error) {
	before, err := root.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, historyError(HistoryErrorRetryable, "inspect private history snapshot failed")
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 {
		return false, historyError(HistoryErrorInvalid, "private history snapshot must be a regular 0600 file")
	}
	if before.Size() != ref.SizeBytes {
		return false, historyError(HistoryErrorInvalid, "existing private history snapshot conflicts with manifest")
	}
	file, err := root.Open(name)
	if err != nil {
		return false, historyError(HistoryErrorRetryable, "open private history snapshot failed")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return false, historyError(HistoryErrorRetryable, "private history snapshot changed while opening")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return false, historyError(HistoryErrorRetryable, "read private history snapshot failed")
	}
	if written != ref.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != ref.SHA256 {
		return false, historyError(HistoryErrorInvalid, "existing private history snapshot conflicts with manifest")
	}
	after, err := root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return false, historyError(HistoryErrorRetryable, "private history snapshot changed while validating")
	}
	return true, nil
}

func resolveHistoryRoot(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return "", historyError(HistoryErrorInvalid, "configured history root is invalid")
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", historyError(HistoryErrorInvalid, "resolve home: %v", err)
		}
		if raw == "~" {
			raw = home
		} else {
			raw = filepath.Join(home, raw[2:])
		}
	} else if strings.HasPrefix(raw, "~") {
		return "", historyError(HistoryErrorInvalid, "unsupported home expansion")
	}
	if !filepath.IsAbs(raw) {
		return "", historyError(HistoryErrorInvalid, "configured history root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "resolve configured history root: %v", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", historyError(HistoryErrorInvalid, "make configured history root absolute: %v", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "stat configured history root: %v", err)
	}
	if !info.IsDir() {
		return "", historyError(HistoryErrorInvalid, "configured history root must be a directory")
	}
	return filepath.Clean(resolved), nil
}

func withinResolvedRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func historyError(code HistoryErrorCode, format string, args ...any) error {
	return &HistoryError{Code: code, Err: fmt.Errorf(format, args...)}
}
