package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestPublishSourceHistoryPinsExactContentForTarget(t *testing.T) {
	root := t.TempDir()
	originalName := "2026-07-15-handoff.md"
	originalContent := []byte("# Session\n\nFirst stable turn.\n")
	if err := os.WriteFile(filepath.Join(root, originalName), originalContent, 0o600); err != nil {
		t.Fatal(err)
	}

	inspected, err := InspectHistory(root, originalName, "conversation-1")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PublishSourceHistory(root, originalName, "conversation-1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ArtifactID != inspected.ArtifactID || ref.SHA256 != inspected.SHA256 || ref.SizeBytes != inspected.SizeBytes || ref.ConversationID != inspected.ConversationID {
		t.Fatalf("publication changed inspected integrity metadata: inspected=%#v published=%#v", inspected, ref)
	}
	if ref.Basename == originalName || !strings.HasPrefix(ref.Basename, sourceHistoryPrefix) || strings.Contains(ref.Basename, "2026") {
		t.Fatalf("publication basename leaks mutable identity: %q", ref.Basename)
	}
	if filepath.Ext(ref.Basename) == ".md" || !strings.HasSuffix(ref.Basename, ".snapshot") {
		t.Fatalf("publication can be mistaken for a canonical Markdown session log: %q", ref.Basename)
	}
	info, err := os.Lstat(filepath.Join(root, ref.Basename))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("publication info=%v err=%v", info, err)
	}

	file, err := os.OpenFile(filepath.Join(root, originalName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("Second turn after handoff queued.\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	private := privateHistoryRoot(t)
	targetPath, err := SnapshotHistory(root, private, "handoff-pinned", ref)
	if err != nil {
		t.Fatalf("target snapshot after source append: %v", err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, originalContent) {
		t.Fatalf("target got mutated history %q, want %q", got, originalContent)
	}
	published, err := os.ReadFile(filepath.Join(root, ref.Basename))
	if err != nil || !reflect.DeepEqual(published, originalContent) {
		t.Fatalf("source publication=%q err=%v", published, err)
	}
}

func TestPublishSourceHistoryThroughConfiguredRootSymlink(t *testing.T) {
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "histories")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "session.md"), []byte("symlinked root history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := PublishSourceHistory(link, "session.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(realRoot, ref.Basename)); err != nil {
		t.Fatalf("publication missing from resolved history root: %v", err)
	}
}

func TestPublishSourceHistoryReplayAndConcurrentSameContent(t *testing.T) {
	root := t.TempDir()
	name := "session.md"
	content := []byte("stable shared history\n")
	if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := PublishSourceHistory(root, name, "conversation-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PublishSourceHistory(root, name, "conversation-1")
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("replay first=%#v second=%#v err=%v", first, second, err)
	}

	concurrentRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(concurrentRoot, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	const publishers = 24
	refs := make(chan HistoryRef, publishers)
	errs := make(chan error, publishers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ref, err := PublishSourceHistory(concurrentRoot, name, "conversation-1")
			if err != nil {
				errs <- err
				return
			}
			refs <- ref
		}()
	}
	close(start)
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent publish: %v", err)
	}
	for ref := range refs {
		if !reflect.DeepEqual(ref, first) {
			t.Errorf("concurrent ref=%#v want %#v", ref, first)
		}
	}
	entries, err := os.ReadDir(concurrentRoot)
	if err != nil {
		t.Fatal(err)
	}
	publications := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), sourceHistoryTempPrefix) {
			t.Fatalf("temporary publication leaked: %s", entry.Name())
		}
		if strings.HasPrefix(entry.Name(), sourceHistoryPrefix) {
			publications++
		}
	}
	if publications != 1 {
		t.Fatalf("content publications=%d, want 1", publications)
	}
}

func TestPublishSourceHistoryRejectsSourceLinkAndMutationAttacks(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("private\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "session.md")); err != nil {
			t.Fatal(err)
		}
		_, err := PublishSourceHistory(root, "session.md", "")
		assertSourceHistoryError(t, err, HistoryErrorInvalid)
	})

	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("private\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(outside, filepath.Join(root, "session.md")); err != nil {
			t.Fatal(err)
		}
		_, err := PublishSourceHistory(root, "session.md", "")
		assertSourceHistoryError(t, err, HistoryErrorInvalid)
	})

	t.Run("replacement after open", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "session.md")
		replacement := filepath.Join(root, "replacement.md")
		if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(replacement, []byte("replacement\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := publishSourceHistory(root, "session.md", "", fixedSourceHistoryNonce("replace"), sourceHistoryPublishHooks{
			afterSourceOpen: func() {
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			},
		})
		assertSourceHistoryError(t, err, HistoryErrorRetryable)
	})

	t.Run("append after copy", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "session.md")
		if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := publishSourceHistory(root, "session.md", "", fixedSourceHistoryNonce("append"), sourceHistoryPublishHooks{
			afterCopy: func() {
				file, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, writeErr := file.WriteString("later\n")
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatalf("append write=%v close=%v", writeErr, closeErr)
				}
			},
		})
		assertSourceHistoryError(t, err, HistoryErrorRetryable)
	})
}

func TestPublishSourceHistoryRejectsDestinationAndTempAttacks(t *testing.T) {
	content := []byte("content addressed history\n")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	destination := sourceHistoryPublicationName(digestHex)

	tests := []struct {
		name  string
		plant func(*testing.T, string, string)
	}{
		{"symlink destination", func(t *testing.T, root, destination string) {
			outside := filepath.Join(t.TempDir(), "outside.md")
			if err := os.WriteFile(outside, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, destination)); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink destination", func(t *testing.T, root, destination string) {
			backing := filepath.Join(root, "backing.md")
			if err := os.WriteFile(backing, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(backing, filepath.Join(root, destination)); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt destination", func(t *testing.T, root, destination string) {
			if err := os.WriteFile(filepath.Join(root, destination), []byte("wrong stable bytes\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "session.md"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			test.plant(t, root, destination)
			_, err := PublishSourceHistory(root, "session.md", "")
			assertSourceHistoryError(t, err, HistoryErrorInvalid)
		})
	}

	t.Run("preplanted temp symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "session.md"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("do not touch\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, sourceHistoryTempPrefix+"fixed")); err != nil {
			t.Fatal(err)
		}
		_, err := publishSourceHistory(root, "session.md", "", fixedSourceHistoryNonce("fixed"), sourceHistoryPublishHooks{})
		assertSourceHistoryError(t, err, HistoryErrorRetryable)
		got, readErr := os.ReadFile(outside)
		if readErr != nil || string(got) != "do not touch\n" {
			t.Fatalf("temp symlink target changed: %q err=%v", got, readErr)
		}
	})
}

func TestPublishSourceHistoryDetectsStablePublishedMismatch(t *testing.T) {
	root := t.TempDir()
	content := []byte("same length original\n")
	if err := os.WriteFile(filepath.Join(root, "session.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := PublishSourceHistory(root, "session.md", "")
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(strings.Repeat("x", len(content)))
	if err := os.WriteFile(filepath.Join(root, ref.Basename), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = PublishSourceHistory(root, "session.md", "")
	assertSourceHistoryError(t, err, HistoryErrorInvalid)
}

func TestPublishSourceHistoryErrorsDoNotExposeRootPath(t *testing.T) {
	secretComponent := "private-user-history-location"
	missing := filepath.Join(t.TempDir(), secretComponent, "missing")
	_, err := PublishSourceHistory(missing, "session.md", "")
	assertSourceHistoryError(t, err, HistoryErrorRetryable)
	if strings.Contains(err.Error(), secretComponent) || strings.Contains(err.Error(), missing) {
		t.Fatalf("history error exposed configured path: %v", err)
	}
}

func fixedSourceHistoryNonce(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}

func assertSourceHistoryError(t *testing.T, err error, want HistoryErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want history %s", want)
	}
	var historyErr *HistoryError
	if !errors.As(err, &historyErr) || historyErr.Code != want {
		t.Fatalf("error=%v, want history %s", err, want)
	}
}
