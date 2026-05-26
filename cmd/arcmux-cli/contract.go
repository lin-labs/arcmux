package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// cmdContract dispatches `arcmux-cli contract <sub>`.
func cmdContract(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-cli contract create|get|list|transition|deps [flags]")
	}
	switch args[0] {
	case "create":
		return cmdContractCreate(args[1:], stdin, stdout)
	case "get":
		return cmdContractGet(args[1:], stdout)
	case "list":
		return cmdContractList(args[1:], stdout)
	case "transition":
		return cmdContractTransition(args[1:], stdout)
	case "deps":
		return cmdContractDeps(args[1:], stdout)
	default:
		return fmt.Errorf("unknown contract subcommand %q (want create|get|list|transition|deps)", args[0])
	}
}

// splitCSV trims and splits a comma-separated flag value. An empty input
// returns nil (not a zero-length slice with a "" element), which JSON-encodes
// as null/omitted — exactly the semantic the Contract struct wants for
// unset list fields.
func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cmdContractCreate(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("contract create", flag.ContinueOnError)
	id := fs.String("id", "", "contract id (required, validated as slug)")
	team := fs.String("team", "", "owning team slug (required)")
	icRole := fs.String("ic-role", "", "preferred IC role (free string; '' = unassigned)")
	priority := fs.Int("priority", 0, "priority (higher = more urgent)")
	objective := fs.String("objective", "", "objective text (or pipe via stdin)")
	outputFormat := fs.String("output-format", "", "expected deliverable shape (PR, file, doc...)")
	tools := fs.String("tools", "", "comma-separated tool list")
	boundaries := fs.String("boundaries", "", "comma-separated boundary list (must-not-touch)")
	acceptance := fs.String("acceptance", "", "comma-separated acceptance criteria")
	dependsOn := fs.String("depends-on", "", "comma-separated parent contract ids")
	parallelizableWith := fs.String("parallelizable-with", "", "comma-separated peer contract ids")
	capstone := fs.Bool("capstone", false, "mark as capstone (final integration) contract")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("contract create: --id required")
	}
	if _, err := paths.Validate(*id); err != nil {
		return fmt.Errorf("contract create: --id: %w", err)
	}
	if *team == "" {
		return fmt.Errorf("contract create: --team required")
	}
	if _, err := paths.Validate(*team); err != nil {
		return fmt.Errorf("contract create: --team: %w", err)
	}
	if *project == "" {
		return fmt.Errorf("contract create: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	// Objective may come from --objective OR stdin, but not both. Long
	// objectives prefer stdin; short ones prefer the flag.
	stdinBody, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("contract create: read stdin: %w", err)
	}
	stdinObjective := strings.TrimRight(string(stdinBody), "\n")
	switch {
	case *objective != "" && stdinObjective != "":
		return fmt.Errorf("contract create: --objective and stdin both set; pick one")
	case stdinObjective != "":
		*objective = stdinObjective
	}
	if *objective == "" {
		return fmt.Errorf("contract create: --objective or non-empty stdin required")
	}

	deps := splitCSV(*dependsOn)
	for _, d := range deps {
		if _, err := paths.Validate(d); err != nil {
			return fmt.Errorf("contract create: --depends-on %q: %w", d, err)
		}
	}
	peers := splitCSV(*parallelizableWith)
	for _, p := range peers {
		if _, err := paths.Validate(p); err != nil {
			return fmt.Errorf("contract create: --parallelizable-with %q: %w", p, err)
		}
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	// Block duplicate-id creates: PutContract upserts, which silently
	// clobbers prior state. From the CLI a "create" is an intent to add a
	// new contract — an upsert here would hide bugs (e.g. caller copy-paste
	// reuses an ID). Loud error names exactly what the caller needs to do.
	if _, err := db.GetContract(*id); err == nil {
		return fmt.Errorf("contract create: id %q already exists", *id)
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("contract create: probe existing: %w", err)
	}

	c := store.Contract{
		ID:                 *id,
		Team:               *team,
		ICRole:             *icRole,
		Priority:           *priority,
		State:              store.ContractPending,
		Objective:          *objective,
		OutputFormat:       *outputFormat,
		Tools:              splitCSV(*tools),
		Boundaries:         splitCSV(*boundaries),
		AcceptanceCriteria: splitCSV(*acceptance),
		DependsOn:          deps,
		ParallelizableWith: peers,
		Capstone:           *capstone,
	}
	if err := db.PutContract(c); err != nil {
		return fmt.Errorf("contract create: %w", err)
	}
	// Re-read so timestamps + final state reflect what the store wrote.
	saved, err := db.GetContract(*id)
	if err != nil {
		return fmt.Errorf("contract create: readback: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":         true,
		"id":         saved.ID,
		"state":      saved.State,
		"team":       saved.Team,
		"priority":   saved.Priority,
		"created_at": saved.CreatedAt,
	})
}

func cmdContractGet(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("contract get", flag.ContinueOnError)
	id := fs.String("id", "", "contract id (required)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("contract get: --id required")
	}
	if _, err := paths.Validate(*id); err != nil {
		return fmt.Errorf("contract get: --id: %w", err)
	}
	if *project == "" {
		return fmt.Errorf("contract get: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := db.GetContract(*id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("contract get: id %q not found", *id)
		}
		return fmt.Errorf("contract get: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"contract": c})
}

func cmdContractList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("contract list", flag.ContinueOnError)
	team := fs.String("team", "", "filter by team slug (empty = all)")
	state := fs.String("state", "", "filter by state (empty = all)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("contract list: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	if *team != "" {
		if _, err := paths.Validate(*team); err != nil {
			return fmt.Errorf("contract list: --team: %w", err)
		}
	}
	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()
	cs, err := db.ListContracts(*team, *state)
	if err != nil {
		return fmt.Errorf("contract list: %w", err)
	}
	if cs == nil {
		cs = []store.Contract{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"contracts": cs})
}

func cmdContractTransition(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("contract transition", flag.ContinueOnError)
	id := fs.String("id", "", "contract id (required)")
	to := fs.String("to", "", "target state (required)")
	reason := fs.String("reason", "", "audit reason (optional)")
	by := fs.String("by", defaultActor(), "actor recording the transition (default $ARCMUX_ROLE or 'arcmux-cli')")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("contract transition: --id required")
	}
	if _, err := paths.Validate(*id); err != nil {
		return fmt.Errorf("contract transition: --id: %w", err)
	}
	if *to == "" {
		return fmt.Errorf("contract transition: --to required")
	}
	if *project == "" {
		return fmt.Errorf("contract transition: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	// Snapshot the prior state so the response can name from→to. The
	// transition itself runs in its own bbolt Update; this extra View is
	// cheap and gives the caller a friendlier echo.
	prior, err := db.GetContract(*id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("contract transition: id %q not found", *id)
		}
		return fmt.Errorf("contract transition: %w", err)
	}
	if err := db.TransitionContract(*id, *to, *reason, *by); err != nil {
		return fmt.Errorf("contract transition: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":   true,
		"id":   *id,
		"from": prior.State,
		"to":   *to,
		"by":   *by,
	})
}

func cmdContractDeps(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("contract deps", flag.ContinueOnError)
	id := fs.String("id", "", "contract id (required)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("contract deps: --id required")
	}
	if _, err := paths.Validate(*id); err != nil {
		return fmt.Errorf("contract deps: --id: %w", err)
	}
	if *project == "" {
		return fmt.Errorf("contract deps: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}
	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	parents, err := db.ParentsOf(*id)
	if err != nil {
		return fmt.Errorf("contract deps: parents: %w", err)
	}
	children, err := db.ChildrenOf(*id)
	if err != nil {
		return fmt.Errorf("contract deps: children: %w", err)
	}
	if parents == nil {
		parents = []string{}
	}
	if children == nil {
		children = []string{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"id":       *id,
		"parents":  parents,
		"children": children,
	})
}

// defaultActor picks who an audit row should credit when --by is omitted.
// In production manager-mode launches, $ARCMUX_ROLE is "elon" (or "manager").
// Outside that context, fall back to "arcmux-cli" so the audit log still
// records *something* identifiable rather than an empty string.
func defaultActor() string {
	if v := os.Getenv("ARCMUX_ROLE"); v != "" {
		return v
	}
	return "arcmux-cli"
}
