// Package scenarios implements the concrete e2e scenarios the harness
// dispatches. Each scenario follows SETUP → ACT → ASSERT → TEARDOWN; the
// Env owns teardown, so scenarios just return an error on failure.
package scenarios

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/e2e"
	"github.com/lin-labs/arcmux/internal/manager/bootstrap"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// Bootstrap proves the project scaffolding + mission-delivery sequence
// works end-to-end. ACT uses the substrate primitives the same way
// `arcmux manager` does, but without going through cmux — that's covered
// by the team-spawn-pipeline scenario, where cmux is the system under
// test. Keeping Bootstrap cmux-free keeps it fast (<2s) and independent
// of whether cmux is running on the test box.
type Bootstrap struct{}

func (Bootstrap) Name() string { return "bootstrap" }

func (Bootstrap) Run(ctx context.Context, env *e2e.Env, log io.Writer) error {
	mission := "e2e bootstrap mission — verify substrate scaffolding"

	// ACT 1: scaffold durable + ephemeral dirs + seed role files.
	pp := paths.ForProject(env.DataRoot, env.VaultRoot, env.ProjectSlug)
	if err := scaffold.Project(pp, env.VaultRoot, mission); err != nil {
		return fmt.Errorf("scaffold: %w", err)
	}

	// ACT 2: open store + seed mission as an inbox "add" message + write
	// ProjectMeta singleton — same shape as manager.Start (project.go).
	db, err := store.Open(pp.StateBolt)
	if err != nil {
		return fmt.Errorf("store open: %w", err)
	}
	defer db.Close()

	missionID, err := store.NewInboxID()
	if err != nil {
		return fmt.Errorf("inbox id: %w", err)
	}
	if err := db.PushElonInbox(store.InboxMsg{
		ID:         missionID,
		Verb:       "add",
		From:       "user",
		Body:       mission,
		ReceivedAt: time.Now(),
	}); err != nil {
		return fmt.Errorf("push mission: %w", err)
	}

	// ACT 3: render Elon bootstrap script (the artifact cmux would have
	// run as the workspace's initial command).
	roleFile := filepath.Join(paths.GlobalRolesDir(env.VaultRoot), "elon.md")
	scriptPath, err := bootstrap.Render(bootstrap.Options{
		Agent:     "claude",
		Project:   env.ProjectSlug,
		Role:      "elon",
		EphemRoot: pp.EphemeralRoot,
		VaultRoot: env.VaultRoot,
		DataRoot:  env.DataRoot,
		RoleFile:  roleFile,
	})
	if err != nil {
		return fmt.Errorf("render bootstrap: %w", err)
	}

	// ACT 4: persist project meta — pulses + future heartbeats locate
	// Elon through this singleton. Use a placeholder pane ref since we
	// aren't going through cmux in this scenario.
	fakePane := e2e.FormatPaneRef(0)
	if err := db.PutProjectMeta(store.ProjectMeta{
		ElonPaneRef:      fakePane,
		ElonSurfaceRef:   "surface:99000",
		ElonWorkspaceRef: "workspace:99000",
	}); err != nil {
		return fmt.Errorf("put project meta: %w", err)
	}

	// ASSERT: every observable side effect that a human running
	// `arcmux manager claude <project>` would expect to find on disk.
	if _, err := os.Stat(pp.StateBolt); err != nil {
		return fmt.Errorf("assert: state.bolt missing at %s: %w", pp.StateBolt, err)
	}

	msgs, err := db.PeekElonInbox(10)
	if err != nil {
		return fmt.Errorf("assert: peek elon inbox: %w", err)
	}
	var found *store.InboxMsg
	for i := range msgs {
		if msgs[i].ID == missionID {
			found = &msgs[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("assert: mission inbox msg %q not found (have %d msgs)", missionID, len(msgs))
	}
	if found.Verb != "add" {
		return fmt.Errorf("assert: mission verb=%q want \"add\"", found.Verb)
	}
	if found.From != "user" {
		return fmt.Errorf("assert: mission from=%q want \"user\"", found.From)
	}
	if !strings.Contains(found.Body, "bootstrap mission") {
		return fmt.Errorf("assert: mission body lost: %q", found.Body)
	}

	missionFile := filepath.Join(pp.ArcmuxDir, "mission.md")
	mb, err := os.ReadFile(missionFile)
	if err != nil {
		return fmt.Errorf("assert: read mission.md: %w", err)
	}
	if !strings.Contains(string(mb), "bootstrap mission") {
		return fmt.Errorf("assert: mission.md missing seeded text:\n%s", string(mb))
	}

	si, err := os.Stat(scriptPath)
	if err != nil {
		return fmt.Errorf("assert: bootstrap script missing: %w", err)
	}
	if si.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("assert: bootstrap script %s not executable (mode=%v)", scriptPath, si.Mode())
	}
	scriptBody, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("assert: read script: %w", err)
	}
	for _, must := range []string{
		"ARCMUX_PROJECT=",
		env.ProjectSlug,
		"ARCMUX_ROLE=",
		"ARCMUX_VAULT=",
		"ARCMUX_DATA=",
		"--append-system-prompt-file",
		roleFile,
	} {
		if !strings.Contains(string(scriptBody), must) {
			return fmt.Errorf("assert: bootstrap script missing %q:\n%s", must, string(scriptBody))
		}
	}

	// Role files seeded on first run. The scaffolder embeds five today.
	for _, role := range []string{"elon", "manager", "ic-base", "validator", "coach"} {
		rf := filepath.Join(paths.GlobalRolesDir(env.VaultRoot), role+".md")
		fi, err := os.Stat(rf)
		if err != nil {
			return fmt.Errorf("assert: role file %s missing: %w", rf, err)
		}
		if fi.Size() < 100 {
			return fmt.Errorf("assert: role file %s suspiciously small (%d bytes)", rf, fi.Size())
		}
	}

	meta, err := db.GetProjectMeta()
	if err != nil {
		return fmt.Errorf("assert: get project meta: %w", err)
	}
	if meta.ElonPaneRef != fakePane {
		return fmt.Errorf("assert: project meta pane mismatch: got %q want %q", meta.ElonPaneRef, fakePane)
	}

	fmt.Fprintf(log, "bootstrap PASS: state.bolt=%s mission_id=%s script=%s\n",
		pp.StateBolt, missionID, scriptPath)
	return nil
}
