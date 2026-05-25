// Package icspawn implements the reactive IC-slot-spawn primitive. Given an
// open project store + cmux client + an existing team and contract, Spawn
// seeds a new IC end-to-end: validates inputs, looks up the team and
// contract (rejecting cross-team / terminal-state / archived-team binds),
// enforces the HC cap, resolves the IC's role file, seeds the per-IC
// scratchpad, renders an IC bootstrap script (carrying ARCMUX_TEAM,
// ARCMUX_CONTRACT, and a slot-unique ARCMUX_ROLE), creates a cmux pane by
// splitting inside the team's workspace, sends the bootstrap command into
// that pane, persists the Slot record, bumps the team's HC, and appends an
// audit row.
//
// The launcher's in-process Start path does NOT call Spawn — IC spawn is
// reactive and out-of-process. cmd/arcmux-call/ic.go is the canonical
// caller; a team's manager (or Elon for hand-spawned diagnostics) invokes
// it when a routed contract warrants a real pane.
package icspawn

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

// ErrSlotExists is returned when a slot with the requested ID already
// exists in a non-dissolved state. Callers must dissolve the prior slot
// before respawning under the same ID.
var ErrSlotExists = errors.New("slot already exists")

// ErrHCCap is returned when the team has already reached the per-team IC
// headcount cap (store.MaxICsPerTeam). The manager must dissolve a slot
// before spawning another.
var ErrHCCap = errors.New("team at IC headcount cap")

// Opts configure Spawn.
type Opts struct {
	DB        *store.DB       // open project store; caller owns Close
	Cmux      *cmuxcli.Client // cmux client (real or fakeRunner-backed)
	Project   string          // project slug
	Team      string          // existing team slug; must be active
	Slot      string          // unique slot id within the project (slug)
	Role      string          // specialization name (ic-base | linus | ... ; default ic-base)
	Contract  string          // initial bound contract id; must belong to Team and not be terminal
	Agent     string          // "claude" | "codex"
	VaultRoot string          // $OBS_AGENTS
	DataRoot  string          // ~/data
	Focus     bool            // focus the new pane after split
}

// Result returns the artifacts created by Spawn.
type Result struct {
	Slot           store.Slot
	Pane           cmuxcli.Pane
	BootstrapPath  string
	ScratchpadPath string
	Team           store.Team     // post-spawn team record (HC incremented)
	Contract       store.Contract // contract bound at spawn time (pre-state)
}

// Spawn creates a new IC slot inside an existing team. See package doc
// for the full sequence.
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
	team, err := paths.Validate(o.Team)
	if err != nil {
		return nil, fmt.Errorf("team: %w", err)
	}
	slot, err := paths.Validate(o.Slot)
	if err != nil {
		return nil, fmt.Errorf("slot: %w", err)
	}
	role := o.Role
	if role == "" {
		role = "ic-base"
	}
	if _, err := paths.Validate(role); err != nil {
		return nil, fmt.Errorf("role: %w", err)
	}
	if o.Contract == "" {
		return nil, fmt.Errorf("Spawn: Contract required")
	}
	contractID, err := paths.Validate(o.Contract)
	if err != nil {
		return nil, fmt.Errorf("contract: %w", err)
	}
	if o.VaultRoot == "" {
		return nil, fmt.Errorf("Spawn: VaultRoot required")
	}
	if o.DataRoot == "" {
		return nil, fmt.Errorf("Spawn: DataRoot required")
	}

	// Team must exist and be active. dissolving/archived/paused teams cannot
	// take new ICs — the manager's pane may be gone or the workspace closed.
	teamRec, err := o.DB.GetTeam(team)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("team %q not found", team)
		}
		return nil, fmt.Errorf("get team: %w", err)
	}
	if teamRec.State != store.TeamActive {
		return nil, fmt.Errorf("team %q is %s (must be active to spawn IC)", team, teamRec.State)
	}
	if teamRec.WorkspaceRef == "" {
		return nil, fmt.Errorf("team %q has no workspace_ref (cannot split a pane)", team)
	}

	// Contract must exist, belong to this team, and not be terminal. We do
	// NOT auto-transition pending→ready — that is the manager's call via
	// `arcmux-call contract transition`. ic spawn is pure plumbing.
	contractRec, err := o.DB.GetContract(contractID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("contract %q not found", contractID)
		}
		return nil, fmt.Errorf("get contract: %w", err)
	}
	if contractRec.Team != team {
		return nil, fmt.Errorf("contract %q belongs to team %q, not %q",
			contractID, contractRec.Team, team)
	}
	switch contractRec.State {
	case store.ContractCompleted, store.ContractCancelled, store.ContractFailed:
		return nil, fmt.Errorf("contract %q is terminal (state=%s); cannot bind to a new IC",
			contractID, contractRec.State)
	}

	// Slot duplicate check. Dissolved tombstones are allowed to be
	// overwritten (mirrors teamspawn's archived-tombstone behavior).
	if existing, err := o.DB.GetSlot(slot); err == nil {
		if existing.State != store.SlotDissolved {
			return nil, fmt.Errorf("%w: slot %q is %s in team %q",
				ErrSlotExists, slot, existing.State, existing.Team)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("check existing slot: %w", err)
	}

	// HC cap. Counts active slots only — dissolved tombstones are out.
	activeSlots, err := o.DB.ListSlots(team, store.SlotActive)
	if err != nil {
		return nil, fmt.Errorf("list active slots: %w", err)
	}
	if len(activeSlots) >= store.MaxICsPerTeam {
		return nil, fmt.Errorf("%w: team %q has %d active ICs (max=%d)",
			ErrHCCap, team, len(activeSlots), store.MaxICsPerTeam)
	}

	// Resolve the role file. Must exist on disk so the bootstrap's
	// --append-system-prompt-file points at something real. v0 doesn't
	// compose ic-base + specialization; one role file, period.
	roleFile := filepath.Join(paths.GlobalRolesDir(o.VaultRoot), role+".md")
	if _, err := os.Stat(roleFile); err != nil {
		return nil, fmt.Errorf("role file: %w (looked at %s)", err, roleFile)
	}

	pp := paths.ForProject(o.DataRoot, o.VaultRoot, o.Project)
	startedAt := time.Now()
	// ARCMUX_ROLE doubles as the unique slot identifier on the wire: it
	// names the scratchpad file, the bootstrap script, and the audit "by:"
	// default. Format "ic-<team>-<slot>" keeps it readable and collision-
	// free across teams in one project.
	arcmuxRole := "ic-" + team + "-" + slot

	// 1. Seed the IC scratchpad with the contract preview so a respawn can
	// pick up identically even if the bbolt store is briefly unreadable.
	spPath, err := scratchpad.Path(o.DataRoot, o.Project, arcmuxRole)
	if err != nil {
		return nil, fmt.Errorf("scratchpad path: %w", err)
	}
	pad := initialICScratchpad(o.Project, team, slot, role, arcmuxRole, contractRec, startedAt)
	padBody, err := json.MarshalIndent(pad, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal scratchpad: %w", err)
	}
	if err := scratchpad.Write(spPath, padBody); err != nil {
		return nil, fmt.Errorf("seed scratchpad: %w", err)
	}

	// 2. Render the IC bootstrap script with ARCMUX_TEAM + ARCMUX_CONTRACT
	// + ARCMUX_SLOT. The slot id is the inbox addressing key (see
	// store.PushICInbox) — exporting it lets an IC peek its own queue
	// with the one-liner `arcmux-call inbox peek --to ic:$ARCMUX_SLOT`
	// without having to derive it from the composite ARCMUX_ROLE.
	bootstrapPath, err := bootstrap.Render(bootstrap.Options{
		Agent:      o.Agent,
		Project:    o.Project,
		Role:       arcmuxRole,
		Team:       team,
		Slot:       slot,
		Contract:   contractID,
		ScriptName: fmt.Sprintf("bootstrap-%s.sh", arcmuxRole),
		EphemRoot:  pp.EphemeralRoot,
		VaultRoot:  o.VaultRoot,
		DataRoot:   o.DataRoot,
		RoleFile:   roleFile,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap render: %w", err)
	}

	// 3. Split a new pane inside the team's existing workspace. Direction
	// "right" matches the convention (manager on the left, ICs to the
	// right). cmux's new-pane has no --command, so we send the bootstrap
	// path into the pane's fresh terminal as a second step.
	pane, err := o.Cmux.NewPane(ctx, cmuxcli.NewPaneOptions{
		Workspace: teamRec.WorkspaceRef,
		Direction: "right",
		Type:      "terminal",
		Focus:     o.Focus,
	})
	if err != nil {
		return nil, fmt.Errorf("cmux new-pane: %w", err)
	}

	// 4. Send the bootstrap command. Use the pane's focused surface — cmux
	// send requires --surface, and a pane ref is not accepted there.
	// NewPane populates SelectedSurf from cmux's multi-token OK output.
	sendTarget := pane.SelectedSurf
	if sendTarget == "" {
		sendTarget = pane.Ref // fallback for unit-test fakes that don't echo surface
	}
	if err := o.Cmux.Send(ctx, sendTarget, bootstrapPath); err != nil {
		return nil, fmt.Errorf("send bootstrap to pane: %w", err)
	}

	// 5. Persist the Slot record. PutSlot stamps timestamps + defaults
	// state to active.
	s := store.Slot{
		ID:             slot,
		Team:           team,
		Role:           role,
		Contract:       contractID,
		PaneRef:        pane.Ref,
		WorkspaceRef:   teamRec.WorkspaceRef,
		ScratchpadPath: spPath,
		BootstrapPath:  bootstrapPath,
		Agent:          o.Agent,
		State:          store.SlotActive,
		CreatedAt:      startedAt,
		UpdatedAt:      startedAt,
	}
	if err := o.DB.PutSlot(s); err != nil {
		return nil, fmt.Errorf("put slot: %w", err)
	}

	// 5b. Ensure the per-IC inbox sub-bucket. Mirrors teamspawn's
	// EnsureManagerInbox: the queue is ready before the IC's first poll
	// and before any manager push (cross-thread spawn/push races would
	// otherwise hit ErrICInboxMissing). Idempotent on respawn over a
	// dissolved tombstone.
	if err := o.DB.EnsureICInbox(slot); err != nil {
		return nil, fmt.Errorf("ensure ic inbox: %w", err)
	}

	// 6. Bump team HC. The active-slot count we just took is authoritative
	// for v0; concurrent ic-spawn on the same team is not yet a real risk
	// (Elon dispatches sequentially), but a future plan should move HC
	// accounting fully inside a single bbolt txn.
	teamRec.HC = len(activeSlots) + 1
	if err := o.DB.PutTeam(teamRec); err != nil {
		return nil, fmt.Errorf("bump team HC: %w", err)
	}

	// 7. Audit. Direct AppendAudit because the spawn is the caller's
	// single atomic action from outside.
	_ = o.DB.AppendAudit(store.AuditEntry{
		Timestamp: startedAt,
		Action:    "ic-spawned",
		Actor:     "arcmux",
		Subject:   slot,
		Detail: map[string]any{
			"team":            team,
			"role":            role,
			"arcmux_role":     arcmuxRole,
			"agent":           o.Agent,
			"contract":        contractID,
			"contract_state":  contractRec.State,
			"pane_ref":        pane.Ref,
			"workspace_ref":   teamRec.WorkspaceRef,
			"bootstrap_path":  bootstrapPath,
			"scratchpad_path": spPath,
			"hc_after":        teamRec.HC,
		},
	})

	return &Result{
		Slot:           s,
		Pane:           pane,
		BootstrapPath:  bootstrapPath,
		ScratchpadPath: spPath,
		Team:           teamRec,
		Contract:       contractRec,
	}, nil
}

// ErrSlotNotFound is returned by Dissolve when no slot record exists under
// the requested id. Distinguished from ErrAlreadyDissolved so callers can
// react differently (a typo vs. a double-dissolve).
var ErrSlotNotFound = errors.New("slot not found")

// ErrAlreadyDissolved is returned by Dissolve when the slot is already in
// the dissolved tombstone state. Loud rejection (not idempotent) because a
// double-dissolve is almost always a caller bug — the manager that wanted
// to retire the slot already did, and a second call is racing against an
// unknown party. The store-layer DropICInbox is the idempotent primitive;
// Dissolve sits one level up and treats the act as a discrete decision.
var ErrAlreadyDissolved = errors.New("slot already dissolved")

// ErrContractInFlight is returned by Dissolve when the slot's bound
// contract is in an active-execution state (working or validating). The
// caller must first transition the contract to a terminal state (cancelled
// / failed / completed) so the contract isn't orphaned — a contract with
// no IC is recoverable, but a contract still flagged "working" with no IC
// is a state lie the audit log can't easily explain. blocked / ready /
// pending / terminal states are all OK to dissolve over.
var ErrContractInFlight = errors.New("bound contract still in flight")

// DissolveOpts configure Dissolve.
type DissolveOpts struct {
	DB   *store.DB       // open project store; caller owns Close
	Cmux *cmuxcli.Client // cmux client; pane close is best-effort
	Slot string          // slot id to dissolve (validated as slug)
	By   string          // audit actor; defaults to "arcmux-call" when empty
}

// DissolveResult returns the artifacts of a successful Dissolve.
type DissolveResult struct {
	Slot           store.Slot // post-dissolve slot record (state=dissolved)
	Team           store.Team // post-dissolve team record (HC decremented)
	PaneCloseError error      // non-nil if cmux close-pane returned an error
	// (best-effort; the dissolve still succeeded). Inspect when you need
	// to know whether the pane was actually torn down or is a zombie.
}

// Dissolve retires an active IC slot. Steps, in order:
//
//  1. Validate the slot slug.
//  2. Load slot, contract, team; reject if slot is missing or already
//     dissolved, or if the bound contract is in working/validating.
//  3. State updates (bbolt): mark slot dissolved, decrement team HC to
//     match the post-dissolve active count, drop the per-IC inbox bucket,
//     append an ic-dissolved audit row.
//  4. Best-effort `cmux close-pane`. If cmux returns an error, record it
//     in PaneCloseError and in the audit row but DO NOT roll back state —
//     the slot record is the source of truth, and a zombie pane is the
//     less-bad failure mode than a half-dissolved slot.
//
// The dropped inbox bucket makes a respawn over the dissolved tombstone
// (same slot id) a genuinely fresh queue, not an inheritance of whatever
// the prior IC didn't ack. This matches the spec §10 stance that a slot
// id is a name, not a continuous identity.
func Dissolve(ctx context.Context, o DissolveOpts) (*DissolveResult, error) {
	if o.DB == nil {
		return nil, fmt.Errorf("Dissolve: DB required")
	}
	if o.Cmux == nil {
		return nil, fmt.Errorf("Dissolve: Cmux required")
	}
	slotID, err := paths.Validate(o.Slot)
	if err != nil {
		return nil, fmt.Errorf("slot: %w", err)
	}
	by := o.By
	if by == "" {
		by = "arcmux-call"
	}

	slot, err := o.DB.GetSlot(slotID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrSlotNotFound, slotID)
		}
		return nil, fmt.Errorf("get slot: %w", err)
	}
	if slot.State == store.SlotDissolved {
		return nil, fmt.Errorf("%w: slot %q (team %q)", ErrAlreadyDissolved, slot.ID, slot.Team)
	}

	// Contract guard. A slot may have outlived its contract (rare but
	// possible if the manager already transitioned it) — that case is
	// fine, dissolve proceeds. The only forbidden case is an IC walking
	// off in the middle of working/validating.
	if slot.Contract != "" {
		contract, err := o.DB.GetContract(slot.Contract)
		if err == nil {
			switch contract.State {
			case store.ContractWorking, store.ContractValidating:
				return nil, fmt.Errorf("%w: contract %q is %s (transition to cancelled/failed/completed first)",
					ErrContractInFlight, contract.ID, contract.State)
			}
		}
		// A not-found contract is OK to dissolve over (contract DAO is
		// the contract's source of truth; slot keeps a stale ref but
		// that's recoverable).
	}

	team, err := o.DB.GetTeam(slot.Team)
	if err != nil {
		return nil, fmt.Errorf("get team for HC update: %w", err)
	}

	dissolvedAt := time.Now()
	prevState := slot.State

	// 1. Mark slot dissolved. Preserves all other fields so the tombstone
	// retains the historical pane ref, contract binding, and bootstrap
	// path for forensics.
	slot.State = store.SlotDissolved
	slot.UpdatedAt = dissolvedAt
	if err := o.DB.PutSlot(slot); err != nil {
		return nil, fmt.Errorf("put dissolved slot: %w", err)
	}

	// 2. Recompute team HC from authoritative active-slot count. Cheaper
	// and safer than "HC--" (concurrent dissolves would compound).
	active, err := o.DB.ListSlots(slot.Team, store.SlotActive)
	if err != nil {
		return nil, fmt.Errorf("recount active slots: %w", err)
	}
	team.HC = len(active)
	if err := o.DB.PutTeam(team); err != nil {
		return nil, fmt.Errorf("update team HC: %w", err)
	}

	// 3. Drop per-IC inbox sub-bucket (purges any queued-but-unacked
	// messages). Idempotent at the store layer — never an error from a
	// missing bucket, so spawn-then-immediate-dissolve before any push
	// also works.
	if err := o.DB.DropICInbox(slot.ID); err != nil {
		return nil, fmt.Errorf("drop ic inbox: %w", err)
	}

	// 4. Best-effort pane close. Capture but do not return the error.
	var paneErr error
	if slot.PaneRef != "" {
		if cerr := o.Cmux.ClosePane(ctx, slot.PaneRef); cerr != nil {
			paneErr = cerr
		}
	}

	// 5. Audit. Record the from-state, team HC after, contract id, and
	// any pane-close error so a postmortem can reconstruct the dissolve.
	auditDetail := map[string]any{
		"team":          slot.Team,
		"role":          slot.Role,
		"contract":      slot.Contract,
		"pane_ref":      slot.PaneRef,
		"workspace_ref": slot.WorkspaceRef,
		"agent":         slot.Agent,
		"prev_state":    prevState,
		"hc_after":      team.HC,
		"inbox_dropped": true,
	}
	if paneErr != nil {
		auditDetail["pane_close_error"] = paneErr.Error()
	}
	_ = o.DB.AppendAudit(store.AuditEntry{
		Timestamp: dissolvedAt,
		Action:    "ic-dissolved",
		Actor:     by,
		Subject:   slot.ID,
		Detail:    auditDetail,
	})

	return &DissolveResult{
		Slot:           slot,
		Team:           team,
		PaneCloseError: paneErr,
	}, nil
}

// initialICScratchpad seeds the IC's per-role scratchpad with everything a
// respawned IC needs to pick up identically: its identity, its bootstrap
// breadcrumb, the bound contract's headline fields (objective, format,
// acceptance), and a checklist of first actions. A hash of the contract
// objective lets a respawn detect mid-flight scope changes.
func initialICScratchpad(project, team, slot, role, arcmuxRole string, c store.Contract, startedAt time.Time) map[string]any {
	objective := strings.TrimSpace(c.Objective)
	focus := fmt.Sprintf("Fresh IC spawn — read contract %s, write turn-0 to IC scratchpad, transition contract to working when ready to start.", c.ID)
	next := []string{
		"arcmux-call contract get --id $ARCMUX_CONTRACT (re-read the bound contract)",
		"Confirm acceptance_criteria are mechanically checkable; if not, ack the contract back to the manager with a clarification request",
		"arcmux-call contract transition --id $ARCMUX_CONTRACT --to working --reason 'IC bootstrap done'",
		"Begin work inside boundaries; checkpoint scratchpad after every meaningful step",
	}
	sum := sha256.Sum256([]byte(c.Objective))
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
			"project":             project,
			"team":                team,
			"slot":                slot,
			"role_specialization": role,
			"arcmux_role":         arcmuxRole,
			"vault_root_ref":      "$ARCMUX_VAULT",
			"contract": map[string]any{
				"id":                  c.ID,
				"state":               c.State,
				"priority":            c.Priority,
				"output_format":       c.OutputFormat,
				"acceptance_criteria": c.AcceptanceCriteria,
				"boundaries":          c.Boundaries,
				"tools":               c.Tools,
				"depends_on":          c.DependsOn,
				"objective_bytes":     len(c.Objective),
				"objective_sha256":    hex.EncodeToString(sum[:]),
				"objective_preview":   firstLine(objective),
			},
		},
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
