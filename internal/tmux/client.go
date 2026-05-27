package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const defaultSocket = "arcmux"

const promptSubmitDelay = 200 * time.Millisecond

type PromptDeliveryStatus string

const (
	PromptDeliveryTypedOnly  PromptDeliveryStatus = "typed_only"
	PromptDeliverySubmitted  PromptDeliveryStatus = "submitted"
	PromptDeliveryBodyFailed PromptDeliveryStatus = "body_failed"
)

type PromptDeliveryResult struct {
	Status    PromptDeliveryStatus
	BodySent  bool
	Submitted bool
	BodyMode  string
	SubmitKey string
	Wait      time.Duration
}

type promptDeliveryPlan struct {
	bodyKeys   []string
	submitKeys []string
	wait       time.Duration
}

func newPromptDeliveryPlan(text string) promptDeliveryPlan {
	return promptDeliveryPlan{
		bodyKeys:   []string{"-l", text},
		submitKeys: []string{"Enter"},
		wait:       promptSubmitDelay,
	}
}

// Client wraps tmux CLI commands, always targeting the isolated arcmux.socket.
type Client struct {
	socket string
}

// NewClient creates a tmux client using the given socket name.
func NewClient(socket string) *Client {
	if socket == "" {
		socket = defaultSocket
	}
	return &Client{socket: socket}
}

// EnsureServer starts the tmux server if not already running.
func (c *Client) EnsureServer(ctx context.Context) error {
	_, err := c.run(ctx, "start-server")
	return err
}

// envFlags converts a map of env vars into repeated `-e KEY=VAL` flags,
// sorted for stable behavior. Empty values are still passed (tmux treats
// `-e KEY=` as setting the variable to the empty string in the new env).
func envFlags(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	flags := make([]string, 0, len(env)*2)
	for _, k := range keys {
		flags = append(flags, "-e", k+"="+env[k])
	}
	return flags
}

// NewSession creates a new tmux session.
func (c *Client) NewSession(ctx context.Context, name, window, cwd string) error {
	return c.NewSessionWithEnv(ctx, name, window, cwd, nil)
}

// NewSessionWithEnv creates a new tmux session, exporting the supplied env
// vars into the new pane's shell via repeated `-e KEY=VAL` flags. Use this
// when callers (e.g. arcmux CreateSession) need to inject ARCMUX_PROJECT,
// ARCMUX_ROLE_FILE, OBS_AGENTS, etc. into the spawned agent process.
func (c *Client) NewSessionWithEnv(ctx context.Context, name, window, cwd string, env map[string]string) error {
	args := []string{"new-session", "-d", "-s", name}
	if window != "" {
		args = append(args, "-n", window)
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, envFlags(env)...)
	_, err := c.run(ctx, args...)
	return err
}

// NewSessionWithEnvPaneID creates a new tmux session and returns the
// `%pane_id` of the initial pane. Use this when callers need an
// unambiguous routing target — pane_id is stable for the pane's lifetime
// and immune to the "two windows with the same name in the same session"
// collision that `<session>:<window-name>` shapes hit.
//
// On failure to create the session, returns an empty string + error so
// callers can fall back to NewWindowPaneID against an existing session.
func (c *Client) NewSessionWithEnvPaneID(ctx context.Context, name, window, cwd string, env map[string]string) (string, error) {
	if err := c.NewSessionWithEnv(ctx, name, window, cwd, env); err != nil {
		return "", err
	}
	// Resolve the pane_id of the freshly-created window. We use
	// display-message against `<session>:<window-name>` immediately after
	// new-session, while the new window is still the only one (or the
	// active one) with that name. tmux returns the active pane's
	// pane_id when targeting a window without disambiguation.
	target := name
	if window != "" {
		target = fmt.Sprintf("%s:%s", name, window)
	}
	out, err := c.run(ctx, "display-message", "-t", target, "-p", "#{pane_id}")
	if err != nil {
		return "", fmt.Errorf("resolve pane_id after new-session: %w", err)
	}
	pid := strings.TrimSpace(out)
	if pid == "" || !strings.HasPrefix(pid, "%") {
		return "", fmt.Errorf("resolve pane_id after new-session: unexpected output %q", pid)
	}
	return pid, nil
}

// NewWindow creates a new window in an existing session and returns the
// `%pane_id` of the created pane. Preserved for backends (notably
// internal/mux/tmuxbackend) that pair NewWindow with ListPanes and expect
// the same shape on both sides.
//
// New callers should prefer NewWindowCanonical, which returns the stable
// `<session>:<window-name>` target shape used elsewhere in arcmux.
func (c *Client) NewWindow(ctx context.Context, session, name, cwd string) (string, error) {
	args := []string{"new-window", "-t", session, "-P", "-F", "#{pane_id}"}
	if name != "" {
		args = append(args, "-n", name)
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// NewWindowCanonical creates a new window and returns the canonical
// `<session>:<window-name>` target. Exports the supplied env vars into the
// pane's shell via repeated `-e KEY=VAL` flags.
//
// When name is empty (tmux auto-generates one), falls back to
// `<session>:<window-index>` from the new-window -P -F result.
//
// Use this — not NewWindow — for arcmux daemon session targets so the
// SessionSummary.TmuxTarget shape is consistent across all sessions.
func (c *Client) NewWindowCanonical(ctx context.Context, session, name, cwd string, env map[string]string) (string, error) {
	// Ask tmux for both the index and the (possibly auto-generated) name
	// so the returned target is stable across renames and never depends on
	// the volatile `%pane_id`. Format separator is tab; tmux preserves it.
	format := "#{window_index}\t#{window_name}"
	args := []string{"new-window", "-t", session, "-P", "-F", format}
	if name != "" {
		args = append(args, "-n", name)
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, envFlags(env)...)
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	parts := strings.SplitN(line, "\t", 2)
	switch {
	case len(parts) == 2 && parts[1] != "":
		return fmt.Sprintf("%s:%s", session, parts[1]), nil
	case len(parts) >= 1 && parts[0] != "":
		return fmt.Sprintf("%s:%s", session, parts[0]), nil
	default:
		return "", fmt.Errorf("tmux new-window: unexpected output %q", line)
	}
}

// NewWindowPaneID creates a new window in an existing session and returns
// the `%pane_id` of the created pane. Exports the supplied env vars via
// repeated `-e KEY=VAL` flags.
//
// Use this — not NewWindowCanonical — when the daemon needs a routing
// target that survives duplicate window names. pane_id is unique across
// the tmux server for the pane's lifetime, so SendKeys / display-message
// / pipe-pane never get ambiguous about which pane they hit.
func (c *Client) NewWindowPaneID(ctx context.Context, session, name, cwd string, env map[string]string) (string, error) {
	args := []string{"new-window", "-t", session, "-P", "-F", "#{pane_id}"}
	if name != "" {
		args = append(args, "-n", name)
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, envFlags(env)...)
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(out)
	if pid == "" || !strings.HasPrefix(pid, "%") {
		return "", fmt.Errorf("tmux new-window: unexpected pane_id %q", pid)
	}
	return pid, nil
}

// SendKeys sends text to a target pane.
func (c *Client) SendKeys(ctx context.Context, target string, keys ...string) error {
	args := []string{"send-keys", "-t", target}
	args = append(args, keys...)
	_, err := c.run(ctx, args...)
	return err
}

func (c *Client) SendPrompt(ctx context.Context, target, text string) (PromptDeliveryResult, error) {
	plan := newPromptDeliveryPlan(text)
	result := PromptDeliveryResult{
		Status:    PromptDeliveryTypedOnly,
		BodyMode:  "literal",
		SubmitKey: "Enter",
		Wait:      plan.wait,
	}

	if err := c.SendKeys(ctx, target, plan.bodyKeys...); err != nil {
		result.Status = PromptDeliveryBodyFailed
		return result, err
	}
	result.BodySent = true

	if err := sleepWithContext(ctx, plan.wait); err != nil {
		return result, err
	}

	if err := c.SendKeys(ctx, target, plan.submitKeys...); err != nil {
		return result, err
	}

	result.Submitted = true
	result.Status = PromptDeliverySubmitted
	return result, nil
}

// CapturePaneVisible captures the visible screen content of a pane.
func (c *Client) CapturePaneVisible(ctx context.Context, target string) (string, error) {
	return c.run(ctx, "capture-pane", "-t", target, "-p")
}

// CapturePaneHistory captures the full scrollback of a pane.
func (c *Client) CapturePaneHistory(ctx context.Context, target string) (string, error) {
	return c.run(ctx, "capture-pane", "-t", target, "-p", "-S", "-")
}

// PaneInfo returns the current command and PID for a pane.
type PaneInfo struct {
	CurrentCommand string
	PID            int
	CWD            string
}

// GetPaneInfo retrieves metadata about a pane.
func (c *Client) GetPaneInfo(ctx context.Context, target string) (PaneInfo, error) {
	out, err := c.run(ctx, "display-message", "-t", target, "-p",
		"#{pane_current_command}\t#{pane_pid}\t#{pane_current_path}")
	if err != nil {
		return PaneInfo{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "\t", 3)
	info := PaneInfo{}
	if len(parts) >= 1 {
		info.CurrentCommand = parts[0]
	}
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &info.PID)
	}
	if len(parts) >= 3 {
		info.CWD = parts[2]
	}
	return info, nil
}

// PaneExists checks if a pane target is still alive.
func (c *Client) PaneExists(ctx context.Context, target string) bool {
	_, err := c.run(ctx, "display-message", "-t", target, "-p", "")
	return err == nil
}

// KillPane terminates a pane.
func (c *Client) KillPane(ctx context.Context, target string) error {
	_, err := c.run(ctx, "kill-pane", "-t", target)
	return err
}

// KillSession terminates a session by name.
func (c *Client) KillSession(ctx context.Context, session string) error {
	_, err := c.run(ctx, "kill-session", "-t", session)
	return err
}

// KillServer terminates the tmux server for this client's socket.
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	return err
}

// ShowEnvironment returns tmux's session-scoped environment value for key.
// This intentionally reads tmux session state, not a pane shell's process env.
func (c *Client) ShowEnvironment(ctx context.Context, session, key string) (string, error) {
	out, err := c.run(ctx, "show-environment", "-t", session, key)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	prefix := key + "="
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("tmux show-environment %s: unexpected output %q", key, line)
	}
	return strings.TrimPrefix(line, prefix), nil
}

// SelectPane brings target to the foreground.
func (c *Client) SelectPane(ctx context.Context, target string) error {
	_, err := c.run(ctx, "select-pane", "-t", target)
	return err
}

// ListPanesRaw returns one tab-delimited line per pane in the given session
// in the form: <pane_id>\t<window_index>\t<pane_active>.
func (c *Client) ListPanesRaw(ctx context.Context, session string) (string, error) {
	return c.run(ctx, "list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{window_index}\t#{pane_active}")
}

// PipePaneStart begins piping pane output to a file.
func (c *Client) PipePaneStart(ctx context.Context, target, outputFile string) error {
	_, err := c.run(ctx, "pipe-pane", "-t", target, "-o",
		fmt.Sprintf("cat >> %s", outputFile))
	return err
}

// PipePaneStop stops piping pane output.
func (c *Client) PipePaneStop(ctx context.Context, target string) error {
	_, err := c.run(ctx, "pipe-pane", "-t", target)
	return err
}

// WaitIdle polls until the pane shows no new output for the settle duration.
func (c *Client) WaitIdle(ctx context.Context, target string, timeout, settle time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastOutput := ""
	settleStart := time.Time{}

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for idle on %s", target)
		}

		current, err := c.CapturePaneVisible(ctx, target)
		if err != nil {
			return err
		}

		if current != lastOutput {
			lastOutput = current
			settleStart = time.Now()
		} else if !settleStart.IsZero() && time.Since(settleStart) >= settle {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	fullArgs := append([]string{"-L", c.socket}, args...)
	cmd := exec.CommandContext(ctx, "tmux", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		if outStr != "" {
			return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, outStr)
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return string(out), nil
}
