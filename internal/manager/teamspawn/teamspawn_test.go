package teamspawn

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// fakeRunner mirrors the in-process fake used by manager/project_test.go.
// Kept locally to avoid a test-only import cycle.
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

func okCmux() (*fakeRunner, *cmuxcli.Client) {
	f := &fakeRunner{outs: map[string]string{
		"new-workspace": "OK workspace:42\n",
		"list-panes":    `{"workspace_ref":"workspace:42","panes":[{"ref":"pane:11","index":0,"focused":true,"surface_refs":["surface:9"]}]}`,
	}}
	return f, cmuxcli.NewWithRunnerForTest(f)
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	bolt := filepath.Join(t.TempDir(), "state.bolt")
	db, err := store.Open(bolt)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSpawnHappyPath validates the full end-to-end: store record, cmux
// call, charter file, scratchpad file, audit row, ARCMUX_TEAM in script.
func TestSpawnHappyPath(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	f, cli := okCmux()
	db := openTestDB(t)

	r, err := Spawn(context.Background(), Opts{
		DB:        db,
		Cmux:      cli,
		Project:   "demo",
		Slug:      "auth-refactor",
		Vision:    "ship the new oauth bridge",
		Agent:     "claude",
		VaultRoot: vault,
		DataRoot:  dataRoot,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Team record persisted with active state.
	if r.Team.ID != "auth-refactor" {
		t.Errorf("Team.ID = %q, want %q", r.Team.ID, "auth-refactor")
	}
	if r.Team.State != store.TeamActive {
		t.Errorf("Team.State = %q, want %q", r.Team.State, store.TeamActive)
	}
	if r.Team.HC != 0 {
		t.Errorf("Team.HC = %d, want 0", r.Team.HC)
	}
	if r.Team.WorkspaceRef != "workspace:42" {
		t.Errorf("Team.WorkspaceRef = %q, want workspace:42", r.Team.WorkspaceRef)
	}
	if r.Team.ManagerPane != "pane:11" {
		t.Errorf("Team.ManagerPane = %q, want pane:11", r.Team.ManagerPane)
	}
	if r.Team.Vision != "ship the new oauth bridge" {
		t.Errorf("Team.Vision = %q, want raw vision preserved", r.Team.Vision)
	}

	// Round-trips via store.
	got, err := db.GetTeam("auth-refactor")
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if got.WorkspaceRef != "workspace:42" || got.ManagerPane != "pane:11" {
		t.Errorf("GetTeam returned %+v", got)
	}

	// cmux saw new-workspace with the right bootstrap script + name.
	var wsCall string
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "new-workspace") {
			wsCall = j
		}
	}
	if wsCall == "" {
		t.Fatal("expected new-workspace call")
	}
	if !strings.Contains(wsCall, "team: auth-refactor") {
		t.Errorf("new-workspace name missing 'team: <slug>': %q", wsCall)
	}
	if !strings.Contains(wsCall, "bootstrap-manager-auth-refactor.sh") {
		t.Errorf("new-workspace missing per-team bootstrap script: %q", wsCall)
	}
	if !strings.Contains(wsCall, vault) {
		t.Errorf("new-workspace CWD missing vault root: %q", wsCall)
	}

	// Bootstrap script on disk carries ARCMUX_TEAM.
	if _, err := os.Stat(r.BootstrapPath); err != nil {
		t.Fatalf("bootstrap script missing: %v", err)
	}
	body, err := os.ReadFile(r.BootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	bs := string(body)
	for _, want := range []string{
		"export ARCMUX_TEAM='auth-refactor'",
		"export ARCMUX_ROLE='manager'",
		"export ARCMUX_PROJECT='demo'",
		"export ARCMUX_AGENT='claude'",
		"manager.md",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("bootstrap missing %q\n---\n%s", want, bs)
		}
	}

	// Charter on disk with the vision and slug header.
	if _, err := os.Stat(r.CharterPath); err != nil {
		t.Fatalf("charter missing: %v", err)
	}
	charter, _ := os.ReadFile(r.CharterPath)
	cs := string(charter)
	if !strings.Contains(cs, "ship the new oauth bridge") {
		t.Errorf("charter missing vision body:\n%s", cs)
	}
	if !strings.Contains(cs, "# Team auth-refactor — Charter") {
		t.Errorf("charter missing slug-titled header:\n%s", cs)
	}

	// Scratchpad on disk: manager-<slug>.json, JSON-parseable, vision_seeded=true.
	if !strings.HasSuffix(r.ScratchpadPath, filepath.Join("scratchpads", "manager-auth-refactor.json")) {
		t.Errorf("scratchpad path suffix unexpected: %q", r.ScratchpadPath)
	}
	pad, err := os.ReadFile(r.ScratchpadPath)
	if err != nil {
		t.Fatalf("read scratchpad: %v", err)
	}
	var parsed struct {
		Turn      int `json:"turn"`
		Bootstrap struct {
			Project       string `json:"project"`
			Team          string `json:"team"`
			Role          string `json:"role"`
			Agent         string `json:"agent"`
			VisionSeeded  bool   `json:"vision_seeded"`
			VisionBytes   int    `json:"vision_bytes"`
			VisionSHA256  string `json:"vision_sha256"`
		} `json:"bootstrap"`
	}
	if err := json.Unmarshal(pad, &parsed); err != nil {
		t.Fatalf("unmarshal scratchpad: %v\nraw:\n%s", err, pad)
	}
	if parsed.Turn != 0 {
		t.Errorf("scratchpad turn = %d, want 0", parsed.Turn)
	}
	if parsed.Bootstrap.Project != "demo" || parsed.Bootstrap.Team != "auth-refactor" || parsed.Bootstrap.Role != "manager" {
		t.Errorf("scratchpad bootstrap header off: %+v", parsed.Bootstrap)
	}
	if !parsed.Bootstrap.VisionSeeded {
		t.Errorf("vision_seeded = false, want true")
	}
	if parsed.Bootstrap.VisionBytes != len("ship the new oauth bridge") {
		t.Errorf("vision_bytes = %d, want %d", parsed.Bootstrap.VisionBytes, len("ship the new oauth bridge"))
	}
	if len(parsed.Bootstrap.VisionSHA256) != 64 {
		t.Errorf("vision_sha256 length = %d, want 64 hex chars", len(parsed.Bootstrap.VisionSHA256))
	}

	// Audit row carries the team-spawn metadata.
	entries, err := db.RecentAudit(5)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(entries) == 0 || entries[0].Action != "team-spawned" {
		t.Fatalf("expected team-spawned audit, got %+v", entries)
	}
	a := entries[0]
	if got, _ := a.Detail["workspace_ref"].(string); got != "workspace:42" {
		t.Errorf("audit workspace_ref = %q, want workspace:42", got)
	}
	if got, _ := a.Detail["vision_seeded"].(bool); !got {
		t.Errorf("audit vision_seeded = false, want true")
	}
}

func TestSpawnEmptyVision(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	_, cli := okCmux()
	db := openTestDB(t)

	cases := []struct {
		name, slug, vision string
	}{
		{"empty", "no-vision-empty", ""},
		{"whitespace-only", "no-vision-ws", "  \n\t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Spawn(context.Background(), Opts{
				DB: db, Cmux: cli, Project: "p", Slug: tc.slug,
				Vision: tc.vision, Agent: "claude",
				VaultRoot: vault, DataRoot: dataRoot,
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			pad, _ := os.ReadFile(r.ScratchpadPath)
			if !strings.Contains(string(pad), "no vision supplied") {
				t.Errorf("scratchpad missing '(no vision supplied)' marker:\n%s", pad)
			}
			ch, _ := os.ReadFile(r.CharterPath)
			if !strings.Contains(string(ch), "vision not supplied at spawn") {
				t.Errorf("charter missing 'vision not supplied' marker:\n%s", ch)
			}
		})
	}
}

func TestSpawnRejectsBadSlug(t *testing.T) {
	_, cli := okCmux()
	db := openTestDB(t)
	for _, bad := range []string{"../evil", "", "has/slash", "has space", ".dotleading"} {
		t.Run("slug="+bad, func(t *testing.T) {
			_, err := Spawn(context.Background(), Opts{
				DB: db, Cmux: cli, Project: "p", Slug: bad,
				Vision: "x", Agent: "claude",
				VaultRoot: t.TempDir(), DataRoot: t.TempDir(),
			})
			if err == nil {
				t.Errorf("expected error for slug %q", bad)
			}
		})
	}
}

func TestSpawnRejectsBadProject(t *testing.T) {
	_, cli := okCmux()
	db := openTestDB(t)
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "../evil", Slug: "t",
		Vision: "x", Agent: "claude",
		VaultRoot: t.TempDir(), DataRoot: t.TempDir(),
	})
	if err == nil {
		t.Error("expected error for invalid project slug")
	}
}

func TestSpawnRequiresAgent(t *testing.T) {
	_, cli := okCmux()
	db := openTestDB(t)
	_, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Slug: "t",
		Vision: "x", Agent: "bash",
		VaultRoot: t.TempDir(), DataRoot: t.TempDir(),
	})
	if err == nil {
		t.Error("expected error for unsupported agent")
	}
}

func TestSpawnRequiresVaultAndDataRoot(t *testing.T) {
	_, cli := okCmux()
	db := openTestDB(t)
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Slug: "t",
		Vision: "x", Agent: "claude", DataRoot: t.TempDir(),
	}); err == nil {
		t.Error("expected error for missing VaultRoot")
	}
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Slug: "t",
		Vision: "x", Agent: "claude", VaultRoot: t.TempDir(),
	}); err == nil {
		t.Error("expected error for missing DataRoot")
	}
}

func TestSpawnRejectsDuplicateActive(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	_, cli := okCmux()
	db := openTestDB(t)

	common := Opts{
		DB: db, Cmux: cli, Project: "p", Slug: "dup",
		Vision: "v", Agent: "claude",
		VaultRoot: vault, DataRoot: dataRoot,
	}
	if _, err := Spawn(context.Background(), common); err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	_, err := Spawn(context.Background(), common)
	if err == nil {
		t.Fatal("expected ErrTeamExists for duplicate active slug")
	}
	if !errors.Is(err, ErrTeamExists) {
		t.Errorf("err = %v, want errors.Is ErrTeamExists", err)
	}
	if !strings.Contains(err.Error(), store.TeamActive) {
		t.Errorf("err missing existing state in message: %v", err)
	}
}

// TestSpawnAllowsRespawnOverArchived covers the tombstone case: a team
// that has been archived can be re-spawned under the same slug.
func TestSpawnAllowsRespawnOverArchived(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	_, cli := okCmux()
	db := openTestDB(t)

	common := Opts{
		DB: db, Cmux: cli, Project: "p", Slug: "phoenix",
		Vision: "v1", Agent: "claude",
		VaultRoot: vault, DataRoot: dataRoot,
	}
	r1, err := Spawn(context.Background(), common)
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	// Archive it.
	r1.Team.State = store.TeamArchived
	if err := db.PutTeam(r1.Team); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// Respawn — must succeed.
	common.Vision = "v2 — restored"
	r2, err := Spawn(context.Background(), common)
	if err != nil {
		t.Fatalf("respawn over archived: %v", err)
	}
	if r2.Team.State != store.TeamActive {
		t.Errorf("respawned team state = %q, want %q", r2.Team.State, store.TeamActive)
	}
	if r2.Team.Vision != "v2 — restored" {
		t.Errorf("respawn vision not overwritten: %q", r2.Team.Vision)
	}
}

// TestSpawnRejectsNilDeps exercises the required-dependency guards so
// callers get useful errors instead of nil-deref panics.
func TestSpawnRejectsNilDeps(t *testing.T) {
	_, cli := okCmux()
	db := openTestDB(t)
	if _, err := Spawn(context.Background(), Opts{
		DB: nil, Cmux: cli, Project: "p", Slug: "t",
		Vision: "x", Agent: "claude",
		VaultRoot: t.TempDir(), DataRoot: t.TempDir(),
	}); err == nil {
		t.Error("expected error for nil DB")
	}
	if _, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: nil, Project: "p", Slug: "t",
		Vision: "x", Agent: "claude",
		VaultRoot: t.TempDir(), DataRoot: t.TempDir(),
	}); err == nil {
		t.Error("expected error for nil Cmux")
	}
}

// TestSpawnPaths sanity-checks that paths.ForProject resolves identically
// to what Spawn reports back — catches future drift between path-rules
// and the spawn implementation.
func TestSpawnPaths(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	_, cli := okCmux()
	db := openTestDB(t)

	r, err := Spawn(context.Background(), Opts{
		DB: db, Cmux: cli, Project: "p", Slug: "checkpaths",
		Vision: "x", Agent: "claude",
		VaultRoot: vault, DataRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pp := paths.ForProject(dataRoot, vault, "p")
	wantCharter := filepath.Join(pp.TeamsDir, "checkpaths", "charter.md")
	if r.CharterPath != wantCharter {
		t.Errorf("CharterPath = %q, want %q", r.CharterPath, wantCharter)
	}
	wantScratch := filepath.Join(pp.Scratchpads, "manager-checkpaths.json")
	if r.ScratchpadPath != wantScratch {
		t.Errorf("ScratchpadPath = %q, want %q", r.ScratchpadPath, wantScratch)
	}
	wantBootstrap := filepath.Join(pp.EphemeralRoot, "bootstrap-manager-checkpaths.sh")
	if r.BootstrapPath != wantBootstrap {
		t.Errorf("BootstrapPath = %q, want %q", r.BootstrapPath, wantBootstrap)
	}
}
