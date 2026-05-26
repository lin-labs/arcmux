// Package manager is arcmux's per-project substrate runtime. It boots a
// per-project pane in cmux, scaffolds the ephemeral storage layout, opens
// the bbolt store, persists the project's pane location into ProjectMeta,
// and returns the resulting Registration to its caller.
//
// arcmux is pure substrate post-C3/C4: it does not know what role, system
// prompt, or agent identity the pane runs with — those are the caller's
// concern (typically elonco's launcher). It does not seed inbox messages,
// scratchpads, or any agent-shaped state. The caller drives that via the
// daemon's gRPC API (Send / Ready / etc.) or directly against the store.
package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/bootstrap"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/mux"
)

// Options configure RegisterSession.
type Options struct {
	// Agent is an informational tag (e.g. "claude", "codex", "shell") for
	// the substrate's records and the bootstrap script's ARCMUX_AGENT
	// export. It does NOT determine the launch command — that is Command.
	Agent string
	// Project is the project slug.
	Project string
	// Command is the exact shell command to `exec` after env exports. The
	// caller (elonco) builds this — e.g.
	//   `claude --dangerously-skip-permissions --append-system-prompt-file /path/to/role.md`
	// If empty, RegisterSession defaults to running the bare Agent name so
	// a manual dispatch still produces a working pane (without any prompt
	// priming).
	Command string
	// DataRoot is typically ~/data.
	DataRoot string
	// VaultRoot is typically $OBS_AGENTS. arcmux does not write here —
	// callers may resolve their own vault-side artifacts off this root.
	VaultRoot string
	// Mux is the configured multiplexer backend. Required.
	Mux mux.Backend
	// Focus focuses the new group after creation.
	Focus bool
}

// Registration is the durable handle returned from a successful
// RegisterSession call. It bundles the resolved paths, the open store
// handle, the spawned cmux group + pane, and the rendered bootstrap
// script path. Callers Close() it to release the bbolt handle.
//
// Pre-C4 this type was called `Project` and carried Elon-specific fields
// like a seeded ScratchpadPath and MissionInboxID. After demolition the
// type is agent-class-agnostic: arcmux records which pane exists, not
// what role it plays.
type Registration struct {
	Opts          Options
	Paths         paths.Project
	DB            *store.DB
	Group         mux.Group
	Pane          mux.Pane
	BootstrapPath string
}

// RegisterSession scaffolds the ephemeral layout, opens the substrate
// store, creates the cmux workspace with the generated bootstrap script
// as its initial command, locates the freshly-spawned pane, and persists
// ProjectMeta so post-launch substrate (pulse, future heartbeats) can
// reach the pane without grepping the audit log. arcmux does not prime
// an agent identity — the bootstrap script execs the caller-supplied
// Command verbatim after exporting the ARCMUX_* env.
//
// Pre-C4 this function was named `Start` and additionally seeded an
// "elon" scratchpad + an Elon-inbox mission message. Those concerns
// moved to elonco; arcmux is now a pure registrar.
func RegisterSession(ctx context.Context, o Options) (*Registration, error) {
	slug, err := paths.Validate(o.Project)
	if err != nil {
		return nil, err
	}
	if o.Agent == "" {
		return nil, fmt.Errorf("Agent required (informational tag)")
	}
	if o.DataRoot == "" {
		o.DataRoot = filepath.Join(os.Getenv("HOME"), "data")
	}
	if o.VaultRoot == "" {
		return nil, fmt.Errorf("VaultRoot required (set OBS_AGENTS)")
	}
	if o.Mux == nil {
		return nil, fmt.Errorf("Mux required")
	}
	command := strings.TrimSpace(o.Command)
	if command == "" {
		// Fallback: bare agent name. The caller didn't supply a launch
		// command, so the pane just runs the agent without prompt priming.
		command = o.Agent
	}

	r := &Registration{Opts: o, Paths: paths.ForProject(o.DataRoot, o.VaultRoot, slug)}

	// 1. Scaffold ephemeral dirs. Vault-side scaffolding is the caller's
	// responsibility (elonco mkdirs Projects/<slug>/... as needed).
	if err := scaffold.Project(r.Paths); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}

	// 2. Open bbolt store.
	db, err := store.Open(r.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	r.DB = db

	startedAt := time.Now()

	// 3. Render the per-launch bootstrap script. Prompt-agnostic: arcmux
	// only exports env + exec's whatever Command the caller supplied.
	bootstrapPath, err := bootstrap.Render(bootstrap.Options{
		Agent:     o.Agent,
		Project:   slug,
		EphemRoot: r.Paths.EphemeralRoot,
		VaultRoot: o.VaultRoot,
		DataRoot:  o.DataRoot,
		Command:   command,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("bootstrap render: %w", err)
	}
	r.BootstrapPath = bootstrapPath

	// 4. Create mux group with the bootstrap script as initial command.
	wsName := slug
	group, err := o.Mux.NewGroup(ctx, mux.GroupOptions{
		Name:        wsName,
		Description: "arcmux substrate — registered session for project " + slug,
		CWD:         r.Paths.VaultRoot,
		Command:     bootstrapPath,
		Focus:       o.Focus,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mux new-group: %w", err)
	}
	r.Group = group

	// 5. Locate the initial pane.
	panes, err := o.Mux.ListPanes(ctx, group.Ref)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mux list-panes: %w", err)
	}
	if len(panes) == 0 {
		_ = db.Close()
		return nil, fmt.Errorf("group %s has no panes after creation", group.Ref)
	}
	r.Pane = panes[0]

	// 6. Persist project meta so post-launch substrate (pulse, future
	// heartbeats) can locate the pane without grepping the audit log.
	// SurfaceRef is left empty under the mux abstraction; the pane ref is
	// the canonical send target on both cmux (resolves to focused surface
	// internally) and tmux.
	if err := db.PutProjectMeta(store.ProjectMeta{
		PaneRef:      r.Pane.Ref,
		WorkspaceRef: group.Ref,
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("persist project meta: %w", err)
	}

	// 7. Audit the registration. Direct AppendAudit (not arcmux-cli
	// subprocess) because the registrar already holds the bbolt write
	// lock — shelling out would block on bbolt's process-wide lock.
	_ = db.AppendAudit(store.AuditEntry{
		Action:    "session-registered",
		Actor:     "arcmux",
		Subject:   slug,
		Timestamp: startedAt,
		Detail: map[string]any{
			"agent":          o.Agent,
			"workspace_ref":  group.Ref,
			"pane_ref":       r.Pane.Ref,
			"bootstrap_path": bootstrapPath,
			"command":        command,
		},
	})

	return r, nil
}

// Close releases the registration's resources.
func (r *Registration) Close() error {
	if r.DB != nil {
		return r.DB.Close()
	}
	return nil
}
