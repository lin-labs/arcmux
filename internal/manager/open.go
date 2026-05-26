package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/mux"
)

// OpenOptions configure Open.
type OpenOptions struct {
	Project   string      // slug
	DataRoot  string      // typically ~/data
	VaultRoot string      // typically $OBS_AGENTS
	Mux       mux.Backend // optional; pure state-attach calls leave it nil
}

// Open attaches to an already-registered project's bbolt store and cmux
// client without creating a new workspace. Use this from arcmux-cli
// subcommands or the debug pulse shim that need to read/write project
// state.
//
// The caller must Close the returned Registration to release the bbolt
// handle. Concurrent Opens on the same project block on bbolt's file
// lock.
func Open(_ context.Context, o OpenOptions) (*Registration, error) {
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
	// Open attaches to state only; callers that need to send into the
	// project's mux can pass o.Mux through. nil is fine — Open does no
	// mux calls itself.
	r := &Registration{
		Opts:  Options{Project: slug, DataRoot: o.DataRoot, VaultRoot: o.VaultRoot, Mux: o.Mux},
		Paths: paths.ForProject(o.DataRoot, o.VaultRoot, slug),
	}

	// The state.bolt file is created by RegisterSession. Open requires it
	// exists; we don't scaffold from Open since that would hide the user
	// error of "talking to a project that was never registered."
	if _, err := os.Stat(r.Paths.StateBolt); err != nil {
		return nil, fmt.Errorf("project %q not registered (no state.bolt at %s): %w", slug, r.Paths.StateBolt, err)
	}

	db, err := store.Open(r.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	r.DB = db
	return r, nil
}
