package delivery

import (
	"context"
	"strings"

	"github.com/lin-labs/arcmux/internal/typesafe"
)

type State string

const (
	StateIngested      State = "ingested"
	StatePendingSubmit State = "pending_submit"
	StateBlocked       State = "blocked"
	StateUnclear       State = "unclear"
)

type Evidence struct {
	Agent            string
	Prompt           string
	BeforeOutput     string
	AfterOutput      string
	WorkingIndicator string
	Attempt          int
}

type Assessment struct {
	State                   State
	Confidence              float64
	WorkStartedProbability  float64
	EnterHelpfulProbability float64
	Source                  string
	Model                   string
}

type Judge interface {
	Assess(ctx context.Context, evidence Evidence) (Assessment, error)
}

type evaluator interface {
	Evaluate(ctx context.Context, document any, prompts []typesafe.Prompt) (*typesafe.EvaluationResponse, error)
}

type HeuristicJudge struct{}

type TypesafeJudge struct {
	evaluator evaluator
	fallback  Judge
}

func NewJudge() Judge {
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

func (j HeuristicJudge) Assess(_ context.Context, evidence Evidence) (Assessment, error) {
	output := normalizeScreen(evidence.AfterOutput)
	promptVisible := containsPromptFragment(output, evidence.Prompt)
	blocked := containsFold(output, "do you trust") ||
		containsFold(output, "press enter to continue") ||
		containsFold(output, "resume")

	switch {
	case blocked:
		return Assessment{
			State:                   StateBlocked,
			Confidence:              0.93,
			EnterHelpfulProbability: 0.95,
			Source:                  "heuristic",
		}, nil
	case evidence.WorkingIndicator != "" && containsFold(output, evidence.WorkingIndicator):
		return Assessment{
			State:                   StateIngested,
			Confidence:              0.9,
			WorkStartedProbability:  0.96,
			EnterHelpfulProbability: 0.05,
			Source:                  "heuristic",
		}, nil
	case promptVisible:
		return Assessment{
			State:                   StatePendingSubmit,
			Confidence:              0.84,
			WorkStartedProbability:  0.18,
			EnterHelpfulProbability: 0.91,
			Source:                  "heuristic",
		}, nil
	case normalizeScreen(evidence.BeforeOutput) != output:
		return Assessment{
			State:                   StateIngested,
			Confidence:              0.74,
			WorkStartedProbability:  0.73,
			EnterHelpfulProbability: 0.1,
			Source:                  "heuristic",
		}, nil
	default:
		return Assessment{
			State:                   StateUnclear,
			Confidence:              0.4,
			WorkStartedProbability:  0.35,
			EnterHelpfulProbability: 0.62,
			Source:                  "heuristic",
		}, nil
	}
}

func (j *TypesafeJudge) Assess(ctx context.Context, evidence Evidence) (Assessment, error) {
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

func trimForDecision(s string) string {
	const max = 4000
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func normalizeScreen(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func containsPromptFragment(output, prompt string) bool {
	normalizedOutput := normalizeScreen(output)
	for _, fragment := range promptFragments(prompt) {
		if strings.Contains(normalizedOutput, normalizeScreen(fragment)) {
			return true
		}
	}
	return false
}

func promptFragments(prompt string) []string {
	var fragments []string
	seen := make(map[string]bool)

	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 18 {
			continue
		}
		if len(line) > 120 {
			line = line[:120]
		}
		key := normalizeScreen(line)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		fragments = append(fragments, line)
		if len(fragments) == 3 {
			return fragments
		}
	}

	if len(fragments) == 0 {
		trimmed := strings.TrimSpace(prompt)
		if len(trimmed) > 120 {
			trimmed = trimmed[:120]
		}
		if len(trimmed) >= 18 {
			fragments = append(fragments, trimmed)
		}
	}

	return fragments
}
