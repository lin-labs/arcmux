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
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/manager/teamspawn"
	"github.com/lin-labs/arcmux/internal/mux"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
)

// cmdTeam dispatches `arcmux-cli team <sub>`.
func cmdTeam(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-cli team spawn|list|get [flags]")
	}
	switch args[0] {
	case "spawn":
		return cmdTeamSpawn(args[1:], stdout, cmuxbackend.New(cmuxcli.New()))
	case "list":
		return cmdTeamList(args[1:], stdout)
	case "get":
		return cmdTeamGet(args[1:], stdout)
	default:
		return fmt.Errorf("unknown team subcommand %q (want spawn|list|get)", args[0])
	}
}

// cmdTeamSpawn handles `arcmux-cli team spawn`. The mux.Backend is
// injected so tests can supply a fake-backed backend; production callers
// go through cmdTeam → cmuxbackend.New(cmuxcli.New()) above.
func cmdTeamSpawn(args []string, stdout io.Writer, backend mux.Backend) error {
	fs := flag.NewFlagSet("team spawn", flag.ContinueOnError)
	slug := fs.String("slug", "", "team slug (required)")
	vision := fs.String("vision", "", "team vision/mission (free text)")
	agent := fs.String("agent", agentFromEnv(), "agent CLI to launch (claude|codex; default $ARCMUX_AGENT or claude)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	vaultRoot := fs.String("vault-root", defaultVaultRoot(), "vault root (default $ARCMUX_VAULT or $OBS_AGENTS)")
	focus := fs.Bool("focus", false, "focus the new cmux workspace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("team spawn: --slug required")
	}
	if *project == "" {
		return fmt.Errorf("team spawn: --project or $ARCMUX_PROJECT required")
	}
	if *vaultRoot == "" {
		return fmt.Errorf("team spawn: --vault-root or $ARCMUX_VAULT/$OBS_AGENTS required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	r, err := teamspawn.Spawn(context.Background(), teamspawn.Opts{
		DB:        db,
		Mux:       backend,
		Project:   *project,
		Slug:      *slug,
		Vision:    *vision,
		Agent:     *agent,
		VaultRoot: *vaultRoot,
		DataRoot:  *dataRoot,
		Focus:     *focus,
	})
	if err != nil {
		return fmt.Errorf("team spawn: %w", err)
	}

	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":              true,
		"team":            r.Team,
		"workspace_ref":   r.Group.Ref,
		"manager_pane":    r.ManagerPane.Ref,
		"bootstrap_path":  r.BootstrapPath,
		"scratchpad_path": r.ScratchpadPath,
		"charter_path":    r.CharterPath,
	})
}

func cmdTeamList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("team list", flag.ContinueOnError)
	state := fs.String("state", "", "filter by team state (empty = all)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("team list: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	teams, err := db.ListTeams(*state)
	if err != nil {
		return fmt.Errorf("team list: %w", err)
	}
	if teams == nil {
		teams = []store.Team{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"teams": teams})
}

func cmdTeamGet(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("team get", flag.ContinueOnError)
	slug := fs.String("slug", "", "team slug (required)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *slug == "" {
		return fmt.Errorf("team get: --slug required")
	}
	if *project == "" {
		return fmt.Errorf("team get: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	// Validate the slug at the CLI boundary too: defense-in-depth so a bad
	// slug returns a clear validation error rather than a misleading
	// "not found" from the underlying bbolt lookup.
	if _, err := paths.Validate(*slug); err != nil {
		return fmt.Errorf("team get: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	team, err := db.GetTeam(*slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("team get: slug %q not found", *slug)
		}
		return fmt.Errorf("team get: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"team": team})
}

func agentFromEnv() string {
	if v := os.Getenv("ARCMUX_AGENT"); v != "" {
		return v
	}
	return "claude"
}

func defaultVaultRoot() string {
	if v := os.Getenv("ARCMUX_VAULT"); v != "" {
		return v
	}
	return os.Getenv("OBS_AGENTS")
}
