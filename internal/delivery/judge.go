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
	// LocalOnly prohibits judges from exporting this evidence. It is set from
	// trusted daemon provenance for private supervised sessions; screen output
	// may contain exact local paths or other continuation context.
	LocalOnly bool

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

// JudgeKind names the available delivery judges (config [delivery].judge).
type JudgeKind string

const (
	// JudgeAuto is the cascade hooks → typesafe → heuristic and the default:
	// hook events are ground truth whenever the session's agent emits them, so
	// they always win when available; sessions without a usable hook signal
	// (non-hook-backed agents, or the window before the first event lands)
	// degrade to the typesafe judge, which itself degrades to the heuristic
	// when no API key is configured.
	JudgeAuto JudgeKind = "auto"
	// JudgeTypesafe asks the Typesafe AI API (heuristic fallback on error or
	// missing key). Pin this only to bypass hook state entirely.
	JudgeTypesafe JudgeKind = "typesafe"
	// JudgeHooks reads cached per-session hook state ("the cached data") and
	// falls back to the heuristic (NOT typesafe) before the first hook event
	// lands. Pin this to keep deliveries off the network entirely.
	JudgeHooks JudgeKind = "hooks"
	// JudgeHeuristic uses only the screen-scraping heuristic — no network, no
	// hook state.
	JudgeHeuristic JudgeKind = "heuristic"
)

// JudgeOptions is the decoupled config NewJudge needs, so the delivery package
// does not import internal/config. The daemon translates config into this.
type JudgeOptions struct {
	// Kind selects the judge. Empty is treated as JudgeAuto.
	Kind JudgeKind
	// SessionStateDir is the directory holding per-session hook state files
	// (~/data/arcmux/sessions). Required when Kind is JudgeHooks or JudgeAuto.
	SessionStateDir string
}

// NewJudge returns the single judge selected by opts.Kind. An unknown kind is
// an error so a typo in config fails loudly rather than silently degrading.
func NewJudge(opts JudgeOptions) (Judge, error) {
	switch opts.Kind {
	case "", JudgeAuto:
		if opts.SessionStateDir == "" {
			if opts.Kind == JudgeAuto {
				return nil, fmt.Errorf("delivery judge %q requires a non-empty SessionStateDir", JudgeAuto)
			}
			// Implicit default with no hook state configured: the cascade
			// minus its hooks tier.
			return newTypesafeJudge(), nil
		}
		return newHooksJudge(opts.SessionStateDir, newTypesafeJudge()), nil
	case JudgeTypesafe:
		return newTypesafeJudge(), nil
	case JudgeHeuristic:
		return HeuristicJudge{}, nil
	case JudgeHooks:
		if opts.SessionStateDir == "" {
			return nil, fmt.Errorf("delivery judge %q requires a non-empty SessionStateDir", JudgeHooks)
		}
		return newHooksJudge(opts.SessionStateDir, HeuristicJudge{}), nil
	default:
		return nil, fmt.Errorf("unknown delivery judge %q (want one of: %s, %s, %s, %s)",
			opts.Kind, JudgeAuto, JudgeTypesafe, JudgeHooks, JudgeHeuristic)
	}
}

// HeuristicJudge classifies delivery state from the screen text alone. It is
// always available (no network, no hook state) and serves as the internal
// fallback for the other judges.
type HeuristicJudge struct{}

func (j HeuristicJudge) Assess(_ context.Context, evidence Evidence) (Assessment, error) {
	output := normalizeScreen(evidence.AfterOutput)
	beforePromptCount := promptOccurrenceCount(evidence.BeforeOutput, evidence.Prompt)
	afterPromptCount := promptOccurrenceCount(evidence.AfterOutput, evidence.Prompt)
	promptVisible := afterPromptCount > 0
	promptRetained := !containsStandaloneComposerMarker(evidence.Prompt) &&
		afterPromptCount > beforePromptCount &&
		containsFreshComposerAfterLatestPrompt(evidence.AfterOutput, evidence.Prompt, afterPromptCount)
	blocked := containsBlockerOutsidePrompt(output, evidence.Prompt, afterPromptCount)

	switch {
	case promptRetained:
		return Assessment{
			State:                   StateIngested,
			Confidence:              0.95,
			WorkStartedProbability:  0.97,
			EnterHelpfulProbability: 0.02,
			Source:                  "heuristic",
		}, nil
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
	return promptOccurrenceCount(output, prompt) > 0
}

func promptOccurrenceCount(output, prompt string) int {
	normalizedOutput := normalizeScreen(output)
	count := 0
	for _, fragment := range promptFragments(prompt) {
		fragmentCount := strings.Count(normalizedOutput, normalizeScreen(fragment))
		if fragmentCount > count {
			count = fragmentCount
		}
	}
	return count
}

// containsFreshComposerAfterLatestPrompt distinguishes a newly retained prompt
// from the same text still waiting in the active composer. Codex renders both
// with a leading ›, but only an ingested, completed turn can leave the latest
// prompt occurrence above a new empty composer.
func containsFreshComposerAfterLatestPrompt(output, prompt string, totalPromptCount int) bool {
	lineStart := 0
	for lineStart <= len(output) {
		lineEnd := strings.IndexByte(output[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(output)
		} else {
			lineEnd += lineStart
		}
		if strings.TrimSpace(output[lineStart:lineEnd]) == "›" &&
			promptOccurrenceCount(output[:lineStart], prompt) == totalPromptCount {
			return true
		}
		if lineEnd == len(output) {
			break
		}
		lineStart = lineEnd + 1
	}
	return false
}

func containsStandaloneComposerMarker(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "›" {
			return true
		}
	}
	return false
}

func containsBlockerOutsidePrompt(output, prompt string, promptOccurrences int) bool {
	normalizedPrompt := normalizeScreen(prompt)
	for _, blocker := range []string{"do you trust", "press enter to continue", "resume"} {
		promptBlockers := strings.Count(normalizedPrompt, blocker) * promptOccurrences
		if strings.Count(output, blocker) > promptBlockers {
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
