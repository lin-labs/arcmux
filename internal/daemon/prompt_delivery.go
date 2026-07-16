package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/lin-labs/arcmux/internal/delivery"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

type promptDeliveryRuntime struct {
	daemon    *Daemon
	sessionID string
	target    string
	profile   profile.Profile
}

func (d *Daemon) ensurePromptIngested(ctx context.Context, sess *session.Session, prof profile.Profile, prompt, beforeOutput string, deliveryStartedAt time.Time) error {
	snap := sess.Snapshot()
	assessment, err := d.delivery.EnsureIngested(ctx, delivery.Evidence{
		Agent:             prof.Name,
		Prompt:            prompt,
		BeforeOutput:      beforeOutput,
		WorkingIndicator:  prof.WorkingIndicator,
		SessionID:         snap.ID,
		DeliveryStartedAt: deliveryStartedAt,
		LocalOnly:         snap.Private,
	}, &promptDeliveryRuntime{
		daemon:    d,
		sessionID: snap.ID,
		target:    snap.TmuxTarget,
		profile:   prof,
	})
	if err != nil {
		d.emitEvent(Event{
			SessionID: snap.ID,
			Type:      "prompt_ingestion_failed",
			Message:   err.Error(),
			Timestamp: time.Now(),
			Data: map[string]string{
				"judge_source":              assessment.Source,
				"judge_state":               string(assessment.State),
				"judge_confidence":          fmt.Sprintf("%.2f", assessment.Confidence),
				"enter_helpful_probability": fmt.Sprintf("%.2f", assessment.EnterHelpfulProbability),
				"work_started_probability":  fmt.Sprintf("%.2f", assessment.WorkStartedProbability),
			},
		})
		return err
	}

	d.emitEvent(Event{
		SessionID: snap.ID,
		Type:      "prompt_ingested",
		Timestamp: time.Now(),
		Data: map[string]string{
			"judge_source":              assessment.Source,
			"judge_state":               string(assessment.State),
			"judge_confidence":          fmt.Sprintf("%.2f", assessment.Confidence),
			"enter_helpful_probability": fmt.Sprintf("%.2f", assessment.EnterHelpfulProbability),
			"work_started_probability":  fmt.Sprintf("%.2f", assessment.WorkStartedProbability),
		},
	})
	return nil
}

func (r *promptDeliveryRuntime) Capture(ctx context.Context) (string, error) {
	return r.daemon.tmux.CapturePaneVisible(ctx, r.target)
}

func (r *promptDeliveryRuntime) Submit(ctx context.Context) error {
	if err := r.daemon.tmux.SendKeys(ctx, r.target, "Enter"); err != nil {
		return err
	}
	r.daemon.emitEvent(Event{
		SessionID: r.sessionID,
		Type:      "prompt_redelivery",
		Message:   "sent Enter to advance prompt delivery",
		Timestamp: time.Now(),
		Data: map[string]string{
			"delivery_status": "submitted",
			"submit_key":      "Enter",
		},
	})
	return nil
}

func (r *promptDeliveryRuntime) ResolveKnownBlockers(ctx context.Context) (bool, error) {
	handled, err := r.daemon.handleTrustPrompt(ctx, r.target, r.profile)
	if err != nil || handled {
		if handled {
			r.daemon.emitEvent(Event{
				SessionID: r.sessionID,
				Type:      "prompt_blocker_resolved",
				Message:   "resolved trust prompt while delivering prompt",
				Timestamp: time.Now(),
			})
		}
		return handled, err
	}

	handled, err = r.daemon.handleResumePrompt(ctx, r.target, r.profile)
	if err != nil || handled {
		if handled {
			r.daemon.emitEvent(Event{
				SessionID: r.sessionID,
				Type:      "prompt_blocker_resolved",
				Message:   "resolved resume prompt while delivering prompt",
				Timestamp: time.Now(),
			})
		}
		return handled, err
	}

	return false, nil
}
