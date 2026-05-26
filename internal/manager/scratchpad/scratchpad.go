// Package scratchpad owns the on-disk representation of a role's per-project
// scratchpad: one JSON-ish blob per role at
// $ARCMUX_DATA/arcmux/<project>/scratchpads/<role>.json, written atomically.
//
// Two callers consume this package:
//   - cmd/arcmux-cli (out-of-process callers — spawned role panes)
//   - internal/manager (in-process launcher seeding Elon's initial state)
//
// Both paths share one implementation so the byte-level semantics (perms,
// atomic rename, fsync) cannot drift between callers.
package scratchpad

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/lin-labs/arcmux/internal/manager/paths"
)

// roleSlug constrains role identifiers to the same shape as project slugs:
// a leading alphanumeric, then up to 63 of {alnum, _, ., -}. Forbids "../",
// slashes, leading dots — anything that could break out of the scratchpads
// dir or shadow a hidden file.
var roleSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidateRole returns nil iff role matches roleSlug. Both writers and
// readers run this so a bad role never produces a file the other side
// cannot find (or, worse, escapes the scratchpads dir).
func ValidateRole(role string) error {
	if role == "" {
		return fmt.Errorf("role required")
	}
	if !roleSlug.MatchString(role) {
		return fmt.Errorf("invalid role %q (must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63})", role)
	}
	return nil
}

// Path returns the canonical scratchpad file path for (project, role) and
// ensures the parent dir exists with 0700 perms. dataRoot is typically
// ~/data; project must already be validated by paths.Validate.
func Path(dataRoot, project, role string) (string, error) {
	if err := ValidateRole(role); err != nil {
		return "", err
	}
	if _, err := paths.Validate(project); err != nil {
		return "", err
	}
	p := paths.ForProject(dataRoot, "", project)
	if err := os.MkdirAll(p.Scratchpads, 0o700); err != nil {
		return "", fmt.Errorf("ensure %s: %w", p.Scratchpads, err)
	}
	return filepath.Join(p.Scratchpads, role+".json"), nil
}

// Write atomically replaces path's contents with body via a sibling tmp file
// plus rename, fsyncing both the file and its parent dir to survive a crash
// between rename and flush. Permissions are 0600 — machine-local ephemeral
// state, least-priv default.
func Write(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
