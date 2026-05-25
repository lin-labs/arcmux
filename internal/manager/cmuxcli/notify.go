package cmuxcli

import (
	"context"
	"fmt"
)

// Notify fires a user-attention notification on the given workspace/surface.
func (c *Client) Notify(ctx context.Context, target, title, body string) error {
	_, err := c.r.Run(ctx, "notify", "--target", target, "--title", title, "--body", body)
	if err != nil {
		return fmt.Errorf("cmux notify: %w", err)
	}
	return nil
}

// SetStatus sets a sidebar status pill on a surface.
func (c *Client) SetStatus(ctx context.Context, target, label string) error {
	_, err := c.r.Run(ctx, "set-status", "--target", target, "--label", label)
	if err != nil {
		return fmt.Errorf("cmux set-status: %w", err)
	}
	return nil
}

// SetProgress sets sidebar progress (0.0–1.0).
func (c *Client) SetProgress(ctx context.Context, target string, pct float64) error {
	_, err := c.r.Run(ctx, "set-progress", "--target", target, "--value", fmt.Sprintf("%.2f", pct))
	if err != nil {
		return fmt.Errorf("cmux set-progress: %w", err)
	}
	return nil
}

// Log appends a sidebar log entry.
func (c *Client) Log(ctx context.Context, target, message string) error {
	_, err := c.r.Run(ctx, "log", "--target", target, "--message", message)
	if err != nil {
		return fmt.Errorf("cmux log: %w", err)
	}
	return nil
}

// TriggerFlash flashes a surface to grab attention.
func (c *Client) TriggerFlash(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "trigger-flash", "--surface", target)
	if err != nil {
		return fmt.Errorf("cmux trigger-flash: %w", err)
	}
	return nil
}
