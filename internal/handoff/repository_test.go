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
	if prepared.WorktreePath != wantPath || prepared.Head != fixture.head || prepared.LocalBranch != "arcmux/handoff/clean-fetch" || prepared.SourceBranch != "main" {
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

func TestPrepareRepositoryWorktreesRootOwnershipAndPermissions(t *testing.T) {
	t.Run("owner 0755 accepted", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.Chmod(fixture.worktrees, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := PrepareRepository(context.Background(), fixture.manifest("root-0755"), fixture.project()); err != nil {
			t.Fatalf("PrepareRepository: %v", err)
		}
	})
	t.Run("owner 0777 rejected", func(t *testing.T) {
		fixture := newRepositoryFixture(t)
		if err := os.Chmod(fixture.worktrees, 0o777); err != nil {
			t.Fatal(err)
		}
		_, err := PrepareRepository(context.Background(), fixture.manifest("root-0777"), fixture.project())
		requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	})
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

func TestPrepareRepositoryReplayRejectsTamperedPushPolicy(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("push-policy")
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, prepared.WorktreePath, "config", "--worktree", "push.default", "simple")
	_, err = PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	if got := testGit(t, prepared.WorktreePath, "config", "--worktree", "--get", "push.default"); got != "simple" {
		t.Fatalf("completed replay mutated push policy to %q", got)
	}
}

func TestPrepareRepositoryRepairsPushPolicyOnlyWithPreparationMarker(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("config-crash")
	candidate := filepath.Join(fixture.worktrees, "handoff-config-crash")
	localBranch, err := chooseLocalBranch(context.Background(), fixture.checkout, candidate, manifest.HandoffID, manifest.Repository.Branch)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := newPreparationMarker(context.Background(), fixture.checkout, manifest.HandoffID, localBranch, manifest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, fixture.checkout, "fetch", "origin", "refs/heads/main:refs/remotes/origin/main")
	testGit(t, fixture.checkout, "branch", "--track", localBranch, "refs/remotes/origin/main")
	if err := writePreparationMarker(fixture.worktrees, manifest.HandoffID, marker); err != nil {
		t.Fatal(err)
	}
	testGit(t, fixture.checkout, "worktree", "add", candidate, localBranch)
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository recovery: %v", err)
	}
	if got := testGit(t, prepared.WorktreePath, "config", "--worktree", "--get", "push.default"); got != "upstream" {
		t.Fatalf("recovered push policy = %q", got)
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
		testGit(t, prepared.WorktreePath, "commit", "-m", "advance source branch")
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

func TestPrepareRepositoryPlainPushUpdatesOriginalRemoteBranch(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("plain-push")
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prepared.WorktreePath, "continued.txt"), []byte("continued\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, prepared.WorktreePath, "add", "continued.txt")
	testGit(t, prepared.WorktreePath, "commit", "-m", "continue on target")
	newHead := testGit(t, prepared.WorktreePath, "rev-parse", "HEAD")
	testGit(t, prepared.WorktreePath, "push")
	if remoteHead := testGit(t, fixture.origin, "rev-parse", "refs/heads/main"); remoteHead != newHead {
		t.Fatalf("remote head = %q, want pushed %q", remoteHead, newHead)
	}
}

func TestPrepareRepositoryUsesFreeSourceBranchDirectly(t *testing.T) {
	fixture := newRepositoryFixture(t)
	branch := "feature/free"
	testGit(t, fixture.seed, "branch", branch, fixture.head)
	testGit(t, fixture.seed, "push", "origin", "refs/heads/"+branch)
	manifest := fixture.manifest("free-source")
	manifest.Repository.Branch = branch
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository: %v", err)
	}
	if prepared.LocalBranch != branch || prepared.SourceBranch != branch {
		t.Fatalf("preparation branches = %#v", prepared)
	}
}

func TestPrepareRepositoryRejectsLocalSourceBranchCollision(t *testing.T) {
	fixture := newRepositoryFixture(t)
	testGit(t, fixture.seed, "branch", "collision", fixture.head)
	testGit(t, fixture.seed, "push", "origin", "refs/heads/collision")
	testGit(t, fixture.checkout, "branch", "collision", fixture.baseCommit)
	manifest := fixture.manifest("branch-conflict")
	manifest.Repository.Branch = "collision"
	_, err := PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestPrepareRepositoryRejectsFallbackBranchCollision(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("fallback-collision")
	testGit(t, fixture.checkout, "branch", "arcmux/handoff/fallback-collision", fixture.baseCommit)
	_, err := PrepareRepository(context.Background(), manifest, fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
}

func TestPrepareRepositoryRecoversOwnedInterruptedCandidate(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := fixture.manifest("partial")
	localBranch, err := chooseLocalBranch(context.Background(), fixture.checkout, filepath.Join(fixture.worktrees, "handoff-partial"), manifest.HandoffID, manifest.Repository.Branch)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := newPreparationMarker(context.Background(), fixture.checkout, manifest.HandoffID, localBranch, manifest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	interruptedMarker := filepath.Join(fixture.worktrees, preparationMarkerName(manifest.HandoffID)+".tmp")
	if err := os.WriteFile(interruptedMarker, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePreparationMarker(fixture.worktrees, manifest.HandoffID, marker); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(fixture.worktrees, "handoff-partial")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "partial-file"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRepository(context.Background(), manifest, fixture.project())
	if err != nil {
		t.Fatalf("PrepareRepository recovery: %v", err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.WorktreePath != resolvedCandidate || prepared.LocalBranch != "arcmux/handoff/partial" || prepared.SourceBranch != "main" {
		t.Fatalf("preparation = %#v", prepared)
	}
	if _, err := os.Stat(filepath.Join(candidate, "partial-file")); !os.IsNotExist(err) {
		t.Fatalf("partial file survived recovery: %v", err)
	}
}

func TestPrepareRepositoryRejectsForeignOccupiedCandidate(t *testing.T) {
	fixture := newRepositoryFixture(t)
	candidate := filepath.Join(fixture.worktrees, "handoff-foreign")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "keep.txt"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRepository(context.Background(), fixture.manifest("foreign"), fixture.project())
	requireRepositoryCode(t, err, RepositoryErrorDeterministic)
	if data, readErr := os.ReadFile(filepath.Join(candidate, "keep.txt")); readErr != nil || string(data) != "foreign" {
		t.Fatalf("foreign path was mutated: %q, %v", data, readErr)
	}
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
