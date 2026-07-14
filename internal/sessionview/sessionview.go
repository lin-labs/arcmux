// Package sessionview defines the safe, transport-neutral view of local
// sessions that arcmux may expose to another authenticated device.
//
// These DTOs are intentionally allowlisted. Adding a field here is a security
// decision: the runtime session snapshot also contains environment variables,
// process IDs, backend session handles, tmux targets, and raw commands, none of
// which belong in this package.
package sessionview

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/session"
)

const (
	// LocatorVersion identifies the local session-locator schema. The peer or
	// device identity is supplied by the authenticated mesh connection and is
	// deliberately not accepted from local session state.
	LocatorVersion = 1

	RootProfileScope ProfileScope = "root"

	maxGoalRunes        = 1024
	maxOverallGoalRunes = 4096
	maxSourceRunes      = 128
	maxCWDRunes         = 4096
)

var (
	profileNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$`)
	sessionIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	secretAssignment   = regexp.MustCompile(`(?i)\b(api[_ -]?key|access[_ -]?token|auth[_ -]?token|password|secret)\s*[:=]\s*([^\s,;]+)`)
	bearerToken        = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`)
	commonToken        = regexp.MustCompile(`\b(?:sk|ghp|github_pat|xox[baprs])[-_][A-Za-z0-9_-]{8,}\b`)
)

// ProfileScope disambiguates sessions that have the same ID in independent
// profile daemons. Valid values are "root" and "profile:<normalized-name>".
type ProfileScope string

// NamedProfileScope constructs the canonical scope for a normalized profile.
func NamedProfileScope(normalizedName string) (ProfileScope, error) {
	if !profileNamePattern.MatchString(normalizedName) {
		return "", fmt.Errorf("invalid normalized profile name %q", normalizedName)
	}
	return ProfileScope("profile:" + normalizedName), nil
}

// Validate rejects ambiguous or non-canonical profile scopes.
func (s ProfileScope) Validate() error {
	if s == RootProfileScope {
		return nil
	}
	name, ok := strings.CutPrefix(string(s), "profile:")
	if !ok || !profileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid profile scope %q", s)
	}
	return nil
}

// ProfileName returns the normalized name for a named scope. Root has no
// profile name.
func (s ProfileScope) ProfileName() (string, bool) {
	name, ok := strings.CutPrefix(string(s), "profile:")
	if !ok || !profileNamePattern.MatchString(name) {
		return "", false
	}
	return name, true
}

// Locator is stable within one arcmux device. A mesh consumer must pair it
// with the authenticated peer identity; a peer ID received in a payload must
// never override connection identity.
type Locator struct {
	Version      int          `json:"version"`
	ProfileScope ProfileScope `json:"profile_scope"`
	SessionID    string       `json:"session_id"`
}

func NewLocator(scope ProfileScope, sessionID string) (Locator, error) {
	locator := Locator{Version: LocatorVersion, ProfileScope: scope, SessionID: sessionID}
	return locator, locator.Validate()
}

func (l Locator) Validate() error {
	if l.Version != LocatorVersion {
		return fmt.Errorf("unsupported locator version %d", l.Version)
	}
	if err := l.ProfileScope.Validate(); err != nil {
		return err
	}
	if !sessionIDPattern.MatchString(l.SessionID) {
		return fmt.Errorf("invalid session id %q", l.SessionID)
	}
	return nil
}

// Freshness records when the catalog observed the daemon and when its safe
// source data most recently changed. It lets consumers distinguish a current
// observation of an idle session from a stale cached response.
type Freshness struct {
	ObservedAt      time.Time `json:"observed_at"`
	SourceUpdatedAt time.Time `json:"source_updated_at"`
}

// WorkSummary contains only summarized work context. LatestAsk remains empty
// until hook state has an explicitly summarized field for it; LastUserMessage
// is a raw prompt and must never be substituted.
type WorkSummary struct {
	Goal        string    `json:"goal,omitempty"`
	OverallGoal string    `json:"overall_goal,omitempty"`
	LatestAsk   string    `json:"latest_ask,omitempty"`
	Source      string    `json:"source,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// HistoryReference names a shared history artifact without exposing its local
// absolute path. The artifact layer may later resolve the basename under an
// authorized store.
type HistoryReference struct {
	Basename   string    `json:"basename"`
	Provenance string    `json:"provenance"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Summary is the safe list representation. LaunchCWD means the cwd recorded at
// session launch; it is not a claim about the pane's current directory. A
// later grant filter may omit it for a particular peer.
type Summary struct {
	Locator        Locator           `json:"locator"`
	Name           string            `json:"name,omitempty"`
	Agent          string            `json:"agent"`
	Transport      string            `json:"transport,omitempty"`
	LaunchCWD      string            `json:"launch_cwd,omitempty"`
	OwnerID        string            `json:"owner_id,omitempty"`
	State          string            `json:"state"`
	Health         string            `json:"health,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
	LastActivityAt time.Time         `json:"last_activity_at"`
	IdleSince      *time.Time        `json:"idle_since,omitempty"`
	WorkingSince   *time.Time        `json:"working_since,omitempty"`
	Work           *WorkSummary      `json:"work,omitempty"`
	History        *HistoryReference `json:"history,omitempty"`
	Freshness      Freshness         `json:"freshness"`
}

// TurnActivity is a bounded, safe subset of hook lifecycle data. Event counts
// and timestamps are included; event payloads and tool output are not.
type TurnActivity struct {
	TurnCount          int        `json:"turn_count"`
	EventsSeen         int        `json:"events_seen"`
	LastPromptSubmitAt *time.Time `json:"last_prompt_submit_at,omitempty"`
	LastTurnEndAt      *time.Time `json:"last_turn_end_at,omitempty"`
}

// Detail adds bounded lifecycle counters to Summary. It intentionally has no
// capture, command, environment, process, tmux, or backend-session fields.
type Detail struct {
	Summary    Summary       `json:"summary"`
	NudgeCount int           `json:"nudge_count"`
	Turn       *TurnActivity `json:"turn,omitempty"`
}

// List is one coherent catalog observation.
type List struct {
	ObservedAt time.Time `json:"observed_at"`
	Sessions   []Summary `json:"sessions"`
}

// Build creates summary and detail DTOs from one read-consistent daemon
// snapshot. Hook state is optional and should be passed as nil after any
// missing/corrupt read.
func Build(scope ProfileScope, snap session.Snapshot, hookState *hooks.SessionState, observedAt time.Time) (Summary, Detail, error) {
	locator, err := NewLocator(scope, snap.ID)
	if err != nil {
		return Summary{}, Detail{}, err
	}

	summary := Summary{
		Locator:        locator,
		Name:           cleanText(snap.Name, maxSourceRunes),
		Agent:          cleanText(snap.Agent, maxSourceRunes),
		Transport:      cleanText(snap.Transport, maxSourceRunes),
		LaunchCWD:      cleanText(snap.CWD, maxCWDRunes),
		OwnerID:        cleanText(snap.OwnerID, maxGoalRunes),
		State:          string(snap.State),
		Health:         cleanText(snap.Health, maxSourceRunes),
		StartedAt:      snap.StartedAt,
		LastActivityAt: snap.LastActivityAt,
		IdleSince:      cloneTime(snap.IdleSince),
		WorkingSince:   cloneTime(snap.WorkingSince),
		Freshness: Freshness{
			ObservedAt:      observedAt,
			SourceUpdatedAt: snap.LastActivityAt,
		},
	}
	detail := Detail{Summary: summary, NudgeCount: snap.NudgeCount}

	if hookState == nil {
		return summary, detail, nil
	}
	if hookState.UpdatedAt.After(summary.Freshness.SourceUpdatedAt) {
		summary.Freshness.SourceUpdatedAt = hookState.UpdatedAt
	}
	if tc := hookState.TurnContract; tc != nil {
		goal := cleanText(tc.Goal, maxGoalRunes)
		work := &WorkSummary{
			Goal:        goal,
			OverallGoal: cleanText(tc.OverallGoal, maxOverallGoalRunes),
			Source:      cleanText(tc.Source, maxSourceRunes),
			UpdatedAt:   tc.UpdatedAt,
		}
		if work.Goal != "" || work.OverallGoal != "" || work.Source != "" || !work.UpdatedAt.IsZero() {
			summary.Work = work
		}
		if basename := safeHistoryBasename(tc.VaultLink); basename != "" {
			summary.History = &HistoryReference{
				Basename:   basename,
				Provenance: "hook.turn_contract.vault_link",
				UpdatedAt:  tc.UpdatedAt,
			}
		}
	}
	detail.Summary = summary
	detail.Turn = &TurnActivity{
		TurnCount:          hookState.TurnCount,
		EventsSeen:         hookState.EventsSeen,
		LastPromptSubmitAt: nonZeroTime(hookState.LastPromptSubmitAt),
		LastTurnEndAt:      nonZeroTime(hookState.LastTurnEndAt),
	}
	return summary, detail, nil
}

// Sort orders summaries by profile scope and then session ID. A duplicated ID
// in two profiles therefore remains stable and unambiguous.
func Sort(summaries []Summary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Locator.ProfileScope != summaries[j].Locator.ProfileScope {
			return summaries[i].Locator.ProfileScope < summaries[j].Locator.ProfileScope
		}
		return summaries[i].Locator.SessionID < summaries[j].Locator.SessionID
	})
}

func cleanText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' {
			return r
		}
		return -1
	}, value)
	value = secretAssignment.ReplaceAllString(value, "$1=[REDACTED]")
	value = bearerToken.ReplaceAllString(value, "Bearer [REDACTED]")
	value = commonToken.ReplaceAllString(value, "[REDACTED]")
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}

func safeHistoryBasename(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	return cleanText(base, 255)
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func nonZeroTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return cloneTime(&value)
}
