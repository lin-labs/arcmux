// Package muxbuild constructs a mux.Backend from config. It lives outside
// internal/mux to avoid an import cycle (concrete adapters import the
// interface; this wiring layer imports both).
package muxbuild

import (
	"fmt"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/mux"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
	"github.com/lin-labs/arcmux/internal/mux/tmuxbackend"
	"github.com/lin-labs/arcmux/internal/tmux"
)

// New constructs the configured Backend. Returns an error if
// cfg.Mux.Backend is not one of "cmux" or "tmux".
//
// For "tmux", cfg.Tmux.SocketName is honored.
func New(cfg *config.Config) (mux.Backend, error) {
	switch cfg.Mux.Backend {
	case "cmux":
		return cmuxbackend.New(cmuxcli.New()), nil
	case "tmux":
		return tmuxbackend.New(tmux.NewClient(cfg.Tmux.SocketName)), nil
	default:
		return nil, fmt.Errorf("mux: unknown backend %q", cfg.Mux.Backend)
	}
}
