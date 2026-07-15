package delivery

import (
	"context"

	"github.com/lin-labs/arcmux/internal/typesafe"
)

// evaluator is the narrow slice of the typesafe client the judge needs. Kept an
// interface so tests can inject a fake (see judge_typesafe_test.go).
type evaluator interface {
	Evaluate(ctx context.Context, document any, prompts []typesafe.Prompt) (*typesafe.EvaluationResponse, error)
}

// TypesafeJudge asks the Typesafe AI API to classify the delivery state from a
// structured snapshot of the screen. It falls back to the always-available
// heuristic judge when the API errors — that internal fallback is the
// heuristic, NOT a cross-judge typesafe<->hooks fallback (selection between
// judges is an explicit single choice; see NewJudge).
type TypesafeJudge struct {
	evaluator evaluator
	fallback  Judge
}

// newTypesafeJudge builds a TypesafeJudge from the ambient TYPESAFE_API_KEY. It
// returns the heuristic judge unchanged when no key is configured, so callers
// always get a working judge.
func newTypesafeJudge() Judge {
	fallback := HeuristicJudge{}
	client := typesafe.NewFromEnv()
	if client == nil {
		return fallback
	}
	return &TypesafeJudge{
		evaluator: client,
		fallback:  fallback,
	}
}

func (j *TypesafeJudge) Assess(ctx context.Context, evidence Evidence) (Assessment, error) {
	if evidence.LocalOnly {
		return (HeuristicJudge{}).Assess(ctx, evidence)
	}
	if j == nil || j.evaluator == nil {
		return j.fallback.Assess(ctx, evidence)
	}

	document := map[string]any{
		"agent":              evidence.Agent,
		"requested_prompt":   evidence.Prompt,
		"screen_before_send": trimForDecision(evidence.BeforeOutput),
		"screen_after_send":  trimForDecision(evidence.AfterOutput),
		"working_indicator":  evidence.WorkingIndicator,
		"attempt":            evidence.Attempt,
	}

	resp, err := j.evaluator.Evaluate(ctx, document, []typesafe.Prompt{
		{
			Key:          "delivery_state",
			Type:         "choice",
			Instructions: "Which state best describes whether the requested prompt has been ingested by the agent?",
			Options: []typesafe.ChoiceOption{
				{Option: string(StateIngested), Description: "The agent has accepted the prompt and is now responding, running tools, or otherwise working on it."},
				{Option: string(StatePendingSubmit), Description: "The prompt text is still sitting in the input composer and likely needs another submit action such as pressing Enter."},
				{Option: string(StateBlocked), Description: "A trust prompt, resume prompt, confirmation prompt, or similar interactive blocker is preventing the requested prompt from being ingested."},
				{Option: string(StateUnclear), Description: "The screen does not provide enough evidence to tell whether the prompt was ingested."},
			},
		},
		{
			Key:          "enter_helpful",
			Type:         "noul",
			Instructions: "Would pressing Enter now likely advance delivery of the requested prompt rather than interrupt active work?",
		},
		{
			Key:          "work_started",
			Type:         "noul",
			Instructions: "Has the agent already started acting on the requested prompt?",
		},
		{
			Key:          "prompt_pending_input",
			Type:         "noul",
			Instructions: "Is the requested prompt still visible in the agent input composer rather than already submitted?",
		},
	})
	if err != nil {
		if j.fallback == nil {
			return Assessment{}, err
		}
		return j.fallback.Assess(ctx, evidence)
	}

	assessment := Assessment{
		State:  StateUnclear,
		Source: "typesafe",
		Model:  resp.Model,
	}

	for _, response := range resp.Responses {
		switch response.Key {
		case "delivery_state":
			assessment.State = State(response.Chosen)
			assessment.Confidence = response.Confidence
		case "enter_helpful":
			assessment.EnterHelpfulProbability = response.Probability
		case "work_started":
			assessment.WorkStartedProbability = response.Probability
		case "prompt_pending_input":
			if assessment.State == StateUnclear && response.Probability >= 0.8 {
				assessment.State = StatePendingSubmit
				if assessment.Confidence < response.Probability {
					assessment.Confidence = response.Probability
				}
			}
		}
	}

	if assessment.State == StateUnclear && assessment.WorkStartedProbability >= 0.88 {
		assessment.State = StateIngested
		if assessment.Confidence < assessment.WorkStartedProbability {
			assessment.Confidence = assessment.WorkStartedProbability
		}
	}

	return assessment, nil
}

// trimForDecision caps the screen text sent to the typesafe API to the most
// recent bytes — the tail is where delivery state is visible.
func trimForDecision(s string) string {
	const max = 4000
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
