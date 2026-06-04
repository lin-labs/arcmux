package delivery

import (
	"context"
	"testing"
)

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
