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

func privateHistoryRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func requireHistoryCode(t *testing.T, err error, want HistoryErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("SnapshotHistory error = nil, want code %q", want)
	}
	got, ok := HistoryErrorCodeOf(err)
	if !ok || got != want {
		t.Fatalf("SnapshotHistory error = %v, code = %q, want %q", err, got, want)
	}
	var typed *HistoryError
	if !errors.As(err, &typed) {
		t.Fatalf("error %T is not *HistoryError", err)
	}
}

func TestSnapshotHistoryThroughSymlinkedSourceRoot(t *testing.T) {
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
	private := privateHistoryRoot(t)

	got, err := SnapshotHistory(rootLink, private, "handoff-one", ref)
	if err != nil {
		t.Fatalf("SnapshotHistory: %v", err)
	}
	resolvedPrivate, err := filepath.EvalSymlinks(private)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedPrivate, "handoff-handoff-one", "history.md")
	if got != want {
		t.Fatalf("SnapshotHistory path = %q, want %q", got, want)
	}
	if data, err := os.ReadFile(got); err != nil || string(data) != string(content) {
		t.Fatalf("snapshot data = %q, err = %v", data, err)
	}
	if mode := mustMode(t, filepath.Dir(got)); mode != 0o700 {
		t.Fatalf("handoff dir mode = %o, want 0700", mode)
	}
	if mode := mustMode(t, got); mode != 0o600 {
		t.Fatalf("snapshot mode = %o, want 0600", mode)
	}
}

func TestSnapshotHistorySurvivesSyncedEntryReplacement(t *testing.T) {
	historyRoot := t.TempDir()
	content := []byte("original synced conversation")
	ref := historyRefFor("session.md", content)
	synced := filepath.Join(historyRoot, ref.Basename)
	if err := os.WriteFile(synced, content, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotHistory(historyRoot, privateHistoryRoot(t), "replacement", ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(synced, synced+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(synced, []byte("attacker replacement content"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("snapshot changed with synced entry: %q", got)
	}
	digest := sha256.Sum256(got)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		t.Fatalf("snapshot digest changed")
	}
}

func TestSnapshotHistoryIdempotentReplayAndConflict(t *testing.T) {
	historyRoot := t.TempDir()
	content := []byte("stable history")
	ref := historyRefFor("session.md", content)
	if err := os.WriteFile(filepath.Join(historyRoot, ref.Basename), content, 0o600); err != nil {
		t.Fatal(err)
	}
	private := privateHistoryRoot(t)
	first, err := SnapshotHistory(historyRoot, private, "replay", ref)
	if err != nil {
		t.Fatal(err)
	}
	// Replay is snapshot-local and still succeeds after the synced source goes
	// away.
	if err := os.Remove(filepath.Join(historyRoot, ref.Basename)); err != nil {
		t.Fatal(err)
	}
	second, err := SnapshotHistory(historyRoot, private, "replay", ref)
	if err != nil || second != first {
		t.Fatalf("replay path = %q, err = %v, want %q", second, err, first)
	}

	conflict := historyRefFor("other.md", []byte("other history!"))
	_, err = SnapshotHistory(historyRoot, private, "replay", conflict)
	requireHistoryCode(t, err, HistoryErrorInvalid)
}

func TestSnapshotHistoryRejectsInvalidBasenames(t *testing.T) {
	content := []byte("history")
	for _, basename := range []string{"../escape.md", "nested/session.md", `nested\\session.md`, ".", "..", "/absolute.md", "line\nbreak.md"} {
		t.Run(basename, func(t *testing.T) {
			ref := historyRefFor(basename, content)
			_, err := SnapshotHistory(t.TempDir(), privateHistoryRoot(t), "invalid-name", ref)
			requireHistoryCode(t, err, HistoryErrorInvalid)
		})
	}
}

func TestSnapshotHistoryRejectsSourceFileSymlinkEscape(t *testing.T) {
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
	_, err := SnapshotHistory(root, privateHistoryRoot(t), "source-link", ref)
	requireHistoryCode(t, err, HistoryErrorInvalid)
}

func TestSnapshotHistoryRejectsPrivateSymlinkAttacks(t *testing.T) {
	historyRoot := t.TempDir()
	content := []byte("history")
	ref := historyRefFor("session.md", content)
	if err := os.WriteFile(filepath.Join(historyRoot, ref.Basename), content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("root symlink", func(t *testing.T) {
		realPrivate := privateHistoryRoot(t)
		link := filepath.Join(t.TempDir(), "private-link")
		if err := os.Symlink(realPrivate, link); err != nil {
			t.Fatal(err)
		}
		_, err := SnapshotHistory(historyRoot, link, "root-link", ref)
		requireHistoryCode(t, err, HistoryErrorInvalid)
	})
	t.Run("handoff dir symlink", func(t *testing.T) {
		private := privateHistoryRoot(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(private, "handoff-dir-link")); err != nil {
			t.Fatal(err)
		}
		_, err := SnapshotHistory(historyRoot, private, "dir-link", ref)
		requireHistoryCode(t, err, HistoryErrorInvalid)
	})
	t.Run("snapshot symlink", func(t *testing.T) {
		private := privateHistoryRoot(t)
		dir := filepath.Join(private, "handoff-file-link")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "history.md")); err != nil {
			t.Fatal(err)
		}
		_, err := SnapshotHistory(historyRoot, private, "file-link", ref)
		requireHistoryCode(t, err, HistoryErrorInvalid)
	})
}

func TestSnapshotHistoryRejectsInsecurePrivatePermissions(t *testing.T) {
	historyRoot := t.TempDir()
	content := []byte("history")
	ref := historyRefFor("session.md", content)
	if err := os.WriteFile(filepath.Join(historyRoot, ref.Basename), content, 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := SnapshotHistory(historyRoot, private, "bad-perms", ref)
	requireHistoryCode(t, err, HistoryErrorInvalid)
}

func TestSnapshotHistoryMissingAndMismatchAreRetryable(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		ref := historyRefFor("not-synced-yet.md", []byte("history"))
		_, err := SnapshotHistory(t.TempDir(), privateHistoryRoot(t), "missing", ref)
		requireHistoryCode(t, err, HistoryErrorRetryable)
	})
	t.Run("size", func(t *testing.T) {
		root := t.TempDir()
		ref := historyRefFor("session.md", []byte("expected"))
		if err := os.WriteFile(filepath.Join(root, ref.Basename), []byte("different-size"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := SnapshotHistory(root, privateHistoryRoot(t), "size", ref)
		requireHistoryCode(t, err, HistoryErrorRetryable)
	})
	t.Run("digest", func(t *testing.T) {
		root := t.TempDir()
		ref := historyRefFor("session.md", []byte("expected"))
		if err := os.WriteFile(filepath.Join(root, ref.Basename), []byte("differnt"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := SnapshotHistory(root, privateHistoryRoot(t), "digest", ref)
		requireHistoryCode(t, err, HistoryErrorRetryable)
	})
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
