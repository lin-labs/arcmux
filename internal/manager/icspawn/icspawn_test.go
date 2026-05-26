package icspawn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/manager/teamspawn"
)

// fakeRunner mirrors teamspawn_test's fake. Kept local to avoid a test-
// only import cycle.
type fakeRunner struct {
	calls [][]string
	outs  map[string]string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for k, v := range f.outs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

// okCmuxForTeam answers team-spawn calls (NewWorkspace + ListPanes).
func okCmuxForTeam() (*fakeRunner, *cmuxcli.Client) {
	f := &fakeRunner{outs: map[string]string{
		"new-workspace": "OK workspace:42\n",
		"list-panes":    `{"workspace_ref":"workspace:42","panes":[{"ref":"pane:11","index":0,"focused":true,"surface_refs":["surface:9"]}]}`,
	}}
	return f, cmuxcli.NewWithRunnerForTest(f)
}

// okCmuxForIC answers IC-spawn calls (NewPane + Send). The pane ref is the
// reserved "pane:55" so tests can assert it ended up on the slot record.
func okCmuxForIC() (*fakeRunner, *cmuxcli.Client) {
	f := &fakeRunner{outs: map[string]string{
		"new-pane": "OK pane:55\n",
		"send":     "OK\n",
	}}
	return f, cmuxcli.NewWithRunnerForTest(f)
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.bolt"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// writeRoleFile materializes a minimal ic-base.md inside vault/0Prompts/roles/.
// Returns the absolute role file path so a test can assert
// --append-system-prompt-file points there.
func writeRoleFile(t *testing.T, vaultRoot, name, body string) string {
	t.Helper()
	dir := filepath.Join(vaultRoot, "0Prompts", "roles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir roles: %v", err)
	}
	p := filepath.Join(dir, name+".md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write role file: %v", err)
	}
	return p
}

// seedTeamAndContract spawns a team via teamspawn and creates a pending
// contract bound to it. Returns the team slug and the contract id. Each
// helper opens its own DB handle and closes it; the caller's DB must be
// closed before invoking this (bbolt holds an exclusive file lock).
func seedTeamAndContract(t *testing.T, dataRoot, vaultRoot, project, teamSlug, contractID string) {
	t.Helper()
	db, err := store.Open(filepath.Join(dataRoot, "arcmux", project, "state.bolt"))
	if err != nil {
		// fallback: when test directly opens via icspawn it'll use a single DB —
		// but here we always go through a fresh handle.
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()

	_, cli := okCmuxForTeam()
	if _, err := teamspawn.Spawn(context.Background(), teamspawn.Opts{
		DB: db, Cmux: cli, Project: project, Slug: teamSlug,
		Vision: "seeded for icspawn test", Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("teamspawn.Spawn %s: %v", teamSlug, err)
	}
	if err := db.PutContract(store.Contract{
		ID: contractID, Team: teamSlug, Priority: 5, State: store.ContractPending,
		Objective: "design the auth flow end-to-end", OutputFormat: "PR",
		Tools:              []string{"go", "bbolt"},
		Boundaries:         []string{"no breaking API"},
		AcceptanceCriteria: []string{"tests pass", "audit row written"},
	}); err != nil {
		t.Fatalf("put contract: %v", err)
	}
}

// projectDBPath gives icspawn tests a single bolt file path so the seed
// step and the Spawn step talk to the same store.
func projectDBPath(dataRoot, project string) string {
	return filepath.Join(dataRoot, "arcmux", project, "state.bolt")
}

func ensureProjectDir(t *testing.T, dataRoot, project string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataRoot, "arcmux", project), 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
}

// TestSpawnHappyPath end-to-end: real seed → IC spawn → assert slot
// record, pane ref, scratchpad on disk, bootstrap script with
// ARCMUX_CONTRACT, audit row, HC bump on team.
func TestSpawnHappyPath(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "auth-refactor"
	contract := "design-auth"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, cli := okCmuxForIC()

	r, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project,
		Team: team, Slot: "linus-1", Role: "ic-base",
		Contract: contract, Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if r.Slot.ID != "linus-1" || r.Slot.Team != team || r.Slot.Contract != contract {
		t.Errorf("slot record mismatched: %+v", r.Slot)
	}
	if r.Slot.PaneRef != "pane:55" {
		t.Errorf("slot pane = %q, want pane:55", r.Slot.PaneRef)
	}
	if r.Slot.State != store.SlotActive {
		t.Errorf("slot state = %q, want %q", r.Slot.State, store.SlotActive)
	}
	if r.Team.HC != 1 {
		t.Errorf("team HC = %d, want 1 after first IC spawn", r.Team.HC)
	}

	if _, err := os.Stat(r.BootstrapPath); err != nil {
		t.Errorf("bootstrap missing: %v", err)
	}
	if _, err := os.Stat(r.ScratchpadPath); err != nil {
		t.Errorf("scratchpad missing: %v", err)
	}

	body, _ := os.ReadFile(r.BootstrapPath)
	bs := string(body)
	for _, want := range []string{
		"export ARCMUX_TEAM='auth-refactor'",
		"export ARCMUX_SLOT='linus-1'",
		"export ARCMUX_CONTRACT='design-auth'",
		"export ARCMUX_ROLE='ic-auth-refactor-linus-1'",
		"exec claude --dangerously-skip-permissions --append-system-prompt-file",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("bootstrap missing %q:\n%s", want, bs)
		}
	}

	// The per-IC inbox sub-bucket must exist immediately after Spawn,
	// before the IC's first poll. icspawn calls EnsureICInbox after
	// PutSlot so that race against a manager pushing to a freshly-spawned
	// IC is impossible.
	if !db.HasICInbox("linus-1") {
		t.Errorf("HasICInbox(linus-1) = false after spawn; want true (Ensure must happen at spawn time)")
	}

	// Scratchpad should carry the contract preview + first-step plan.
	padRaw, _ := os.ReadFile(r.ScratchpadPath)
	var pad map[string]any
	if err := json.Unmarshal(padRaw, &pad); err != nil {
		t.Fatalf("scratchpad parse: %v\n%s", err, padRaw)
	}
	if pad["turn"].(float64) != 0 {
		t.Errorf("scratchpad turn = %v, want 0", pad["turn"])
	}
	boot := pad["bootstrap"].(map[string]any)
	if boot["team"] != team || boot["slot"] != "linus-1" {
		t.Errorf("scratchpad bootstrap mismatched: %+v", boot)
	}
	contractMap := boot["contract"].(map[string]any)
	if contractMap["id"] != contract {
		t.Errorf("scratchpad contract.id = %v, want %q", contractMap["id"], contract)
	}

	// Slot is readable via the store after spawn.
	got, err := db.GetSlot("linus-1")
	if err != nil {
		t.Errorf("GetSlot: %v", err)
	}
	if got.PaneRef != "pane:55" {
		t.Errorf("persisted slot pane = %q", got.PaneRef)
	}

	// Audit row recorded.
	rows, _ := db.RecentAudit(10)
	saw := false
	for _, e := range rows {
		if e.Action == "ic-spawned" && e.Subject == "linus-1" {
			saw = true
			if e.Detail["contract"] != contract {
				t.Errorf("audit contract detail = %v", e.Detail["contract"])
			}
			if e.Detail["hc_after"] != float64(1) && e.Detail["hc_after"] != 1 {
				t.Errorf("audit hc_after = %v", e.Detail["hc_after"])
			}
		}
	}
	if !saw {
		t.Errorf("no ic-spawned audit row found in %+v", rows)
	}
}

func TestSpawnRejectsBadAgent(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Team: "t", Slot: "s",
		Contract: "c", Agent: "bash", VaultRoot: "/v", DataRoot: "/d",
	})
	if err == nil {
		t.Error("expected unsupported-agent error")
	}
}

func TestSpawnRejectsBadSlugs(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForIC()
	for _, tc := range []struct {
		name string
		opts Opts
	}{
		{"bad-project", Opts{DB: db, Cmux: cli, Project: "../evil", Team: "t", Slot: "s", Contract: "c", Agent: "claude", VaultRoot: "/v", DataRoot: "/d"}},
		{"bad-team", Opts{DB: db, Cmux: cli, Project: "p", Team: "../evil", Slot: "s", Contract: "c", Agent: "claude", VaultRoot: "/v", DataRoot: "/d"}},
		{"bad-slot", Opts{DB: db, Cmux: cli, Project: "p", Team: "t", Slot: "../evil", Contract: "c", Agent: "claude", VaultRoot: "/v", DataRoot: "/d"}},
		{"bad-contract", Opts{DB: db, Cmux: cli, Project: "p", Team: "t", Slot: "s", Contract: "../evil", Agent: "claude", VaultRoot: "/v", DataRoot: "/d"}},
		{"bad-role", Opts{DB: db, Cmux: cli, Project: "p", Team: "t", Slot: "s", Role: "../evil", Contract: "c", Agent: "claude", VaultRoot: "/v", DataRoot: "/d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Spawn(context.Background(), tc.opts); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestSpawnRequiresVaultAndDataRoot(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForIC()
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Team: "t", Slot: "s", Contract: "c", Agent: "claude", VaultRoot: "", DataRoot: "/d",
	}); err == nil {
		t.Error("expected error for empty VaultRoot")
	}
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Team: "t", Slot: "s", Contract: "c", Agent: "claude", VaultRoot: "/v", DataRoot: "",
	}); err == nil {
		t.Error("expected error for empty DataRoot")
	}
}

func TestSpawnRejectsMissingTeam(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	_, cli := okCmuxForIC()
	if err := db.PutContract(store.Contract{ID: "c1", Team: "ghost", State: store.ContractPending, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "ghost", Slot: "s",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want not-found error, got %v", err)
	}
}

func TestSpawnRejectsInactiveTeam(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamArchived, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutContract(store.Contract{ID: "c1", Team: "t1", State: store.ContractPending, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "t1", Slot: "s",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Errorf("want archived-team rejection, got %v", err)
	}
}

func TestSpawnRejectsMissingContract(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "t1", Slot: "s",
		Contract: "missing", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want contract not-found error, got %v", err)
	}
}

func TestSpawnRejectsCrossTeamContract(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "team-a", State: store.TeamActive, WorkspaceRef: "workspace:a"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutContract(store.Contract{ID: "c1", Team: "team-b", State: store.ContractPending, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "team-a", Slot: "s",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "team-b") {
		t.Errorf("want cross-team error citing team-b, got %v", err)
	}
}

func TestSpawnRejectsTerminalContract(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutContract(store.Contract{ID: "c1", Team: "t1", State: store.ContractCompleted, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "t1", Slot: "s",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("want terminal-state error, got %v", err)
	}
}

func TestSpawnRejectsDuplicateSlot(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutContract(store.Contract{ID: "c1", Team: "t1", State: store.ContractPending, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSlot(store.Slot{ID: "dup", Team: "t1", Role: "ic-base", Contract: "c1", State: store.SlotActive}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "t1", Slot: "dup",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if !errors.Is(err, ErrSlotExists) {
		t.Errorf("want ErrSlotExists, got %v", err)
	}
}

func TestSpawnAllowsRespawnOverDissolvedSlot(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "t1"
	contract := "c1"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Plant a dissolved slot tombstone.
	if err := db.PutSlot(store.Slot{ID: "phoenix", Team: team, Role: "ic-base", Contract: contract, State: store.SlotDissolved}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	r, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team, Slot: "phoenix",
		Contract: contract, Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("respawn over dissolved: %v", err)
	}
	if r.Slot.State != store.SlotActive {
		t.Errorf("post-respawn state = %q, want %q", r.Slot.State, store.SlotActive)
	}
}

func TestSpawnHCCap(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutContract(store.Contract{ID: "c1", Team: "t1", State: store.ContractPending, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	// Fill the team to the cap with active slot tombstones.
	for i := 0; i < store.MaxICsPerTeam; i++ {
		id := fmt.Sprintf("slot-%d", i)
		if err := db.PutSlot(store.Slot{ID: id, Team: "t1", Role: "ic-base", Contract: "c1", State: store.SlotActive}); err != nil {
			t.Fatal(err)
		}
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "t1", Slot: "overflow",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if !errors.Is(err, ErrHCCap) {
		t.Errorf("want ErrHCCap, got %v", err)
	}
}

func TestSpawnMissingRoleFile(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir() // no role files written
	db := openTestDB(t)
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutContract(store.Contract{ID: "c1", Team: "t1", State: store.ContractPending, Objective: "x"}); err != nil {
		t.Fatal(err)
	}
	_, cli := okCmuxForIC()
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "demo", Team: "t1", Slot: "s",
		Contract: "c1", Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "role file") {
		t.Errorf("want role-file error, got %v", err)
	}
}

// okCmuxForDissolve combines IC-spawn outputs with a close-pane ack so a
// single fake runner can drive spawn+dissolve in one test.
func okCmuxForDissolve() (*fakeRunner, *cmuxcli.Client) {
	f := &fakeRunner{outs: map[string]string{
		"new-pane":   "OK pane:55\n",
		"send":       "OK\n",
		"close-pane": "OK\n",
	}}
	return f, cmuxcli.NewWithRunnerForTest(f)
}

// failingClosePaneCmux makes the close-pane call return an error so the
// test can verify Dissolve does NOT roll back state on a pane-close
// failure (state is the source of truth, zombie pane is the lesser evil).
type failingClosePaneRunner struct {
	*fakeRunner
}

func (r *failingClosePaneRunner) Run(ctx context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, args)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "close-pane") {
		return "", fmt.Errorf("cmux daemon unreachable")
	}
	for k, v := range r.outs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

func cmuxForSpawnButFailingClose() (*failingClosePaneRunner, *cmuxcli.Client) {
	f := &failingClosePaneRunner{fakeRunner: &fakeRunner{outs: map[string]string{
		"new-pane": "OK pane:55\n",
		"send":     "OK\n",
	}}}
	return f, cmuxcli.NewWithRunnerForTest(f)
}

func TestDissolveHappyPath(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "auth-refactor"
	contract := "design-auth"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	f, cli := okCmuxForDissolve()
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team,
		Slot: "linus-1", Role: "ic-base", Contract: contract, Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Queue a manager push so we can prove DropICInbox purged it.
	if err := db.PushICInbox("linus-1", store.InboxMsg{ID: "ghost-msg", Verb: "consult", Body: "x"}); err != nil {
		t.Fatalf("push pre-dissolve: %v", err)
	}

	pre, _ := db.GetTeam(team)
	if pre.HC != 1 {
		t.Fatalf("pre-dissolve team HC = %d, want 1", pre.HC)
	}

	r, err := Dissolve(context.Background(), DissolveOpts{
		DB: db, Cmux: cli, Slot: "linus-1", By: "manager:" + team,
	})
	if err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	if r.Slot.State != store.SlotDissolved {
		t.Errorf("post-dissolve slot state = %q, want %q", r.Slot.State, store.SlotDissolved)
	}
	if r.Team.HC != 0 {
		t.Errorf("post-dissolve team HC = %d, want 0", r.Team.HC)
	}
	if r.PaneCloseError != nil {
		t.Errorf("happy path PaneCloseError = %v, want nil", r.PaneCloseError)
	}

	// Persisted on disk too.
	got, _ := db.GetSlot("linus-1")
	if got.State != store.SlotDissolved {
		t.Errorf("persisted slot state = %q, want dissolved", got.State)
	}
	gotTeam, _ := db.GetTeam(team)
	if gotTeam.HC != 0 {
		t.Errorf("persisted team HC = %d, want 0", gotTeam.HC)
	}

	// Inbox bucket gone — peek now errors with ErrICInboxMissing.
	if db.HasICInbox("linus-1") {
		t.Errorf("HasICInbox after dissolve = true, want false (DropICInbox should have purged)")
	}

	// cmux close-pane was called with the slot's pane ref.
	sawClose := false
	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "close-pane") {
			sawClose = true
			if !strings.Contains(joined, "--pane pane:55") {
				t.Errorf("close-pane wrong target: %s", joined)
			}
		}
	}
	if !sawClose {
		t.Error("expected close-pane call")
	}

	// Audit row recorded with prev_state, hc_after, inbox_dropped.
	rows, _ := db.RecentAudit(20)
	sawDissolve := false
	for _, e := range rows {
		if e.Action == "ic-dissolved" && e.Subject == "linus-1" {
			sawDissolve = true
			if e.Actor != "manager:"+team {
				t.Errorf("audit actor = %q, want %q", e.Actor, "manager:"+team)
			}
			if e.Detail["prev_state"] != store.SlotActive {
				t.Errorf("audit prev_state = %v, want %q", e.Detail["prev_state"], store.SlotActive)
			}
			if e.Detail["hc_after"] != float64(0) && e.Detail["hc_after"] != 0 {
				t.Errorf("audit hc_after = %v, want 0", e.Detail["hc_after"])
			}
			if e.Detail["inbox_dropped"] != true {
				t.Errorf("audit inbox_dropped = %v, want true", e.Detail["inbox_dropped"])
			}
			if _, hasErr := e.Detail["pane_close_error"]; hasErr {
				t.Errorf("happy path audit unexpectedly has pane_close_error: %v", e.Detail)
			}
		}
	}
	if !sawDissolve {
		t.Errorf("no ic-dissolved audit row found in %+v", rows)
	}
}

func TestDissolveAllowsRespawnUnderSameID(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "t1"
	contract := "c1"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, cli := okCmuxForDissolve()
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team,
		Slot: "phoenix", Role: "ic-base", Contract: contract, Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	if _, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "phoenix"}); err != nil {
		t.Fatalf("Dissolve: %v", err)
	}
	// Respawn under the same id — the dissolved tombstone is overwritable
	// (already tested in TestSpawnAllowsRespawnOverDissolvedSlot) AND the
	// fresh spawn re-creates the inbox bucket.
	r2, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team,
		Slot: "phoenix", Role: "ic-base", Contract: contract, Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("respawn after dissolve: %v", err)
	}
	if r2.Slot.State != store.SlotActive {
		t.Errorf("respawned slot state = %q, want active", r2.Slot.State)
	}
	if !db.HasICInbox("phoenix") {
		t.Error("HasICInbox after respawn = false, want true (EnsureICInbox should re-create)")
	}
	// Team HC: we dissolved one then spawned one → HC=1.
	tt, _ := db.GetTeam(team)
	if tt.HC != 1 {
		t.Errorf("team HC after respawn = %d, want 1", tt.HC)
	}
}

func TestDissolveRejectsMissingSlot(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForDissolve()
	_, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "ghost"})
	if !errors.Is(err, ErrSlotNotFound) {
		t.Errorf("err = %v, want ErrSlotNotFound", err)
	}
}

func TestDissolveRejectsAlreadyDissolved(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForDissolve()
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSlot(store.Slot{
		ID: "tombstone", Team: "t1", Role: "ic-base",
		Contract: "c1", State: store.SlotDissolved,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "tombstone"})
	if !errors.Is(err, ErrAlreadyDissolved) {
		t.Errorf("err = %v, want ErrAlreadyDissolved", err)
	}
}

func TestDissolveRejectsContractInFlight(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForDissolve()
	if err := db.PutTeam(store.Team{ID: "t1", State: store.TeamActive, WorkspaceRef: "workspace:1", HC: 1}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{store.ContractWorking, store.ContractValidating} {
		t.Run(state, func(t *testing.T) {
			cid := "c-" + state
			sid := "slot-" + state
			if err := db.PutContract(store.Contract{ID: cid, Team: "t1", State: state, Objective: "x"}); err != nil {
				t.Fatal(err)
			}
			if err := db.PutSlot(store.Slot{
				ID: sid, Team: "t1", Role: "ic-base",
				Contract: cid, State: store.SlotActive, PaneRef: "pane:99",
			}); err != nil {
				t.Fatal(err)
			}
			_, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: sid})
			if !errors.Is(err, ErrContractInFlight) {
				t.Errorf("state=%s: err = %v, want ErrContractInFlight", state, err)
			}
			// State must NOT have flipped on a rejected dissolve.
			got, _ := db.GetSlot(sid)
			if got.State != store.SlotActive {
				t.Errorf("rejected dissolve mutated state: got %q, want active", got.State)
			}
		})
	}
}

func TestDissolveAllowsBlockedContract(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "t1"
	contract := "stuck"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, cli := okCmuxForDissolve()
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team,
		Slot: "stuck-slot", Role: "ic-base", Contract: contract, Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Mark contract blocked — manager should still be able to dissolve.
	if err := db.TransitionContract(contract, store.ContractReady, "manager", "deps met"); err != nil {
		t.Fatalf("transition ready: %v", err)
	}
	if err := db.TransitionContract(contract, store.ContractWorking, "ic", "start"); err != nil {
		t.Fatalf("transition working: %v", err)
	}
	if err := db.TransitionContract(contract, store.ContractBlocked, "ic", "dep gap"); err != nil {
		t.Fatalf("transition blocked: %v", err)
	}
	if _, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "stuck-slot"}); err != nil {
		t.Errorf("dissolve over blocked contract: %v (want nil — blocked is dissolvable)", err)
	}
}

func TestDissolveSurvivesPaneCloseFailure(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "t1"
	contract := "c1"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, cli := cmuxForSpawnButFailingClose()
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team,
		Slot: "zombie", Role: "ic-base", Contract: contract, Agent: "claude",
		VaultRoot: vaultRoot, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	r, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "zombie"})
	if err != nil {
		t.Fatalf("Dissolve with failing close-pane: %v (want nil — state is SoT, pane close is best-effort)", err)
	}
	if r.PaneCloseError == nil {
		t.Error("PaneCloseError = nil, want the cmux unreachable error captured")
	}
	if r.Slot.State != store.SlotDissolved {
		t.Errorf("slot state = %q despite pane-close failure; state must still flip", r.Slot.State)
	}
	if r.Team.HC != 0 {
		t.Errorf("team HC = %d despite pane-close failure; HC must still decrement", r.Team.HC)
	}
	// Audit row must carry the pane_close_error detail.
	rows, _ := db.RecentAudit(20)
	saw := false
	for _, e := range rows {
		if e.Action == "ic-dissolved" && e.Subject == "zombie" {
			saw = true
			if msg, ok := e.Detail["pane_close_error"].(string); !ok || msg == "" {
				t.Errorf("audit detail missing pane_close_error: %+v", e.Detail)
			}
		}
	}
	if !saw {
		t.Error("ic-dissolved audit row missing")
	}
}

func TestDissolveRejectsBadSlug(t *testing.T) {
	db := openTestDB(t)
	_, cli := okCmuxForDissolve()
	_, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "../evil"})
	if err == nil {
		t.Error("expected validation error for bad slug")
	}
}

func TestDissolveDecrementsHCMultipleSlots(t *testing.T) {
	// Spawn two ICs, dissolve one, verify HC = 1 (not 0 or 2).
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "t1"
	contract := "c1"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, cli := okCmuxForDissolve()
	for _, slotID := range []string{"a", "b"} {
		if _, err := Spawn(context.Background(), Opts{
			DB: db, Cmux: cli, Project: project, Team: team,
			Slot: slotID, Role: "ic-base", Contract: contract, Agent: "claude",
			VaultRoot: vaultRoot, DataRoot: dataRoot,
		}); err != nil {
			t.Fatalf("Spawn %s: %v", slotID, err)
		}
	}
	pre, _ := db.GetTeam(team)
	if pre.HC != 2 {
		t.Fatalf("pre-dissolve HC = %d, want 2", pre.HC)
	}
	r, err := Dissolve(context.Background(), DissolveOpts{DB: db, Cmux: cli, Slot: "a"})
	if err != nil {
		t.Fatalf("Dissolve a: %v", err)
	}
	if r.Team.HC != 1 {
		t.Errorf("after dissolving one of two, HC = %d, want 1", r.Team.HC)
	}
	// Surviving slot still active.
	bSlot, _ := db.GetSlot("b")
	if bSlot.State != store.SlotActive {
		t.Errorf("surviving slot b state = %q, want active", bSlot.State)
	}
	// And b's inbox still present.
	if !db.HasICInbox("b") {
		t.Error("surviving slot b's inbox unexpectedly dropped")
	}
}

func TestSpawnSendsBootstrapIntoPane(t *testing.T) {
	dataRoot := t.TempDir()
	vaultRoot := t.TempDir()
	project := "demo"
	team := "t1"
	contract := "c1"

	ensureProjectDir(t, dataRoot, project)
	writeRoleFile(t, vaultRoot, "ic-base", "# IC base")
	seedTeamAndContract(t, dataRoot, vaultRoot, project, team, contract)

	db, err := store.Open(projectDBPath(dataRoot, project))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	f, cli := okCmuxForIC()
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: project, Team: team, Slot: "watcher",
		Contract: contract, Agent: "claude", VaultRoot: vaultRoot, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	sawNewPane, sawSend := false, false
	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "new-pane"):
			sawNewPane = true
			if !strings.Contains(joined, "--workspace workspace:42") {
				t.Errorf("new-pane wrong workspace: %s", joined)
			}
			if !strings.Contains(joined, "--direction right") {
				t.Errorf("new-pane wrong direction: %s", joined)
			}
		case strings.Contains(joined, "send"):
			sawSend = true
			// cmux send uses --surface. Pane refs are rejected; the surface
			// ref must come from NewPane's multi-token OK response, or the
			// fake-runner's fallback to the pane ref (since the fake-runner
			// doesn't echo a surface today).
			if !strings.Contains(joined, "--surface ") {
				t.Errorf("send missing --surface: %s", joined)
			}
			if !strings.Contains(joined, "bootstrap-ic-t1-watcher.sh") {
				t.Errorf("send did not carry bootstrap path: %s", joined)
			}
		}
	}
	if !sawNewPane {
		t.Error("expected new-pane call")
	}
	if !sawSend {
		t.Error("expected send call")
	}
}
