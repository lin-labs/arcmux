package delivery

import (
	"context"

	"github.com/lin-labs/arcmux/internal/hooks"
)

// stateReader is the slice of hooks state access the judge needs. An interface
// so tests can inject session states without touching the filesystem.
type stateReader interface {
	ReadSessionState(stateDir, sessionID string) (*hooks.SessionState, error)
}

type fsStateReader struct{}

func (fsStateReader) ReadSessionState(stateDir, sessionID string) (*hooks.SessionState, error) {
	return hooks.ReadSessionState(stateDir, sessionID)
}

// HooksJudge decides delivery state from the agent's own cached hook events
// ("the cached data") rather than from the screen or the Typesafe API. A
// prompt_submit event at or after the delivery start instant is ground truth
// that the agent ingested the prompt. Before any usable hook signal exists it
// defers to the heuristic fallback, so it is never blind during the early
// delivery window.
type HooksJudge struct {
	stateDir string
	reader   stateReader
	fallback Judge
}

func newHooksJudge(stateDir string, fallback Judge) *HooksJudge {
	return &HooksJudge{
		stateDir: stateDir,
		reader:   fsStateReader{},
		fallback: fallback,
	}
}

func (j *HooksJudge) Assess(ctx context.Context, evidence Evidence) (Assessment, error) {
	// Without a session id or a known delivery start we can't time-bound the
	// hook state — fall back to the screen heuristic.
	if evidence.SessionID == "" || evidence.DeliveryStartedAt.IsZero() {
		return j.fallback.Assess(ctx, evidence)
	}

	state, err := j.reader.ReadSessionState(j.stateDir, evidence.SessionID)
	if err != nil || state == nil {
		// No hook data yet (or unreadable) — defer to the heuristic. The
		// controller keeps polling, and the next assessment will see the file
		// once the agent's hook fires.
		return j.fallback.Assess(ctx, evidence)
	}

	// Ground truth (with one caveat): a prompt-submit hook fired at or after
	// this delivery began. DeliveryStartedAt is stamped on the same host, just
	// before the submit key is sent, so there is no clock skew and no
	// false-negative race. The caveat is identity, not freshness: this proves a
	// prompt was submitted in the window, not that it was *this* exact prompt —
	// a pathologically delayed prior-turn hook could in theory land here. In
	// practice prompt-submit hooks fire synchronously on submit, so the window
	// is tight. A future hardening can carry a prompt hash/nonce in the hook
	// payload and match it against Evidence.Prompt for true identity.
	if !state.LastPromptSubmitAt.Before(evidence.DeliveryStartedAt) {
		assessment := Assessment{
			State:                   StateIngested,
			Confidence:              0.97,
			WorkStartedProbability:  0.5,
			EnterHelpfulProbability: 0.05,
			Source:                  "hooks",
		}
		if state.Working {
			// Still mid-turn: the agent is actively working the prompt.
			assessment.WorkStartedProbability = 0.97
		} else if !state.LastTurnEndAt.Before(state.LastPromptSubmitAt) {
			// The turn already ended after ingesting — work happened and
			// completed within the delivery window.
			assessment.WorkStartedProbability = 0.97
		}
		return assessment, nil
	}

	// Hook state exists but predates this delivery: the prompt isn't ingested
	// yet. Let the heuristic shape the pending/blocked nuance the controller
	// uses to decide whether to re-submit.
	return j.fallback.Assess(ctx, evidence)
}
