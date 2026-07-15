package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectSourceRepositoryCleanPushedBranch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	subdir := filepath.Join(fixture.checkout, "nested", "session")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved := fixture.project()
	resolved.RepoPaths = append([]string{filepath.Join(fixture.root, "unrelated")}, resolved.RepoPaths...)

	got, err := InspectSourceRepository(context.Background(), subdir, resolved)
	if err != nil {
		t.Fatalf("InspectSourceRepository: %v", err)
	}
	if got.ProjectSlug != "arcmux" || got.RepoSlug != "lin-labs/arcmux" || got.Branch != "main" {
		t.Fatalf("repository identity = %#v", got)
	}
	if got.SourceHead != fixture.head || got.BaseCommit != fixture.head || got.TreeDigest != fixture.tree {
		t.Fatalf("repository revisions = %#v", got)
	}
	if got.Cleanliness != RepositoryClean || got.Transfer != TransferRemoteBranch || got.Patch != nil {
		t.Fatalf("repository transfer = %#v", got)
	}
	if refs := testGit(t, fixture.checkout, "for-each-ref", "--format=%(refname)", "refs/arcmux/source-inspection/"); refs != "" {
		t.Fatalf("private inspection refs leaked: %q", refs)
	}
}

func TestInspectSourceRepositoryRejectsTrackedAndUntrackedDirt(t *testing.T) {
	tests := []struct {
		name  string
		dirty func(*testing.T, *repositoryFixture)
	}{
		{
			name: "tracked",
			dirty: func(t *testing.T, fixture *repositoryFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.checkout, "README.md"), []byte("dirty\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "untracked",
			dirty: func(t *testing.T, fixture *repositoryFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.checkout, "untracked.txt"), []byte("dirty\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRepositoryFixture(t)
			tt.dirty(t, fixture)
			_, err := InspectSourceRepository(context.Background(), fixture.checkout, fixture.project())
			requireRepositoryCode(t, err, RepositoryErrorDeterministic)
			if !strings.Contains(err.Error(), "commit and push") {
				t.Fatalf("error lacks safe remedy: %v", err)
			}
		})
	}
}

func TestInspectSourceRepositoryRejectsDetachedAndUnpushedHeads(t *testing.T) {
	t.Run("detached", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		testGit(t, fixture.checkout, "checkout", "--detach", fixture.head)
		_, err := InspectSourceRepository(context.Background(), fixture.checkout, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
		if !strings.Contains(err.Error(), "named branch") {
			t.Fatalf("error lacks detached-HEAD remedy: %v", err)
		}
	})

	t.Run("local head ahead of origin", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.WriteFile(filepath.Join(fixture.checkout, "local.txt"), []byte("local commit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		testGit(t, fixture.checkout, "add", "local.txt")
		testGit(t, fixture.checkout, "commit", "-m", "local only")
		_, err := InspectSourceRepository(context.Background(), fixture.checkout, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
		if !strings.Contains(err.Error(), "push the current branch") {
			t.Fatalf("error lacks push remedy: %v", err)
		}
	})

	t.Run("same-named origin branch missing", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		testGit(t, fixture.checkout, "checkout", "-b", "local-only")
		_, err := InspectSourceRepository(context.Background(), fixture.checkout, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
		if !strings.Contains(err.Error(), "push") {
			t.Fatalf("error lacks push remedy: %v", err)
		}
	})
}

func TestInspectSourceRepositoryRequiresSessionInsideConfiguredWorktree(t *testing.T) {
	fixture := newRepositoryFixture(t)
	outside := filepath.Join(fixture.root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := InspectSourceRepository(context.Background(), outside, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	if strings.Contains(err.Error(), fixture.root) || strings.Contains(err.Error(), fixture.checkout) {
		t.Fatalf("error leaked local paths: %v", err)
	}
}

func TestInspectSourceRepositoryResolvesSymlinksComponentSafely(t *testing.T) {
	fixture := newRepositoryFixture(t)
	subdir := filepath.Join(fixture.checkout, "sessions", "one")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	checkoutLink := filepath.Join(fixture.root, "checkout-link")
	if err := os.Symlink(fixture.checkout, checkoutLink); err != nil {
		t.Fatal(err)
	}
	resolved := fixture.project()
	resolved.RepoPaths = []string{checkoutLink}
	linkedCWD := filepath.Join(checkoutLink, "sessions", "one")
	if _, err := InspectSourceRepository(context.Background(), linkedCWD, resolved); err != nil {
		t.Fatalf("InspectSourceRepository through symlinks: %v", err)
	}

	prefixSibling := filepath.Join(fixture.root, "checkout-sibling")
	if err := os.Mkdir(prefixSibling, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := InspectSourceRepository(context.Background(), prefixSibling, resolved)
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestInspectSourceRepositoryAcceptsRegisteredManagedWorktree(t *testing.T) {
	fixture := newRepositoryFixture(t)
	worktree := addPushedSourceWorktree(t, fixture, fixture.worktrees, "registered")
	sessionCWD := filepath.Join(worktree, "nested", "session")
	if err := os.MkdirAll(sessionCWD, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := InspectSourceRepository(context.Background(), sessionCWD, fixture.project())
	if err != nil {
		t.Fatalf("InspectSourceRepository managed worktree: %v", err)
	}
	if got.Branch != "boyan/registered" || got.SourceHead != fixture.head || got.RepoSlug != "lin-labs/arcmux" {
		t.Fatalf("managed worktree snapshot = %#v", got)
	}
}

func TestInspectSourceRepositoryRejectsForeignRepositoryUnderManagedRoot(t *testing.T) {
	fixture := newRepositoryFixture(t)
	foreign := filepath.Join(fixture.worktrees, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	testGit(t, foreign, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(foreign, "README.md"), []byte("foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, foreign, "add", "README.md")
	testGit(t, foreign, "commit", "-m", "foreign")

	_, err := InspectSourceRepository(context.Background(), foreign, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestInspectSourceRepositoryManagedRootUsesRealComponentContainment(t *testing.T) {
	fixture := newRepositoryFixture(t)
	prefixSibling := fixture.worktrees + "-escape"
	if err := os.Mkdir(prefixSibling, 0o700); err != nil {
		t.Fatal(err)
	}
	escaped := addPushedSourceWorktree(t, fixture, prefixSibling, "prefix-escape")

	_, err := InspectSourceRepository(context.Background(), escaped, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)

	link := filepath.Join(fixture.worktrees, "symlink-escape")
	if err := os.Symlink(escaped, link); err != nil {
		t.Fatal(err)
	}
	_, err = InspectSourceRepository(context.Background(), link, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestInspectSourceRepositoryMissingOrUnsafeManagedRootDoesNotBroaden(t *testing.T) {
	fixture := newRepositoryFixture(t)
	worktree := addPushedSourceWorktree(t, fixture, fixture.worktrees, "root-policy")

	t.Run("missing root", func(t *testing.T) {
		resolved := fixture.project()
		resolved.WorktreesRoot = filepath.Join(fixture.root, "missing-worktrees")
		_, err := InspectSourceRepository(context.Background(), worktree, resolved)
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})

	t.Run("symlink root", func(t *testing.T) {
		rootLink := filepath.Join(fixture.root, "worktrees-link")
		if err := os.Symlink(fixture.worktrees, rootLink); err != nil {
			t.Fatal(err)
		}
		resolved := fixture.project()
		resolved.WorktreesRoot = rootLink
		_, err := InspectSourceRepository(context.Background(), worktree, resolved)
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
}

func addPushedSourceWorktree(t *testing.T, fixture *repositoryFixture, root, name string) string {
	t.Helper()
	worktree := filepath.Join(root, name)
	branch := "boyan/" + name
	testGit(t, fixture.checkout, "worktree", "add", "-b", branch, worktree, "HEAD")
	ref := "refs/heads/" + branch
	testGit(t, worktree, "push", "origin", ref+":"+ref)
	return worktree
}

func TestInspectSourceRepositoryTreatsShellLikeBranchAsArgv(t *testing.T) {
	fixture := newRepositoryFixture(t)
	marker := filepath.Join(fixture.root, "shell-owned")
	branch := "topic/$(touch${IFS}" + marker + ");safe"
	testGit(t, fixture.checkout, "checkout", "-b", branch)
	ref := "refs/heads/" + branch
	testGit(t, fixture.checkout, "push", "origin", ref+":"+ref)

	got, err := InspectSourceRepository(context.Background(), fixture.checkout, fixture.project())
	if err != nil {
		t.Fatalf("InspectSourceRepository shell-like branch: %v", err)
	}
	if got.Branch != branch {
		t.Fatalf("branch = %q, want %q", got.Branch, branch)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell-like branch was interpreted; marker stat error = %v", err)
	}
}

func TestInspectSourceRepositoryNormalizesOriginWithoutLeakingIt(t *testing.T) {
	tests := map[string]string{
		"https": "https://user:credential@example.invalid/lin-labs/arcmux.git",
		"ssh":   "git@example.invalid:lin-labs/arcmux.git",
		"file":  "/private/cache/lin-labs/arcmux.git",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if got, ok := normalizeOriginSlug(raw); !ok || got != "lin-labs/arcmux" {
				t.Fatalf("normalizeOriginSlug(%q) = %q, %v", raw, got, ok)
			}
		})
	}

	fixture := newRepositoryFixture(t)
	secret := "SUPER-SECRET-ORIGIN"
	missing := filepath.Join(fixture.root, secret, "lin-labs", "arcmux.git")
	testGit(t, fixture.checkout, "remote", "set-url", "origin", missing)
	_, err := InspectSourceRepository(context.Background(), fixture.checkout, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorRetryable)
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), missing) {
		t.Fatalf("error leaked origin: %v", err)
	}
}

func TestInspectHistoryReturnsDigestBoundSafeReference(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(parent, "histories")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Fatal(err)
	}
	content := []byte("# Synced conversation\n\nExact source bytes.\n")
	for _, name := range []string{"session-a.md", "session-b.md"} {
		if err := os.WriteFile(filepath.Join(realRoot, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, err := InspectHistory(rootLink, "session-a.md", "conversation-1")
	if err != nil {
		t.Fatalf("InspectHistory: %v", err)
	}
	digest := sha256.Sum256(content)
	wantDigest := hex.EncodeToString(digest[:])
	if first.SHA256 != wantDigest || first.SizeBytes != int64(len(content)) || first.ConversationID != "conversation-1" {
		t.Fatalf("history ref = %#v", first)
	}
	if first.ArtifactID != "history-"+wantDigest {
		t.Fatalf("artifact id = %q", first.ArtifactID)
	}
	second, err := InspectHistory(rootLink, "session-b.md", "")
	if err != nil {
		t.Fatalf("InspectHistory second basename: %v", err)
	}
	if second.ArtifactID != first.ArtifactID {
		t.Fatalf("artifact id depends on basename: %q != %q", second.ArtifactID, first.ArtifactID)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("returned ref does not validate: %v", err)
	}
}

func TestInspectHistoryRejectsUnsafeEntriesAndMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "session.md"), []byte("history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "../session.md", "nested/session.md", "/absolute.md", "bad\\name.md"} {
		t.Run("basename_"+strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			_, err := InspectHistory(root, name, "")
			requireHistoryCode(t, err, HistoryErrorInvalid)
		})
	}
	_, err := InspectHistory(root, "session.md", "bad conversation")
	requireHistoryCode(t, err, HistoryErrorInvalid)

	if err := os.Symlink(filepath.Join(root, "session.md"), filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	_, err = InspectHistory(root, "linked.md", "")
	requireHistoryCode(t, err, HistoryErrorInvalid)

	if err := os.Mkdir(filepath.Join(root, "directory.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = InspectHistory(root, "directory.md", "")
	requireHistoryCode(t, err, HistoryErrorInvalid)

	if err := os.WriteFile(filepath.Join(root, "empty.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = InspectHistory(root, "empty.md", "")
	requireHistoryCode(t, err, HistoryErrorInvalid)
}

func TestInspectHistoryMissingIsRetryableAndSafe(t *testing.T) {
	root := t.TempDir()
	secretName := "not-synced-secret.md"
	_, err := InspectHistory(root, secretName, "")
	requireHistoryCode(t, err, HistoryErrorRetryable)
	if strings.Contains(err.Error(), secretName) || strings.Contains(err.Error(), root) {
		t.Fatalf("missing-history error leaked a path or basename: %v", err)
	}
}
