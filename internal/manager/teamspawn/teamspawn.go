// Package teamspawn implements the reactive team-spawn primitive. Given an
// open project store + cmux client, Spawn seeds a new team end-to-end:
// validates the slug, writes a charter to the vault, seeds the manager's
// scratchpad, renders the manager bootstrap script (carrying ARCMUX_TEAM),
// creates a cmux workspace named "team: <slug>", locates the manager pane,
// persists the team record (state=active, HC=0), and appends an audit row.
//
// The launcher's in-process Start path does NOT call Spawn — team spawn is
// reactive and out-of-process. cmd/arcmux-call/team.go is the canonical
// caller; Elon's pane invokes it when a routed order warrants a new team.
package teamspawn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/bootstrap"
	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scratchpad"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// ErrTeamExists is returned when a team with the requested slug already
// exists in a non-archived state. Callers must dissolve/archive the prior
// team before respawning under the same slug.
var ErrTeamExists = errors.New("team already exists")

// Opts configure Spawn.
type Opts struct {
	DB        *store.DB       // open project store; caller owns Close
	Cmux      *cmuxcli.Client // cmux client (real or fakeRunner-backed)
	Project   string          // project slug
	Slug      string          // team slug
	Vision    string          // free-text mission for the team
	Agent     string          // "claude" | "codex"
	VaultRoot string          // $OBS_AGENTS
	DataRoot  string          // ~/data
	Focus     bool            // focus the new cmux workspace
}

// Result returns the artifacts created by Spawn.
type Result struct {
	Team           store.Team
	Workspace      cmuxcli.Workspace
	ManagerPane    cmuxcli.Pane
	BootstrapPath  string
	ScratchpadPath string
	CharterPath    string
	// VisionInboxID is the manager-inbox message ID for the seeded vision.
	// Empty when Vision was empty/whitespace-only.
	VisionInboxID string
}

// Spawn creates a new team. See package doc for the full sequence.
func Spawn(ctx context.Context, o Opts) (*Result, error) {
	if o.DB == nil {
		return nil, fmt.Errorf("Spawn: DB required")
	}
	if o.Cmux == nil {
		return nil, fmt.Errorf("Spawn: Cmux required")
	}
	if o.Agent != "claude" && o.Agent != "codex" {
		return nil, fmt.Errorf("unsupported agent %q (want claude or codex)", o.Agent)
	}
	if _, err := paths.Validate(o.Project); err != nil {
		return nil, fmt.Errorf("project: %w", err)
	}
	slug, err := paths.Validate(o.Slug)
	if err != nil {
		return nil, fmt.Errorf("slug: %w", err)
	}
	if o.VaultRoot == "" {
		return nil, fmt.Errorf("Spawn: VaultRoot required")
	}
	if o.DataRoot == "" {
		return nil, fmt.Errorf("Spawn: DataRoot required")
	}

	// Duplicate check. Active/paused/dissolving teams block respawn; an
	// archived tombstone is allowed to be overwritten so callers can
	// re-spawn under a familiar slug after a clean dissolution.
	if existing, err := o.DB.GetTeam(slug); err == nil {
		if existing.State != store.TeamArchived {
			return nil, fmt.Errorf("%w: team %q is %s (workspace=%s)",
				ErrTeamExists, slug, existing.State, existing.WorkspaceRef)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("check existing team: %w", err)
	}

	pp := paths.ForProject(o.DataRoot, o.VaultRoot, o.Project)
	startedAt := time.Now()
	vision := strings.TrimSpace(o.Vision)

	// Generate the vision inbox ID up front so the scratchpad bootstrap
	// fields and the actual inbox push agree on a single value. Empty when
	// no vision was supplied.
	var visionInboxID string
	if vision != "" {
		id, err := store.NewInboxID()
		if err != nil {
			return nil, fmt.Errorf("generate vision inbox id: %w", err)
		}
		visionInboxID = id
	}

	// 1. Seed the manager scratchpad. role = "manager-<slug>" so multiple
	// managers within one project keep separate files.
	role := "manager-" + slug
	spPath, err := scratchpad.Path(o.DataRoot, o.Project, role)
	if err != nil {
		return nil, fmt.Errorf("scratchpad path: %w", err)
	}
	pad := initialManagerScratchpad(o.Project, slug, o, startedAt, visionInboxID)
	padBody, err := json.MarshalIndent(pad, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal scratchpad: %w", err)
	}
	if err := scratchpad.Write(spPath, padBody); err != nil {
		return nil, fmt.Errorf("seed scratchpad: %w", err)
	}

	// 2. Materialize teams/<slug>/charter.md. The manager pane reads this
	// on bootstrap (see roles/files/manager.md §Bootstrap protocol).
	teamDir := filepath.Join(pp.TeamsDir, slug)
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure team dir: %w", err)
	}
	charterPath := filepath.Join(teamDir, "charter.md")
	charterBody := renderCharter(o.Project, slug, vision, startedAt)
	if err := os.WriteFile(charterPath, []byte(charterBody), 0o644); err != nil {
		return nil, fmt.Errorf("write charter: %w", err)
	}

	// 3. Render the manager bootstrap script. ScriptName disambiguates
	// per-team scripts inside one project's ephemeral dir; ARCMUX_TEAM is
	// exported so the manager pane knows its identity from $env alone.
	roleFile := filepath.Join(paths.GlobalRolesDir(o.VaultRoot), "manager.md")
	bootstrapPath, err := bootstrap.Render(bootstrap.Options{
		Agent:      o.Agent,
		Project:    o.Project,
		Role:       "manager",
		Team:       slug,
		ScriptName: fmt.Sprintf("bootstrap-manager-%s.sh", slug),
		EphemRoot:  pp.EphemeralRoot,
		VaultRoot:  o.VaultRoot,
		DataRoot:   o.DataRoot,
		RoleFile:   roleFile,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap render: %w", err)
	}

	// 4. Create the cmux workspace. Name convention "team: <slug>" mirrors
	// the manager-launch "elon: <project>" convention so cmux users can
	// scan workspaces by purpose.
	wsName := "team: " + slug
	ws, err := o.Cmux.NewWorkspace(ctx, cmuxcli.NewWorkspaceOptions{
		Name:        wsName,
		Description: "arcmux manager pane for team " + slug + " (project " + o.Project + ")",
		CWD:         pp.VaultRoot,
		Command:     bootstrapPath,
		Focus:       o.Focus,
	})
	if err != nil {
		return nil, fmt.Errorf("cmux new-workspace: %w", err)
	}

	panes, err := o.Cmux.ListPanes(ctx, ws.Ref)
	if err != nil {
		return nil, fmt.Errorf("cmux list-panes: %w", err)
	}
	if len(panes) == 0 {
		return nil, fmt.Errorf("workspace %s has no panes after creation", ws.Ref)
	}
	managerPane := panes[0]

	// 5. Persist the team record. HC=0 because no ICs spawn here — a
	// manager has to add ICs via subsequent calls (Plan 5+).
	team := store.Team{
		ID:           slug,
		Vision:       o.Vision,
		State:        store.TeamActive,
		HC:           0,
		TargetHC:     0,
		WorkspaceRef: ws.Ref,
		ManagerPane:  managerPane.Ref,
		CreatedAt:    startedAt,
		UpdatedAt:    startedAt,
	}
	if err := o.DB.PutTeam(team); err != nil {
		return nil, fmt.Errorf("put team: %w", err)
	}

	// 6. Create the per-team manager inbox bucket. This is the recurring
	// channel by which Elon (or any out-of-process caller) dispatches new
	// orders to the manager after spawn — charter is one-shot, inbox is
	// recurring. Mirrors how BucketInboxElon underpins user→Elon orders.
	if err := o.DB.EnsureManagerInbox(slug); err != nil {
		return nil, fmt.Errorf("ensure manager inbox: %w", err)
	}

	// 7. If a vision was supplied, push it as the first inbox message so the
	// manager's bootstrap protocol can consume it through the same primitive
	// as every subsequent order. Verb "add" mirrors mission delivery to
	// Elon (see project.go Step 3).
	if vision != "" {
		if err := o.DB.PushManagerInbox(slug, store.InboxMsg{
			ID:         visionInboxID,
			Verb:       "add",
			From:       "elon",
			Priority:   0,
			Body:       o.Vision,
			ReceivedAt: startedAt,
		}); err != nil {
			return nil, fmt.Errorf("push vision: %w", err)
		}
	}

	// 8. Audit. Direct AppendAudit because this caller already holds the
	// bbolt write lock for the dispatch.
	_ = o.DB.AppendAudit(store.AuditEntry{
		Timestamp: startedAt,
		Action:    "team-spawned",
		Actor:     "arcmux",
		Subject:   slug,
		Detail: map[string]any{
			"agent":           o.Agent,
			"workspace_ref":   ws.Ref,
			"manager_pane":    managerPane.Ref,
			"bootstrap_path":  bootstrapPath,
			"scratchpad_path": spPath,
			"charter_path":    charterPath,
			"vision_bytes":    len(o.Vision),
			"vision_seeded":   vision != "",
			"vision_inbox_id": visionInboxID,
		},
	})

	return &Result{
		Team:           team,
		Workspace:      ws,
		ManagerPane:    managerPane,
		BootstrapPath:  bootstrapPath,
		ScratchpadPath: spPath,
		CharterPath:    charterPath,
		VisionInboxID:  visionInboxID,
	}, nil
}

func renderCharter(project, slug, vision string, startedAt time.Time) string {
	body := vision
	if body == "" {
		body = "(vision not supplied at spawn — manager should solicit clarification before dispatching ICs)"
	}
	return fmt.Sprintf(`---
project: %s
team: %s
created: %s
state: active
---

# Team %s — Charter

## Vision

%s

## Headcount

Manager only at spawn (HC=0). ICs join via arcmux-call (Plan 5+).

## Active contracts

(none yet)

## Journal

See teams/%s/journal.md (append-only, created on first manager activation).
`, project, slug, startedAt.UTC().Format("2006-01-02"), slug, body, slug)
}

func initialManagerScratchpad(project, slug string, o Opts, startedAt time.Time, visionInboxID string) map[string]any {
	vision := strings.TrimSpace(o.Vision)
	focus := "Fresh team spawn — read charter + inbox, write turn-0 journal, decompose vision into IC contracts."
	next := []string{
		"arcmux-call inbox peek --to manager:" + slug + " --n 5 (read seeded vision)",
		"Read $ARCMUX_VAULT/Projects/" + project + "/teams/" + slug + "/charter.md",
		"Append turn-0 entry to teams/" + slug + "/journal.md",
		"Decompose vision into IC contracts (CLI surface lands in Plan 5+)",
	}
	if vision == "" {
		focus = "(no vision supplied) — solicit clarification from Elon before dispatching ICs."
		next = []string{
			"Re-read charter for any updates",
			"arcmux-call inbox peek --to manager:" + slug + " --n 5 (poll for Elon clarification)",
			"Until Elon clarifies, no spawn decisions to make",
		}
	}

	sum := sha256.Sum256([]byte(o.Vision))
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
			"project":         project,
			"team":            slug,
			"role":            "manager",
			"agent":           o.Agent,
			"vault_root":      o.VaultRoot,
			"data_root":       o.DataRoot,
			"ephemeral_root":  filepath.Join(o.DataRoot, "arcmux", project),
			"started_at":      startedAt.Format(time.RFC3339Nano),
			"vision_seeded":   vision != "",
			"vision_bytes":    len(o.Vision),
			"vision_sha256":   hex.EncodeToString(sum[:]),
			"vision_inbox_id": visionInboxID,
		},
	}
}
