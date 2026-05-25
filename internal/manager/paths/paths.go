// Package paths resolves the canonical filesystem locations arcmux's
// manager mode uses, separating machine-local ephemeral state from
// vault-backed durable artifacts.
package paths

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Project bundles every path a manager-mode project needs.
type Project struct {
	Project       string
	EphemeralRoot string // ~/data/arcmux/<project>/
	StateBolt     string // ~/data/arcmux/<project>/state.bolt
	Scratchpads   string // ~/data/arcmux/<project>/scratchpads/
	ConsultInbox  string // ~/data/arcmux/<project>/consult_inboxes/
	Heartbeats    string // ~/data/arcmux/<project>/heartbeats/

	VaultRoot     string // <vault>/Projects/<project>/
	ArcmuxDir     string // <vault>/Projects/<project>/arcmux/
	PrinciplesDir string // <vault>/Projects/<project>/arcmux/principles/
	DeliverDir    string // <vault>/Projects/<project>/arcmux/deliverables/
	ElonDir       string // <vault>/Projects/<project>/elon/
	TeamsDir      string // <vault>/Projects/<project>/teams/
	RetrosDir     string // <vault>/Projects/<project>/retros/
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
		ArcmuxDir:     filepath.Join(v, "arcmux"),
		PrinciplesDir: filepath.Join(v, "arcmux", "principles"),
		DeliverDir:    filepath.Join(v, "arcmux", "deliverables"),
		ElonDir:       filepath.Join(v, "elon"),
		TeamsDir:      filepath.Join(v, "teams"),
		RetrosDir:     filepath.Join(v, "retros"),
	}
}

// GlobalRolesDir returns the cross-project role library path.
func GlobalRolesDir(vaultRoot string) string {
	return filepath.Join(vaultRoot, "0Prompts", "roles")
}

var projectSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Validate ensures the project slug is filesystem-safe.
func Validate(project string) (string, error) {
	if !projectSlug.MatchString(project) {
		return "", fmt.Errorf("invalid project slug %q: must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63}", project)
	}
	return project, nil
}
