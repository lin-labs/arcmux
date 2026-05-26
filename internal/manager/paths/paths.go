// Package paths resolves the canonical filesystem locations arcmux uses for
// per-project ephemeral state.
//
// Vault-side concerns (role libraries, project subtrees like elon/, teams/,
// retros/, principles/) are no longer arcmux's responsibility — those are
// elonco-managed. arcmux is the substrate librarian and only knows about
// ~/data/arcmux/<project>/.
package paths

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Project bundles every path a per-project ephemeral substrate needs.
type Project struct {
	Project       string
	EphemeralRoot string // ~/data/arcmux/<project>/
	StateBolt     string // ~/data/arcmux/<project>/state.bolt
	Scratchpads   string // ~/data/arcmux/<project>/scratchpads/
	ConsultInbox  string // ~/data/arcmux/<project>/consult_inboxes/
	Heartbeats    string // ~/data/arcmux/<project>/heartbeats/

	// VaultRoot is kept as a derived path because callers may want to
	// resolve vault-side artifacts they own (e.g. elonco resolving a role
	// file). arcmux itself does not write under VaultRoot anymore.
	VaultRoot string // <vault>/Projects/<project>/
}

// ForProject computes every path given the ephemeral data root, vault root,
// and a validated project slug.
func ForProject(dataRoot, vaultRoot, project string) Project {
	eph := filepath.Join(dataRoot, "arcmux", project)
	v := filepath.Join(vaultRoot, "Projects", project)
	return Project{
		Project:       project,
		EphemeralRoot: eph,
		StateBolt:     filepath.Join(eph, "state.bolt"),
		Scratchpads:   filepath.Join(eph, "scratchpads"),
		ConsultInbox:  filepath.Join(eph, "consult_inboxes"),
		Heartbeats:    filepath.Join(eph, "heartbeats"),
		VaultRoot:     v,
	}
}

var projectSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Validate ensures the project slug is filesystem-safe.
func Validate(project string) (string, error) {
	if !projectSlug.MatchString(project) {
		return "", fmt.Errorf("invalid project slug %q: must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63}", project)
	}
	return project, nil
}
