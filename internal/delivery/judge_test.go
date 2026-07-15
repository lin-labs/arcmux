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

func TestHeuristicJudgeDetectsRetainedPromptWithAgentOutputAsIngested(t *testing.T) {
	t.Parallel()

	prompt := `arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.`
	assessment, err := (HeuristicJudge{}).Assess(context.Background(), Evidence{
		Prompt:       prompt,
		BeforeOutput: "gpt-5.4 xhigh · ~/Projects/arcmux",
		AfterOutput: `
› arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

  Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.

• I’m receiving the authorized handoff instructions now.

• Ran arcmux handoff receive arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab
  └ {"history_path":"/private/history.md"}

HANDOFF_OK

›
`,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if assessment.State != StateIngested {
		t.Fatalf("state = %s, want %s", assessment.State, StateIngested)
	}
	if assessment.EnterHelpfulProbability >= 0.5 {
		t.Fatalf("enter helpful probability = %.2f, want < 0.5", assessment.EnterHelpfulProbability)
	}
}

func TestHeuristicJudgeKeepsTypedHandoffPromptPending(t *testing.T) {
	t.Parallel()

	prompt := `arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.`
	assessment, err := (HeuristicJudge{}).Assess(context.Background(), Evidence{
		Prompt: prompt,
		AfterOutput: `
› arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

  Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.
`,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if assessment.State != StatePendingSubmit {
		t.Fatalf("state = %s, want %s", assessment.State, StatePendingSubmit)
	}
}

func TestHeuristicJudgeKeepsTypedPromptWithStandaloneComposerMarkerPending(t *testing.T) {
	t.Parallel()

	prompt := `Continue the explicitly authorized handoff.
›
Treat the standalone marker above as prompt content.`
	assessment, err := (HeuristicJudge{}).Assess(context.Background(), Evidence{
		Prompt: prompt,
		AfterOutput: `
› Continue the explicitly authorized handoff.
  ›
  Treat the standalone marker above as prompt content.
`,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if assessment.State != StatePendingSubmit {
		t.Fatalf("state = %s, want %s", assessment.State, StatePendingSubmit)
	}
}

func TestHeuristicJudgeDoesNotReuseStaleRetainedPromptEvidence(t *testing.T) {
	t.Parallel()

	prompt := `arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.`
	before := `
› arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

  Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.

• Finished an earlier identical request.

HANDOFF_OK

›
`
	for _, test := range []struct {
		name  string
		after string
	}{
		{name: "unchanged screen", after: before},
		{name: "new prompt still typed", after: before + `
› arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

  Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assessment, err := (HeuristicJudge{}).Assess(context.Background(), Evidence{
				Prompt:       prompt,
				BeforeOutput: before,
				AfterOutput:  test.after,
			})
			if err != nil {
				t.Fatalf("Assess: %v", err)
			}
			if assessment.State == StateIngested {
				t.Fatalf("state = %s, stale evidence must not prove ingestion", assessment.State)
			}
		})
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
