package handoff

import (
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

// HistoryError is returned by ResolveHistory. Callers may use its Code to
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
// ResolveHistory.
func HistoryErrorCodeOf(err error) (HistoryErrorCode, bool) {
	var historyErr *HistoryError
	if !errors.As(err, &historyErr) {
		return "", false
	}
	return historyErr.Code, true
}

// ResolveHistory validates ref against a file in the target's configured
// history root and returns only the target-local resolved path. The ref never
// carries a source path or file content. The root itself may be a symlink, but
// the referenced history file must be a regular non-symlink file directly
// beneath the resolved root.
func ResolveHistory(historyRoot string, ref HistoryRef) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", historyError(HistoryErrorInvalid, "%v", err)
	}

	resolvedRoot, err := resolveHistoryRoot(historyRoot)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(resolvedRoot, ref.Basename)
	if !withinResolvedRoot(resolvedRoot, candidate) {
		return "", historyError(HistoryErrorInvalid, "basename resolves outside configured root")
	}

	root, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "open configured root: %v", err)
	}
	defer root.Close()

	before, err := root.Lstat(ref.Basename)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "history file unavailable: %v", err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return "", historyError(HistoryErrorInvalid, "history file must not be a symlink")
	}
	if !before.Mode().IsRegular() {
		return "", historyError(HistoryErrorInvalid, "history file must be regular")
	}
	if before.Size() != ref.SizeBytes {
		return "", historyError(HistoryErrorRetryable, "history size mismatch: got %d, want %d", before.Size(), ref.SizeBytes)
	}

	file, err := root.Open(ref.Basename)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "open history file: %v", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "stat open history file: %v", err)
	}
	if !opened.Mode().IsRegular() {
		return "", historyError(HistoryErrorInvalid, "opened history file must be regular")
	}
	if !os.SameFile(before, opened) || opened.Size() != ref.SizeBytes {
		return "", historyError(HistoryErrorRetryable, "history file changed while opening")
	}

	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return "", historyError(HistoryErrorRetryable, "read history file: %v", err)
	}
	if written != ref.SizeBytes {
		return "", historyError(HistoryErrorRetryable, "history size changed while reading: got %d, want %d", written, ref.SizeBytes)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != ref.SHA256 {
		return "", historyError(HistoryErrorRetryable, "history sha256 mismatch")
	}

	// Re-check the directory entry after reading so a replacement during
	// validation cannot turn the returned path into an unverified file.
	after, err := root.Lstat(ref.Basename)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return "", historyError(HistoryErrorRetryable, "history file changed while validating")
	}
	return candidate, nil
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

func historyError(code HistoryErrorCode, format string, args ...any) error {
	return &HistoryError{Code: code, Err: fmt.Errorf(format, args...)}
}
