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
	"github.com/lin-labs/arcmux/internal/manager/paths"
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
	dir := paths.GlobalRolesDir(vaultRoot)
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
		Tools: []string{"go", "bbolt"},
		Boundaries: []string{"no breaking API"},
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
			if !strings.Contains(joined, "--target pane:55") {
				t.Errorf("send wrong target: %s", joined)
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
