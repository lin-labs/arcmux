package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lin-labs/arcmux/internal/e2e"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// TeamSpawnPipeline exercises the full reactive team-spawn primitive
// end-to-end against a real cmux daemon. The scenario calls
// `arcmux-call team spawn` directly (bypassing Elon) so the substrate
// surface itself is under test.
//
// ACT: spawn one team in a freshly scaffolded project.
// ASSERT (six invariants, all of which must hold):
//
//	(a) Team record persisted in bbolt with state=active, HC=0,
//	    non-empty WorkspaceRef + ManagerPane.
//	(b) cmux list-workspaces shows a workspace named "team: <slug>".
//	(c) charter.md materialized in the vault at
//	    <vault>/Projects/<project>/teams/<slug>/charter.md.
//	(d) Per-team manager inbox bucket created (HasManagerInbox=true).
//	(e) Vision delivered as the first inbox message (verb="add",
//	    from="elon", body matches what we passed).
//	(f) Audit log shows team-spawned + manager inbox creation. The
//	    manager-inbox creation isn't its own audit row today —
//	    EnsureManagerInbox is silent because team-spawned already
//	    implies it. We assert team-spawned + a HasManagerInbox check.
//
// Notes:
//   - Slug embeds env.WorkspacePrefix so the teardown's cmux scan
//     finds "team: e2e-…" cleanly.
//   - Agent="claude". The bootstrap script will `exec claude ...`;
//     claude almost certainly isn't installed inside the spawned
//     workspace's PATH in CI, so the inner process dies. The cmux
//     workspace + pane still exist — that's all our assertions need.
type TeamSpawnPipeline struct{}

func (TeamSpawnPipeline) Name() string { return "team-spawn-pipeline" }

func (TeamSpawnPipeline) Run(ctx context.Context, env *e2e.Env, log io.Writer) error {
	pp := paths.ForProject(env.DataRoot, env.VaultRoot, env.ProjectSlug)
	if err := scaffold.Project(pp, env.VaultRoot, "e2e team-spawn-pipeline mission"); err != nil {
		return fmt.Errorf("scaffold: %w", err)
	}

	teamSlug := env.WorkspacePrefix + "team"
	// paths.Validate caps at 64 chars; truncate cleanly if needed.
	if len(teamSlug) > 64 {
		teamSlug = teamSlug[:64]
		teamSlug = strings.TrimRight(teamSlug, "-_.")
	}

	vision := "e2e team-spawn-pipeline vision — exercise the full reactive primitive"

	// Pre-check: cmux must be reachable, else the scenario is meaningless.
	if err := exec.Command("cmux", "list-workspaces").Run(); err != nil {
		return fmt.Errorf("cmux not reachable (start cmux first): %w", err)
	}

	// ACT: invoke `arcmux-call team spawn`. Use --focus=false so the
	// scenario doesn't steal the user's focused workspace.
	out, err := env.RunCall(ctx,
		"team", "spawn",
		"--slug", teamSlug,
		"--project", env.ProjectSlug,
		"--vision", vision,
		"--agent", "claude",
		"--data-root", env.DataRoot,
		"--vault-root", env.VaultRoot,
		"--focus=false",
	)
	if err != nil {
		return fmt.Errorf("team spawn: %w", err)
	}
	var spawnResult struct {
		OK             bool       `json:"ok"`
		Team           store.Team `json:"team"`
		WorkspaceRef   string     `json:"workspace_ref"`
		ManagerPane    string     `json:"manager_pane"`
		BootstrapPath  string     `json:"bootstrap_path"`
		ScratchpadPath string     `json:"scratchpad_path"`
		CharterPath    string     `json:"charter_path"`
	}
	if err := json.Unmarshal(out, &spawnResult); err != nil {
		return fmt.Errorf("decode spawn output: %w (raw=%s)", err, string(out))
	}
	if !spawnResult.OK {
		return fmt.Errorf("spawn returned ok=false")
	}

	// ASSERT (a) team record persisted with the right shape.
	db, err := store.Open(pp.StateBolt)
	if err != nil {
		return fmt.Errorf("assert: store reopen: %w", err)
	}
	defer db.Close()

	team, err := db.GetTeam(teamSlug)
	if err != nil {
		return fmt.Errorf("assert: get team %s: %w", teamSlug, err)
	}
	if team.State != store.TeamActive {
		return fmt.Errorf("assert: team state=%q want %q", team.State, store.TeamActive)
	}
	if team.HC != 0 {
		return fmt.Errorf("assert: team hc=%d want 0", team.HC)
	}
	if team.WorkspaceRef == "" || team.ManagerPane == "" {
		return fmt.Errorf("assert: team has empty workspace_ref=%q or manager_pane=%q",
			team.WorkspaceRef, team.ManagerPane)
	}
	if team.WorkspaceRef != spawnResult.WorkspaceRef {
		return fmt.Errorf("assert: team workspace_ref %q != spawn output %q",
			team.WorkspaceRef, spawnResult.WorkspaceRef)
	}

	// ASSERT (b) cmux workspace exists with the expected name.
	wsBytes, err := exec.Command("cmux", "list-workspaces").Output()
	if err != nil {
		return fmt.Errorf("assert: cmux list-workspaces: %w", err)
	}
	wantName := "team: " + teamSlug
	if !strings.Contains(string(wsBytes), wantName) {
		return fmt.Errorf("assert: cmux workspace %q not listed:\n%s", wantName, string(wsBytes))
	}

	// ASSERT (c) charter.md materialized + contains the vision.
	charterPath := filepath.Join(pp.TeamsDir, teamSlug, "charter.md")
	if charterPath != spawnResult.CharterPath {
		return fmt.Errorf("assert: charter path %q != spawn output %q",
			charterPath, spawnResult.CharterPath)
	}
	cb, err := os.ReadFile(charterPath)
	if err != nil {
		return fmt.Errorf("assert: read charter: %w", err)
	}
	if !strings.Contains(string(cb), vision) {
		return fmt.Errorf("assert: charter missing vision:\n%s", string(cb))
	}

	// ASSERT (d) manager inbox bucket exists.
	if !db.HasManagerInbox(teamSlug) {
		return fmt.Errorf("assert: manager inbox bucket missing for %s", teamSlug)
	}

	// ASSERT (e) vision delivered as first inbox message.
	msgs, err := db.PeekManagerInbox(teamSlug, 5)
	if err != nil {
		return fmt.Errorf("assert: peek manager inbox: %w", err)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("assert: manager inbox empty — vision not delivered")
	}
	first := msgs[0]
	if first.Verb != "add" {
		return fmt.Errorf("assert: vision verb=%q want \"add\"", first.Verb)
	}
	if first.From != "elon" {
		return fmt.Errorf("assert: vision from=%q want \"elon\"", first.From)
	}
	if !strings.Contains(first.Body, "team-spawn-pipeline vision") {
		return fmt.Errorf("assert: vision body lost: %q", first.Body)
	}

	// ASSERT (f) audit log shows team-spawned.
	entries, err := db.RecentAudit(50)
	if err != nil {
		return fmt.Errorf("assert: recent audit: %w", err)
	}
	var sawSpawn bool
	for _, e := range entries {
		if e.Action == "team-spawned" && e.Subject == teamSlug {
			sawSpawn = true
			break
		}
	}
	if !sawSpawn {
		return fmt.Errorf("assert: no team-spawned audit row for %s (have %d entries)",
			teamSlug, len(entries))
	}

	fmt.Fprintf(log, "team-spawn-pipeline PASS: slug=%s workspace=%s manager_pane=%s\n",
		teamSlug, team.WorkspaceRef, team.ManagerPane)
	return nil
}
