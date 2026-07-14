package handoff

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lin-labs/arcmux/internal/project"
)

// RepositoryErrorCode distinguishes deterministic input/configuration
// conflicts from target-local failures that can succeed on a later retry.
type RepositoryErrorCode string

const (
	RepositoryErrorDeterministic RepositoryErrorCode = "deterministic"
	RepositoryErrorRetryable     RepositoryErrorCode = "retryable"
)

// RepositoryError is deliberately bounded: its message never contains Git
// command output or a configured origin URL.
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

// RepositoryErrorCodeOf extracts the classification returned by
// PrepareRepository.
func RepositoryErrorCodeOf(err error) (RepositoryErrorCode, bool) {
	var repositoryErr *RepositoryError
	if !errors.As(err, &repositoryErr) {
		return "", false
	}
	return repositoryErr.Code, true
}

// RepositoryPreparation identifies the verified target-local worktree. The
// commit and synthetic branch are repeated so callers do not need to inspect
// the worktree with another subprocess before launching a session.
type RepositoryPreparation struct {
	WorktreePath string
	Head         string
	Branch       string
}

// PrepareRepository materializes or recovers the deterministic target-local
// worktree for a remote_branch handoff. It executes Git directly with argv;
// neither manifest values nor local configuration are interpreted by a shell.
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

	syntheticBranch := "arcmux/handoff/" + manifest.HandoffID
	if err := validateGitRef(syntheticBranch); err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "handoff id cannot form a synthetic branch")
	}
	checkout, err := selectRepositoryCheckout(ctx, resolved.RepoPaths, snapshot.RepoSlug)
	if err != nil {
		return RepositoryPreparation{}, err
	}
	root, candidate, err := resolveWorktreeCandidate(resolved.WorktreesRoot, manifest.HandoffID)
	if err != nil {
		return RepositoryPreparation{}, err
	}

	if exists, err := candidateExists(candidate); err != nil {
		return RepositoryPreparation{}, err
	} else if exists {
		return validateRepositoryReplay(ctx, checkout, root, candidate, snapshot, syntheticBranch)
	}

	privateRef := privateHandoffRef(manifest.HandoffID)
	refspec := "refs/heads/" + snapshot.Branch + ":" + privateRef
	if _, err := gitOutput(ctx, checkout, "fetch", "--no-tags", "--force", "--no-write-fetch-head", "origin", refspec); err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "fetch exact remote branch failed")
	}
	fetchedHead, err := gitOutput(ctx, checkout, "rev-parse", "--verify", privateRef+"^{commit}")
	if err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "fetched branch is not available")
	}
	if fetchedHead != snapshot.SourceHead {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "fetched branch has not reached the declared head")
	}
	if err := validateRepositoryCommitClaims(ctx, checkout, fetchedHead, snapshot); err != nil {
		return RepositoryPreparation{}, err
	}

	branchRef := "refs/heads/" + syntheticBranch
	branchHead, branchErr := gitOutput(ctx, checkout, "rev-parse", "--verify", "--quiet", branchRef+"^{commit}")
	branchExists := branchErr == nil
	if branchErr != nil && !isExitCode(branchErr, 1) {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "inspect synthetic branch failed")
	}
	if branchExists && branchHead != snapshot.SourceHead {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "synthetic branch already exists at a different head")
	}

	var addErr error
	if branchExists {
		_, addErr = gitOutput(ctx, checkout, "worktree", "add", candidate, syntheticBranch)
	} else {
		_, addErr = gitOutput(ctx, checkout, "worktree", "add", "-b", syntheticBranch, candidate, privateRef)
	}
	if addErr != nil {
		// A concurrent replay may have completed between the existence check and
		// worktree add. Revalidate it before reporting a transient failure.
		if exists, _ := candidateExists(candidate); exists {
			if prepared, replayErr := validateRepositoryReplay(ctx, checkout, root, candidate, snapshot, syntheticBranch); replayErr == nil {
				return prepared, nil
			}
		}
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "attach deterministic worktree failed")
	}
	return validateRepositoryReplay(ctx, checkout, root, candidate, snapshot, syntheticBranch)
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
		// SCP-style Git URL: [user@]host:owner/repo.git.
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

func validateRepositoryReplay(ctx context.Context, checkout, root, candidate string, snapshot RepositorySnapshot, wantBranch string) (RepositoryPreparation, error) {
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
	if err != nil || branch != wantBranch {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing worktree branch does not match")
	}
	status, err := gitOutput(ctx, resolvedCandidate, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorRetryable, "inspect existing worktree status failed")
	}
	if status != "" {
		return RepositoryPreparation{}, repositoryError(RepositoryErrorDeterministic, "existing worktree is dirty")
	}
	return RepositoryPreparation{WorktreePath: resolvedCandidate, Head: head, Branch: branch}, nil
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

func registeredWorktree(ctx context.Context, checkout, candidate string) (bool, error) {
	output, err := gitOutput(ctx, checkout, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, repositoryError(RepositoryErrorRetryable, "list registered worktrees failed")
	}
	for _, field := range strings.Split(output, "\x00") {
		if !strings.HasPrefix(field, "worktree ") {
			continue
		}
		listed, err := filepath.EvalSymlinks(strings.TrimPrefix(field, "worktree "))
		if err == nil && filepath.Clean(listed) == filepath.Clean(candidate) {
			return true, nil
		}
	}
	return false, nil
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

func privateHandoffRef(handoffID string) string {
	digest := sha256.Sum256([]byte(handoffID))
	return fmt.Sprintf("refs/arcmux/handoffs/%x/source", digest[:16])
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
