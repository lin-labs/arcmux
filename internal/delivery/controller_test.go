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
