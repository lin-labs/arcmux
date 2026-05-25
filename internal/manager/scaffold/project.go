// Package scaffold creates the durable + ephemeral directory layout for a
// manager-mode project and seeds the global role library if absent.
package scaffold

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/roles"
)

const readmeTemplate = "# arcmux project: %s\n\n" +
	"This directory holds the durable artifacts for the **%s** arcmux project.\n\n" +
	"## Layout\n\n" +
	"- `mission.md` — original mission statement\n" +
	"- `playbook.md` — project-specific overrides to default playbook\n" +
	"- `principles/` — accumulated per-project principles (Elon, Manager, IC roles, gotchas)\n" +
	"- `deliverables/` — final outputs ready for the user\n\n" +
	"Sibling directories:\n\n" +
	"- `../elon/` — Elon's journal + curated decisions\n" +
	"- `../teams/<slug>/` — per-team charters, journals, decisions\n" +
	"- `../retros/` — heavy-retro archives\n\n" +
	"Machine-local ephemeral state (state.bolt, scratchpads, heartbeats) lives at\n" +
	"`~/data/arcmux/%s/`.\n\n" +
	"Global, cross-project role definitions live at\n" +
	"`~obsAgents/0Prompts/roles/` and are authored by Elon over time.\n"

const missionTemplate = "---\n" +
	"project: %s\n" +
	"created: %s\n" +
	"status: active\n" +
	"---\n\n" +
	"# Mission\n\n" +
	"%s\n\n" +
	"## Active teams\n\n" +
	"(none yet — Elon spawns teams reactively as orders arrive)\n\n" +
	"## Goals\n\n" +
	"(populate as orders are received)\n"

const playbookTemplate = "---\n" +
	"project: %s\n" +
	"---\n\n" +
	"# %s — Playbook overrides\n\n" +
	"This file overrides defaults from the global arcmux playbook. Leave empty to\n" +
	"inherit defaults. Add only rules that genuinely differ for this project.\n\n" +
	"## Team formation\n\n" +
	"(uses defaults: reactive-only spawn)\n\n" +
	"## HC\n\n" +
	"(uses defaults: Validator at HC ≥ 2, max 4 ICs, shrink at 50%% utilization)\n\n" +
	"## Review cadence\n\n" +
	"(uses defaults: Elon 15 min, Manager 10 min)\n"

// Project scaffolds the durable + ephemeral layout. Existing files are not
// overwritten; the function is idempotent and safe to call repeatedly.
//
// vault is the absolute path to the user's $OBS_AGENTS root.
func Project(p paths.Project, vault, mission string) error {
	if p.Project == "" {
		return fmt.Errorf("paths.Project not populated")
	}

	dirs := []string{
		p.EphemeralRoot, p.Scratchpads, p.ConsultInbox, p.Heartbeats,
		p.VaultRoot, p.ArcmuxDir, p.PrinciplesDir, p.DeliverDir,
		p.ElonDir, p.TeamsDir, p.RetrosDir,
		paths.GlobalRolesDir(vault),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	now := nowISO()
	writes := []struct {
		path string
		body string
	}{
		{filepath.Join(p.ArcmuxDir, "README.md"), fmt.Sprintf(readmeTemplate, p.Project, p.Project, p.Project)},
		{filepath.Join(p.ArcmuxDir, "mission.md"), fmt.Sprintf(missionTemplate, p.Project, now, mission)},
		{filepath.Join(p.ArcmuxDir, "playbook.md"), fmt.Sprintf(playbookTemplate, p.Project, p.Project)},
	}
	for _, w := range writes {
		if err := writeIfMissing(w.path, w.body); err != nil {
			return err
		}
	}

	rolesDir := paths.GlobalRolesDir(vault)
	for _, name := range roles.List() {
		body, ok := roles.Get(name)
		if !ok {
			continue
		}
		if err := writeIfMissing(filepath.Join(rolesDir, name+".md"), body); err != nil {
			return err
		}
	}

	return nil
}

func writeIfMissing(path, body string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func nowISO() string {
	return timeNowFn().UTC().Format("2006-01-02")
}
