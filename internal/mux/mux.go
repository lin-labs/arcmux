// Package mux is the shared abstraction over terminal multiplexer backends.
//
// arcmux supports two backends as equal first-class citizens: cmux and tmux.
// The active backend is chosen globally per daemon via config (`[mux] backend`).
// Manager code (registration, pulse, health, daemon) talks to
// `mux.Backend` and is backend-agnostic. Backend-specific operations that
// don't translate (e.g. cmux browser panes, cmux multi-surface panes,
// tmux Direction-based pane splitting) return ErrUnsupported when called
// through the interface; callers that need them must use the concrete
// package directly.
//
// Vocabulary: the interface uses neutral terms.
//
//	Group  — cmux workspace / tmux session
//	Pane   — cmux pane     / tmux window
//
// Concrete backends keep their native types and APIs in their own packages
// (internal/manager/cmuxcli, internal/tmux). The interface only covers the
// common surface that the manager pipeline actually uses.
package mux

import (
	"context"
	"time"
)

// Group identifies a top-level container — a cmux workspace or tmux session.
type Group struct {
	Ref string
}

// Pane identifies a child surface within a group — a cmux pane or tmux window.
//
// Ref is the canonical address for non-send ops (Focus, ClosePane,
// ListPanes filtering). SendTarget is the address that Send wants:
//
//   - cmux: SendTarget is the pane's selected surface ref (when known);
//     cmux send needs a surface ref, and pane→surface resolution is not
//     always available immediately after group creation.
//   - tmux: SendTarget == Ref (pane_id works for both send-keys and the
//     other pane-targeted commands).
//
// Callers with a fresh Pane in hand should prefer SendTarget for Send.
// Callers working from a stored Ref string (e.g. slot.PaneRef from bbolt)
// can still pass Ref to Send — cmux resolves pane refs internally once
// the terminal surface is provisioned.
type Pane struct {
	Ref        string
	SendTarget string
	Index      int
	Focused    bool
}

// GroupOptions configures NewGroup. CWD, Command, and Focus are honored
// best-effort by each backend; backends ignore fields they cannot express.
type GroupOptions struct {
	Name        string
	Description string
	CWD         string
	Command     string // run in the implicit initial child surface, if any
	Focus       bool
}

// PaneOptions configures NewPane.
//
// Direction is cmux-only ("left" | "right" | "up" | "down"). When the tmux
// backend receives a non-empty Direction, it returns ErrUnsupported — pane
// layout is fundamentally different between cmux (split) and tmux (window).
type PaneOptions struct {
	Group     string
	Direction string
	Focus     bool
}

// Backend is the cmux/tmux-neutral surface that arcmux's manager pipeline
// uses. Backends must implement every method; unsupported operations
// return ErrUnsupported.
type Backend interface {
	// EnsureServer starts the backend's server process if not already running.
	EnsureServer(ctx context.Context) error

	// NewGroup creates a new group (workspace/session).
	NewGroup(ctx context.Context, opts GroupOptions) (Group, error)
	// NewPane creates a new pane (pane/window) inside a group.
	NewPane(ctx context.Context, opts PaneOptions) (Pane, error)

	// Send pushes text into a target and submits it. The backend is
	// responsible for its own Enter encoding — callers pass plain text.
	Send(ctx context.Context, target, text string) error
	// SendRaw pushes text without submitting. Use only when priming a
	// prompt the user will edit before sending.
	SendRaw(ctx context.Context, target, text string) error

	// ReadScreen returns the currently visible terminal text.
	ReadScreen(ctx context.Context, target string) (string, error)
	// CaptureHistory returns terminal text including scrollback when the
	// backend supports it; otherwise it returns the visible screen.
	CaptureHistory(ctx context.Context, target string) (string, error)

	// Focus brings target to the foreground within its group.
	Focus(ctx context.Context, target string) error

	// ClosePane kills a pane (cmux pane / tmux window).
	ClosePane(ctx context.Context, target string) error
	// CloseGroup kills a group and everything in it.
	CloseGroup(ctx context.Context, target string) error

	// ListPanes enumerates panes in a group.
	ListPanes(ctx context.Context, group string) ([]Pane, error)
	// PaneExists is a fast probe — false if the pane is gone or unreachable.
	PaneExists(ctx context.Context, target string) bool

	// WaitIdle blocks until target's output has been quiet for `settle`,
	// or until `timeout` elapses (whichever comes first).
	WaitIdle(ctx context.Context, target string, timeout, settle time.Duration) error

	// PipePaneStart begins tee-ing target's output to outPath. Returns
	// ErrUnsupported on backends without a native equivalent.
	PipePaneStart(ctx context.Context, target, outPath string) error
	// PipePaneStop stops a previously-started pipe.
	PipePaneStop(ctx context.Context, target string) error
}
