package handoff

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/project"
)

type repositoryFixture struct {
	root       string
	origin     string
	seed       string
	checkout   string
	worktrees  string
	baseCommit string
	head       string
	tree       string
}

func newRepositoryFixture(t *testing.T) *repositoryFixture {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origins", "lin-labs", "arcmux.git")
	if err := os.MkdirAll(filepath.Dir(origin), 0o700); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "init", "--bare", origin)

	seed := filepath.Join(root, "seed")
	if err := os.Mkdir(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	testGit(t, seed, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, seed, "add", "README.md")
	testGit(t, seed, "commit", "-m", "first")
	base := testGit(t, seed, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, seed, "add", "README.md")
	testGit(t, seed, "commit", "-m", "second")
	head := testGit(t, seed, "rev-parse", "HEAD")
	tree := testGit(t, seed, "rev-parse", "HEAD^{tree}")
	testGit(t, seed, "remote", "add", "origin", origin)
	testGit(t, seed, "push", "-u", "origin", "main")

	checkout := filepath.Join(root, "checkout")
	testGit(t, root, "clone", "--branch", "main", origin, checkout)
	worktrees := filepath.Join(root, "worktrees")
	if err := os.Mkdir(worktrees, 0o700); err != nil {
		t.Fatal(err)
	}
	return &repositoryFixture{
		root: root, origin: origin, seed: seed, checkout: checkout,
		worktrees: worktrees, baseCommit: base, head: head, tree: tree,
	}
}

func (f *repositoryFixture) manifest(id string) Manifest {
	return Manifest{
		HandoffID: id,
		Repository: RepositorySnapshot{
			ProjectSlug: "arcmux",
			RepoSlug:    "lin-labs/arcmux",
			Branch:      "main",
			SourceHead:  f.head,
			BaseCommit:  f.baseCommit,
			TreeDigest:  f.tree,
			Cleanliness: RepositoryClean,
			Transfer:    TransferRemoteBranch,
		},
	}
}

func (f *repositoryFixture) project() project.ResolvedProject {
	return project.ResolvedProject{Slug: "arcmux", RepoPaths: []string{f.checkout}, WorktreesRoot: f.worktrees}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Arcmux Test", "GIT_AUTHOR_EMAIL=arcmux@example.invalid",
		"GIT_COMMITTER_NAME=Arcmux Test", "GIT_COMMITTER_EMAIL=arcmux@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func requireRepositoryCode(t *testing.T, err error, want RepositoryErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("PrepareRepository error = nil, want %q", want)
	}
	got, ok := RepositoryErrorCodeOf(err)
	if !ok || got != want {
		t.Fatalf("PrepareRepository error = %v, code = %q, want %q", err, got, want)
	}
	var typed *RepositoryError
	if !errors.As(err, &typed) {
		t.Fatalf("error %T is not *RepositoryError", err)
	}
}

func TestPrepareRepositoryFetchesExactRemoteBranch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("clean-fetch")
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(filepath.Join(fixture.worktrees, "handoff-clean-fetch"))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WorktreePath != wantPath || prepared.Head != fixture.head || prepared.Branch != "arcmux/handoff/clean-fetch" {
		t.Fatalf("preparation = %#v", prepared)
	}
	if got := testGit(t, prepared.WorktreePath, "status", "--porcelain"); got != "" {
		t.Fatalf("prepared worktree dirty: %q", got)
	}
}

func TestPrepareRepositoryWrongRemoteHeadIsRetryable(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("wrong-head")
	manifest.Repository.SourceHead = strings.Repeat("0", 40)
	_, err := PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorRetryable)
}

func TestPrepareRepositoryRejectsTreeOrBaseMismatch(t *testing.T) {
	t.Run("tree digest", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		manifest := fixture.manifest("wrong-tree")
		manifest.Repository.TreeDigest = strings.Repeat("0", 40)
		_, err := PrepareRepository(context.Background(), manifest, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
	t.Run("base is not ancestor", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		unrelated := testGit(t, fixture.checkout, "commit-tree", fixture.tree, "-m", "unrelated")
		manifest := fixture.manifest("wrong-base")
		manifest.Repository.BaseCommit = unrelated
		_, err := PrepareRepository(context.Background(), manifest, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
}

func TestPrepareRepositoryMissingOrInaccessibleBranchIsRetryable(t *testing.T) {
	t.Run("missing branch", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		manifest := fixture.manifest("missing-branch")
		manifest.Repository.Branch = "missing-branch"
		_, err := PrepareRepository(context.Background(), manifest, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorRetryable)
	})
	t.Run("inaccessible origin", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		missingOrigin := filepath.Join(fixture.root, "unavailable", "lin-labs", "arcmux.git")
		testGit(t, fixture.checkout, "remote", "set-url", "origin", missingOrigin)
		_, err := PrepareRepository(context.Background(), fixture.manifest("origin-offline"), fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorRetryable)
	})
}

func TestPrepareRepositoryRejectsOriginSlugMismatch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("wrong-repo")
	manifest.Repository.RepoSlug = "other/repository"
	_, err := PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestPrepareRepositoryRequiresExistingWorktreesRoot(t *testing.T) {
	fixture := newRepositoryFixture(t)
	resolved := fixture.project()
	resolved.WorktreesRoot = ""
	_, err := PrepareRepository(context.Background(), fixture.manifest("no-root"), resolved)
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)

	resolved.WorktreesRoot = filepath.Join(fixture.root, "does-not-exist")
	_, err = PrepareRepository(context.Background(), fixture.manifest("missing-root"), resolved)
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestPrepareRepositoryCleanReplayReusesExactWorktree(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("replay")
	first, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if first != second {
		t.Fatalf("replay = %#v, first = %#v", second, first)
	}
}

func TestPrepareRepositoryRejectsDirtyOrMismatchedExistingWorktree(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		manifest := fixture.manifest("dirty")
		prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prepared.WorktreePath, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = PrepareRepository(context.Background(), manifest, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
	t.Run("mismatched head", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		manifest := fixture.manifest("mismatch")
		prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prepared.WorktreePath, "third.txt"), []byte("third"), 0o600); err != nil {
			t.Fatal(err)
		}
		testGit(t, prepared.WorktreePath, "add", "third.txt")
		testGit(t, prepared.WorktreePath, "commit", "-m", "advance synthetic branch")
		_, err = PrepareRepository(context.Background(), manifest, fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
}

func TestPrepareRepositoryRejectsSymlinkedRootOrCandidate(t *testing.T) {
	t.Run("root symlink", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		link := filepath.Join(fixture.root, "worktrees-link")
		if err := os.Symlink(fixture.worktrees, link); err != nil {
			t.Fatal(err)
		}
		resolved := fixture.project()
		resolved.WorktreesRoot = link
		_, err := PrepareRepository(context.Background(), fixture.manifest("root-link"), resolved)
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
	t.Run("candidate symlink escape", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		outside := filepath.Join(fixture.root, "outside")
		if err := os.Mkdir(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(fixture.worktrees, "handoff-candidate-link")
		if err := os.Symlink(outside, candidate); err != nil {
			t.Fatal(err)
		}
		_, err := PrepareRepository(context.Background(), fixture.manifest("candidate-link"), fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
}

func TestPrepareRepositoryRecoversExactSyntheticBranch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("recover")
	testGit(t, fixture.checkout, "branch", "arcmux/handoff/recover", fixture.head)
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository: %v", err)
	}
	if prepared.Head != fixture.head || prepared.Branch != "arcmux/handoff/recover" {
		t.Fatalf("preparation = %#v", prepared)
	}
}

func TestPrepareRepositoryRejectsMismatchedSyntheticBranch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("branch-conflict")
	testGit(t, fixture.checkout, "branch", "arcmux/handoff/branch-conflict", fixture.baseCommit)
	_, err := PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestPrepareRepositoryDoesNotInterpretBranchWithShell(t *testing.T) {
	fixture := newRepositoryFixture(t)
	marker := filepath.Join(fixture.root, "PWNED")
	branch := "feature/odd;touch${IFS}" + marker + ";#"
	testGit(t, fixture.seed, "check-ref-format", "--branch", branch)
	testGit(t, fixture.seed, "branch", branch, fixture.head)
	testGit(t, fixture.seed, "push", "origin", "refs/heads/"+branch)
	manifest := fixture.manifest("odd-branch")
	manifest.Repository.Branch = branch
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository: %v", err)
	}
	if prepared.Head != fixture.head {
		t.Fatalf("head = %q, want %q", prepared.Head, fixture.head)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("branch was interpreted by a shell; marker stat error = %v", err)
	}
}

func TestPrepareRepositoryStoredPatchIsExplicitlyUnsupported(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("stored-patch")
	manifest.Repository.Transfer = TransferStoredPatch
	_, err := PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}
