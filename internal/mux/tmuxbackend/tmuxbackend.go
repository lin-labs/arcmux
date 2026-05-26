// Package tmuxbackend adapts *tmux.Client to mux.Backend so manager code
// can talk to tmux through the shared interface.
//
// Mapping:
//
//	mux.Group → tmux session  (Ref = session name)
//	mux.Pane  → tmux window   (Ref = #{pane_id} of the window's first pane)
//
// arcmux only ever creates one tmux pane per window, so window ≡ pane for
// our purposes. send-keys, select-pane, kill-pane, capture-pane, and
// pipe-pane all accept pane_id targets, which keeps the Ref usable across
// every Backend method.
package tmuxbackend

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/mux"
	"github.com/lin-labs/arcmux/internal/tmux"
)

// Backend is the mux.Backend implementation for tmux.
type Backend struct {
	c *tmux.Client
}

// New returns a Backend wrapping c.
func New(c *tmux.Client) *Backend { return &Backend{c: c} }

// Underlying returns the wrapped tmux client.
func (b *Backend) Underlying() *tmux.Client { return b.c }

func (b *Backend) EnsureServer(ctx context.Context) error {
	return b.c.EnsureServer(ctx)
}

// NewGroup creates a tmux session. If opts.Name is empty, a unique name is
// generated. If opts.Command is set, it is sent + Enter to the session's
// initial pane after creation.
func (b *Backend) NewGroup(ctx context.Context, opts mux.GroupOptions) (mux.Group, error) {
	name := opts.Name
	if name == "" {
		name = fmt.Sprintf("arcmux-%d", time.Now().UnixNano())
	}
	if err := b.c.NewSession(ctx, name, "", opts.CWD); err != nil {
		return mux.Group{}, err
	}
	if opts.Command != "" {
		if err := b.c.SendKeys(ctx, name, opts.Command, "Enter"); err != nil {
			return mux.Group{}, fmt.Errorf("tmux send initial command: %w", err)
		}
	}
	// opts.Focus and opts.Description have no tmux equivalents; silently ignored.
	return mux.Group{Ref: name}, nil
}

// NewPane creates a new tmux window in the group. Direction is silently
// ignored — tmux windows don't have a split direction. The hint stays on
// PaneOptions because cmux uses it; tmux just drops it.
func (b *Backend) NewPane(ctx context.Context, opts mux.PaneOptions) (mux.Pane, error) {
	paneID, err := b.c.NewWindow(ctx, opts.Group, "", "")
	if err != nil {
		return mux.Pane{}, err
	}
	if opts.Focus {
		if err := b.c.SelectPane(ctx, paneID); err != nil {
			return mux.Pane{}, fmt.Errorf("tmux focus new pane: %w", err)
		}
	}
	return mux.Pane{Ref: paneID, SendTarget: paneID, Focused: opts.Focus}, nil
}

// Send writes text + Enter to a target pane.
func (b *Backend) Send(ctx context.Context, target, text string) error {
	return b.c.SendKeys(ctx, target, text, "Enter")
}

// SendRaw writes text without Enter. Uses send-keys -l (literal) to avoid
// tmux's keyword interpretation (e.g. "Enter", "C-c").
func (b *Backend) SendRaw(ctx context.Context, target, text string) error {
	return b.c.SendKeys(ctx, target, "-l", text)
}

func (b *Backend) ReadScreen(ctx context.Context, target string) (string, error) {
	return b.c.CapturePaneVisible(ctx, target)
}

func (b *Backend) CaptureHistory(ctx context.Context, target string) (string, error) {
	return b.c.CapturePaneHistory(ctx, target)
}

// Focus selects the pane (which also surfaces its window).
func (b *Backend) Focus(ctx context.Context, target string) error {
	return b.c.SelectPane(ctx, target)
}

func (b *Backend) ClosePane(ctx context.Context, target string) error {
	return b.c.KillPane(ctx, target)
}

// CloseGroup kills the tmux session.
func (b *Backend) CloseGroup(ctx context.Context, target string) error {
	return b.c.KillSession(ctx, target)
}

func (b *Backend) ListPanes(ctx context.Context, group string) ([]mux.Pane, error) {
	out, err := b.c.ListPanesRaw(ctx, group)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	panes := make([]mux.Pane, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		var idx int
		fmt.Sscanf(parts[1], "%d", &idx)
		panes = append(panes, mux.Pane{
			Ref:        parts[0],
			SendTarget: parts[0],
			Index:      idx,
			Focused:    parts[2] == "1",
		})
	}
	return panes, nil
}

func (b *Backend) PaneExists(ctx context.Context, target string) bool {
	return b.c.PaneExists(ctx, target)
}

func (b *Backend) WaitIdle(ctx context.Context, target string, timeout, settle time.Duration) error {
	return b.c.WaitIdle(ctx, target, timeout, settle)
}

func (b *Backend) PipePaneStart(ctx context.Context, target, outPath string) error {
	return b.c.PipePaneStart(ctx, target, outPath)
}

func (b *Backend) PipePaneStop(ctx context.Context, target string) error {
	return b.c.PipePaneStop(ctx, target)
}

var _ mux.Backend = (*Backend)(nil)
