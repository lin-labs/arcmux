package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// OpenOptions configure Open.
type OpenOptions struct {
	Project   string         // slug
	DataRoot  string         // typically ~/data
	VaultRoot string         // typically $OBS_AGENTS
	Cmux      *cmuxcli.Client // optional; defaults to real cmux client
}

// Open attaches to an already-scaffolded project's bbolt store and cmux
// client without creating an Elon workspace. Use this from arcmux-call
// subcommands that need to read/write project state.
//
// The caller must Close the returned Project to release the bbolt handle.
// Concurrent Opens on the same project block on bbolt's file lock.
func Open(_ context.Context, o OpenOptions) (*Project, error) {
	slug, err := paths.Validate(o.Project)
	if err != nil {
		return nil, err
	}
	if o.DataRoot == "" {
		o.DataRoot = filepath.Join(os.Getenv("HOME"), "data")
	}
	if o.VaultRoot == "" {
		o.VaultRoot = os.Getenv("OBS_AGENTS")
		if o.VaultRoot == "" {
			return nil, fmt.Errorf("VaultRoot required (set OBS_AGENTS)")
		}
	}
	if o.Cmux == nil {
		o.Cmux = cmuxcli.New()
	}

	p := &Project{
		Opts:  Options{Project: slug, DataRoot: o.DataRoot, VaultRoot: o.VaultRoot, Cmux: o.Cmux},
		Paths: paths.ForProject(o.DataRoot, o.VaultRoot, slug),
	}

	// The state.bolt file is created by Bootstrap (the manager-mode Start).
	// Open requires it exists; we don't scaffold from Open since that would
	// hide the user error of "talking to a project that was never started."
	if _, err := os.Stat(p.Paths.StateBolt); err != nil {
		return nil, fmt.Errorf("project %q not started (no state.bolt at %s): %w", slug, p.Paths.StateBolt, err)
	}

	db, err := store.Open(p.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	p.DB = db
	return p, nil
}
