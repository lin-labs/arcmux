package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const defaultSocket = "arcmux"

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

// NewSession creates a new tmux session.
func (c *Client) NewSession(ctx context.Context, name, window, cwd string) error {
	args := []string{"new-session", "-d", "-s", name}
	if window != "" {
		args = append(args, "-n", window)
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	_, err := c.run(ctx, args...)
	return err
}

// NewWindow creates a new window in an existing session.
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

// SendKeys sends text to a target pane.
func (c *Client) SendKeys(ctx context.Context, target string, keys ...string) error {
	args := []string{"send-keys", "-t", target}
	args = append(args, keys...)
	_, err := c.run(ctx, args...)
	return err
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
