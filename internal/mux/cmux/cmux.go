// Package cmux adapts *cmuxcli.Client to mux.Backend so that manager code
// can talk to cmux through the shared interface. The native cmuxcli API
// (workspaces, multi-surface panes, browser panes, Identify) stays on the
// underlying client for callers that need it.
package cmux

import (
	"context"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/mux"
)

// Backend is the mux.Backend implementation for cmux.
type Backend struct {
	c *cmuxcli.Client
}

// New returns a Backend wrapping c. c must be non-nil in production.
func New(c *cmuxcli.Client) *Backend { return &Backend{c: c} }

// Underlying returns the wrapped cmuxcli.Client. Use when calling
// backend-specific cmux APIs (browser panes, surface-level ops) that the
// neutral mux.Backend interface does not expose.
func (b *Backend) Underlying() *cmuxcli.Client { return b.c }

// EnsureServer is a no-op for cmux. The cmux daemon is managed externally;
// if it is not running, subsequent calls will fail naturally.
func (b *Backend) EnsureServer(ctx context.Context) error { return nil }

// NewGroup creates a cmux workspace.
func (b *Backend) NewGroup(ctx context.Context, opts mux.GroupOptions) (mux.Group, error) {
	ws, err := b.c.NewWorkspace(ctx, cmuxcli.NewWorkspaceOptions{
		Name:        opts.Name,
		Description: opts.Description,
		CWD:         opts.CWD,
		Command:     opts.Command,
		Focus:       opts.Focus,
	})
	if err != nil {
		return mux.Group{}, err
	}
	return mux.Group{Ref: ws.Ref}, nil
}

// NewPane creates a cmux pane by splitting the workspace. The neutral
// mux abstraction only models terminal panes; cmux's browser-pane Type
// stays on the underlying cmuxcli.Client for callers who need it.
func (b *Backend) NewPane(ctx context.Context, opts mux.PaneOptions) (mux.Pane, error) {
	pane, err := b.c.NewPane(ctx, cmuxcli.NewPaneOptions{
		Workspace: opts.Group,
		Direction: opts.Direction,
		Type:      "terminal",
		Focus:     opts.Focus,
	})
	if err != nil {
		return mux.Pane{}, err
	}
	return mux.Pane{
		Ref:        pane.Ref,
		SendTarget: paneSendTarget(pane),
		Index:      pane.Index,
		Focused:    pane.Focused,
	}, nil
}

// Send pushes text into target and submits it (cmuxcli.Send appends a
// literal "\n", which cmux interprets as Enter).
func (b *Backend) Send(ctx context.Context, target, text string) error {
	return b.c.Send(ctx, target, text)
}

// SendRaw pushes text without appending Enter.
func (b *Backend) SendRaw(ctx context.Context, target, text string) error {
	return b.c.SendRaw(ctx, target, text)
}

// ReadScreen returns terminal text from a target.
func (b *Backend) ReadScreen(ctx context.Context, target string) (string, error) {
	return b.c.ReadScreen(ctx, target)
}

// CaptureHistory returns terminal text. cmux does not expose scrollback
// separately today; this returns the visible screen.
func (b *Backend) CaptureHistory(ctx context.Context, target string) (string, error) {
	return b.c.ReadScreen(ctx, target)
}

// Focus brings the pane to the foreground.
func (b *Backend) Focus(ctx context.Context, target string) error {
	return b.c.FocusPane(ctx, target)
}

// ClosePane closes a cmux pane.
func (b *Backend) ClosePane(ctx context.Context, target string) error {
	return b.c.ClosePane(ctx, target)
}

// CloseGroup closes a cmux workspace and everything in it.
func (b *Backend) CloseGroup(ctx context.Context, target string) error {
	return b.c.CloseWorkspace(ctx, target)
}

// ListPanes returns the panes in a workspace.
func (b *Backend) ListPanes(ctx context.Context, group string) ([]mux.Pane, error) {
	cps, err := b.c.ListPanes(ctx, group)
	if err != nil {
		return nil, err
	}
	out := make([]mux.Pane, 0, len(cps))
	for _, p := range cps {
		out = append(out, mux.Pane{
			Ref:        p.Ref,
			SendTarget: paneSendTarget(p),
			Index:      p.Index,
			Focused:    p.Focused,
		})
	}
	return out, nil
}

// paneSendTarget picks the right cmux send address for a pane: prefer the
// selected surface (cmux send takes --surface), falling back to the pane
// ref when no surface is exposed.
func paneSendTarget(p cmuxcli.Pane) string {
	if p.SelectedSurf != "" {
		return p.SelectedSurf
	}
	if len(p.SurfaceRefs) > 0 {
		return p.SurfaceRefs[0]
	}
	return p.Ref
}

// PaneExists probes for a pane's presence by listing the parent workspace.
// Refs are of the form "pane:<id>"; we can't easily derive the workspace
// from the pane ref alone, so we fall back to a best-effort read-screen
// probe and treat any non-error as "exists".
func (b *Backend) PaneExists(ctx context.Context, target string) bool {
	_, err := b.c.ReadScreen(ctx, target)
	return err == nil
}

// WaitIdle polls ReadScreen until output is unchanged for `settle` or
// until `timeout` elapses. cmux has no native idle wait.
func (b *Backend) WaitIdle(ctx context.Context, target string, timeout, settle time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := settle / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	var last string
	stableSince := time.Time{}
	for {
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		cur, err := b.c.ReadScreen(ctx, target)
		if err != nil {
			return err
		}
		// Normalize trailing whitespace — terminals often flap by a
		// single space-cursor difference.
		cur = strings.TrimRight(cur, " \n\t")
		if cur == last {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= settle {
				return nil
			}
		} else {
			last = cur
			stableSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
		}
	}
}

// PipePaneStart is not supported on cmux today.
func (b *Backend) PipePaneStart(ctx context.Context, target, outPath string) error {
	return mux.ErrUnsupported
}

// PipePaneStop is not supported on cmux today.
func (b *Backend) PipePaneStop(ctx context.Context, target string) error {
	return mux.ErrUnsupported
}

// Compile-time assertion that *Backend satisfies mux.Backend.
var _ mux.Backend = (*Backend)(nil)
