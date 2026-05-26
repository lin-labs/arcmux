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
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/manager/teamspawn"
)

// teamFakeRunner is the CLI-side cmux fake (mirrors teamspawn_test.go).
type teamFakeRunner struct {
	calls [][]string
	outs  map[string]string
}

func (f *teamFakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for k, v := range f.outs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

func newTeamOKCmux() *cmuxcli.Client {
	return cmuxcli.NewWithRunnerForTest(&teamFakeRunner{outs: map[string]string{
		"new-workspace": "OK workspace:cli-1\n",
		"list-panes":    `{"workspace_ref":"workspace:cli-1","panes":[{"ref":"pane:cli-3","index":0,"focused":true,"surface_refs":["surface:s1"]}]}`,
	}})
}

// preseedTeam directly seeds a team via teamspawn.Spawn against an
// in-process cmux fake. Mirrors what the binary would do but lets list/get
// tests avoid duplicating the spawn ceremony.
func preseedTeam(t *testing.T, dataRoot, vault, project, slug, vision string) {
	t.Helper()
	db, _, err := openProjectDB(dataRoot, project)
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	defer db.Close()
	if _, err := teamspawn.Spawn(context.Background(), teamspawn.Opts{
		DB: db, Cmux: newTeamOKCmux(), Project: project, Slug: slug,
		Vision: vision, Agent: "claude", VaultRoot: vault, DataRoot: dataRoot,
	}); err != nil {
		t.Fatalf("preseed Spawn: %v", err)
	}
}

func TestCmdTeamSpawnHappy(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	var out bytes.Buffer
	err := cmdTeamSpawn(
		[]string{
			"--project", "cliproj", "--data-root", dataRoot, "--vault-root", vault,
			"--slug", "ux-pass", "--vision", "polish the dash", "--agent", "claude",
		},
		&out,
		newTeamOKCmux(),
	)
	if err != nil {
		t.Fatalf("cmdTeamSpawn: %v", err)
	}
	var ack struct {
		OK             bool       `json:"ok"`
		Team           store.Team `json:"team"`
		WorkspaceRef   string     `json:"workspace_ref"`
		ManagerPane    string     `json:"manager_pane"`
		BootstrapPath  string     `json:"bootstrap_path"`
		ScratchpadPath string     `json:"scratchpad_path"`
		CharterPath    string     `json:"charter_path"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v\nraw: %s", err, out.String())
	}
	if !ack.OK {
		t.Errorf("ack.ok = false")
	}
	if ack.Team.ID != "ux-pass" || ack.Team.State != store.TeamActive {
		t.Errorf("team summary off: %+v", ack.Team)
	}
	if ack.WorkspaceRef != "workspace:cli-1" || ack.ManagerPane != "pane:cli-3" {
		t.Errorf("refs off: ws=%q pane=%q", ack.WorkspaceRef, ack.ManagerPane)
	}
	if _, err := os.Stat(ack.BootstrapPath); err != nil {
		t.Errorf("bootstrap missing: %v", err)
	}
	if _, err := os.Stat(ack.CharterPath); err != nil {
		t.Errorf("charter missing: %v", err)
	}

	// Charter under vault.
	pp := paths.ForProject(dataRoot, vault, "cliproj")
	wantCharter := filepath.Join(pp.VaultRoot, "teams", "ux-pass", "charter.md")
	if ack.CharterPath != wantCharter {
		t.Errorf("charter path = %q, want %q", ack.CharterPath, wantCharter)
	}
}

func TestCmdTeamSpawnRequiresFlags(t *testing.T) {
	cli := newTeamOKCmux()
	cases := []struct {
		name string
		args []string
	}{
		{"no-slug", []string{"--project", "p", "--vault-root", "/v", "--data-root", "/d"}},
		{"no-project", []string{"--slug", "x", "--vault-root", "/v", "--data-root", "/d"}},
		{"no-vault", []string{"--slug", "x", "--project", "p", "--data-root", "/d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdTeamSpawn(tc.args, &out, cli); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestCmdTeamSpawnRejectsBadSlug(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	var out bytes.Buffer
	err := cmdTeamSpawn(
		[]string{
			"--project", "p", "--data-root", dataRoot, "--vault-root", vault,
			"--slug", "../evil", "--agent", "claude",
		},
		&out,
		newTeamOKCmux(),
	)
	if err == nil {
		t.Fatal("expected error for slug ../evil")
	}
}

func TestCmdTeamSpawnRejectsBadProject(t *testing.T) {
	var out bytes.Buffer
	err := cmdTeamSpawn(
		[]string{
			"--project", "../evil", "--data-root", "/d", "--vault-root", "/v",
			"--slug", "ok", "--agent", "claude",
		},
		&out,
		newTeamOKCmux(),
	)
	if err == nil {
		t.Fatal("expected error for project ../evil")
	}
}

func TestCmdTeamSpawnRejectsDuplicate(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	args := []string{
		"--project", "p", "--data-root", dataRoot, "--vault-root", vault,
		"--slug", "dup", "--vision", "v", "--agent", "claude",
	}
	var out bytes.Buffer
	if err := cmdTeamSpawn(args, &out, newTeamOKCmux()); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	out.Reset()
	if err := cmdTeamSpawn(args, &out, newTeamOKCmux()); err == nil {
		t.Fatal("expected error on duplicate slug")
	}
}

func TestCmdTeamList(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	preseedTeam(t, dataRoot, vault, "p", "alpha", "first")
	preseedTeam(t, dataRoot, vault, "p", "beta", "second")

	var out bytes.Buffer
	if err := cmdTeamList(
		[]string{"--project", "p", "--data-root", dataRoot},
		&out,
	); err != nil {
		t.Fatalf("cmdTeamList: %v", err)
	}
	var got struct {
		Teams []store.Team `json:"teams"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, out.String())
	}
	if len(got.Teams) != 2 {
		t.Errorf("got %d teams, want 2; raw=%s", len(got.Teams), out.String())
	}
	seen := map[string]bool{}
	for _, tm := range got.Teams {
		seen[tm.ID] = true
		if tm.State != store.TeamActive {
			t.Errorf("team %q state = %q, want %q", tm.ID, tm.State, store.TeamActive)
		}
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Errorf("missing seeded teams: seen=%v", seen)
	}
}

func TestCmdTeamListFiltersByState(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	preseedTeam(t, dataRoot, vault, "p", "active1", "v")

	// Archive one team directly via store. Each open/close is sequential —
	// bbolt holds an exclusive file lock and preseedTeam opens its own
	// handle, so we cannot keep this one open across the next preseed.
	func() {
		db, _, err := openProjectDB(dataRoot, "p")
		if err != nil {
			t.Fatalf("openProjectDB: %v", err)
		}
		defer db.Close()
		tm, _ := db.GetTeam("active1")
		tm.State = store.TeamArchived
		if err := db.PutTeam(tm); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}()

	preseedTeam(t, dataRoot, vault, "p", "active2", "v")
	preseedTeam(t, dataRoot, vault, "p", "active3", "v")

	var out bytes.Buffer
	if err := cmdTeamList(
		[]string{"--project", "p", "--data-root", dataRoot, "--state", store.TeamActive},
		&out,
	); err != nil {
		t.Fatalf("cmdTeamList: %v", err)
	}
	var got struct {
		Teams []store.Team `json:"teams"`
	}
	_ = json.Unmarshal(out.Bytes(), &got)
	if len(got.Teams) != 2 {
		t.Errorf("active-only filter: got %d, want 2 (active2 + active3); raw=%s", len(got.Teams), out.String())
	}
	for _, tm := range got.Teams {
		if tm.State != store.TeamActive {
			t.Errorf("filtered team %q state = %q, want active", tm.ID, tm.State)
		}
	}
}

func TestCmdTeamGet(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	preseedTeam(t, dataRoot, vault, "p", "lookup-me", "find it")

	var out bytes.Buffer
	if err := cmdTeamGet(
		[]string{"--project", "p", "--data-root", dataRoot, "--slug", "lookup-me"},
		&out,
	); err != nil {
		t.Fatalf("cmdTeamGet: %v", err)
	}
	var got struct {
		Team store.Team `json:"team"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Team.ID != "lookup-me" || got.Team.Vision != "find it" {
		t.Errorf("got %+v", got.Team)
	}
}

func TestCmdTeamGetMissing(t *testing.T) {
	dataRoot := t.TempDir()
	// Open + close so state.bolt exists with empty teams bucket.
	db, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	err = cmdTeamGet(
		[]string{"--project", "p", "--data-root", dataRoot, "--slug", "ghost"},
		&out,
	)
	if err == nil {
		t.Fatal("expected error for missing slug")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err message missing 'not found': %v", err)
	}
}

func TestCmdTeamGetRejectsBadSlug(t *testing.T) {
	dataRoot := t.TempDir()
	// Create the bolt file so the error must come from slug validation,
	// not from openProjectDB.
	db, _, err := openProjectDB(dataRoot, "p")
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	db.Close()

	var out bytes.Buffer
	err = cmdTeamGet(
		[]string{"--project", "p", "--data-root", dataRoot, "--slug", "../escape"},
		&out,
	)
	if err == nil {
		t.Fatal("expected validation error for slug ../escape")
	}
	if !strings.Contains(err.Error(), "invalid project slug") {
		t.Errorf("err should surface slug validation, got: %v", err)
	}
}

func TestCmdTeamGetRequiresFlags(t *testing.T) {
	var out bytes.Buffer
	if err := cmdTeamGet([]string{"--project", "p", "--data-root", "/d"}, &out); err == nil {
		t.Error("expected error for missing --slug")
	}
	if err := cmdTeamGet([]string{"--slug", "x", "--data-root", "/d"}, &out); err == nil {
		t.Error("expected error for missing --project")
	}
}

// TestTeamE2EBinaryReadback builds bin/arcmux-cli and uses it to read the
// teams seeded in-process. `team spawn` itself is not exercised through the
// binary because the spawn flow calls real cmux; faking cmux across a
// subprocess boundary buys nothing the in-process tests don't already
// cover. The binary path under test here is the JSON shape of list/get,
// which is what every spawned role-holder will actually see.
func TestTeamE2EBinaryReadback(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e — requires building arcmux-cli binary")
	}
	bin := filepath.Join(t.TempDir(), "arcmux-cli")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build arcmux-cli: %v", err)
	}

	dataRoot := t.TempDir()
	vault := t.TempDir()
	preseedTeam(t, dataRoot, vault, "e2eproj", "alpha", "deliver A")
	preseedTeam(t, dataRoot, vault, "e2eproj", "beta", "deliver B")

	list := exec.Command(bin, "team", "list",
		"--project", "e2eproj", "--data-root", dataRoot)
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("team list: %v", err)
	}
	var listOut struct {
		Teams []store.Team `json:"teams"`
	}
	if err := json.Unmarshal(out, &listOut); err != nil {
		t.Fatalf("decode list: %v\nraw: %s", err, out)
	}
	if len(listOut.Teams) != 2 {
		t.Fatalf("binary list returned %d teams, want 2; raw=%s", len(listOut.Teams), out)
	}

	get := exec.Command(bin, "team", "get",
		"--project", "e2eproj", "--data-root", dataRoot, "--slug", "alpha")
	get.Stderr = os.Stderr
	out, err = get.Output()
	if err != nil {
		t.Fatalf("team get: %v", err)
	}
	var getOut struct {
		Team store.Team `json:"team"`
	}
	if err := json.Unmarshal(out, &getOut); err != nil {
		t.Fatalf("decode get: %v\nraw: %s", err, out)
	}
	if getOut.Team.ID != "alpha" || getOut.Team.Vision != "deliver A" {
		t.Errorf("binary team get returned %+v", getOut.Team)
	}
}

func TestCmdTeamDispatcher(t *testing.T) {
	var out bytes.Buffer
	if err := cmdTeam([]string{}, &out); err == nil {
		t.Error("expected error for empty args")
	}
	if err := cmdTeam([]string{"frobnicate"}, &out); err == nil {
		t.Error("expected error for unknown subcommand")
	}
}
