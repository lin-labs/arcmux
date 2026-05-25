package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/icspawn"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/manager/teamspawn"
)

// icFakeRunner answers both team-spawn AND ic-spawn cmux calls so a single
// runner can be reused across preseed + spawn. NewWorkspace returns
// workspace:cli-7, ListPanes returns the manager pane, NewPane returns
// pane:cli-9, and Send is acknowledged.
type icFakeRunner struct {
	calls [][]string
	outs  map[string]string
}

func (f *icFakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for k, v := range f.outs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

func newICCmux() *cmuxcli.Client {
	return cmuxcli.NewWithRunnerForTest(&icFakeRunner{outs: map[string]string{
		"new-workspace": "OK workspace:cli-7\n",
		"list-panes":    `{"workspace_ref":"workspace:cli-7","panes":[{"ref":"pane:cli-mgr","index":0,"focused":true,"surface_refs":["surface:s1"]}]}`,
		"new-pane":      "OK pane:cli-9\n",
		"close-pane":    "OK\n",
	}})
}

// writeICRoleFile drops ic-base.md (or whatever) into the test vault so
// icspawn.Spawn can resolve the role file. Without this the CLI happy-path
// test would fail at the role-file-exists check.
func writeICRoleFile(t *testing.T, vault, name string) {
	t.Helper()
	dir := paths.GlobalRolesDir(vault)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir roles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("# "+name), 0o644); err != nil {
		t.Fatalf("write role: %v", err)
	}
}

// preseedTeamAndContract mirrors team_test.preseedTeam but also creates
// a contract bound to the team — exactly the state ic-spawn expects.
func preseedTeamAndContract(t *testing.T, dataRoot, vault, project, teamSlug, contractID string) {
	t.Helper()
	db, _, err := openProjectDB(dataRoot, project)
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	defer db.Close()
	if _, err := teamspawn.Spawn(context.Background(), teamspawn.Opts{
		DB: db, Cmux: newICCmux(), Project: project, Slug: teamSlug,
		Vision: "seeded for ic-cli", Agent: "claude",
		VaultRoot: vault, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("preseed team: %v", err)
	}
	if err := db.PutContract(store.Contract{
		ID: contractID, Team: teamSlug, Priority: 3,
		State:    store.ContractPending,
		Objective: "do the thing",
	}); err != nil {
		t.Fatalf("put contract: %v", err)
	}
}

// preseedSlot writes a slot directly via the icspawn primitive so list/get
// tests don't repeat the spawn ceremony.
func preseedSlot(t *testing.T, dataRoot, vault, project, team, slot, contract string) {
	t.Helper()
	db, _, err := openProjectDB(dataRoot, project)
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	defer db.Close()
	if _, err := icspawn.Spawn(context.Background(), icspawn.Opts{
		DB: db, Cmux: newICCmux(), Project: project, Team: team,
		Slot: slot, Role: "ic-base", Contract: contract, Agent: "claude",
		VaultRoot: vault, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("preseed slot %s: %v", slot, err)
	}
}

func TestCmdICSpawnHappy(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	writeICRoleFile(t, vault, "ic-base")
	preseedTeamAndContract(t, dataRoot, vault, "cliproj", "ux-pass", "make-it-pretty")

	var out bytes.Buffer
	err := cmdICSpawn(
		[]string{
			"--project", "cliproj", "--data-root", dataRoot, "--vault-root", vault,
			"--team", "ux-pass", "--slot", "linus-1",
			"--contract", "make-it-pretty", "--agent", "claude",
		},
		&out,
		newICCmux(),
	)
	if err != nil {
		t.Fatalf("cmdICSpawn: %v", err)
	}
	var ack struct {
		OK             bool       `json:"ok"`
		Slot           store.Slot `json:"slot"`
		PaneRef        string     `json:"pane_ref"`
		WorkspaceRef   string     `json:"workspace_ref"`
		BootstrapPath  string     `json:"bootstrap_path"`
		ScratchpadPath string     `json:"scratchpad_path"`
		TeamHC         int        `json:"team_hc"`
		Contract       struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v\nraw: %s", err, out.String())
	}
	if !ack.OK || ack.Slot.ID != "linus-1" || ack.PaneRef != "pane:cli-9" {
		t.Errorf("ack: %+v", ack)
	}
	if ack.TeamHC != 1 {
		t.Errorf("team HC = %d, want 1 after first spawn", ack.TeamHC)
	}
	if ack.Contract.ID != "make-it-pretty" {
		t.Errorf("contract id echo: %s", ack.Contract.ID)
	}
	if _, err := os.Stat(ack.BootstrapPath); err != nil {
		t.Errorf("bootstrap missing: %v", err)
	}
	if _, err := os.Stat(ack.ScratchpadPath); err != nil {
		t.Errorf("scratchpad missing: %v", err)
	}
}

func TestCmdICSpawnRequiresFlags(t *testing.T) {
	cli := newICCmux()
	cases := []struct {
		name string
		args []string
	}{
		{"no-team", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d", "--slot", "s", "--contract", "c"}},
		{"no-slot", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d", "--team", "t", "--contract", "c"}},
		{"no-contract", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d", "--team", "t", "--slot", "s"}},
		{"no-project", []string{"--vault-root", "/v", "--data-root", "/d", "--team", "t", "--slot", "s", "--contract", "c"}},
		{"no-vault", []string{"--project", "p", "--data-root", "/d", "--team", "t", "--slot", "s", "--contract", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdICSpawn(tc.args, &out, cli); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestCmdICSpawnRejectsBadSlugs(t *testing.T) {
	cli := newICCmux()
	cases := []struct {
		name string
		args []string
	}{
		{"bad-slot", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d", "--team", "t", "--slot", "../escape", "--contract", "c"}},
		{"bad-team", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d", "--team", "../escape", "--slot", "s", "--contract", "c"}},
		{"bad-contract", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d", "--team", "t", "--slot", "s", "--contract", "../escape"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdICSpawn(tc.args, &out, cli); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestCmdICList(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	writeICRoleFile(t, vault, "ic-base")
	preseedTeamAndContract(t, dataRoot, vault, "p", "t1", "c1")
	preseedTeamAndContract(t, dataRoot, vault, "p", "t2", "c2")
	preseedSlot(t, dataRoot, vault, "p", "t1", "slot-a", "c1")
	preseedSlot(t, dataRoot, vault, "p", "t1", "slot-b", "c1")
	preseedSlot(t, dataRoot, vault, "p", "t2", "slot-c", "c2")

	var out bytes.Buffer
	if err := cmdICList(
		[]string{"--project", "p", "--data-root", dataRoot, "--team", "t1"},
		&out,
	); err != nil {
		t.Fatalf("cmdICList: %v", err)
	}
	var got struct {
		Slots []store.Slot `json:"slots"`
	}
	_ = json.Unmarshal(out.Bytes(), &got)
	if len(got.Slots) != 2 {
		t.Errorf("team t1: got %d slots, want 2; raw=%s", len(got.Slots), out.String())
	}
	if got.Slots[0].ID != "slot-a" || got.Slots[1].ID != "slot-b" {
		t.Errorf("slots not sorted by ID: %+v", got.Slots)
	}

	out.Reset()
	if err := cmdICList(
		[]string{"--project", "p", "--data-root", dataRoot},
		&out,
	); err != nil {
		t.Fatalf("cmdICList all: %v", err)
	}
	_ = json.Unmarshal(out.Bytes(), &got)
	if len(got.Slots) != 3 {
		t.Errorf("all-teams: got %d slots, want 3", len(got.Slots))
	}
}

func TestCmdICGet(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	writeICRoleFile(t, vault, "ic-base")
	preseedTeamAndContract(t, dataRoot, vault, "p", "t1", "c1")
	preseedSlot(t, dataRoot, vault, "p", "t1", "lookup-me", "c1")

	var out bytes.Buffer
	if err := cmdICGet(
		[]string{"--project", "p", "--data-root", dataRoot, "--slot", "lookup-me"},
		&out,
	); err != nil {
		t.Fatalf("cmdICGet: %v", err)
	}
	var got struct {
		Slot store.Slot `json:"slot"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slot.ID != "lookup-me" || got.Slot.Contract != "c1" {
		t.Errorf("got: %+v", got.Slot)
	}
}

func TestCmdICGetMissing(t *testing.T) {
	dataRoot := t.TempDir()
	db, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	err = cmdICGet(
		[]string{"--project", "p", "--data-root", dataRoot, "--slot", "ghost"},
		&out,
	)
	if err == nil {
		t.Fatal("expected error for missing slot")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err message missing 'not found': %v", err)
	}
}

func TestCmdICGetRejectsBadSlug(t *testing.T) {
	dataRoot := t.TempDir()
	db, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	err = cmdICGet(
		[]string{"--project", "p", "--data-root", dataRoot, "--slot", "../escape"},
		&out,
	)
	if err == nil {
		t.Fatal("expected validation error for slot ../escape")
	}
	if !strings.Contains(err.Error(), "invalid project slug") {
		t.Errorf("err should surface slug validation, got: %v", err)
	}
}

func TestCmdICGetRequiresFlags(t *testing.T) {
	var out bytes.Buffer
	if err := cmdICGet([]string{"--project", "p", "--data-root", "/d"}, &out); err == nil {
		t.Error("expected error for missing --slot")
	}
	if err := cmdICGet([]string{"--slot", "x", "--data-root", "/d"}, &out); err == nil {
		t.Error("expected error for missing --project")
	}
}

// TestICE2EBinaryReadback mirrors TestTeamE2EBinaryReadback: builds
// bin/arcmux-call and uses it to read slots seeded in-process via
// icspawn. The binary's spawn path itself isn't exercised end-to-end
// because it needs a real cmux daemon; the readback shape (list/get JSON)
// is what every spawned IC pane will see.
func TestICE2EBinaryReadback(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e — requires building arcmux-call binary")
	}
	bin := filepath.Join(t.TempDir(), "arcmux-call")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build arcmux-call: %v", err)
	}

	dataRoot := t.TempDir()
	vault := t.TempDir()
	writeICRoleFile(t, vault, "ic-base")
	preseedTeamAndContract(t, dataRoot, vault, "e2eproj", "alpha", "deliver-A")
	preseedSlot(t, dataRoot, vault, "e2eproj", "alpha", "linus-1", "deliver-A")
	preseedSlot(t, dataRoot, vault, "e2eproj", "alpha", "linus-2", "deliver-A")

	list := exec.Command(bin, "ic", "list",
		"--project", "e2eproj", "--data-root", dataRoot, "--team", "alpha")
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("ic list: %v", err)
	}
	var listOut struct {
		Slots []store.Slot `json:"slots"`
	}
	if err := json.Unmarshal(out, &listOut); err != nil {
		t.Fatalf("decode list: %v\nraw: %s", err, out)
	}
	if len(listOut.Slots) != 2 {
		t.Fatalf("binary list returned %d slots, want 2; raw=%s", len(listOut.Slots), out)
	}

	get := exec.Command(bin, "ic", "get",
		"--project", "e2eproj", "--data-root", dataRoot, "--slot", "linus-1")
	get.Stderr = os.Stderr
	out, err = get.Output()
	if err != nil {
		t.Fatalf("ic get: %v", err)
	}
	var getOut struct {
		Slot store.Slot `json:"slot"`
	}
	if err := json.Unmarshal(out, &getOut); err != nil {
		t.Fatalf("decode get: %v\nraw: %s", err, out)
	}
	if getOut.Slot.ID != "linus-1" || getOut.Slot.Contract != "deliver-A" {
		t.Errorf("binary ic get returned %+v", getOut.Slot)
	}
}

func TestCmdICDispatcher(t *testing.T) {
	var out bytes.Buffer
	if err := cmdIC([]string{}, &out); err == nil {
		t.Error("expected error for empty args")
	}
	if err := cmdIC([]string{"frobnicate"}, &out); err == nil {
		t.Error("expected error for unknown subcommand")
	}
}

func TestCmdICDissolveHappy(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	writeICRoleFile(t, vault, "ic-base")
	preseedTeamAndContract(t, dataRoot, vault, "p", "alpha", "c1")
	preseedSlot(t, dataRoot, vault, "p", "alpha", "linus-1", "c1")

	// Sanity: pre-dissolve HC=1, slot active, inbox bucket present.
	pre, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	preTeam, _ := pre.GetTeam("alpha")
	if preTeam.HC != 1 {
		t.Fatalf("pre-dissolve HC = %d, want 1", preTeam.HC)
	}
	if !pre.HasICInbox("linus-1") {
		t.Fatalf("pre-dissolve inbox bucket missing")
	}
	pre.Close()

	var out bytes.Buffer
	if err := cmdICDissolve(
		[]string{"--project", "p", "--data-root", dataRoot, "--slot", "linus-1", "--by", "manager:alpha"},
		&out,
		newICCmux(),
	); err != nil {
		t.Fatalf("cmdICDissolve: %v", err)
	}
	var ack struct {
		OK           bool       `json:"ok"`
		Slot         store.Slot `json:"slot"`
		TeamHC       int        `json:"team_hc"`
		InboxDropped bool       `json:"inbox_dropped"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v\nraw: %s", err, out.String())
	}
	if !ack.OK || ack.Slot.State != store.SlotDissolved {
		t.Errorf("ack = %+v", ack)
	}
	if ack.TeamHC != 0 {
		t.Errorf("post-dissolve team HC echo = %d, want 0", ack.TeamHC)
	}
	if !ack.InboxDropped {
		t.Errorf("ack.inbox_dropped = false, want true")
	}

	// Verify persistence.
	post, _, _ := openProjectDB(dataRoot, "p")
	defer post.Close()
	gotSlot, _ := post.GetSlot("linus-1")
	if gotSlot.State != store.SlotDissolved {
		t.Errorf("persisted slot state = %q, want dissolved", gotSlot.State)
	}
	if post.HasICInbox("linus-1") {
		t.Errorf("post-dissolve inbox bucket still present; want dropped")
	}
	gotTeam, _ := post.GetTeam("alpha")
	if gotTeam.HC != 0 {
		t.Errorf("persisted team HC = %d, want 0", gotTeam.HC)
	}
}

func TestCmdICDissolveRequiresFlags(t *testing.T) {
	cli := newICCmux()
	var out bytes.Buffer
	if err := cmdICDissolve([]string{"--project", "p", "--data-root", "/d"}, &out, cli); err == nil {
		t.Error("expected error for missing --slot")
	}
	if err := cmdICDissolve([]string{"--slot", "x", "--data-root", "/d"}, &out, cli); err == nil {
		t.Error("expected error for missing --project")
	}
}

func TestCmdICDissolveRejectsBadSlug(t *testing.T) {
	dataRoot := t.TempDir()
	db, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	err = cmdICDissolve(
		[]string{"--project", "p", "--data-root", dataRoot, "--slot", "../escape"},
		&out,
		newICCmux(),
	)
	if err == nil || !strings.Contains(err.Error(), "invalid project slug") {
		t.Errorf("expected slug validation error, got %v", err)
	}
}

func TestCmdICDissolveRejectsMissingSlot(t *testing.T) {
	dataRoot := t.TempDir()
	db, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	err = cmdICDissolve(
		[]string{"--project", "p", "--data-root", dataRoot, "--slot", "ghost"},
		&out,
		newICCmux(),
	)
	if err == nil || !strings.Contains(err.Error(), "slot not found") {
		t.Errorf("expected slot-not-found error, got %v", err)
	}
}

// TestICDissolveE2EBinary builds arcmux-call and exercises spawn→dissolve→
// list end-to-end against a real bolt file. The spawn step uses icspawn
// in-process (cmux daemon isn't available); the dissolve step goes
// through the binary's dispatcher. We can't actually run the dissolve
// binary path because it would call cmux too — but we exercise the
// readback shape (list shows the dissolved tombstone, get returns it
// with state=dissolved).
func TestICDissolveStateReadbackBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e — requires building arcmux-call binary")
	}
	bin := filepath.Join(t.TempDir(), "arcmux-call")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	dataRoot := t.TempDir()
	vault := t.TempDir()
	writeICRoleFile(t, vault, "ic-base")
	preseedTeamAndContract(t, dataRoot, vault, "e2e", "team-x", "c-x")
	preseedSlot(t, dataRoot, vault, "e2e", "team-x", "linus-1", "c-x")

	// Flip slot to dissolved directly so the readback shape is exercised
	// without needing a live cmux daemon.
	db, _, err := openProjectDB(dataRoot, "e2e")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, _ := db.GetSlot("linus-1")
	got.State = store.SlotDissolved
	if err := db.PutSlot(got); err != nil {
		t.Fatalf("put dissolved: %v", err)
	}
	db.Close()

	// `ic list` should still surface the tombstone by default.
	list := exec.Command(bin, "ic", "list",
		"--project", "e2e", "--data-root", dataRoot, "--team", "team-x")
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("ic list: %v", err)
	}
	var listOut struct {
		Slots []store.Slot `json:"slots"`
	}
	_ = json.Unmarshal(out, &listOut)
	if len(listOut.Slots) != 1 || listOut.Slots[0].State != store.SlotDissolved {
		t.Errorf("ic list: %+v", listOut)
	}
	// `ic list --state active` should hide it.
	listActive := exec.Command(bin, "ic", "list",
		"--project", "e2e", "--data-root", dataRoot, "--team", "team-x", "--state", "active")
	listActive.Stderr = os.Stderr
	out, err = listActive.Output()
	if err != nil {
		t.Fatalf("ic list --state active: %v", err)
	}
	_ = json.Unmarshal(out, &listOut)
	if len(listOut.Slots) != 0 {
		t.Errorf("ic list active-only: got %d slots, want 0", len(listOut.Slots))
	}
}
