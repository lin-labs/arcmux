package manager

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lin-labs/arcmux/internal/manager/scaffold"
)

// CmdManager parses args and runs the manager-mode launcher.
//
// Usage: arcmux manager <agent> <project> [flags...]
// Flags may appear before or after the positional args.
func CmdManager(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	mission := fs.String("mission", "", "initial mission statement (free text)")
	dataRoot := fs.String("data-root", "", "override data root (default $HOME/data)")
	vaultRoot := fs.String("vault-root", os.Getenv("OBS_AGENTS"), "override vault root (default $OBS_AGENTS)")
	updateRoles := fs.Bool("update-roles", false, "overwrite global role-file seeds with the binary's embedded versions")
	focus := fs.Bool("focus", true, "focus the new cmux workspace after creation")

	// Pre-split positionals from flags so users can write them in either order.
	flagArgs, positional := splitFlagsAndPositionals(args, fs)

	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) < 2 {
		return fmt.Errorf("usage: arcmux manager <agent> <project> [--mission \"...\"] [--update-roles] [--focus=false]")
	}
	agent, project := positional[0], positional[1]

	var scaffoldOpts []scaffold.Opt
	if *updateRoles {
		scaffoldOpts = append(scaffoldOpts, scaffold.WithUpdateRoles())
	}

	p, err := Start(ctx, Options{
		Agent:        agent,
		Project:      project,
		Mission:      *mission,
		DataRoot:     *dataRoot,
		VaultRoot:    *vaultRoot,
		Focus:        *focus,
		ScaffoldOpts: scaffoldOpts,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Fprintf(stdout, "manager mode started: project=%s agent=%s workspace=%s elon-pane=%s\n",
		p.Paths.Project, p.Opts.Agent, p.Workspace.Ref, p.ElonPane.Ref)
	fmt.Fprintf(stdout, "bootstrap script: %s\n", p.BootstrapPath)
	fmt.Fprintf(stdout, "journal:          %s\n", p.Paths.ElonDir+"/journal.md")
	fmt.Fprintf(stdout, "decisions:        %s\n", p.Paths.ElonDir+"/decisions.md")
	return nil
}

// splitFlagsAndPositionals separates a mixed args slice into flag args
// (recognized by the given FlagSet) and positionals. Lets the user mix
// orderings — `arcmux manager claude foo --update-roles` and
// `arcmux manager --update-roles claude foo` both work.
//
// Recognized flag forms: --flag, --flag=value, --flag value (for non-bool
// flags), and the single-dash equivalents.
func splitFlagsAndPositionals(args []string, fs *flag.FlagSet) (flagArgs, positional []string) {
	knownFlags := map[string]bool{}
	knownBoolFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		knownFlags[f.Name] = true
		if bf, ok := f.Value.(boolFlagLike); ok && bf.IsBoolFlag() {
			knownBoolFlags[f.Name] = true
		}
	})

	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			// Treat everything after -- as positional.
			positional = append(positional, args[i+1:]...)
			return
		case strings.HasPrefix(a, "--") || (strings.HasPrefix(a, "-") && a != "-"):
			name := strings.TrimLeft(a, "-")
			if eq := strings.Index(name, "="); eq >= 0 {
				// --flag=value form: always self-contained.
				flagArgs = append(flagArgs, a)
				i++
				continue
			}
			if !knownFlags[name] {
				// Unknown flag — pass through to FlagSet so it can error
				// instead of silently treating as positional.
				flagArgs = append(flagArgs, a)
				i++
				continue
			}
			flagArgs = append(flagArgs, a)
			if !knownBoolFlags[name] && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i += 2
				continue
			}
			i++
		default:
			positional = append(positional, a)
			i++
		}
	}
	return
}

type boolFlagLike interface{ IsBoolFlag() bool }
