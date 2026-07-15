package handoff

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	sourceHistoryPrefix     = ".arcmux-handoff-sha256-"
	sourceHistoryTempPrefix = ".arcmux-handoff-tmp-"
	// A non-Markdown suffix keeps Mission Control's recursive history
	// discovery from mistaking this retained transport snapshot for a live
	// canonical conversation log.
	sourceHistorySuffix    = ".snapshot"
	sourceHistoryTempTries = 8
)

type sourceHistoryPublishHooks struct {
	afterSourceOpen func()
	afterCopy       func()
}

// PublishSourceHistory copies one exact, verified session-history version to
// a content-addressed hidden file in the configured synced history root. The
// returned reference names only that immutable publication, so later turns may
// safely keep appending to or rewriting the human-readable session log.
//
// Published files are intentionally retained. They may be shared by multiple
// handoffs or still awaiting sync to a disconnected target. Crash-leftover
// temp files are likewise not swept by an unrelated publication. Cleanup for
// either class requires a future reference-aware, ownership-verifying retention
// policy and must not be inferred here.
func PublishSourceHistory(historyRoot, basename, conversationID string) (HistoryRef, error) {
	return publishSourceHistory(historyRoot, basename, conversationID, randomSourceHistoryNonce, sourceHistoryPublishHooks{})
}

func publishSourceHistory(historyRoot, basename, conversationID string, nonce func() (string, error), hooks sourceHistoryPublishHooks) (HistoryRef, error) {
	if !validSourceHistoryBasename(basename) {
		return HistoryRef{}, historyError(HistoryErrorInvalid, "history basename is invalid; select a file directly under the history root")
	}
	if conversationID != "" {
		if err := validateID("conversation_id", conversationID); err != nil {
			return HistoryRef{}, historyError(HistoryErrorInvalid, "conversation id is invalid")
		}
	}
	resolvedRoot, err := resolveHistoryRoot(historyRoot)
	if err != nil {
		if code, ok := HistoryErrorCodeOf(err); ok && code == HistoryErrorInvalid {
			return HistoryRef{}, historyError(HistoryErrorInvalid, "configured history root is invalid")
		}
		return HistoryRef{}, historyError(HistoryErrorRetryable, "configured history root is unavailable; retry after storage is available")
	}
	rootFD, err := unix.Open(resolvedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "open configured history root failed; retry after storage is available")
	}
	defer unix.Close(rootFD)
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "configured history root changed while opening; retry after storage is stable")
	}

	sourceFD, before, err := openVerifiedSourceHistory(rootFD, basename)
	if err != nil {
		return HistoryRef{}, err
	}
	source := os.NewFile(uintptr(sourceFD), basename)
	if source == nil {
		_ = unix.Close(sourceFD)
		return HistoryRef{}, historyError(HistoryErrorRetryable, "open history file failed; retry after storage is available")
	}
	defer source.Close()
	if hooks.afterSourceOpen != nil {
		hooks.afterSourceOpen()
	}

	temp, tempName, err := createSourceHistoryTemp(rootFD, nonce)
	if err != nil {
		return HistoryRef{}, err
	}
	defer func() {
		_ = temp.Close()
		_ = unix.Unlinkat(rootFD, tempName, 0)
	}()

	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(source, maxHistoryBytes+1))
	if err != nil {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "copy history publication failed; retry after storage is available")
	}
	if hooks.afterCopy != nil {
		hooks.afterCopy()
	}
	if written != before.Size {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "history file changed while reading; wait for sync to settle and retry")
	}
	if err := verifySourceHistoryUnchanged(rootFD, sourceFD, basename, before); err != nil {
		return HistoryRef{}, err
	}
	if err := temp.Sync(); err != nil {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "sync history publication failed; retry after storage is available")
	}
	if err := verifySourceHistoryTemp(rootFD, int(temp.Fd()), tempName, written); err != nil {
		return HistoryRef{}, err
	}

	digestHex := hex.EncodeToString(digest.Sum(nil))
	publishedName := sourceHistoryPublicationName(digestHex)
	ref := HistoryRef{
		ArtifactID:     "history-" + digestHex,
		Basename:       publishedName,
		SHA256:         digestHex,
		SizeBytes:      written,
		ConversationID: conversationID,
	}
	if err := ref.Validate(); err != nil {
		return HistoryRef{}, historyError(HistoryErrorInvalid, "history publication cannot form a safe handoff reference")
	}

	err = renameSourceHistoryNoReplace(rootFD, tempName, publishedName)
	switch {
	case err == nil:
		if err := unix.Fsync(rootFD); err != nil {
			return HistoryRef{}, historyError(HistoryErrorRetryable, "sync published history directory failed; retry after storage is available")
		}
	case errors.Is(err, unix.EEXIST):
		// Another publisher or a replay already owns the content address. The
		// existing file is accepted only after a complete stable verification.
	default:
		return HistoryRef{}, historyError(HistoryErrorRetryable, "atomically publish history failed; retry after storage is available")
	}
	if err := verifyPublishedSourceHistory(rootFD, ref); err != nil {
		return HistoryRef{}, err
	}
	return ref, nil
}

func openVerifiedSourceHistory(rootFD int, basename string) (int, unix.Stat_t, error) {
	var before unix.Stat_t
	if err := unix.Fstatat(rootFD, basename, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return -1, unix.Stat_t{}, historyError(HistoryErrorRetryable, "history file is not synced; wait for history sync and retry")
		}
		return -1, unix.Stat_t{}, historyError(HistoryErrorRetryable, "inspect history file failed; retry after storage is available")
	}
	if !safeRegularHistoryStat(before) {
		return -1, unix.Stat_t{}, historyError(HistoryErrorInvalid, "history file must be one regular non-symlink, non-hardlinked file")
	}
	if before.Size <= 0 || before.Size > maxHistoryBytes {
		return -1, unix.Stat_t{}, historyError(HistoryErrorInvalid, "history file size is outside the supported handoff range")
	}
	fd, err := unix.Openat(rootFD, basename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, unix.Stat_t{}, historyError(HistoryErrorRetryable, "open history file failed; retry after storage is available")
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || !sameHistorySnapshot(before, opened) {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, historyError(HistoryErrorRetryable, "history file changed while opening; wait for sync to settle and retry")
	}
	return fd, opened, nil
}

func verifySourceHistoryUnchanged(rootFD, sourceFD int, basename string, before unix.Stat_t) error {
	var afterFD, afterPath unix.Stat_t
	if err := unix.Fstat(sourceFD, &afterFD); err != nil ||
		unix.Fstatat(rootFD, basename, &afterPath, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameHistorySnapshot(before, afterFD) || !sameHistorySnapshot(before, afterPath) {
		return historyError(HistoryErrorRetryable, "history file changed while validating; wait for sync to settle and retry")
	}
	return nil
}

func createSourceHistoryTemp(rootFD int, nonce func() (string, error)) (*os.File, string, error) {
	for attempt := 0; attempt < sourceHistoryTempTries; attempt++ {
		random, err := nonce()
		if err != nil {
			return nil, "", historyError(HistoryErrorRetryable, "allocate history publication failed; retry")
		}
		if random == "" || strings.ContainsAny(random, "/\\\x00\r\n") {
			return nil, "", historyError(HistoryErrorInvalid, "history publication nonce is invalid")
		}
		name := sourceHistoryTempPrefix + random
		fd, err := unix.Openat(rootFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", historyError(HistoryErrorRetryable, "create history publication failed; retry after storage is available")
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(rootFD, name, 0)
			return nil, "", historyError(HistoryErrorRetryable, "secure history publication failed; retry after storage is available")
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(rootFD, name, 0)
			return nil, "", historyError(HistoryErrorRetryable, "open history publication failed; retry")
		}
		return file, name, nil
	}
	return nil, "", historyError(HistoryErrorRetryable, "secure history publication name is unavailable; retry")
}

func verifySourceHistoryTemp(rootFD, tempFD int, name string, size int64) error {
	var byFD, byPath unix.Stat_t
	if err := unix.Fstat(tempFD, &byFD); err != nil ||
		unix.Fstatat(rootFD, name, &byPath, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameHistorySnapshot(byFD, byPath) || !safeRegularHistoryStat(byFD) || byFD.Size != size || byFD.Mode&0o777 != 0o600 {
		return historyError(HistoryErrorRetryable, "history publication changed before commit; retry")
	}
	return nil
}

func verifyPublishedSourceHistory(rootFD int, ref HistoryRef) error {
	var before unix.Stat_t
	if err := unix.Fstatat(rootFD, ref.Basename, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return historyError(HistoryErrorRetryable, "published history is unavailable; retry after sync storage is stable")
	}
	if !safeRegularHistoryStat(before) || before.Mode&0o777 != 0o600 {
		return historyError(HistoryErrorInvalid, "published history is not a private regular non-hardlinked file")
	}
	if before.Size != ref.SizeBytes {
		return historyError(HistoryErrorInvalid, "published history conflicts with its content address")
	}
	fd, err := unix.Openat(rootFD, ref.Basename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return historyError(HistoryErrorRetryable, "open published history failed; retry after storage is stable")
	}
	file := os.NewFile(uintptr(fd), ref.Basename)
	if file == nil {
		_ = unix.Close(fd)
		return historyError(HistoryErrorRetryable, "open published history failed; retry")
	}
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil || !sameHistorySnapshot(before, opened) {
		return historyError(HistoryErrorRetryable, "published history changed while opening; retry after storage is stable")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maxHistoryBytes+1))
	if err != nil {
		return historyError(HistoryErrorRetryable, "read published history failed; retry after storage is stable")
	}
	var afterFD, afterPath unix.Stat_t
	if err := unix.Fstat(fd, &afterFD); err != nil ||
		unix.Fstatat(rootFD, ref.Basename, &afterPath, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameHistorySnapshot(before, afterFD) || !sameHistorySnapshot(before, afterPath) {
		return historyError(HistoryErrorRetryable, "published history changed while validating; retry after storage is stable")
	}
	if written != ref.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != ref.SHA256 {
		return historyError(HistoryErrorInvalid, "published history conflicts with its content address")
	}
	return nil
}

func safeRegularHistoryStat(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1
}

func sameHistorySnapshot(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode == right.Mode && left.Nlink == right.Nlink &&
		left.Size == right.Size && sourceHistoryMtimeNsec(left) == sourceHistoryMtimeNsec(right)
}

func randomSourceHistoryNonce() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func sourceHistoryPublicationName(digest string) string {
	return fmt.Sprintf("%s%s%s", sourceHistoryPrefix, digest, sourceHistorySuffix)
}
