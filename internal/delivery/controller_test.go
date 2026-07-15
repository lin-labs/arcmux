package delivery

import (
	"context"
	"testing"
	"time"
)

type fakeRuntime struct {
	outputs         []string
	captureIndex    int
	submitCount     int
	resolveCount    int
	blockersHandled bool
}

func (r *fakeRuntime) Capture(_ context.Context) (string, error) {
	if len(r.outputs) == 0 {
		return "", nil
	}
	if r.captureIndex >= len(r.outputs) {
		return r.outputs[len(r.outputs)-1], nil
	}
	out := r.outputs[r.captureIndex]
	if r.captureIndex < len(r.outputs)-1 {
		r.captureIndex++
	}
	return out, nil
}

func (r *fakeRuntime) Submit(_ context.Context) error {
	r.submitCount++
	return nil
}

func (r *fakeRuntime) ResolveKnownBlockers(_ context.Context) (bool, error) {
	r.resolveCount++
	return r.blockersHandled, nil
}

func TestControllerRetriesSubmitUntilIngested(t *testing.T) {
	t.Parallel()

	controller := NewController(HeuristicJudge{}, ControllerConfig{
		IngestionTimeout:  100 * time.Millisecond,
		RetryInterval:     time.Millisecond,
		MaxSubmitAttempts: 2,
		MinConfidence:     0.7,
	})
	runtime := &fakeRuntime{
		outputs: []string{
			"› Read Diary/2026-05-16.md and report 5 concise bullets.",
			"◦ Working (2s • esc to interrupt)\n• Reading the diary now.",
		},
	}

	assessment, err := controller.EnsureIngested(context.Background(), Evidence{
		Prompt:           "Read Diary/2026-05-16.md and report 5 concise bullets.",
		WorkingIndicator: "Working",
	}, runtime)
	if err != nil {
		t.Fatalf("EnsureIngested: %v", err)
	}
	if assessment.State != StateIngested {
		t.Fatalf("state = %s", assessment.State)
	}
	if runtime.submitCount != 1 {
		t.Fatalf("submit count = %d", runtime.submitCount)
	}
}

func TestControllerFailsAfterTimeout(t *testing.T) {
	t.Parallel()

	controller := NewController(HeuristicJudge{}, ControllerConfig{
		IngestionTimeout:  10 * time.Millisecond,
		RetryInterval:     time.Millisecond,
		MaxSubmitAttempts: 1,
		MinConfidence:     0.7,
	})
	runtime := &fakeRuntime{
		outputs: []string{
			"gpt-5.4 xhigh · ~/iCloud/Obsidian",
		},
	}

	_, err := controller.EnsureIngested(context.Background(), Evidence{
		Prompt:       "Do the work.",
		BeforeOutput: "gpt-5.4 xhigh · ~/iCloud/Obsidian",
	}, runtime)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if runtime.submitCount != 1 {
		t.Fatalf("submit count = %d", runtime.submitCount)
	}
}

func TestControllerDoesNotAcceptTypedPromptAsDelivered(t *testing.T) {
	t.Parallel()

	controller := NewController(HeuristicJudge{}, ControllerConfig{
		IngestionTimeout:  10 * time.Millisecond,
		RetryInterval:     time.Millisecond,
		MaxSubmitAttempts: 1,
		MinConfidence:     0.7,
	})
	runtime := &fakeRuntime{
		outputs: []string{
			"› Do the implementation work now.\n\ngpt-5.5 medium · ~/Projects/test6",
		},
	}

	assessment, err := controller.EnsureIngested(context.Background(), Evidence{
		Prompt: "Do the implementation work now.",
	}, runtime)
	if err == nil {
		t.Fatal("expected typed-only prompt to fail delivery confirmation")
	}
	if assessment.State != StatePendingSubmit {
		t.Fatalf("state = %s, want %s", assessment.State, StatePendingSubmit)
	}
	if runtime.submitCount != 1 {
		t.Fatalf("submit count = %d, want 1", runtime.submitCount)
	}
}

func TestControllerAcceptsRetainedPromptWithAgentOutputWithoutExtraSubmit(t *testing.T) {
	t.Parallel()

	prompt := `arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.`
	controller := NewController(HeuristicJudge{}, ControllerConfig{
		IngestionTimeout:  20 * time.Millisecond,
		RetryInterval:     time.Millisecond,
		MaxSubmitAttempts: 1,
		MinConfidence:     0.7,
	})
	runtime := &fakeRuntime{outputs: []string{`
› arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab--------------------------------------------------------------------------

  Resume this explicitly authorized handoff. Run arcmux handoff receive before acting.

• I’m receiving the authorized handoff instructions now.

• Ran arcmux handoff receive arcmux-handoff-v1:ef01fc475b773bc9ef38af6789a029ab
  └ {"history_path":"/private/history.md"}

HANDOFF_OK

›
`}}

	assessment, err := controller.EnsureIngested(context.Background(), Evidence{
		Prompt:       prompt,
		BeforeOutput: "gpt-5.4 xhigh · ~/Projects/arcmux",
	}, runtime)
	if err != nil {
		t.Fatalf("EnsureIngested: %v", err)
	}
	if assessment.State != StateIngested {
		t.Fatalf("state = %s, want %s", assessment.State, StateIngested)
	}
	if runtime.submitCount != 0 {
		t.Fatalf("submit count = %d, want 0", runtime.submitCount)
	}
}

func TestIsIngested_SoftPassPath(t *testing.T) {
	t.Parallel()
	c := NewController(HeuristicJudge{}, ControllerConfig{MinConfidence: 0.7})

	cases := []struct {
		name string
		a    Assessment
		want bool
	}{
		{
			"strict pass — high confidence",
			Assessment{State: StateIngested, Confidence: 0.85, WorkStartedProbability: 0.30},
			true,
		},
		{
			"strong work_started — bypass everything",
			Assessment{State: StateUnclear, Confidence: 0.10, WorkStartedProbability: 0.92},
			true,
		},
		{
			"soft pass — elonco's case (confidence 0.59, work_started 0.71)",
			Assessment{State: StateIngested, Confidence: 0.59, WorkStartedProbability: 0.71},
			true,
		},
		{
			"soft pass — minimum thresholds (0.4 / 0.5)",
			Assessment{State: StateIngested, Confidence: 0.40, WorkStartedProbability: 0.50},
			true,
		},
		{
			"hard fail — below soft-pass confidence",
			Assessment{State: StateIngested, Confidence: 0.39, WorkStartedProbability: 0.70},
			false,
		},
		{
			"hard fail — below soft-pass work_started",
			Assessment{State: StateIngested, Confidence: 0.60, WorkStartedProbability: 0.49},
			false,
		},
		{
			"hard fail — wrong state",
			Assessment{State: StatePendingSubmit, Confidence: 0.85, WorkStartedProbability: 0.70},
			false,
		},
		{
			"earlier reported error 1 — confidence 0.10, work_started 0.45",
			Assessment{State: StateIngested, Confidence: 0.10, WorkStartedProbability: 0.45},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.isIngested(tc.a)
			if got != tc.want {
				t.Errorf("isIngested(%+v) = %v, want %v", tc.a, got, tc.want)
			}
		})
	}
}
