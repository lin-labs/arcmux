package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Canonical, agent-agnostic hook events. Both the claude and codex hooks
// translate their native event types into these before calling `arcmux hook`,
// so judges never need per-agent logic.
const (
	EventPromptSubmit = "prompt_submit" // agent ingested a new user prompt
	EventToolStart    = "tool_start"    // agent began a tool call (still working)
	EventToolEnd      = "tool_end"      // a tool call finished (still in turn)
	EventTurnEnd      = "turn_end"      // agent finished its turn (now idle)
	EventNotification = "notification"  // informational, no state transition
)

// CanonicalEvents lists the accepted --event values for `arcmux hook`.
var CanonicalEvents = []string{
	EventPromptSubmit, EventToolStart, EventToolEnd, EventTurnEnd, EventNotification,
}

// SessionState is the cached per-session view the hooks judge reads. It is
// mutated only through ApplyEvent (single-writer `arcmux hook`) and seeded by
// InitSessionState (daemon on session start).
type SessionState struct {
	SessionID  string    `json:"session_id"`
	Agent      string    `json:"agent"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	LastEvent  string    `json:"last_event,omitempty"`
	LastTool   string    `json:"last_tool,omitempty"`
	Working    bool      `json:"working"`
	TurnCount  int       `json:"turn_count"`
	EventsSeen int       `json:"events_seen"`

	// LastPromptSubmitAt is the timestamp of the most recent prompt_submit
	// event. The hooks judge compares it to Evidence.DeliveryStartedAt to
	// confirm a specific delivery was ingested.
	LastPromptSubmitAt time.Time     `json:"last_prompt_submit_at,omitempty"`
	LastTurnEndAt      time.Time     `json:"last_turn_end_at,omitempty"`
	TurnContract       *TurnContract `json:"turn_contract,omitempty"`
}

// TurnContract is the compact, current per-session contract that arcmux-parent
// agents refresh every turn. It is an accurate, evolving RECORDING of the work
// (recording, not steering — nothing here changes the agent's behavior), kept
// as one consolidated snapshot rather than an append-only log.
//
// Three recorded views, all valued:
//   - Goal (latest): the agent's latest "Your ask:" restatement — the current
//     sub-task being steered, scraped from the transcript (no model call).
//   - LastUserMessage: the raw last user turn, verbatim (3-line truncated).
//   - OverallGoal: the entirety of the conversation, CONTINUOUSLY EVOLVING. It
//     is seeded from the launch prompt and then re-derived each turn by passing
//     the kept user turns + the current overall goal to a summarizer (xAI grok);
//     normally one succinct line, or — when the conversation has clearly shifted
//     gears into separate themes — a short checklist with earlier goals checked
//     off. It is NOT frozen.
type TurnContract struct {
	// Goal is the current gauged goal — the agent's latest "Your ask:". Shifts
	// each turn while still reflecting the objective.
	Goal string `json:"goal,omitempty"`
	// OverallGoal is the whole-conversation objective, continuously evolving
	// (see the type doc). Seeded from the launch prompt, then refreshed by the
	// background summarizer; may hold a multi-theme checklist.
	OverallGoal string `json:"overall_goal,omitempty"`
	// LastUserMessage is the raw, verbatim most-recent user prompt (truncated to
	// 3 lines) — recorded alongside the gauged goal, never as a substitute.
	LastUserMessage string `json:"last_user_message,omitempty"`
	// VaultLink is the best-effort path to where the conversation is saved in
	// the vault (~/agents/histories/<…>.md), resolved by cwd/host match.
	VaultLink string `json:"vault_link,omitempty"`
	// SuccessVerification and Path are optional, retained from the original
	// contract: how success is/was verified, and the consolidated approach.
	SuccessVerification string    `json:"success_verification,omitempty"`
	Path                string    `json:"path,omitempty"`
	Source              string    `json:"source,omitempty"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
}

// TurnContractUpdate carries optional replacements for the current contract.
// Empty fields mean "leave the current value unchanged" so hook callers can
// refresh one dimension without erasing the others.
type TurnContractUpdate struct {
	Goal                string
	OverallGoal         string
	LastUserMessage     string
	VaultLink           string
	SuccessVerification string
	Path                string
	Source              string
}

// SessionStatePath returns the live state file path for a session.
func SessionStatePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, sessionID+".json")
}

// ArchivedSessionStatePath returns the archived state file path for a session.
func ArchivedSessionStatePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "archived", sessionID+".json")
}

// ReadSessionState loads a session's live state file. A missing file returns
// (nil, nil) so callers can treat "no hook data yet" distinctly from an error.
func ReadSessionState(stateDir, sessionID string) (*SessionState, error) {
	if !sessionIDRe.MatchString(sessionID) {
		return nil, fmt.Errorf("invalid session id %q", sessionID)
	}
	data, err := os.ReadFile(SessionStatePath(stateDir, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read session state: %w", err)
	}
	var st SessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse session state %s: %w", sessionID, err)
	}
	return &st, nil
}

// InitSessionState creates (or refreshes the identity fields of) a session's
// live state file. Called by the daemon when it starts watching a session.
// Idempotent: an existing file's event-derived fields are preserved.
//
// launchGoal is the prompt the session was started with (`arcmux create/exec`
// prompt). It seeds OverallGoal as the initial objective — so the thing that
// launched the agent influences the record from turn zero — which the
// summarizer then continuously refines. Empty launchGoal is fine.
func InitSessionState(stateDir, sessionID, agent, launchGoal string, now time.Time) error {
	return mutateSessionState(stateDir, sessionID, func(st *SessionState) {
		st.SessionID = sessionID
		if agent != "" {
			st.Agent = agent
		}
		if st.CreatedAt.IsZero() {
			st.CreatedAt = now
		}
		if st.UpdatedAt.IsZero() {
			st.UpdatedAt = now
		}
		// Seed the overall goal from the launch prompt; the summarizer refines it.
		if launchGoal := compactContractText(launchGoal); launchGoal != "" {
			if st.TurnContract == nil {
				st.TurnContract = &TurnContract{}
			}
			if st.TurnContract.OverallGoal == "" {
				st.TurnContract.OverallGoal = launchGoal
			}
		}
	})
}

// ApplyEvent records a canonical hook event into a session's live state file,
// creating it if needed. This is the single mutation entry point used by
// `arcmux hook`. Returns an error for an unknown event so a miswired hook
// fails loudly.
func ApplyEvent(stateDir, sessionID, agent, event, tool string, now time.Time) error {
	return ApplyEventWithContract(stateDir, sessionID, agent, event, tool, TurnContractUpdate{}, now)
}

// ApplyContractOnly updates just the turn-contract recording fields without
// recording an event — no counters move, Working/TurnCount/last_event are
// untouched. It is the write path for the background overall-goal summarizer,
// which refreshes OverallGoal out-of-band after a turn has already ended.
func ApplyContractOnly(stateDir, sessionID, agent string, contract TurnContractUpdate, now time.Time) error {
	return mutateSessionState(stateDir, sessionID, func(st *SessionState) {
		st.SessionID = sessionID
		if agent != "" {
			st.Agent = agent
		}
		if st.CreatedAt.IsZero() {
			st.CreatedAt = now
		}
		applyTurnContractUpdate(st, contract, now)
	})
}

// ApplyEventWithContract records a canonical hook event and, when provided,
// replaces the current compact turn contract fields. The contract is stored as
// one snapshot so repeated turn updates consolidate instead of bloating state.
func ApplyEventWithContract(stateDir, sessionID, agent, event, tool string, contract TurnContractUpdate, now time.Time) error {
	switch event {
	case EventPromptSubmit, EventToolStart, EventToolEnd, EventTurnEnd, EventNotification:
	default:
		return fmt.Errorf("unknown hook event %q (want one of %v)", event, CanonicalEvents)
	}
	return mutateSessionState(stateDir, sessionID, func(st *SessionState) {
		st.SessionID = sessionID
		if agent != "" {
			st.Agent = agent
		}
		if st.CreatedAt.IsZero() {
			st.CreatedAt = now
		}
		st.LastEvent = event
		st.EventsSeen++
		st.UpdatedAt = now

		switch event {
		case EventPromptSubmit:
			st.Working = true
			st.TurnCount++
			st.LastPromptSubmitAt = now
		case EventToolStart:
			st.Working = true
			if tool != "" {
				st.LastTool = tool
			}
		case EventToolEnd:
			st.Working = true
			if tool != "" {
				st.LastTool = tool
			}
		case EventTurnEnd:
			st.Working = false
			st.LastTurnEndAt = now
		case EventNotification:
			// record-only
		}

		applyTurnContractUpdate(st, contract, now)
	})
}

func applyTurnContractUpdate(st *SessionState, update TurnContractUpdate, now time.Time) {
	goal := compactContractText(update.Goal)
	overall := compactContractText(update.OverallGoal)
	lastMsg := truncateLines(update.LastUserMessage, 3)
	vault := compactContractText(update.VaultLink)
	verification := compactContractText(update.SuccessVerification)
	path := compactContractText(update.Path)
	source := compactContractText(update.Source)
	if goal == "" && overall == "" && lastMsg == "" && vault == "" &&
		verification == "" && path == "" && source == "" {
		return
	}

	if st.TurnContract == nil {
		st.TurnContract = &TurnContract{}
	}
	if goal != "" {
		st.TurnContract.Goal = goal
	}
	// OverallGoal evolves continuously — always take the latest summary.
	if overall != "" {
		st.TurnContract.OverallGoal = overall
	}
	if lastMsg != "" {
		st.TurnContract.LastUserMessage = lastMsg
	}
	if vault != "" {
		st.TurnContract.VaultLink = vault
	}
	if verification != "" {
		st.TurnContract.SuccessVerification = verification
	}
	if path != "" {
		st.TurnContract.Path = path
	}
	if source != "" {
		st.TurnContract.Source = source
	}
	st.TurnContract.UpdatedAt = now
}

func compactContractText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	const maxRunes = 600
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}

// truncateLines keeps the raw shape of the last user message but caps it at the
// first n non-trailing lines, appending an ellipsis when content was dropped.
// Unlike compactContractText it preserves line breaks (the message is shown
// verbatim, just bounded). Each kept line is still rune-capped to stay sane.
func truncateLines(value string, n int) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	lines := strings.Split(value, "\n")
	kept := make([]string, 0, n)
	for _, line := range lines {
		if len(kept) >= n {
			break
		}
		runes := []rune(line)
		if len(runes) > 300 {
			line = string(runes[:300])
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	if len(lines) > n {
		out += "\n…"
	}
	return out
}

// ArchiveSessionState moves a session's live state file into the archived/
// subdirectory. Called by the daemon when it stops watching a session.
// Best-effort: a missing live file is not an error (e.g. screen-only agents).
func ArchiveSessionState(stateDir, sessionID string) error {
	if !sessionIDRe.MatchString(sessionID) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	live := SessionStatePath(stateDir, sessionID)
	// Hold the same per-session lock writers use, so a late `arcmux hook`
	// read-modify-write can't race the rename (stat/rename TOCTOU).
	unlock, err := lockSessionState(live)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := os.Stat(live); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dst := ArchivedSessionStatePath(stateDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	if err := os.Rename(live, dst); err != nil {
		return fmt.Errorf("archive session state: %w", err)
	}
	return nil
}

// mutateSessionState performs a locked read-modify-write on a session's live
// state file. The lock (a sidecar .lock file held with flock) serializes
// concurrent `arcmux hook` invocations; the write itself is atomic via
// temp-file + rename so a reader never sees a half-written document.
func mutateSessionState(stateDir, sessionID string, mutate func(*SessionState)) error {
	if !sessionIDRe.MatchString(sessionID) {
		return fmt.Errorf("invalid session id %q", sessionID)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create session state dir: %w", err)
	}

	path := SessionStatePath(stateDir, sessionID)
	unlock, err := lockSessionState(path)
	if err != nil {
		return err
	}
	defer unlock()

	st := &SessionState{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, st); err != nil {
			// A corrupt file shouldn't wedge the session forever — start fresh
			// but keep the id so the judge can still key off it.
			st = &SessionState{}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read session state: %w", err)
	}

	mutate(st)

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session state: %w", err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write session state tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename session state: %w", err)
	}
	return nil
}
