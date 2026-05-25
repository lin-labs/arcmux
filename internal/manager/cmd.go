package manager

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

// CmdManager parses args and runs the manager-mode launcher.
//
// Usage: arcmux manager <agent> <project> [--mission "..."]
func CmdManager(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	mission := fs.String("mission", "", "initial mission statement (free text)")
	dataRoot := fs.String("data-root", "", "override data root (default $HOME/data)")
	vaultRoot := fs.String("vault-root", os.Getenv("OBS_AGENTS"), "override vault root (default $OBS_AGENTS)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: arcmux manager <agent> <project> [--mission \"...\"]")
	}
	agent, project := rest[0], rest[1]

	p, err := Start(ctx, Options{
		Agent:     agent,
		Project:   project,
		Mission:   *mission,
		DataRoot:  *dataRoot,
		VaultRoot: *vaultRoot,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Fprintf(stdout, "manager mode started: project=%s agent=%s workspace=%s elon-pane=%s\n",
		p.Paths.Project, p.Opts.Agent, p.Workspace.Ref, p.ElonPane.Ref)
	return nil
}
