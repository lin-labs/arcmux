package manager

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
)

// CmdManager parses args and runs the manager-mode launcher.
//
// Usage: arcmux manager <agent> <project> [flags...]
// Flags may appear before or after the positional args.
//
// Post-C2 this subcommand is a thin shim over manager.Start. arcmux is
// prompt-agnostic — the caller must supply the exact launch command via
// --command (e.g. `claude --append-system-prompt-file /path/to/role.md`).
// The subcommand is slated for removal in C4; new callers should use
// elonco's launcher instead.
func CmdManager(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	mission := fs.String("mission", "", "initial mission statement (free text)")
	dataRoot := fs.String("data-root", os.Getenv("ARCMUX_DATA"), "override data root (default $ARCMUX_DATA, then $HOME/data)")
	vaultRoot := fs.String("vault-root", os.Getenv("OBS_AGENTS"), "override vault root (default $OBS_AGENTS)")
	command := fs.String("command", "", "exact shell command to exec after env exports (e.g. 'claude --append-system-prompt-file /path/to/elon.md'); defaults to the bare agent name")
	focus := fs.Bool("focus", true, "focus the new cmux workspace after creation")

	// Pre-split positionals from flags so users can write them in either order.
	flagArgs, positional := splitFlagsAndPositionals(args, fs)

	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) < 2 {
		return fmt.Errorf("usage: arcmux manager <agent> <project> [--mission \"...\"] [--command \"...\"] [--focus=false]")
	}
	agent, project := positional[0], positional[1]

	// CmdManager is a CLI shim — it has no config layer, so default to the
	// cmux backend (historical behavior). The daemon's main constructs a
	// backend from [mux] config via muxbuild.New.
	backend := cmuxbackend.New(cmuxcli.New())

	p, err := Start(ctx, Options{
		Agent:     agent,
		Project:   project,
		Mission:   *mission,
		Command:   *command,
		DataRoot:  *dataRoot,
		VaultRoot: *vaultRoot,
		Mux:       backend,
		Focus:     *focus,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Fprintf(stdout, "manager mode started: project=%s agent=%s group=%s pane=%s\n",
		p.Paths.Project, p.Opts.Agent, p.Group.Ref, p.ElonPane.Ref)
	fmt.Fprintf(stdout, "bootstrap script: %s\n", p.BootstrapPath)
	fmt.Fprintf(stdout, "scratchpad:       %s\n", p.ScratchpadPath)
	return nil
}

// splitFlagsAndPositionals separates a mixed args slice into flag args
// (recognized by the given FlagSet) and positionals. Lets the user mix
// orderings — `arcmux manager claude foo --focus=false` and
// `arcmux manager --focus=false claude foo` both work.
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
