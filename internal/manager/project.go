// Package manager is arcmux's three-tier orchestration runtime. It boots a
// per-project Elon pane in cmux, scaffolds durable + ephemeral storage, and
// owns the lifecycle of teams, contracts, and notifications.
//
// This file is the top-level Project struct; sub-packages own the substrate
// primitives (store, cmuxcli, scaffold, paths, roles, bootstrap, scratchpad).
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
	Agent        string         // "claude" | "codex"
	Project      string         // slug
	Mission      string         // free-text mission statement (initial)
	DataRoot     string         // typically ~/data
	VaultRoot    string         // typically $OBS_AGENTS
	Cmux         *cmuxcli.Client
	Focus        bool           // focus the new workspace after creation
	ScaffoldOpts []scaffold.Opt // optional flags forwarded to scaffold.Project
}

// Project is a running manager-mode project.
type Project struct {
	Opts           Options
	Paths          paths.Project
	DB             *store.DB
	Workspace      cmuxcli.Workspace
	ElonPane       cmuxcli.Pane
	BootstrapPath  string
	ScratchpadPath string // path to the seeded Elon scratchpad
	MissionInboxID string // empty when no mission was supplied
}

// Start scaffolds, opens the store, seeds Elon's runtime substrate (initial
// scratchpad + mission inbox push), creates the cmux workspace with the
// generated bootstrap script as its initial command, and locates the Elon
// pane. The bootstrap script primes the agent's identity (role file via
// --append-system-prompt-file), exports ARCMUX_* env vars, and exec's the
// agent. After Start returns, the Elon pane is a fully-primed Elon agent
// whose first activation will find a populated inbox and scratchpad — no
// mission is delivered as ambient context.
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
	if err := scaffold.Project(p.Paths, o.VaultRoot, o.Mission, o.ScaffoldOpts...); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}

	// 2. Open bbolt store.
	db, err := store.Open(p.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	p.DB = db

	// 3. Seed Elon's runtime substrate. Mission (when non-empty) lands as an
	// inbox message rather than ambient context so Elon's first activation
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

	// 4. Seed Elon's initial scratchpad. Written even when mission is empty
	// so a respawned Elon always has a non-zero "as_of" state to read.
	scratchPath, err := scratchpad.Path(o.DataRoot, slug, "elon")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("scratchpad path: %w", err)
	}
	pad := initialElonScratchpad(slug, o, startedAt, p.MissionInboxID)
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

	// 5. Render the per-launch bootstrap script that cmux will run.
	roleFile := filepath.Join(paths.GlobalRolesDir(o.VaultRoot), "elon.md")
	bootstrapPath, err := bootstrap.Render(bootstrap.Options{
		Agent:     o.Agent,
		Project:   slug,
		Role:      "elon",
		EphemRoot: p.Paths.EphemeralRoot,
		VaultRoot: o.VaultRoot,
		DataRoot:  o.DataRoot,
		RoleFile:  roleFile,
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("bootstrap render: %w", err)
	}
	p.BootstrapPath = bootstrapPath

	// 6. Create cmux workspace with the bootstrap script as initial command.
	wsName := "elon: " + slug
	ws, err := o.Cmux.NewWorkspace(ctx, cmuxcli.NewWorkspaceOptions{
		Name:        wsName,
		Description: "arcmux manager mode — Elon front desk for project " + slug,
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

	// 8. Audit the start. Direct AppendAudit (not arcmux-call subprocess)
	// because the launcher already holds the bbolt write lock — shelling out
	// would block on bbolt's process-wide lock. Out-of-process callers
	// (spawned panes) use arcmux-call audit instead.
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
			"role_file":        roleFile,
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

// initialElonScratchpad returns the JSON blob written to
// scratchpads/elon.json on launch. The shape mirrors the live scratchpad
// Elon maintains across turns so a respawn reads a familiar structure with
// turn=0 and an empty decisions/goals set.
func initialElonScratchpad(slug string, o Options, startedAt time.Time, missionInboxID string) map[string]any {
	mission := strings.TrimSpace(o.Mission)
	focus := "Fresh manager-mode launch — peek inbox (mission delivered as 'add' message), then act."
	next := []string{
		"Read $ARCMUX_VAULT/Projects/" + slug + "/arcmux/mission.md",
		"`arcmux-call inbox peek` to consume the mission order",
		"Append turn-1 entry to elon/journal.md before any spawn",
	}
	if mission == "" {
		focus = "(no mission supplied) — awaiting first inbox push from user."
		next = []string{
			"Wait for `arcmux-call inbox push --verb add --from user` to arrive",
			"Until then, no spawn decisions to make",
		}
	}

	sum := sha256.Sum256([]byte(o.Mission))
	return map[string]any{
		"as_of":          startedAt.Format(time.RFC3339Nano),
		"turn":           0,
		"active_goals":   []string{},
		"current_focus":  focus,
		"key_decisions":  map[string]any{},
		"open_consults":  []string{},
		"next_steps":     next,
		"deferred":       []string{},
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
