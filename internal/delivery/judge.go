package delivery

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type State string

const (
	StateIngested      State = "ingested"
	StatePendingSubmit State = "pending_submit"
	StateBlocked       State = "blocked"
	StateUnclear       State = "unclear"
)

// Evidence is the input every judge sees for one delivery assessment. Screen
// fields drive the heuristic and typesafe judges; SessionID + DeliveryStartedAt
// let the hooks judge locate and time-bound the agent's cached hook state.
type Evidence struct {
	Agent            string
	Prompt           string
	BeforeOutput     string
	AfterOutput      string
	WorkingIndicator string
	Attempt          int

	// SessionID is the arcmux session whose hook state file the hooks judge
	// reads. Empty for callers that only use screen-based judges.
	SessionID string
	// DeliveryStartedAt marks when this delivery began (right after the prompt
	// was submitted). The hooks judge treats a prompt-submit hook event at or
	// after this instant as proof the agent ingested this delivery. Zero means
	// "unknown" and the hooks judge falls back to the heuristic.
	DeliveryStartedAt time.Time
}

type Assessment struct {
	State                   State
	Confidence              float64
	WorkStartedProbability  float64
	EnterHelpfulProbability float64
	Source                  string
	Model                   string
}

// Judge classifies whether a requested prompt has been ingested by the agent.
// Exactly one judge is selected per daemon via NewJudge.
type Judge interface {
	Assess(ctx context.Context, evidence Evidence) (Assessment, error)
}

// JudgeKind names the available delivery judges. Selection is explicit (config
// [delivery].judge); there is no automatic fallback between typesafe and hooks.
type JudgeKind string

const (
	// JudgeTypesafe asks the Typesafe AI API (heuristic fallback on error or
	// missing key). This is the default to preserve historical behavior.
	JudgeTypesafe JudgeKind = "typesafe"
	// JudgeHooks reads cached per-session hook state ("the cached data") and
	// falls back to the heuristic before the first hook event lands.
	JudgeHooks JudgeKind = "hooks"
	// JudgeHeuristic uses only the screen-scraping heuristic — no network, no
	// hook state.
	JudgeHeuristic JudgeKind = "heuristic"
)

// JudgeOptions is the decoupled config NewJudge needs, so the delivery package
// does not import internal/config. The daemon translates config into this.
type JudgeOptions struct {
	// Kind selects the judge. Empty is treated as JudgeTypesafe.
	Kind JudgeKind
	// SessionStateDir is the directory holding per-session hook state files
	// (~/data/arcmux/sessions). Required when Kind == JudgeHooks.
	SessionStateDir string
}

// NewJudge returns the single judge selected by opts.Kind. An unknown kind is
// an error so a typo in config fails loudly rather than silently degrading.
func NewJudge(opts JudgeOptions) (Judge, error) {
	switch opts.Kind {
	case "", JudgeTypesafe:
		return newTypesafeJudge(), nil
	case JudgeHeuristic:
		return HeuristicJudge{}, nil
	case JudgeHooks:
		if opts.SessionStateDir == "" {
			return nil, fmt.Errorf("delivery judge %q requires a non-empty SessionStateDir", JudgeHooks)
		}
		return newHooksJudge(opts.SessionStateDir, HeuristicJudge{}), nil
	default:
		return nil, fmt.Errorf("unknown delivery judge %q (want one of: %s, %s, %s)",
			opts.Kind, JudgeTypesafe, JudgeHooks, JudgeHeuristic)
	}
}

// HeuristicJudge classifies delivery state from the screen text alone. It is
// always available (no network, no hook state) and serves as the internal
// fallback for the other judges.
type HeuristicJudge struct{}

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
