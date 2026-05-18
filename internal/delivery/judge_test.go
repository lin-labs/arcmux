package delivery

import (
	"context"
	"testing"

	"github.com/lin-labs/arcmux/internal/typesafe"
)

type fakeEvaluator struct {
	resp *typesafe.EvaluationResponse
	err  error
}

func (f fakeEvaluator) Evaluate(_ context.Context, _ any, _ []typesafe.Prompt) (*typesafe.EvaluationResponse, error) {
	return f.resp, f.err
}

func TestHeuristicJudgeDetectsPendingSubmit(t *testing.T) {
	t.Parallel()

	assessment, err := (HeuristicJudge{}).Assess(context.Background(), Evidence{
		Prompt:       "Read Diary/2026-05-16.md and report 5 concise bullets.",
		BeforeOutput: "gpt-5.4 xhigh · ~/iCloud/Obsidian",
		AfterOutput: `
› Read Diary/2026-05-16.md and report 5 concise bullets.

  gpt-5.4 xhigh · ~/iCloud/Obsidian
`,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if assessment.State != StatePendingSubmit {
		t.Fatalf("state = %s", assessment.State)
	}
	if assessment.EnterHelpfulProbability < 0.8 {
		t.Fatalf("enter helpful probability = %.2f", assessment.EnterHelpfulProbability)
	}
}

func TestHeuristicJudgeDetectsIngestedWhileWorking(t *testing.T) {
	t.Parallel()

	assessment, err := (HeuristicJudge{}).Assess(context.Background(), Evidence{
		Prompt:           "Read Diary/2026-05-16.md and report 5 concise bullets.",
		WorkingIndicator: "Working",
		AfterOutput: `
◦ Working (18s • esc to interrupt)
• I’m treating this as a read-only vault task.
`,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if assessment.State != StateIngested {
		t.Fatalf("state = %s", assessment.State)
	}
	if assessment.WorkStartedProbability < 0.9 {
		t.Fatalf("work started probability = %.2f", assessment.WorkStartedProbability)
	}
}

func TestTypesafeJudgeMapsStructuredResults(t *testing.T) {
	t.Parallel()

	judge := &TypesafeJudge{
		evaluator: fakeEvaluator{
			resp: &typesafe.EvaluationResponse{
				Model: "speed_v9_angry_pig",
				Responses: []typesafe.Response{
					{Key: "delivery_state", Type: "choice", Chosen: string(StatePendingSubmit), Confidence: 0.92},
					{Key: "enter_helpful", Type: "noul", Probability: 0.95},
					{Key: "work_started", Type: "noul", Probability: 0.11},
					{Key: "prompt_pending_input", Type: "noul", Probability: 0.93},
				},
			},
		},
		fallback: HeuristicJudge{},
	}

	assessment, err := judge.Assess(context.Background(), Evidence{
		Prompt:      "Read the diary note.",
		AfterOutput: "› Read the diary note.",
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	if assessment.State != StatePendingSubmit {
		t.Fatalf("state = %s", assessment.State)
	}
	if assessment.Source != "typesafe" {
		t.Fatalf("source = %s", assessment.Source)
	}
	if assessment.Model != "speed_v9_angry_pig" {
		t.Fatalf("model = %s", assessment.Model)
	}
	if assessment.EnterHelpfulProbability < 0.9 {
		t.Fatalf("enter helpful probability = %.2f", assessment.EnterHelpfulProbability)
	}
}
