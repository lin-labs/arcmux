package handoff

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/project"
)

// InspectSourceRepository verifies that a session is running in one of the
// project's configured checkouts and that its clean symbolic branch is already
// available from origin at exactly HEAD. The returned snapshot is safe to put
// in a v1 remote_branch handoff manifest: it contains no local path or remote
// URL, and BaseCommit deliberately equals SourceHead for this clean-only slice.
//
// Git is always invoked directly with an argv. Branch names are expanded only
// into fully qualified refs and are never interpreted by a shell.
func InspectSourceRepository(ctx context.Context, sessionCWD string, resolved project.ResolvedProject) (RepositorySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "source inspection was interrupted; retry the handoff")
	}
	if err := validateID("project_slug", resolved.Slug); err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "project metadata is invalid; repair the project registry")
	}

	checkout, err := sourceCheckoutForSession(ctx, sessionCWD, resolved.RepoPaths)
	if err != nil {
		return RepositorySnapshot{}, err
	}
	branch, err := gitOutput(ctx, checkout, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if isExitCode(err, 1) {
			return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "source checkout is detached; check out and push a named branch")
		}
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "inspect source branch failed; retry the handoff")
	}
	if err := validateGitRef(branch); err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "source branch cannot be handed off safely; rename and push the branch")
	}

	status, err := gitOutput(ctx, checkout, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "inspect source cleanliness failed; retry the handoff")
	}
	if status != "" {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "source checkout is dirty; commit and push the changes before handoff")
	}

	head, err := gitOutput(ctx, checkout, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !objectID.MatchString(head) {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "inspect source HEAD failed; retry the handoff")
	}
	tree, err := gitOutput(ctx, checkout, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil || !objectID.MatchString(tree) {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "inspect source tree failed; retry the handoff")
	}

	origin, err := gitOutput(ctx, checkout, "remote", "get-url", "origin")
	if err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "source checkout has no usable origin; configure and push origin")
	}
	repoSlug, ok := normalizeOriginSlug(origin)
	if !ok {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "source origin cannot be normalized; configure an owner/repository origin")
	}

	branchRef := "refs/heads/" + branch
	advertised, err := gitOutput(ctx, checkout, "ls-remote", "--exit-code", "--refs", "origin", branchRef)
	if err != nil {
		if isExitCode(err, 2) {
			return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "origin branch is missing; push the current branch before handoff")
		}
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "origin is unavailable; reconnect and retry the handoff")
	}
	remoteHead, ok := advertisedBranchHead(advertised, branchRef)
	if !ok {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "origin returned an unverifiable branch; retry the handoff")
	}
	if remoteHead != head {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "origin branch does not match source HEAD; push the current branch before handoff")
	}

	privateRef, err := newSourceInspectionRef()
	if err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "allocate private fetch ref failed; retry the handoff")
	}
	defer deleteSourceInspectionRef(checkout, privateRef)
	refspec := branchRef + ":" + privateRef
	if _, err := gitOutput(ctx, checkout, "fetch", "--no-tags", "--force", "--no-write-fetch-head", "origin", refspec); err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "fetch exact origin branch failed; reconnect and retry the handoff")
	}
	fetchedHead, err := gitOutput(ctx, checkout, "rev-parse", "--verify", privateRef+"^{commit}")
	if err != nil || fetchedHead != head {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "origin branch changed during inspection; retry after it stabilizes")
	}
	// Re-read every mutable local claim after the network round trip. This
	// keeps the manifest internally coherent if another process checks out,
	// commits, edits, or rewrites origin while inspection is in progress.
	finalBranch, branchErr := gitOutput(ctx, checkout, "symbolic-ref", "--quiet", "--short", "HEAD")
	finalStatus, statusErr := gitOutput(ctx, checkout, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	finalHead, headErr := gitOutput(ctx, checkout, "rev-parse", "--verify", "HEAD^{commit}")
	finalTree, treeErr := gitOutput(ctx, checkout, "rev-parse", "--verify", "HEAD^{tree}")
	finalOrigin, originErr := gitOutput(ctx, checkout, "remote", "get-url", "origin")
	finalRepoSlug, finalOriginOK := normalizeOriginSlug(finalOrigin)
	if branchErr != nil || statusErr != nil || headErr != nil || treeErr != nil || originErr != nil || !finalOriginOK ||
		finalBranch != branch || finalStatus != "" || finalHead != head || finalTree != tree || finalRepoSlug != repoSlug {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorRetryable, "source checkout changed during inspection; wait for it to settle and retry")
	}

	snapshot := RepositorySnapshot{
		ProjectSlug: resolved.Slug,
		RepoSlug:    repoSlug,
		Branch:      branch,
		SourceHead:  head,
		BaseCommit:  head,
		TreeDigest:  tree,
		Cleanliness: RepositoryClean,
		Transfer:    TransferRemoteBranch,
	}
	if err := snapshot.validate(); err != nil {
		return RepositorySnapshot{}, repositoryError(RepositoryErrorDeterministic, "source metadata cannot form a safe handoff; repair the project registry and branch")
	}
	return snapshot, nil
}

func sourceCheckoutForSession(ctx context.Context, sessionCWD string, candidates []string) (string, error) {
	if sessionCWD == "" || strings.ContainsRune(sessionCWD, '\x00') || !filepath.IsAbs(sessionCWD) {
		return "", repositoryError(RepositoryErrorDeterministic, "session directory is invalid; start the session in a registered checkout")
	}
	realCWD, err := filepath.EvalSymlinks(filepath.Clean(sessionCWD))
	if err != nil {
		return "", repositoryError(RepositoryErrorDeterministic, "session directory is unavailable; start the session in a registered checkout")
	}
	realCWD, err = filepath.Abs(realCWD)
	if err != nil {
		return "", repositoryError(RepositoryErrorDeterministic, "session directory is invalid; start the session in a registered checkout")
	}
	info, err := os.Stat(realCWD)
	if err != nil || !info.IsDir() {
		return "", repositoryError(RepositoryErrorDeterministic, "session directory is unavailable; start the session in a registered checkout")
	}

	var matches []string
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) || strings.ContainsRune(candidate, '\x00') {
			continue
		}
		realCandidate, err := filepath.EvalSymlinks(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		realCandidate, err = filepath.Abs(realCandidate)
		if err != nil || !withinResolvedRoot(realCandidate, realCWD) {
			continue
		}
		candidateInfo, err := os.Stat(realCandidate)
		if err != nil || !candidateInfo.IsDir() {
			continue
		}
		matches = append(matches, filepath.Clean(realCandidate))
	}
	// Prefer the most specific configured root if a registry contains nested
	// entries. A candidate still has to prove that it is the Git worktree root.
	sort.Slice(matches, func(i, j int) bool { return len(matches[i]) > len(matches[j]) })
	for _, candidate := range matches {
		inside, err := gitOutput(ctx, candidate, "rev-parse", "--is-inside-work-tree")
		if err != nil || inside != "true" {
			continue
		}
		top, err := gitOutput(ctx, candidate, "rev-parse", "--show-toplevel")
		if err != nil {
			continue
		}
		realTop, err := filepath.EvalSymlinks(top)
		if err != nil {
			continue
		}
		realTop, err = filepath.Abs(realTop)
		if err == nil && filepath.Clean(realTop) == candidate && withinResolvedRoot(candidate, realCWD) {
			return candidate, nil
		}
	}
	return "", repositoryError(RepositoryErrorDeterministic, "session directory is outside the configured Git worktree; choose the registered project checkout")
}

func advertisedBranchHead(output, branchRef string) (string, bool) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		return "", false
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[1] != branchRef || !objectID.MatchString(fields[0]) {
		return "", false
	}
	return fields[0], true
}

func newSourceInspectionRef() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return "refs/arcmux/source-inspection/" + hex.EncodeToString(nonce[:]), nil
}

func deleteSourceInspectionRef(checkout, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = gitOutput(ctx, checkout, "update-ref", "-d", ref)
}

// InspectHistory verifies a basename-only synced history reference and hashes
// the exact bytes read from an already-open file descriptor. The root may be a
// symlink, but the history file itself must be a regular non-symlink file. The
// returned reference contains no local path or content.
func InspectHistory(historyRoot, basename, conversationID string) (HistoryRef, error) {
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
		return HistoryRef{}, err
	}
	root, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "open configured history root failed; retry after storage is available")
	}
	defer root.Close()

	before, err := root.Lstat(basename)
	if err != nil {
		if os.IsNotExist(err) {
			return HistoryRef{}, historyError(HistoryErrorRetryable, "history file is not synced; wait for history sync and retry")
		}
		return HistoryRef{}, historyError(HistoryErrorRetryable, "inspect history file failed; retry after storage is available")
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return HistoryRef{}, historyError(HistoryErrorInvalid, "history file must be a regular non-symlink file")
	}
	if before.Size() <= 0 || before.Size() > maxHistoryBytes {
		return HistoryRef{}, historyError(HistoryErrorInvalid, "history file size is outside the supported handoff range")
	}

	file, err := root.Open(basename)
	if err != nil {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "open history file failed; retry after storage is available")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() || !opened.ModTime().Equal(before.ModTime()) {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "history file changed while opening; wait for sync to settle and retry")
	}

	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maxHistoryBytes+1))
	if err != nil {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "read history file failed; retry after storage is available")
	}
	if written != before.Size() {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "history file changed while reading; wait for sync to settle and retry")
	}
	afterFD, fdErr := file.Stat()
	afterPath, pathErr := root.Lstat(basename)
	if fdErr != nil || pathErr != nil || afterPath.Mode()&os.ModeSymlink != 0 || !afterPath.Mode().IsRegular() ||
		!os.SameFile(opened, afterFD) || !os.SameFile(opened, afterPath) || afterFD.Size() != written || afterPath.Size() != written ||
		!afterFD.ModTime().Equal(opened.ModTime()) || !afterPath.ModTime().Equal(opened.ModTime()) {
		return HistoryRef{}, historyError(HistoryErrorRetryable, "history file changed while validating; wait for sync to settle and retry")
	}

	digestHex := hex.EncodeToString(digest.Sum(nil))
	ref := HistoryRef{
		ArtifactID:     "history-" + digestHex,
		Basename:       basename,
		SHA256:         digestHex,
		SizeBytes:      written,
		ConversationID: conversationID,
	}
	if err := ref.Validate(); err != nil {
		return HistoryRef{}, historyError(HistoryErrorInvalid, "history metadata cannot form a safe handoff reference")
	}
	return ref, nil
}

func validSourceHistoryBasename(name string) bool {
	return name != "" && len([]rune(name)) <= 255 && filepath.Base(name) == name && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00\r\n")
}
