// Package scenarios implements the concrete substrate scenariotest
// cases the harness dispatches. Each scenario follows SETUP → ACT →
// ASSERT → TEARDOWN; the Env owns teardown, so scenarios just return
// an error on failure.
package scenarios

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/bootstrap"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/scenariotest"
)

// Bootstrap proves the substrate-only invariants of project
// scaffolding + session inbox queueing work end-to-end. ACT uses the
// substrate primitives the same way `manager.RegisterSession` does, but
// without going through cmux.
//
// Post-C3 assertions are substrate-only: state.bolt opens, a mission
// queues onto the project's per-session inbox (no role-class addressing
// involved), the bootstrap script is generated (prompt-agnostic), and
// ProjectMeta singleton roundtrips. Vault-tree scaffolding and any
// agent-shaped state are not arcmux's responsibility — those moved to
// elonco.
type Bootstrap struct{}

func (Bootstrap) Name() string { return "bootstrap" }

func (Bootstrap) Run(ctx context.Context, env *scenariotest.Env, log io.Writer) error {
	mission := "scenariotest bootstrap mission — verify substrate scaffolding"

	// ACT 1: scaffold ephemeral dirs only.
	pp := paths.ForProject(env.DataRoot, env.VaultRoot, env.ProjectSlug)
	if err := scaffold.Project(pp); err != nil {
		return fmt.Errorf("scaffold: %w", err)
	}

	// ACT 2: open store + seed a mission as an "add" message on the
	// project's per-session inbox + write ProjectMeta singleton — the
	// post-C3 shape (no role-class addressing).
	db, err := store.Open(pp.StateBolt)
	if err != nil {
		return fmt.Errorf("store open: %w", err)
	}
	defer db.Close()

	missionID, err := store.NewInboxID()
	if err != nil {
		return fmt.Errorf("inbox id: %w", err)
	}
	if err := db.EnsureSessionInbox(env.ProjectSlug); err != nil {
		return fmt.Errorf("ensure session inbox: %w", err)
	}
	if err := db.PushSessionInbox(env.ProjectSlug, store.InboxMsg{
		ID:         missionID,
		Verb:       "add",
		From:       "user",
		Body:       mission,
		ReceivedAt: time.Now(),
	}); err != nil {
		return fmt.Errorf("push mission: %w", err)
	}

	// ACT 3: render a bootstrap script. arcmux is prompt-agnostic — the
	// caller supplies the exact launch command. Here we use a placeholder
	// to prove the renderer wires env exports + exec line correctly.
	command := "claude --dangerously-skip-permissions --append-system-prompt-file /tmp/placeholder.md"
	scriptPath, err := bootstrap.Render(bootstrap.Options{
		Agent:     "claude",
		Project:   env.ProjectSlug,
		EphemRoot: pp.EphemeralRoot,
		VaultRoot: env.VaultRoot,
		DataRoot:  env.DataRoot,
		Command:   command,
		Env:       map[string]string{"ROLE": "elon"},
	})
	if err != nil {
		return fmt.Errorf("render bootstrap: %w", err)
	}

	// ACT 4: persist project meta — pulses + future heartbeats locate
	// the registered pane through this singleton. Use a placeholder pane
	// ref since we aren't going through cmux in this scenario.
	fakePane := scenariotest.FormatPaneRef(0)
	if err := db.PutProjectMeta(store.ProjectMeta{
		PaneRef:      fakePane,
		SurfaceRef:   "surface:99000",
		WorkspaceRef: "workspace:99000",
	}); err != nil {
		return fmt.Errorf("put project meta: %w", err)
	}

	// ASSERT: substrate-only invariants.
	if _, err := os.Stat(pp.StateBolt); err != nil {
		return fmt.Errorf("assert: state.bolt missing at %s: %w", pp.StateBolt, err)
	}

	msgs, err := db.PeekSessionInbox(env.ProjectSlug, 10)
	if err != nil {
		return fmt.Errorf("assert: peek session inbox: %w", err)
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
		"ARCMUX_AGENT='claude'",
		"ARCMUX_VAULT=",
		"ARCMUX_DATA=",
		"ARCMUX_ROLE='elon'",
		"exec " + command,
	} {
		if !strings.Contains(string(scriptBody), must) {
			return fmt.Errorf("assert: bootstrap script missing %q:\n%s", must, string(scriptBody))
		}
	}

	meta, err := db.GetProjectMeta()
	if err != nil {
		return fmt.Errorf("assert: get project meta: %w", err)
	}
	if meta.PaneRef != fakePane {
		return fmt.Errorf("assert: project meta pane mismatch: got %q want %q", meta.PaneRef, fakePane)
	}

	fmt.Fprintf(log, "bootstrap PASS: state.bolt=%s mission_id=%s script=%s\n",
		pp.StateBolt, missionID, scriptPath)
	return nil
}
