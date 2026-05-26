// Package manager is arcmux's per-project substrate runtime. It boots a
// per-project front-desk pane in cmux, scaffolds the ephemeral storage
// layout, and seeds the substrate primitives (inbox, scratchpad, audit)
// that downstream callers (elonco, etc.) drive.
//
// arcmux is prompt-agnostic post-C2: the caller supplies the exact launch
// command for the agent. arcmux does not know what role, system prompt, or
// identity the agent runs with — those are caller concerns.
package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/bootstrap"
	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/scratchpad"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// Options configure Start.
type Options struct {
	// Agent is an informational tag (e.g. "claude", "codex", "shell") for
	// the substrate's records and the bootstrap script's ARCMUX_AGENT
	// export. It does NOT determine the launch command — that is Command.
	Agent string
	// Project is the project slug.
	Project string
	// Mission is the free-text initial mission. When non-empty it is
	// pushed as the first inbox message (verb=add, from=user) so the
	// agent's first activation finds work in the same primitive as every
	// subsequent order.
	Mission string
	// Command is the exact shell command to `exec` after env exports. The
	// caller (elonco) builds this — e.g.
	//   `claude --dangerously-skip-permissions --append-system-prompt-file /path/to/elon.md`
	// If empty, Start defaults to running the bare Agent name so a manual
	// dispatch still produces a working pane (without any prompt priming).
	Command string
	// DataRoot is typically ~/data.
	DataRoot string
	// VaultRoot is typically $OBS_AGENTS.
	VaultRoot string
	// Cmux is the cmux client; one is created lazily when nil.
	Cmux *cmuxcli.Client
	// Focus focuses the new workspace after creation.
	Focus bool
}

// Project is a running per-project substrate instance.
type Project struct {
	Opts           Options
	Paths          paths.Project
	DB             *store.DB
	Workspace      cmuxcli.Workspace
	ElonPane       cmuxcli.Pane
	BootstrapPath  string
	ScratchpadPath string // path to the seeded front-desk scratchpad
	MissionInboxID string // empty when no mission was supplied
}

// Start scaffolds the ephemeral layout, opens the substrate store, seeds the
// front-desk inbox + scratchpad, creates the cmux workspace with the
// generated bootstrap script as its initial command, and locates the
// front-desk pane. arcmux does not prime an identity — the bootstrap
// script execs the caller-supplied Command verbatim after exporting the
// ARCMUX_* env.
func Start(ctx context.Context, o Options) (*Project, error) {
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
	if o.Cmux == nil {
		o.Cmux = cmuxcli.New()
	}
	command := strings.TrimSpace(o.Command)
	if command == "" {
		// Fallback: bare agent name. The caller didn't supply a launch
		// command, so the pane just runs the agent without prompt priming.
		command = o.Agent
	}

	p := &Project{Opts: o, Paths: paths.ForProject(o.DataRoot, o.VaultRoot, slug)}

	// 1. Scaffold ephemeral dirs. Vault-side scaffolding is the caller's
	// responsibility (elonco mkdirs Projects/<slug>/... as needed).
	if err := scaffold.Project(p.Paths); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}

	// 2. Open bbolt store.
	db, err := store.Open(p.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	p.DB = db

	// 3. Seed the front-desk inbox. Mission (when non-empty) lands as an
	// inbox message rather than ambient context so the first activation
	// finds its work in the same primitive that all subsequent orders use.
	startedAt := time.Now()
	mission := strings.TrimSpace(o.Mission)
	if mission != "" {
		msgID, err := store.NewInboxID()
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("inbox id: %w", err)
		}
		if err := db.PushElonInbox(store.InboxMsg{
			ID:         msgID,
			Verb:       "add",
			From:       "user",
			Body:       o.Mission,
			ReceivedAt: startedAt,
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("seed mission inbox: %w", err)
		}
		p.MissionInboxID = msgID
	}

	// 4. Seed the front-desk scratchpad. Written even when mission is empty
	// so a respawn always has a non-zero "as_of" state to read.
	scratchPath, err := scratchpad.Path(o.DataRoot, slug, "elon")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("scratchpad path: %w", err)
	}
	pad := initialFrontDeskScratchpad(slug, o, startedAt, p.MissionInboxID)
	padBody, err := json.MarshalIndent(pad, "", "  ")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("marshal scratchpad: %w", err)
	}
	if err := scratchpad.Write(scratchPath, padBody); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed scratchpad: %w", err)
	}
	p.ScratchpadPath = scratchPath

	// 5. Render the per-launch bootstrap script. Prompt-agnostic: arcmux
	// only exports env + exec's whatever Command the caller supplied.
	bootstrapPath, err := bootstrap.Render(bootstrap.Options{
		Agent:     o.Agent,
		Project:   slug,
		EphemRoot: p.Paths.EphemeralRoot,
		VaultRoot: o.VaultRoot,
		DataRoot:  o.DataRoot,
		Command:   command,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("bootstrap render: %w", err)
	}
	p.BootstrapPath = bootstrapPath

	// 6. Create cmux workspace with the bootstrap script as initial command.
	wsName := slug
	ws, err := o.Cmux.NewWorkspace(ctx, cmuxcli.NewWorkspaceOptions{
		Name:        wsName,
		Description: "arcmux substrate — front-desk pane for project " + slug,
		CWD:         p.Paths.VaultRoot,
		Command:     bootstrapPath,
		Focus:       o.Focus,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cmux new-workspace: %w", err)
	}
	p.Workspace = ws

	// 7. Locate the initial pane.
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

	// 8. Persist project meta so post-launch substrate (pulse, future
	// heartbeats) can locate the front-desk without grepping the audit log.
	if err := db.PutProjectMeta(store.ProjectMeta{
		ElonPaneRef:      p.ElonPane.Ref,
		ElonSurfaceRef:   p.ElonPane.SelectedSurf,
		ElonWorkspaceRef: ws.Ref,
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("persist project meta: %w", err)
	}

	// 9. Audit the start. Direct AppendAudit (not arcmux-cli subprocess)
	// because the launcher already holds the bbolt write lock — shelling
	// out would block on bbolt's process-wide lock.
	_ = db.AppendAudit(store.AuditEntry{
		Action:    "manager-mode-started",
		Actor:     "arcmux",
		Subject:   slug,
		Timestamp: startedAt,
		Detail: map[string]any{
			"agent":            o.Agent,
			"workspace_ref":    ws.Ref,
			"pane_ref":         p.ElonPane.Ref,
			"bootstrap_path":   bootstrapPath,
			"command":          command,
			"scratchpad_path":  scratchPath,
			"mission_seeded":   mission != "",
			"mission_inbox_id": p.MissionInboxID,
			"mission_bytes":    len(o.Mission),
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

// initialFrontDeskScratchpad returns the JSON blob written to
// scratchpads/elon.json on launch. The shape is intentionally generic —
// arcmux doesn't prescribe an Elon-flavored field set anymore. Callers
// (elonco) can overwrite the file with their own richer shape on the next
// turn.
func initialFrontDeskScratchpad(slug string, o Options, startedAt time.Time, missionInboxID string) map[string]any {
	mission := strings.TrimSpace(o.Mission)
	focus := "Fresh substrate launch — peek inbox (mission delivered as 'add' message), then act."
	next := []string{
		"`arcmux-cli inbox peek` to consume the mission order",
	}
	if mission == "" {
		focus = "(no mission supplied) — awaiting first inbox push from user."
		next = []string{
			"Wait for `arcmux-cli inbox push --verb add --from user` to arrive",
		}
	}

	sum := sha256.Sum256([]byte(o.Mission))
	return map[string]any{
		"as_of":         startedAt.Format(time.RFC3339Nano),
		"turn":          0,
		"active_goals":  []string{},
		"current_focus": focus,
		"key_decisions": map[string]any{},
		"open_consults": []string{},
		"next_steps":    next,
		"deferred":      []string{},
		"bootstrap": map[string]any{
			"project":          slug,
			"role":             "elon",
			"agent":            o.Agent,
			"vault_root":       o.VaultRoot,
			"data_root":        o.DataRoot,
			"ephemeral_root":   filepath.Join(o.DataRoot, "arcmux", slug),
			"started_at":       startedAt.Format(time.RFC3339Nano),
			"mission_seeded":   mission != "",
			"mission_inbox_id": missionInboxID,
			"mission_bytes":    len(o.Mission),
			"mission_sha256":   hex.EncodeToString(sum[:]),
		},
	}
}
