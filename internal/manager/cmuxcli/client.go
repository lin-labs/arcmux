// Package cmuxcli wraps the cmux command-line tool. Every method shells out
// to the cmux binary; the runner interface lets tests substitute a fake.
package cmuxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Runner abstracts process execution for testability.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

type execRunner struct{ bin string }

func (e *execRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s %v: %s", e.bin, args, string(ee.Stderr))
		}
		return "", err
	}
	return string(out), nil
}

// Client talks to a local cmux daemon via its CLI.
type Client struct {
	r Runner
}

// New returns a Client that shells out to the `cmux` binary.
func New() *Client {
	return &Client{r: &execRunner{bin: "cmux"}}
}

func newWithRunner(r Runner) *Client { return &Client{r: r} }

// NewWithRunnerForTest exposes the runner constructor to integration tests
// in sibling packages. Production code should call New().
func NewWithRunnerForTest(r Runner) *Client {
	return newWithRunner(r)
}

// Workspace identifies a cmux workspace.
type Workspace struct {
	Ref string `json:"ref"`
}

// Pane identifies a cmux pane.
type Pane struct {
	Ref          string   `json:"ref"`
	Index        int      `json:"index"`
	Focused      bool     `json:"focused"`
	SurfaceRefs  []string `json:"surface_refs"`
	SelectedSurf string   `json:"selected_surface_ref"`
}

// NewWorkspaceOptions configures workspace creation.
type NewWorkspaceOptions struct {
	Name        string
	Description string
	CWD         string
	Command     string // sent (with Enter) to the workspace's initial terminal
	Focus       bool
}

// NewWorkspace creates a cmux workspace. The workspace gets one implicit
// initial terminal pane; if Command is set, it runs there.
func (c *Client) NewWorkspace(ctx context.Context, opts NewWorkspaceOptions) (Workspace, error) {
	args := []string{"new-workspace"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.Description != "" {
		args = append(args, "--description", opts.Description)
	}
	if opts.CWD != "" {
		args = append(args, "--cwd", opts.CWD)
	}
	if opts.Command != "" {
		args = append(args, "--command", opts.Command)
	}
	args = append(args, "--focus", boolStr(opts.Focus))

	out, err := c.r.Run(ctx, args...)
	if err != nil {
		return Workspace{}, fmt.Errorf("cmux new-workspace: %w", err)
	}
	// cmux new-workspace returns "OK workspace:N" (single token today, may
	// gain more in future); pick the workspace explicitly to be robust.
	ref := parseOKRefKind(out, "workspace")
	if ref == "" {
		return Workspace{}, fmt.Errorf("cmux new-workspace: unparsable output %q", out)
	}
	return Workspace{Ref: ref}, nil
}

// NewPaneOptions configures pane creation.
type NewPaneOptions struct {
	Workspace string
	Direction string // left | right | up | down (default: right per cmux)
	Type      string // terminal | browser (default: terminal)
	Focus     bool
}

// NewPane creates a new pane in a workspace by splitting.
func (c *Client) NewPane(ctx context.Context, opts NewPaneOptions) (Pane, error) {
	args := []string{"new-pane"}
	if opts.Workspace != "" {
		args = append(args, "--workspace", opts.Workspace)
	}
	if opts.Direction != "" {
		args = append(args, "--direction", opts.Direction)
	}
	if opts.Type != "" {
		args = append(args, "--type", opts.Type)
	}
	args = append(args, "--focus", boolStr(opts.Focus))

	out, err := c.r.Run(ctx, args...)
	if err != nil {
		return Pane{}, fmt.Errorf("cmux new-pane: %w", err)
	}
	// cmux new-pane returns "OK surface:N pane:N workspace:N" — pick the pane.
	ref := parseOKRefKind(out, "pane")
	if ref == "" {
		return Pane{}, fmt.Errorf("cmux new-pane: unparsable output %q", out)
	}
	surf := parseOKRefKind(out, "surface")
	return Pane{Ref: ref, SelectedSurf: surf}, nil
}

// ListPanes returns panes in a workspace.
func (c *Client) ListPanes(ctx context.Context, workspaceRef string) ([]Pane, error) {
	args := []string{"--json", "list-panes"}
	if workspaceRef != "" {
		args = append(args, "--workspace", workspaceRef)
	}
	out, err := c.r.Run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("cmux list-panes: %w", err)
	}
	var v struct {
		Panes []Pane `json:"panes"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("cmux list-panes: parse %w (out=%q)", err, out)
	}
	return v.Panes, nil
}

// Send pushes text into a surface/pane reference.
func (c *Client) Send(ctx context.Context, target, text string) error {
	_, err := c.r.Run(ctx, "send", "--target", target, "--", text)
	if err != nil {
		return fmt.Errorf("cmux send: %w", err)
	}
	return nil
}

// CloseSurface closes a surface.
func (c *Client) CloseSurface(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "close-surface", "--surface", target)
	if err != nil {
		return fmt.Errorf("cmux close-surface: %w", err)
	}
	return nil
}

// ClosePane closes a pane by ref. Mirrors NewPane on the verb axis; used by
// icspawn.Dissolve to retire an IC's pane when its slot lifecycle ends.
func (c *Client) ClosePane(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "close-pane", "--pane", target)
	if err != nil {
		return fmt.Errorf("cmux close-pane: %w", err)
	}
	return nil
}

// CloseWorkspace closes a workspace.
func (c *Client) CloseWorkspace(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "close-workspace", "--workspace", target)
	if err != nil {
		return fmt.Errorf("cmux close-workspace: %w", err)
	}
	return nil
}

// FocusPane focuses a pane.
func (c *Client) FocusPane(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "focus-pane", "--pane", target)
	if err != nil {
		return fmt.Errorf("cmux focus-pane: %w", err)
	}
	return nil
}

// ReadScreen returns terminal text from a surface.
func (c *Client) ReadScreen(ctx context.Context, target string) (string, error) {
	out, err := c.r.Run(ctx, "read-screen", "--surface", target)
	if err != nil {
		return "", fmt.Errorf("cmux read-screen: %w", err)
	}
	return out, nil
}

// Identify reports server identity + caller context.
func (c *Client) Identify(ctx context.Context) (string, error) {
	return c.r.Run(ctx, "--json", "identify")
}

// parseOKRef extracts the first ref from cmux's "OK <ref> [<ref>...]" output.
// Some cmux commands return one ref (e.g. new-workspace → "OK workspace:N");
// others return several (e.g. new-pane → "OK surface:N pane:N workspace:N").
// Callers that need a specific kind use parseOKRefKind.
func parseOKRef(out string) string {
	toks := strings.Fields(strings.TrimSpace(out))
	if len(toks) < 2 || toks[0] != "OK" {
		return ""
	}
	for _, tok := range toks[1:] {
		if isRefShaped(tok) {
			return tok
		}
		return "" // first non-OK token isn't ref-shaped — malformed
	}
	return ""
}

// parseOKRefKind extracts a ref of the requested kind (e.g. "pane", "workspace",
// "surface") from cmux's multi-token "OK …" output. Returns "" if absent or if
// the output is not an OK line.
func parseOKRefKind(out, kind string) string {
	toks := strings.Fields(strings.TrimSpace(out))
	if len(toks) < 2 || toks[0] != "OK" {
		return ""
	}
	prefix := kind + ":"
	for _, tok := range toks[1:] {
		if strings.HasPrefix(tok, prefix) && isRefShaped(tok) {
			return tok
		}
	}
	return ""
}

// isRefShaped reports whether s looks like a cmux ref "<kind>:<id>". The kind
// is a non-empty lowercase identifier; the id is non-empty.
func isRefShaped(s string) bool {
	i := strings.Index(s, ":")
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, r := range s[:i] {
		if !(r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
