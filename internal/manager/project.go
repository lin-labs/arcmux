// Package manager is arcmux's three-tier orchestration runtime. It boots a
// per-project Elon pane in cmux, scaffolds durable + ephemeral storage, and
// owns the lifecycle of teams, contracts, and notifications.
//
// This file is the top-level Project struct; sub-packages own the substrate
// primitives (store, cmuxcli, scaffold, paths, roles).
package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// Options configure Start.
type Options struct {
	Agent     string // "claude" | "codex"
	Project   string // slug
	Mission   string // free-text mission statement (initial)
	DataRoot  string // typically ~/data
	VaultRoot string // typically $OBS_AGENTS
	Cmux      *cmuxcli.Client
}

// Project is a running manager-mode project.
type Project struct {
	Opts      Options
	Paths     paths.Project
	DB        *store.DB
	Workspace cmuxcli.Workspace
	ElonPane  cmuxcli.Pane
}

// Start scaffolds, opens the store, creates the cmux workspace with Elon as
// its initial command, and locates the Elon pane. It does not run any agent
// loop yet; that is Plan 2 territory.
func Start(ctx context.Context, o Options) (*Project, error) {
	slug, err := paths.Validate(o.Project)
	if err != nil {
		return nil, err
	}
	if o.Agent != "claude" && o.Agent != "codex" {
		return nil, fmt.Errorf("unsupported agent %q (want claude or codex)", o.Agent)
	}
	if o.DataRoot == "" {
		o.DataRoot = filepath.Join(os.Getenv("HOME"), "data")
	}
	if o.VaultRoot == "" {
		return nil, fmt.Errorf("VaultRoot required (set OBS_AGENTS)")
	}
	if o.Cmux == nil {
		o.Cmux = cmuxcli.New()
	}

	p := &Project{Opts: o, Paths: paths.ForProject(o.DataRoot, o.VaultRoot, slug)}

	// 1. Scaffold durable + ephemeral dirs + role seeds.
	if err := scaffold.Project(p.Paths, o.VaultRoot, o.Mission); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}

	// 2. Open bbolt store.
	db, err := store.Open(p.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	p.DB = db

	// 3. Create cmux workspace with the elon agent as its initial command.
	wsName := "elon: " + slug
	ws, err := o.Cmux.NewWorkspace(ctx, cmuxcli.NewWorkspaceOptions{
		Name:        wsName,
		Description: "arcmux manager mode — Elon front desk for project " + slug,
		CWD:         p.Paths.VaultRoot,
		Command:     o.Agent,
		Focus:       true,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cmux new-workspace: %w", err)
	}
	p.Workspace = ws

	// 4. Locate the initial pane (the one cmux created with the workspace).
	panes, err := o.Cmux.ListPanes(ctx, ws.Ref)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cmux list-panes: %w", err)
	}
	if len(panes) == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("workspace %s has no panes after creation", ws.Ref)
	}
	p.ElonPane = panes[0]

	// 5. Audit the start.
	_ = db.AppendAudit(store.AuditEntry{
		Action:  "manager-mode-started",
		Actor:   "arcmux",
		Subject: slug,
		Detail: map[string]any{
			"agent":         o.Agent,
			"workspace_ref": ws.Ref,
			"pane_ref":      p.ElonPane.Ref,
		},
	})

	return p, nil
}

// Close releases the project's resources.
func (p *Project) Close() error {
	if p.DB != nil {
		return p.DB.Close()
	}
	return nil
}
