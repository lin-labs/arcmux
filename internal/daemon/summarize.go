package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/lin-labs/arcmux/internal/hooks"
)

// ErrSummarizeInFlight signals a debounced duplicate summarize request.
var ErrSummarizeInFlight = errors.New("summarize already in flight for this session")

// SummarizeSessionByID is the synchronous on-demand entry point for the same
// daemon-owned summarization path used by the background scheduler. It never
// captures pane scrollback and has no subprocess fallback: only verified hook
// state, trusted provenance, and the explicitly configured direct API provider
// are reachable through refreshOverallGoalOnce.
func (d *Daemon) SummarizeSessionByID(ctx context.Context, sessionID string) (string, error) {
	candidate, err := d.onDemandGoalSummaryCandidate(sessionID)
	if err != nil {
		return "", err
	}
	result := make(chan error, 1)
	if err := d.enqueueOverallGoalSummary(d.ctx, candidate, true, result); err != nil {
		return "", err
	}
	select {
	case err := <-result:
		if err != nil {
			return "", err
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, sessionID)
	if err != nil {
		return "", err
	}
	if state == nil || state.TurnContract == nil ||
		state.TurnContract.OverallGoalProvenance != hooks.OverallGoalSummarizerProvenance {
		return "", fmt.Errorf("trusted overall-goal summary was not recorded")
	}
	d.logger.Info("session.summarized", "session_id", sessionID)
	return state.TurnContract.OverallGoal, nil
}

// QueueSessionSummaryByID admits an on-demand request without creating a
// request-owned goroutine. The process-wide pool provides the only workers.
func (d *Daemon) QueueSessionSummaryByID(sessionID string) error {
	candidate, err := d.onDemandGoalSummaryCandidate(sessionID)
	if err != nil {
		return err
	}
	return d.enqueueOverallGoalSummary(d.ctx, candidate, true, nil)
}

func (d *Daemon) onDemandGoalSummaryCandidate(sessionID string) (goalSummaryCandidate, error) {
	managed, ok := d.GetSession(sessionID)
	if !ok {
		return goalSummaryCandidate{}, errors.New("session not found")
	}
	snap := managed.Snapshot()
	if snap.Private {
		return goalSummaryCandidate{}, errors.New("private session summary is disabled")
	}
	candidate, ok := d.goalSummaryCandidateWithPolicy(snap, true)
	if !ok {
		return goalSummaryCandidate{}, errors.New("session has no pending summarizable turn")
	}
	return candidate, nil
}
