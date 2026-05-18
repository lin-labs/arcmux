package delivery

import (
	"context"
	"fmt"
	"time"
)

const (
	defaultIngestionTimeout  = 20 * time.Second
	defaultRetryInterval     = 1500 * time.Millisecond
	defaultMaxSubmitAttempts = 3
	defaultMinConfidence     = 0.7
)

type Runtime interface {
	Capture(ctx context.Context) (string, error)
	Submit(ctx context.Context) error
	ResolveKnownBlockers(ctx context.Context) (bool, error)
}

type ControllerConfig struct {
	IngestionTimeout  time.Duration
	RetryInterval     time.Duration
	MaxSubmitAttempts int
	MinConfidence     float64
}

type Controller struct {
	judge Judge
	cfg   ControllerConfig
}

func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{
		IngestionTimeout:  defaultIngestionTimeout,
		RetryInterval:     defaultRetryInterval,
		MaxSubmitAttempts: defaultMaxSubmitAttempts,
		MinConfidence:     defaultMinConfidence,
	}
}

func NewController(judge Judge, cfg ControllerConfig) *Controller {
	if cfg.IngestionTimeout <= 0 {
		cfg.IngestionTimeout = defaultIngestionTimeout
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = defaultRetryInterval
	}
	if cfg.MaxSubmitAttempts <= 0 {
		cfg.MaxSubmitAttempts = defaultMaxSubmitAttempts
	}
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = defaultMinConfidence
	}
	return &Controller{
		judge: judge,
		cfg:   cfg,
	}
}

func (c *Controller) EnsureIngested(ctx context.Context, evidence Evidence, runtime Runtime) (Assessment, error) {
	if c == nil || c.judge == nil {
		return Assessment{}, fmt.Errorf("prompt delivery controller is not configured")
	}

	deadline := time.Now().Add(c.cfg.IngestionTimeout)
	lastOutput := evidence.BeforeOutput
	submitAttempts := 0
	lastAssessment := Assessment{
		State:  StateUnclear,
		Source: "none",
	}

	for {
		output, err := runtime.Capture(ctx)
		if err != nil {
			return lastAssessment, fmt.Errorf("capture delivery state: %w", err)
		}

		current := evidence
		current.AfterOutput = output
		current.Attempt = submitAttempts

		assessment, err := c.judge.Assess(ctx, current)
		if err != nil {
			return lastAssessment, fmt.Errorf("assess prompt delivery: %w", err)
		}
		lastAssessment = assessment

		if c.isIngested(assessment) {
			return assessment, nil
		}

		handled, err := runtime.ResolveKnownBlockers(ctx)
		if err != nil {
			return assessment, fmt.Errorf("resolve delivery blockers: %w", err)
		}
		if handled {
			if err := sleepWithContext(ctx, c.cfg.RetryInterval); err != nil {
				return assessment, err
			}
			lastOutput = output
			continue
		}

		progressed := normalizeScreen(lastOutput) != normalizeScreen(output)
		if c.shouldSubmit(assessment, progressed, submitAttempts) {
			if err := runtime.Submit(ctx); err != nil {
				return assessment, fmt.Errorf("retry prompt submission: %w", err)
			}
			submitAttempts++
			if err := sleepWithContext(ctx, c.cfg.RetryInterval); err != nil {
				return assessment, err
			}
			lastOutput = output
			continue
		}

		if time.Now().After(deadline) {
			break
		}

		if err := sleepWithContext(ctx, c.cfg.RetryInterval); err != nil {
			return assessment, err
		}
		lastOutput = output
	}

	return lastAssessment, fmt.Errorf(
		"prompt not ingested before timeout (state=%s confidence=%.2f source=%s work_started=%.2f enter_helpful=%.2f)",
		lastAssessment.State,
		lastAssessment.Confidence,
		lastAssessment.Source,
		lastAssessment.WorkStartedProbability,
		lastAssessment.EnterHelpfulProbability,
	)
}

func (c *Controller) isIngested(assessment Assessment) bool {
	if assessment.WorkStartedProbability >= 0.88 {
		return true
	}
	return assessment.State == StateIngested && assessment.Confidence >= c.cfg.MinConfidence
}

func (c *Controller) shouldSubmit(assessment Assessment, progressed bool, submitAttempts int) bool {
	if submitAttempts >= c.cfg.MaxSubmitAttempts {
		return false
	}

	switch assessment.State {
	case StatePendingSubmit:
		return assessment.Confidence >= c.cfg.MinConfidence || assessment.EnterHelpfulProbability >= 0.75
	case StateBlocked:
		return assessment.EnterHelpfulProbability >= 0.8
	case StateUnclear:
		if assessment.EnterHelpfulProbability >= 0.9 {
			return true
		}
		return submitAttempts == 0 && !progressed
	default:
		return false
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
