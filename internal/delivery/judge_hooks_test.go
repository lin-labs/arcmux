package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
)

type fakeStateReader struct {
	state *hooks.SessionState
	err   error
}

func (f fakeStateReader) ReadSessionState(_, _ string) (*hooks.SessionState, error) {
	return f.state, f.err
}

func hooksJudgeWith(reader stateReader) *HooksJudge {
	return &HooksJudge{stateDir: "/unused", reader: reader, fallback: HeuristicJudge{}}
}

func TestHooksJudgeIngestedWhenPromptSubmitAfterDeliveryStart(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 3, 18, 0, 0, 0, time.UTC)
	j := hooksJudgeWith(fakeStateReader{state: &hooks.SessionState{
		SessionID:          "s-1",
		Working:            true,
		LastPromptSubmitAt: start.Add(2 * time.Second),
	}})

	a, err := j.Assess(context.Background(), Evidence{SessionID: "s-1", DeliveryStartedAt: start})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.State != StateIngested {
		t.Fatalf("state = %s, want ingested", a.State)
	}
	if a.Source != "hooks" {
		t.Fatalf("source = %s, want hooks", a.Source)
	}
	if a.WorkStartedProbability < 0.9 {
		t.Fatalf("work_started = %.2f, want >=0.9 (working)", a.WorkStartedProbability)
	}
}

func TestHooksJudgeIngestedWhenTurnEndedAfterSubmit(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 3, 18, 0, 0, 0, time.UTC)
	submit := start.Add(1 * time.Second)
	j := hooksJudgeWith(fakeStateReader{state: &hooks.SessionState{
		SessionID:          "s-1",
		Working:            false,
		LastPromptSubmitAt: submit,
		LastTurnEndAt:      submit.Add(5 * time.Second),
	}})

	a, err := j.Assess(context.Background(), Evidence{SessionID: "s-1", DeliveryStartedAt: start})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.State != StateIngested {
		t.Fatalf("state = %s, want ingested", a.State)
	}
	if a.WorkStartedProbability < 0.9 {
		t.Fatalf("work_started = %.2f, want >=0.9 (turn completed)", a.WorkStartedProbability)
	}
}

func TestHooksJudgeFallsBackWhenSubmitPredatesDelivery(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 3, 18, 0, 0, 0, time.UTC)
	// A submit from a PRIOR delivery — not evidence for this one.
	j := hooksJudgeWith(fakeStateReader{state: &hooks.SessionState{
		SessionID:          "s-1",
		LastPromptSubmitAt: start.Add(-30 * time.Second),
	}})

	a, err := j.Assess(context.Background(), Evidence{
		SessionID:         "s-1",
		DeliveryStartedAt: start,
		// Heuristic should classify this screen as pending submit.
		Prompt:      "Read Diary/2026-05-16.md and report 5 concise bullets.",
		AfterOutput: "› Read Diary/2026-05-16.md and report 5 concise bullets.",
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Source != "heuristic" {
		t.Fatalf("source = %s, want heuristic fallback", a.Source)
	}
	if a.State != StatePendingSubmit {
		t.Fatalf("state = %s, want pending_submit (from heuristic)", a.State)
	}
}

func TestHooksJudgeFallsBackWhenNoState(t *testing.T) {
	t.Parallel()

	j := hooksJudgeWith(fakeStateReader{state: nil})
	a, err := j.Assess(context.Background(), Evidence{
		SessionID:         "s-1",
		DeliveryStartedAt: time.Now(),
		WorkingIndicator:  "Working",
		AfterOutput:       "◦ Working (3s • esc to interrupt)",
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Source != "heuristic" {
		t.Fatalf("source = %s, want heuristic fallback when no hook state", a.Source)
	}
}

func TestHooksJudgeFallsBackWithoutSessionID(t *testing.T) {
	t.Parallel()

	// Reader would error if consulted; it must not be consulted without an id.
	j := hooksJudgeWith(fakeStateReader{err: context.DeadlineExceeded})
	a, err := j.Assess(context.Background(), Evidence{
		DeliveryStartedAt: time.Now(),
		Prompt:            "Read the note.",
		AfterOutput:       "› Read the note.",
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if a.Source != "heuristic" {
		t.Fatalf("source = %s, want heuristic when no session id", a.Source)
	}
}

func TestNewJudgeSelection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind    JudgeKind
		wantErr bool
	}{
		{JudgeHeuristic, false},
		{JudgeHooks, false},
		{JudgeAuto, false},
		{"", false},
		{"bogus", true},
	}
	for _, c := range cases {
		j, err := NewJudge(JudgeOptions{Kind: c.kind, SessionStateDir: "/tmp/x"})
		if c.wantErr {
			if err == nil {
				t.Fatalf("kind %q: expected error", c.kind)
			}
			continue
		}
		if err != nil {
			t.Fatalf("kind %q: unexpected error %v", c.kind, err)
		}
		if j == nil {
			t.Fatalf("kind %q: nil judge", c.kind)
		}
	}
}

// TestNewJudgeAutoCascade pins the default wiring: auto (and the empty default
// with a state dir) is hooks-first — the returned judge must consult per-session
// hook state before anything else. Explicit auto without a state dir is a
// loud error; the implicit empty default degrades to the non-hooks tiers.
func TestNewJudgeAutoCascade(t *testing.T) {
	t.Parallel()

	for _, kind := range []JudgeKind{JudgeAuto, ""} {
		j, err := NewJudge(JudgeOptions{Kind: kind, SessionStateDir: "/tmp/x"})
		if err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
		if _, ok := j.(*HooksJudge); !ok {
			t.Fatalf("kind %q with state dir: judge = %T, want *HooksJudge (hooks must win when available)", kind, j)
		}
	}

	if _, err := NewJudge(JudgeOptions{Kind: JudgeAuto}); err == nil {
		t.Fatal("explicit auto without SessionStateDir must error")
	}
	j, err := NewJudge(JudgeOptions{})
	if err != nil {
		t.Fatalf("empty default without state dir: %v", err)
	}
	if _, ok := j.(*HooksJudge); ok {
		t.Fatal("empty default without state dir must not be a hooks judge")
	}
}
