package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/icspawn"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/mux"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
)

// cmdIC dispatches `arcmux-cli ic <sub>`. Mirrors cmdTeam's shape: the
// `spawn` sub takes an injectable cmuxcli.Client so unit tests can supply
// a fakeRunner-backed client; production callers thread through to
// cmuxcli.New() here.
func cmdIC(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-cli ic spawn|list|get|dissolve [flags]")
	}
	switch args[0] {
	case "spawn":
		return cmdICSpawn(args[1:], stdout, cmuxbackend.New(cmuxcli.New()))
	case "list":
		return cmdICList(args[1:], stdout)
	case "get":
		return cmdICGet(args[1:], stdout)
	case "dissolve":
		return cmdICDissolve(args[1:], stdout, cmuxbackend.New(cmuxcli.New()))
	default:
		return fmt.Errorf("unknown ic subcommand %q (want spawn|list|get|dissolve)", args[0])
	}
}

// cmdICSpawn handles `arcmux-cli ic spawn`.
func cmdICSpawn(args []string, stdout io.Writer, backend mux.Backend) error {
	fs := flag.NewFlagSet("ic spawn", flag.ContinueOnError)
	team := fs.String("team", os.Getenv("ARCMUX_TEAM"), "owning team slug (required; default $ARCMUX_TEAM)")
	slot := fs.String("slot", "", "unique slot id within the project (required, validated as slug)")
	role := fs.String("role", "ic-base", "IC specialization (matches <vault>/0Prompts/roles/<role>.md)")
	contract := fs.String("contract", "", "initial bound contract id (required)")
	agent := fs.String("agent", agentFromEnv(), "agent CLI to launch (claude|codex; default $ARCMUX_AGENT or claude)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	vaultRoot := fs.String("vault-root", defaultVaultRoot(), "vault root (default $ARCMUX_VAULT or $OBS_AGENTS)")
	focus := fs.Bool("focus", false, "focus the new pane after split")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *team == "" {
		return fmt.Errorf("ic spawn: --team or $ARCMUX_TEAM required")
	}
	if _, err := paths.Validate(*team); err != nil {
		return fmt.Errorf("ic spawn: --team: %w", err)
	}
	if *slot == "" {
		return fmt.Errorf("ic spawn: --slot required")
	}
	if _, err := paths.Validate(*slot); err != nil {
		return fmt.Errorf("ic spawn: --slot: %w", err)
	}
	if *contract == "" {
		return fmt.Errorf("ic spawn: --contract required")
	}
	if _, err := paths.Validate(*contract); err != nil {
		return fmt.Errorf("ic spawn: --contract: %w", err)
	}
	if *project == "" {
		return fmt.Errorf("ic spawn: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	if *vaultRoot == "" {
		return fmt.Errorf("ic spawn: --vault-root or $ARCMUX_VAULT/$OBS_AGENTS required")
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	r, err := icspawn.Spawn(context.Background(), icspawn.Opts{
		DB:        db,
		Mux:       backend,
		Project:   *project,
		Team:      *team,
		Slot:      *slot,
		Role:      *role,
		Contract:  *contract,
		Agent:     *agent,
		VaultRoot: *vaultRoot,
		DataRoot:  *dataRoot,
		Focus:     *focus,
	})
	if err != nil {
		return fmt.Errorf("ic spawn: %w", err)
	}

	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":              true,
		"slot":            r.Slot,
		"pane_ref":        r.Pane.Ref,
		"workspace_ref":   r.Slot.WorkspaceRef,
		"bootstrap_path":  r.BootstrapPath,
		"scratchpad_path": r.ScratchpadPath,
		"team_hc":         r.Team.HC,
		"contract": map[string]any{
			"id":    r.Contract.ID,
			"state": r.Contract.State,
		},
	})
}

func cmdICList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ic list", flag.ContinueOnError)
	team := fs.String("team", "", "filter by team slug (empty = all teams)")
	state := fs.String("state", "", "filter by slot state (empty = all)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("ic list: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	if *team != "" {
		if _, err := paths.Validate(*team); err != nil {
			return fmt.Errorf("ic list: --team: %w", err)
		}
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	slots, err := db.ListSlots(*team, *state)
	if err != nil {
		return fmt.Errorf("ic list: %w", err)
	}
	if slots == nil {
		slots = []store.Slot{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"slots": slots})
}

func cmdICGet(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ic get", flag.ContinueOnError)
	slot := fs.String("slot", "", "slot id (required)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slot == "" {
		return fmt.Errorf("ic get: --slot required")
	}
	if *project == "" {
		return fmt.Errorf("ic get: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	// Defense-in-depth: validate slot at the CLI boundary so a bad slug
	// gets a clear error rather than a misleading "not found".
	if _, err := paths.Validate(*slot); err != nil {
		return fmt.Errorf("ic get: --slot: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	got, err := db.GetSlot(*slot)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("ic get: slot %q not found", *slot)
		}
		return fmt.Errorf("ic get: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"slot": got})
}

// cmdICDissolve handles `arcmux-cli ic dissolve --slot <id>`. The mux
// backend is injectable for the same reason cmdICSpawn's is: unit tests
// fake-runner-back the close-pane call, production threads through
// cmuxbackend.New(cmuxcli.New()).
func cmdICDissolve(args []string, stdout io.Writer, backend mux.Backend) error {
	fs := flag.NewFlagSet("ic dissolve", flag.ContinueOnError)
	slot := fs.String("slot", "", "slot id to dissolve (required, validated as slug)")
	by := fs.String("by", defaultActor(), "actor recording the dissolve (default $ARCMUX_ROLE or 'arcmux-cli')")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slot == "" {
		return fmt.Errorf("ic dissolve: --slot required")
	}
	if *project == "" {
		return fmt.Errorf("ic dissolve: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	if _, err := paths.Validate(*slot); err != nil {
		return fmt.Errorf("ic dissolve: --slot: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	r, err := icspawn.Dissolve(context.Background(), icspawn.DissolveOpts{
		DB: db, Mux: backend, Slot: *slot, By: *by,
	})
	if err != nil {
		return fmt.Errorf("ic dissolve: %w", err)
	}
	ack := map[string]any{
		"ok":            true,
		"slot":          r.Slot,
		"team_hc":       r.Team.HC,
		"inbox_dropped": true,
	}
	if r.PaneCloseError != nil {
		// Soft-warn in the JSON envelope so a script can detect a zombie
		// pane without parsing logs. The dissolve itself succeeded.
		ack["pane_close_warning"] = r.PaneCloseError.Error()
	}
	return json.NewEncoder(stdout).Encode(ack)
}
