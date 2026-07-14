package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func historyRefFor(name string, content []byte) HistoryRef {
	digest := sha256.Sum256(content)
	return HistoryRef{
		ArtifactID:     "history-artifact",
		Basename:       name,
		SHA256:         hex.EncodeToString(digest[:]),
		SizeBytes:      int64(len(content)),
		ConversationID: "conversation-1",
	}
}

func requireHistoryCode(t *testing.T, err error, want HistoryErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("ResolveHistory error = nil, want code %q", want)
	}
	got, ok := HistoryErrorCodeOf(err)
	if !ok || got != want {
		t.Fatalf("ResolveHistory error = %v, code = %q, want %q", err, got, want)
	}
	var typed *HistoryError
	if !errors.As(err, &typed) {
		t.Fatalf("error %T is not *HistoryError", err)
	}
}

func TestResolveHistoryThroughSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real-histories")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(parent, "histories")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Fatal(err)
	}
	content := []byte("# Session\n\nA safe synced history.\n")
	ref := historyRefFor("2026-07-14-session.md", content)
	if err := os.WriteFile(filepath.Join(realRoot, ref.Basename), content, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveHistory(rootLink, ref)
	if err != nil {
		t.Fatalf("ResolveHistory: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedRoot, ref.Basename)
	if got != want {
		t.Fatalf("ResolveHistory path = %q, want resolved path %q", got, want)
	}
}

func TestResolveHistoryRejectsInvalidBasenames(t *testing.T) {
	content := []byte("history")
	for _, basename := range []string{"../escape.md", "nested/session.md", `nested\\session.md`, ".", "..", "/absolute.md", "line\nbreak.md"} {
		t.Run(basename, func(t *testing.T) {
			ref := historyRefFor(basename, content)
			_, err := ResolveHistory(t.TempDir(), ref)
			requireHistoryCode(t, err, HistoryErrorInvalid)
		})
	}
}

func TestResolveHistoryRejectsFileSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "histories")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("outside")
	outside := filepath.Join(parent, "outside.md")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := historyRefFor("session.md", content)
	if err := os.Symlink(outside, filepath.Join(root, ref.Basename)); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveHistory(root, ref)
	requireHistoryCode(t, err, HistoryErrorInvalid)
}

func TestResolveHistoryRejectsNonRegularFile(t *testing.T) {
	root := t.TempDir()
	ref := historyRefFor("session.md", []byte("history"))
	if err := os.Mkdir(filepath.Join(root, ref.Basename), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveHistory(root, ref)
	requireHistoryCode(t, err, HistoryErrorInvalid)
}

func TestResolveHistoryMissingIsRetryable(t *testing.T) {
	ref := historyRefFor("not-synced-yet.md", []byte("history"))
	_, err := ResolveHistory(t.TempDir(), ref)
	requireHistoryCode(t, err, HistoryErrorRetryable)
}

func TestResolveHistorySizeMismatchIsRetryable(t *testing.T) {
	root := t.TempDir()
	ref := historyRefFor("session.md", []byte("expected"))
	if err := os.WriteFile(filepath.Join(root, ref.Basename), []byte("different-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveHistory(root, ref)
	requireHistoryCode(t, err, HistoryErrorRetryable)
}

func TestResolveHistoryDigestMismatchIsRetryable(t *testing.T) {
	root := t.TempDir()
	ref := historyRefFor("session.md", []byte("expected"))
	if err := os.WriteFile(filepath.Join(root, ref.Basename), []byte("differnt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveHistory(root, ref)
	requireHistoryCode(t, err, HistoryErrorRetryable)
}

func TestResolveHistoryInvalidRootIsClassified(t *testing.T) {
	ref := historyRefFor("session.md", []byte("history"))
	_, err := ResolveHistory("relative/histories", ref)
	requireHistoryCode(t, err, HistoryErrorInvalid)

	_, err = ResolveHistory(filepath.Join(t.TempDir(), "missing"), ref)
	requireHistoryCode(t, err, HistoryErrorRetryable)
}
