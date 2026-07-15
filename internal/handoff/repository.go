package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lin-labs/arcmux/internal/project"
)

type RepositoryErrorCode string

const (
	RepositoryErrorDeterministic RepositoryErrorCode = "deterministic"
	RepositoryErrorRetryable     RepositoryErrorCode = "retryable"
)

type RepositoryError struct {
	Code RepositoryErrorCode
	Err  error
}

func (e *RepositoryError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("repository %s: %v", e.Code, e.Err)
}

func (e *RepositoryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func RepositoryErrorCodeOf(err error) (RepositoryErrorCode, bool) {
	var repositoryErr *RepositoryError
	if !errors.As(err, &repositoryErr) {
		return "", false
	}
	return repositoryErr.Code, true
}

// RepositoryPreparation is the exact target-local continuation checkout.
// LocalBranch is SourceBranch when free, or a deterministic fallback when the
// source branch is already checked out. Either way its upstream and
// worktree-local push policy target origin/SourceBranch.
type RepositoryPreparation struct {
	WorktreePath string
	Head         string
	LocalBranch  string
	SourceBranch string
}

func PrepareRepository(ctx context.Context, manifest Manifest, resolved project.ResolvedProject) (RepositoryPreparation, error) {
	if err := validateID("handoff_id", manifest.HandoffID); err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "invalid handoff id")
	}
	snapshot := manifest.Repository
	switch snapshot.Transfer {
	case TransferStoredPatch:
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "stored_patch transfer is not supported by remote branch preparation")
	case TransferRemoteBranch:
	default:
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "unsupported transfer mode")
	}
	if err := snapshot.validate(); err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "invalid repository snapshot")
	}
	if resolved.Slug == "" || resolved.Slug != snapshot.ProjectSlug {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "resolved project does not match manifest")
	}

	checkout, err := selectRepositoryCheckout(ctx, resolved.RepoPaths, snapshot.RepoSlug)
	if err != nil {
		return RepositoryPreparation{}, err
	}
	root, candidate, err := resolveWorktreeCandidate(resolved.WorktreesRoot, manifest.HandoffID)
	if err != nil {
		return RepositoryPreparation{}, err
	}
	localBranch, err := chooseLocalBranch(ctx, checkout, candidate, manifest.HandoffID, snapshot.Branch)
	if err != nil {
		return RepositoryPreparation{}, err
	}
	marker, err := newPreparationMarker(ctx, checkout, manifest.HandoffID, localBranch, snapshot)
	if err != nil {
		return RepositoryPreparation{}, err
	}

	if exists, err := candidateExists(candidate); err != nil {
		return RepositoryPreparation{}, err
	} else if exists {
		registered, err := registeredWorktree(ctx, checkout, candidate)
		if err != nil {
			return RepositoryPreparation{}, err
		}
		if registered {
			ownedIncomplete, err := preparationMarkerExists(root, manifest.HandoffID, marker)
			if err != nil {
				return RepositoryPreparation{}, err
			}
			if ownedIncomplete {
				if err := configureWorktreePush(ctx, checkout, candidate); err != nil {
					return RepositoryPreparation{}, err
				}
			}
			prepared, err := validateRepositoryReplay(ctx, checkout, root, candidate, localBranch, snapshot)
			if err != nil {
				return RepositoryPreparation{}, err
			}
			if err := clearPreparationMarker(root, manifest.HandoffID, marker); err != nil {
				return RepositoryPreparation{}, err
			}
			return prepared, nil
		}
		recovered, err := recoverOwnedPartialCandidate(root, candidate, manifest.HandoffID, marker)
		if err != nil {
			return RepositoryPreparation{}, err
		}
		if !recovered {
			return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing path is not a registered worktree")
		}
	}

	remoteRef := "refs/remotes/origin/" + snapshot.Branch
	refspec := "refs/heads/" + snapshot.Branch + ":" + remoteRef
	if _, err := gitOutput(ctx, checkout, "fetch", "--no-tags", "--force", "--no-write-fetch-head", "origin", refspec); err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "fetch exact remote branch failed")
	}
	fetchedHead, err := gitOutput(ctx, checkout, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "fetched branch is not available")
	}
	if fetchedHead != snapshot.SourceHead {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "fetched branch has not reached the declared head")
	}
	if err := validateRepositoryCommitClaims(ctx, checkout, fetchedHead, snapshot); err != nil {
		return RepositoryPreparation{}, err
	}

	localRef := "refs/heads/" + localBranch
	branchHead, branchErr := gitOutput(ctx, checkout, "rev-parse", "--verify", "--quiet", localRef+"^{commit}")
	branchExists := branchErr == nil
	if branchErr != nil && !isExitCode(branchErr, 1) {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "inspect local continuation branch failed")
	}
	if branchExists && branchHead != snapshot.SourceHead {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "local continuation branch already exists at a different head")
	}
	if checkedOut, err := branchWorktree(ctx, checkout, localRef); err != nil {
		return RepositoryPreparation{}, err
	} else if checkedOut != "" {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "local continuation branch is already checked out in another worktree")
	}
	if branchExists {
		if err := verifyBranchUpstream(ctx, checkout, localBranch, snapshot.Branch); err != nil {
			return RepositoryPreparation{}, err
		}
	} else {
		if _, err := gitOutput(ctx, checkout, "branch", "--track", localBranch, remoteRef); err != nil {
			return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "create local continuation branch failed")
		}
		if err := verifyBranchUpstream(ctx, checkout, localBranch, snapshot.Branch); err != nil {
			return RepositoryPreparation{}, err
		}
	}

	if err := writePreparationMarker(root, manifest.HandoffID, marker); err != nil {
		return RepositoryPreparation{}, err
	}
	if _, err := gitOutput(ctx, checkout, "worktree", "add", candidate, localBranch); err != nil {
		if exists, _ := candidateExists(candidate); exists {
			if registered, _ := registeredWorktree(ctx, checkout, candidate); registered {
				if configErr := configureWorktreePush(ctx, checkout, candidate); configErr == nil {
					if prepared, replayErr := validateRepositoryReplay(ctx, checkout, root, candidate, localBranch, snapshot); replayErr == nil {
						_ = clearPreparationMarker(root, manifest.HandoffID, marker)
						return prepared, nil
					}
				}
			}
		}
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "attach deterministic worktree failed")
	}
	if err := configureWorktreePush(ctx, checkout, candidate); err != nil {
		return RepositoryPreparation{}, err
	}
	prepared, err := validateRepositoryReplay(ctx, checkout, root, candidate, localBranch, snapshot)
	if err != nil {
		return RepositoryPreparation{}, err
	}
	if err := clearPreparationMarker(root, manifest.HandoffID, marker); err != nil {
		return RepositoryPreparation{}, err
	}
	return prepared, nil
}

func selectRepositoryCheckout(ctx context.Context, candidates []string, wantSlug string) (string, error) {
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		origin, err := gitOutput(ctx, candidate, "remote", "get-url", "origin")
		if err != nil {
			continue
		}
		if slug, ok := normalizeOriginSlug(origin); ok && slug == wantSlug {
			return candidate, nil
		}
	}
	return "", repositoryError(RepositoryErrorDeterministic, "no configured checkout matches the repository slug")
}

func normalizeOriginSlug(origin string) (string, bool) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return "", false
	}
	pathPart := origin
	if strings.Contains(origin, "://") {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Path == "" {
			return "", false
		}
		pathPart = parsed.Path
	} else if colon := strings.IndexByte(origin, ':'); colon > 0 && !strings.ContainsAny(origin[:colon], `/\`) {
		pathPart = origin[colon+1:]
	}
	pathPart = strings.Trim(strings.ReplaceAll(pathPart, `\`, "/"), "/")
	parts := strings.Split(pathPart, "/")
	if len(parts) < 2 {
		return "", false
	}
	owner := parts[len(parts)-2]
	repo := strings.TrimSuffix(parts[len(parts)-1], ".git")
	slug := owner + "/" + repo
	if !repoSlug.MatchString(slug) || strings.Contains(slug, "..") {
		return "", false
	}
	return slug, true
}

func resolveWorktreeCandidate(rawRoot, handoffID string) (string, string, error) {
	if strings.TrimSpace(rawRoot) == "" || !filepath.IsAbs(rawRoot) {
		return "", "", repositoryError(RepositoryErrorDeterministic, "declared worktrees root is required")
	}
	info, err := os.Lstat(rawRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", repositoryError(RepositoryErrorDeterministic, "declared worktrees root does not exist")
		}
		return "", "", repositoryError(RepositoryErrorRetryable, "inspect declared worktrees root failed")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", "", repositoryError(RepositoryErrorDeterministic, "declared worktrees root must be a real directory")
	}
	if !ownedByCurrentEUID(info) || info.Mode().Perm()&0o022 != 0 {
		return "", "", repositoryError(RepositoryErrorDeterministic, "declared worktrees root must be owner-controlled and not group/world writable")
	}
	root, err := filepath.EvalSymlinks(rawRoot)
	if err != nil {
		return "", "", repositoryError(RepositoryErrorRetryable, "resolve declared worktrees root failed")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", repositoryError(RepositoryErrorDeterministic, "make worktrees root absolute failed")
	}
	root = filepath.Clean(root)
	candidate := filepath.Join(root, "handoff-"+handoffID)
	if !withinResolvedRoot(root, candidate) {
		return "", "", repositoryError(RepositoryErrorDeterministic, "deterministic worktree escapes declared root")
	}
	return root, candidate, nil
}

func ownedByCurrentEUID(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func candidateExists(candidate string) (bool, error) {
	info, err := os.Lstat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, repositoryError(RepositoryErrorRetryable, "inspect deterministic worktree failed")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, repositoryError(RepositoryErrorDeterministic, "deterministic worktree path is occupied")
	}
	return true, nil
}

func chooseLocalBranch(ctx context.Context, checkout, candidate, handoffID, sourceBranch string) (string, error) {
	sourceRef := "refs/heads/" + sourceBranch
	fallback := "arcmux/handoff/" + handoffID
	if err := validateGitRef(fallback); err != nil {
		return "", repositoryError(RepositoryErrorDeterministic, "handoff id cannot form a fallback branch")
	}
	fallbackRef := "refs/heads/" + fallback
	entries, err := listWorktrees(ctx, checkout)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !sameFilesystemPath(entry.Path, candidate) {
			continue
		}
		switch entry.Branch {
		case sourceRef:
			return sourceBranch, nil
		case fallbackRef:
			return fallback, nil
		default:
			return "", repositoryError(RepositoryErrorDeterministic, "deterministic worktree uses an unexpected local branch")
		}
	}
	for _, entry := range entries {
		if entry.Branch == sourceRef {
			return fallback, nil
		}
	}
	return sourceBranch, nil
}

func sameFilesystemPath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil {
		left = leftResolved
	}
	if rightErr == nil {
		right = rightResolved
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func validateRepositoryReplay(ctx context.Context, checkout, root, candidate, wantLocalBranch string, snapshot RepositorySnapshot) (RepositoryPreparation, error) {
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "resolve deterministic worktree failed")
	}
	if !withinResolvedRoot(root, resolvedCandidate) {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "deterministic worktree escapes declared root")
	}
	registered, err := registeredWorktree(ctx, checkout, resolvedCandidate)
	if err != nil {
		return RepositoryPreparation{}, err
	}
	if !registered {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing path is not a registered worktree")
	}
	sourceCommon, err := gitCommonDir(ctx, checkout)
	if err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "configured checkout is not a usable repository")
	}
	candidateCommon, err := gitCommonDir(ctx, resolvedCandidate)
	if err != nil || sourceCommon != candidateCommon {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing worktree belongs to a different repository")
	}
	head, err := gitOutput(ctx, resolvedCandidate, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != snapshot.SourceHead {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing worktree head does not match")
	}
	if err := validateRepositoryCommitClaims(ctx, resolvedCandidate, head, snapshot); err != nil {
		return RepositoryPreparation{}, err
	}
	branch, err := gitOutput(ctx, resolvedCandidate, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != wantLocalBranch {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing worktree branch does not match")
	}
	if err := verifyBranchUpstream(ctx, resolvedCandidate, branch, snapshot.Branch); err != nil {
		return RepositoryPreparation{}, err
	}
	if err := verifyWorktreePush(ctx, resolvedCandidate); err != nil {
		return RepositoryPreparation{}, err
	}
	status, err := gitOutput(ctx, resolvedCandidate, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "inspect existing worktree status failed")
	}
	if status != "" {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing worktree is dirty")
	}
	return RepositoryPreparation{WorktreePath: resolvedCandidate, Head: head, LocalBranch: branch, SourceBranch: snapshot.Branch}, nil
}

func verifyBranchUpstream(ctx context.Context, checkout, localBranch, sourceBranch string) error {
	upstream, err := gitOutput(ctx, checkout, "for-each-ref", "--format=%(upstream:short)", "refs/heads/"+localBranch)
	if err != nil {
		return repositoryError(RepositoryErrorRetryable, "inspect continuation branch upstream failed")
	}
	if upstream != "origin/"+sourceBranch {
		return repositoryError(RepositoryErrorDeterministic, "continuation branch upstream does not match origin")
	}
	return nil
}

func configureWorktreePush(ctx context.Context, checkout, worktree string) error {
	if _, err := gitOutput(ctx, checkout, "config", "extensions.worktreeConfig", "true"); err != nil {
		return repositoryError(RepositoryErrorRetryable, "enable worktree-local Git configuration failed")
	}
	if _, err := gitOutput(ctx, worktree, "config", "--worktree", "push.default", "upstream"); err != nil {
		return repositoryError(RepositoryErrorRetryable, "configure continuation push behavior failed")
	}
	return verifyWorktreePush(ctx, worktree)
}

func verifyWorktreePush(ctx context.Context, worktree string) error {
	got, err := gitOutput(ctx, worktree, "config", "--worktree", "--get", "push.default")
	if err != nil || got != "upstream" {
		return repositoryError(RepositoryErrorDeterministic, "continuation push behavior is not worktree-local upstream")
	}
	return nil
}

func validateRepositoryCommitClaims(ctx context.Context, checkout, head string, snapshot RepositorySnapshot) error {
	tree, err := gitOutput(ctx, checkout, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return repositoryError(RepositoryErrorRetryable, "inspect repository tree failed")
	}
	if tree != snapshot.TreeDigest {
		return repositoryError(RepositoryErrorDeterministic, "repository tree does not match the manifest")
	}
	if _, err := gitOutput(ctx, checkout, "cat-file", "-e", snapshot.BaseCommit+"^{commit}"); err != nil {
		return repositoryError(RepositoryErrorDeterministic, "base commit is not available")
	}
	if _, err := gitOutput(ctx, checkout, "merge-base", "--is-ancestor", snapshot.BaseCommit, head); err != nil {
		if isExitCode(err, 1) {
			return repositoryError(RepositoryErrorDeterministic, "base commit is not an ancestor of the repository head")
		}
		return repositoryError(RepositoryErrorRetryable, "verify base ancestry failed")
	}
	return nil
}

type gitWorktree struct {
	Path   string
	Branch string
}

func listWorktrees(ctx context.Context, checkout string) ([]gitWorktree, error) {
	output, err := gitOutput(ctx, checkout, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, repositoryError(RepositoryErrorRetryable, "list registered worktrees failed")
	}
	var entries []gitWorktree
	var current gitWorktree
	for _, field := range strings.Split(output, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			if current.Path != "" {
				entries = append(entries, current)
			}
			current = gitWorktree{Path: strings.TrimPrefix(field, "worktree ")}
		case strings.HasPrefix(field, "branch "):
			current.Branch = strings.TrimPrefix(field, "branch ")
		}
	}
	if current.Path != "" {
		entries = append(entries, current)
	}
	return entries, nil
}

func registeredWorktree(ctx context.Context, checkout, candidate string) (bool, error) {
	entries, err := listWorktrees(ctx, checkout)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		listed, err := filepath.EvalSymlinks(entry.Path)
		if err == nil && filepath.Clean(listed) == filepath.Clean(candidate) {
			return true, nil
		}
	}
	return false, nil
}

func branchWorktree(ctx context.Context, checkout, branchRef string) (string, error) {
	entries, err := listWorktrees(ctx, checkout)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Branch == branchRef {
			return entry.Path, nil
		}
	}
	return "", nil
}

func gitCommonDir(ctx context.Context, checkout string) (string, error) {
	common, err := gitOutput(ctx, checkout, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return "", err
	}
	return filepath.Clean(common), nil
}

type preparationMarker struct {
	Version      int    `json:"version"`
	HandoffID    string `json:"handoff_id"`
	RepoID       string `json:"repo_id"`
	LocalBranch  string `json:"local_branch"`
	SourceBranch string `json:"source_branch"`
	Head         string `json:"head"`
}

func newPreparationMarker(ctx context.Context, checkout, handoffID, localBranch string, snapshot RepositorySnapshot) (preparationMarker, error) {
	common, err := gitCommonDir(ctx, checkout)
	if err != nil {
		return preparationMarker{}, repositoryError(RepositoryErrorDeterministic, "configured checkout is not a usable repository")
	}
	digest := sha256.Sum256([]byte(common))
	return preparationMarker{
		Version: 1, HandoffID: handoffID, RepoID: hex.EncodeToString(digest[:]),
		LocalBranch: localBranch, SourceBranch: snapshot.Branch, Head: snapshot.SourceHead,
	}, nil
}

func preparationMarkerName(handoffID string) string {
	return ".handoff-" + handoffID + ".preparing"
}

func writePreparationMarker(root, handoffID string, want preparationMarker) error {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return repositoryError(RepositoryErrorRetryable, "open worktrees root failed")
	}
	defer opened.Close()
	name := preparationMarkerName(handoffID)
	if exists, err := verifyPreparationMarker(opened, name, want); err != nil {
		return err
	} else if exists {
		return nil
	}
	data, err := json.Marshal(want)
	if err != nil {
		return repositoryError(RepositoryErrorDeterministic, "encode preparation marker failed")
	}
	tempName := name + ".tmp"
	if info, err := opened.Lstat(tempName); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByCurrentEUID(info) {
			return repositoryError(RepositoryErrorDeterministic, "preparation marker temporary path is unsafe")
		}
		stale, err := opened.OpenFile(tempName, os.O_RDWR, 0)
		if err != nil {
			return repositoryError(RepositoryErrorRetryable, "open preparation marker temporary file failed")
		}
		staleInfo, statErr := stale.Stat()
		if statErr != nil || !os.SameFile(info, staleInfo) {
			stale.Close()
			return repositoryError(RepositoryErrorRetryable, "preparation marker temporary file changed while opening")
		}
		if err := flockExclusiveNonblocking(stale); err != nil {
			stale.Close()
			return repositoryError(RepositoryErrorRetryable, "preparation marker publication is already in progress")
		}
		if err := opened.Remove(tempName); err != nil {
			releaseHistorySnapshotLock(stale)
			return repositoryError(RepositoryErrorRetryable, "remove interrupted preparation marker failed")
		}
		releaseHistorySnapshotLock(stale)
	} else if !os.IsNotExist(err) {
		return repositoryError(RepositoryErrorRetryable, "inspect preparation marker temporary path failed")
	}
	file, err := opened.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return repositoryError(RepositoryErrorRetryable, "create preparation marker temporary file failed")
	}
	if err := flockExclusiveNonblocking(file); err != nil {
		file.Close()
		return repositoryError(RepositoryErrorRetryable, "lock preparation marker temporary file failed")
	}
	defer releaseHistorySnapshotLock(file)
	if err := file.Chmod(0o600); err != nil {
		return repositoryError(RepositoryErrorRetryable, "secure preparation marker failed")
	}
	if exists, err := verifyPreparationMarker(opened, name, want); err != nil {
		return err
	} else if exists {
		_ = opened.Remove(tempName)
		return nil
	}
	if _, err := file.Write(data); err != nil {
		return repositoryError(RepositoryErrorRetryable, "write preparation marker failed")
	}
	if err := file.Sync(); err != nil {
		return repositoryError(RepositoryErrorRetryable, "sync preparation marker failed")
	}
	if err := opened.Rename(tempName, name); err != nil {
		return repositoryError(RepositoryErrorRetryable, "publish preparation marker failed")
	}
	if err := syncDirectory(root); err != nil {
		return repositoryError(RepositoryErrorRetryable, "sync worktrees root failed")
	}
	return nil
}

func verifyPreparationMarker(root *os.Root, name string, want preparationMarker) (bool, error) {
	before, err := root.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, repositoryError(RepositoryErrorRetryable, "inspect preparation marker failed")
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 || !ownedByCurrentEUID(before) {
		return false, repositoryError(RepositoryErrorDeterministic, "preparation marker is unsafe")
	}
	if before.Size() <= 0 || before.Size() > 4096 {
		return false, repositoryError(RepositoryErrorDeterministic, "preparation marker has invalid size")
	}
	file, err := root.Open(name)
	if err != nil {
		return false, repositoryError(RepositoryErrorRetryable, "open preparation marker failed")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return false, repositoryError(RepositoryErrorRetryable, "preparation marker changed while opening")
	}
	var got preparationMarker
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		return false, repositoryError(RepositoryErrorDeterministic, "preparation marker is malformed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false, repositoryError(RepositoryErrorDeterministic, "preparation marker has trailing content")
	}
	if got != want {
		return false, repositoryError(RepositoryErrorDeterministic, "preparation marker conflicts with handoff")
	}
	return true, nil
}

func preparationMarkerExists(root, handoffID string, want preparationMarker) (bool, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return false, repositoryError(RepositoryErrorRetryable, "open worktrees root failed")
	}
	defer opened.Close()
	return verifyPreparationMarker(opened, preparationMarkerName(handoffID), want)
}

func clearPreparationMarker(root, handoffID string, want preparationMarker) error {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return repositoryError(RepositoryErrorRetryable, "open worktrees root failed")
	}
	defer opened.Close()
	name := preparationMarkerName(handoffID)
	exists, err := verifyPreparationMarker(opened, name, want)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := opened.Remove(name); err != nil {
		return repositoryError(RepositoryErrorRetryable, "remove preparation marker failed")
	}
	if err := syncDirectory(root); err != nil {
		return repositoryError(RepositoryErrorRetryable, "sync worktrees root failed")
	}
	return nil
}

func recoverOwnedPartialCandidate(root, candidate, handoffID string, want preparationMarker) (bool, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return false, repositoryError(RepositoryErrorRetryable, "open worktrees root failed")
	}
	defer opened.Close()
	exists, err := verifyPreparationMarker(opened, preparationMarkerName(handoffID), want)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if err := verifyOwnedRemovalTree(root, candidate); err != nil {
		return false, err
	}
	if err := os.RemoveAll(candidate); err != nil {
		return false, repositoryError(RepositoryErrorRetryable, "remove interrupted worktree failed")
	}
	if err := syncDirectory(root); err != nil {
		return false, repositoryError(RepositoryErrorRetryable, "sync worktrees root failed")
	}
	return true, nil
}

func verifyOwnedRemovalTree(root, candidate string) error {
	if !withinResolvedRoot(root, candidate) {
		return repositoryError(RepositoryErrorDeterministic, "interrupted worktree escapes declared root")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return repositoryError(RepositoryErrorRetryable, "inspect worktrees root failed")
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return repositoryError(RepositoryErrorDeterministic, "worktrees root ownership is unavailable")
	}
	return filepath.Walk(candidate, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return repositoryError(RepositoryErrorRetryable, "inspect interrupted worktree failed")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || stat.Dev != rootStat.Dev {
			return repositoryError(RepositoryErrorDeterministic, "interrupted worktree is not safely owned")
		}
		if info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 != 0 {
			return repositoryError(RepositoryErrorDeterministic, "interrupted worktree has unsafe permissions")
		}
		return nil
	})
}

func gitOutput(ctx context.Context, checkout string, args ...string) (string, error) {
	argv := append([]string{"-C", checkout}, args...)
	command := exec.CommandContext(ctx, "git", argv...)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func isExitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func repositoryError(code RepositoryErrorCode, message string) error {
	return &RepositoryError{Code: code, Err: errors.New(message)}
}
