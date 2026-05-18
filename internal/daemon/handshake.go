package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

const (
	handshakeTimeout      = 30 * time.Second
	trustPromptTimeout    = 3 * time.Second
	readyPatternTimeout   = 25 * time.Second
	handshakePollInterval = 500 * time.Millisecond
)

// performHandshake waits for the agent to be ready, handling trust prompts
// and waiting for the ready pattern.
func (d *Daemon) performHandshake(ctx context.Context, sess *session.Session, prof profile.Profile) error {
	snap := sess.Snapshot()
	target := snap.TmuxTarget

	deadline := time.Now().Add(handshakeTimeout)
	handshakeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Phase 1: Wait for initial output (agent starting up)
	if err := d.waitForInitialOutput(handshakeCtx, target); err != nil {
		return fmt.Errorf("wait for initial output: %w", err)
	}

	// Phase 2: Handle trust prompts (loop because some agents show multiple)
	for {
		handled, err := d.handleTrustPrompt(handshakeCtx, target, prof)
		if err != nil {
			return fmt.Errorf("handle trust prompt: %w", err)
		}
		if !handled {
			break
		}
		d.logger.Info("trust prompt handled", "session_id", snap.ID)
		// Wait a beat for the agent to process the trust confirmation
		if err := d.tmux.WaitIdle(handshakeCtx, target, 10*time.Second, 1*time.Second); err != nil {
			return fmt.Errorf("wait after trust prompt: %w", err)
		}
	}

	// Phase 3: Handle resume prompt (Codex-specific)
	if _, err := d.handleResumePrompt(handshakeCtx, target, prof); err != nil {
		return fmt.Errorf("handle resume prompt: %w", err)
	}

	// Phase 4: Wait for ready pattern
	if prof.ReadyPattern != "" {
		if err := d.waitForReadyPattern(handshakeCtx, target, prof); err != nil {
			return fmt.Errorf("wait for ready pattern: %w", err)
		}
	} else {
		// No ready pattern: just wait for idle
		if err := d.tmux.WaitIdle(handshakeCtx, target, readyPatternTimeout, 2*time.Second); err != nil {
			return fmt.Errorf("wait for idle: %w", err)
		}
	}

	return nil
}

func (d *Daemon) waitForInitialOutput(ctx context.Context, target string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for initial output")
		}

		output, err := d.tmux.CapturePaneVisible(ctx, target)
		if err != nil {
			return err
		}
		if strings.TrimSpace(output) != "" {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(handshakePollInterval):
		}
	}
}

func (d *Daemon) handleTrustPrompt(ctx context.Context, target string, prof profile.Profile) (bool, error) {
	if prof.TrustPromptPattern == "" {
		return false, nil
	}

	// Check if trust prompt is visible
	output, err := d.tmux.CapturePaneVisible(ctx, target)
	if err != nil {
		return false, err
	}

	if !containsFold(output, prof.TrustPromptPattern) {
		return false, nil
	}

	// Trust prompt detected — send response
	response := prof.TrustResponse
	if response == "" {
		response = "Enter"
	}

	if response == "Enter" {
		return true, d.tmux.SendKeys(ctx, target, "Enter")
	}
	return true, d.tmux.SendKeys(ctx, target, response, "Enter")
}

func (d *Daemon) handleResumePrompt(ctx context.Context, target string, prof profile.Profile) (bool, error) {
	if !strings.EqualFold(prof.Name, "codex") {
		return false, nil
	}

	output, err := d.tmux.CapturePaneVisible(ctx, target)
	if err != nil {
		return false, err
	}

	if containsFold(output, "resume") {
		// Decline resume — start fresh
		if err := d.tmux.SendKeys(ctx, target, "n", "Enter"); err != nil {
			return false, err
		}
		return true, d.tmux.WaitIdle(ctx, target, 10*time.Second, 2*time.Second)
	}
	return false, nil
}

func (d *Daemon) waitForReadyPattern(ctx context.Context, target string, prof profile.Profile) error {
	deadline := time.Now().Add(readyPatternTimeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for ready pattern %q", prof.ReadyPattern)
		}

		output, err := d.tmux.CapturePaneVisible(ctx, target)
		if err != nil {
			return err
		}

		if containsFold(output, prof.ReadyPattern) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(handshakePollInterval):
		}
	}
}

// deliverPrompt sends a prompt to the agent and optionally confirms delivery.
func (d *Daemon) deliverPrompt(ctx context.Context, sess *session.Session, prof profile.Profile, text string, confirm bool) error {
	snap := sess.Snapshot()
	target := snap.TmuxTarget
	_ = confirm

	beforeOutput, err := d.tmux.CapturePaneVisible(ctx, target)
	if err != nil {
		beforeOutput = ""
	}

	// Send the prompt text + Enter
	if err := d.tmux.SendKeys(ctx, target, text, "Enter"); err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}

	return d.ensurePromptIngested(ctx, sess, prof, text, beforeOutput)
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
