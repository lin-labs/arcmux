package health

import (
	"context"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/tmux"
)

// Event types emitted by the health monitor.
const (
	EventStuckDetected  = "stuck_detected"
	EventNudgeSent      = "nudge_sent"
	EventNudgeExhausted = "nudge_exhausted"
	EventAgentExited    = "agent_exited"
	EventCrashDetected  = "crash_detected"
	EventIdleDetected   = "idle_detected"
)

// idleQuiescence returns how long a working session's visible output must
// stay unchanged (with no working indicator on screen) before the monitor
// transitions it back to idle. Derived from the capture interval so faster
// polling yields faster idle detection, floored so a tiny interval can't
// make us declare idle on a single quiet tick.
func idleQuiescence(interval time.Duration) time.Duration {
	q := 2 * interval
	if q < 5*time.Second {
		q = 5 * time.Second
	}
	return q
}

// quiescentIdle decides whether a working session has finished its turn and
// should transition back to idle. The agent is considered done when it is
// currently working, shows no stuck pattern, has no working indicator on
// screen, and its visible output has been unchanged for at least the
// quiescence window.
func quiescentIdle(state session.State, stuckMatch string, workingVisible bool, sinceChange, quiescence time.Duration) bool {
	if state != session.StateWorking || stuckMatch != "" || workingVisible {
		return false
	}
	return sinceChange >= quiescence
}

// idleDecision is the authoritative-vs-inferred verdict the hook signal can
// give about a working session's turn boundary.
type idleDecision int

const (
	// idleUndecided: no trustworthy hook signal for this turn — the monitor
	// must fall back to screen-quiescence inference (legacy behavior, and the
	// only path for non-hook-backed agents).
	idleUndecided idleDecision = iota
	// idleConfirmed: the agent itself reported turn_end after the current
	// turn began. Ground truth — idle immediately, no quiescence wait.
	idleConfirmed
	// idleWorking: the agent itself reports it is still working on the current
	// turn. Suppress quiescence-based idle so a long silent think/tool call is
	// not mistaken for "done".
	idleWorking
)

// hookIdleDecision interprets a session's cached hook state relative to the
// per-turn WorkingSince reference. It is intentionally pure (no I/O) so the
// turn-boundary logic is unit-testable without a daemon, hook files, or tmux.
//
// The contract:
//   - st nil or no prompt yet           -> idleUndecided (use quiescence)
//   - workingSince unknown              -> idleUndecided (no per-turn anchor)
//   - turn_end landed after WorkingSince -> idleConfirmed (this turn finished)
//   - agent reports working             -> idleWorking (suppress false idle)
//   - otherwise                         -> idleUndecided
//
// Anchoring on WorkingSince is what makes a stale turn_end (from a previous
// turn) or a dropped hook safe: neither advances LastTurnEndAt past the
// current turn's start, so neither yields idleConfirmed.
func hookIdleDecision(st *hooks.SessionState, workingSince time.Time) idleDecision {
	if st == nil || st.LastPromptSubmitAt.IsZero() || workingSince.IsZero() {
		return idleUndecided
	}
	if !st.Working && st.LastTurnEndAt.After(workingSince) {
		return idleConfirmed
	}
	if st.Working {
		return idleWorking
	}
	return idleUndecided
}

// hookStaleBackstop is how long a hook-"working" session may sit with a
// completely static screen before the monitor declares it idle anyway, as a
// safety net against a dropped turn_end hook. Deliberately several times the
// quiescence window so a legitimately thinking agent is never cut off early.
func hookStaleBackstop(interval time.Duration) time.Duration {
	return 3 * idleQuiescence(interval)
}

// HealthEvent is emitted when the monitor detects a state change.
type HealthEvent struct {
	SessionID string
	Type      string
	Reason    string
	Output    string // snippet of relevant output
	Timestamp time.Time
}

// Monitor periodically checks session health.
type Monitor struct {
	tmux     *tmux.Client
	interval time.Duration
	events   chan<- HealthEvent
	// stateDir is the per-session hook state directory. When set, the monitor
	// prefers the agent's own turn_end hook over screen-quiescence inference
	// to decide idle. Empty disables hook-driven idle (quiescence only).
	stateDir string
}

// NewMonitor creates a health monitor. stateDir is the hook session-state
// directory (config Hooks.SessionStateDir); pass "" to rely on screen
// quiescence alone (e.g. agents with no hook integration).
func NewMonitor(tmuxClient *tmux.Client, interval time.Duration, events chan<- HealthEvent, stateDir string) *Monitor {
	return &Monitor{
		tmux:     tmuxClient,
		interval: interval,
		events:   events,
		stateDir: stateDir,
	}
}

// CheckResult holds the outcome of a single health check.
type CheckResult struct {
	Alive       bool
	Output      string
	StuckMatch  string // which stuck pattern matched, if any
	IsIdle      bool
	IdleTooLong bool
}

// Check performs a single health check on a session.
func (m *Monitor) Check(ctx context.Context, sess *session.Session, prof profile.Profile) CheckResult {
	snap := sess.Snapshot()
	result := CheckResult{}

	// Check if pane still exists
	if !m.tmux.PaneExists(ctx, snap.TmuxTarget) {
		result.Alive = false
		return result
	}
	result.Alive = true

	// Capture output
	output, err := m.tmux.CapturePaneVisible(ctx, snap.TmuxTarget)
	if err != nil {
		return result
	}
	result.Output = output

	// Check stuck patterns
	lower := strings.ToLower(output)
	for _, pattern := range prof.StuckTextPatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			result.StuckMatch = pattern
			break
		}
	}

	// Check idle timeout
	if snap.IdleSince != nil {
		idleDuration := time.Since(*snap.IdleSince)
		result.IsIdle = true
		if prof.IdleTimeout > 0 && idleDuration > prof.IdleTimeout {
			result.IdleTooLong = true
		}
	}

	return result
}

// hookDecision consults the agent's own hook state for an authoritative turn
// boundary. Returns idleUndecided for non-hook agents, when the monitor has no
// state dir, when there is no per-turn anchor yet, or when the state file can't
// be read — callers then fall back to screen quiescence.
func (m *Monitor) hookDecision(snap session.Snapshot, prof profile.Profile) idleDecision {
	if m.stateDir == "" || !prof.HookBacked() || snap.WorkingSince == nil {
		return idleUndecided
	}
	st, err := hooks.ReadSessionState(m.stateDir, snap.ID)
	if err != nil {
		return idleUndecided
	}
	return hookIdleDecision(st, *snap.WorkingSince)
}

// Nudge sends the configured nudge command to a stuck session.
func (m *Monitor) Nudge(ctx context.Context, sess *session.Session, prof profile.Profile) error {
	snap := sess.Snapshot()
	return m.tmux.SendKeys(ctx, snap.TmuxTarget, prof.NudgeCommand, "Enter")
}

// runState carries the per-session bookkeeping the tick loop needs across
// ticks (kept here rather than on Monitor so one Monitor can drive many
// sessions without cross-talk).
type runState struct {
	lastOutput   string
	lastChangeAt time.Time
}

// Run starts the monitor loop for a single session.
func (m *Monitor) Run(ctx context.Context, sess *session.Session, prof profile.Profile) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	rs := &runState{lastChangeAt: time.Now()}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx, sess, prof, rs)
		}
	}
}

func (m *Monitor) tick(ctx context.Context, sess *session.Session, prof profile.Profile, rs *runState) {
	result := m.Check(ctx, sess, prof)
	now := time.Now()
	snap := sess.Snapshot()

	// Track visible-output quiescence: reset the timer whenever the screen
	// changes. A working session whose screen has stopped changing (and is
	// not showing a working indicator) has finished its turn.
	if rs.lastOutput != result.Output {
		rs.lastOutput = result.Output
		rs.lastChangeAt = now
	}

	if !result.Alive {
		sess.SetState(session.StateExited)
		m.emit(HealthEvent{
			SessionID: snap.ID,
			Type:      EventAgentExited,
			Reason:    "pane no longer exists",
			Timestamp: now,
		})
		return
	}

	// Stuck pattern detected
	if result.StuckMatch != "" && snap.State == session.StateWorking {
		sess.SetState(session.StateStuck)
		if snap.NudgeCount < prof.MaxNudgeRetries {
			if err := m.Nudge(ctx, sess, prof); err == nil {
				count := sess.IncrementNudge()
				m.emit(HealthEvent{
					SessionID: snap.ID,
					Type:      EventNudgeSent,
					Reason:    result.StuckMatch,
					Output:    truncate(result.Output, 200),
					Timestamp: now,
				})
				_ = count
			}
		} else {
			sess.SetState(session.StateEscalated)
			m.emit(HealthEvent{
				SessionID: snap.ID,
				Type:      EventNudgeExhausted,
				Reason:    result.StuckMatch,
				Output:    truncate(result.Output, 200),
				Timestamp: now,
			})
		}
		return
	}

	// Idle too long
	if result.IdleTooLong && snap.State == session.StateWorking {
		sess.SetState(session.StateStuck)
		m.emit(HealthEvent{
			SessionID: snap.ID,
			Type:      EventStuckDetected,
			Reason:    "idle timeout exceeded",
			Timestamp: now,
		})
		return
	}

	// Working -> idle. Prefer the agent's own turn_end hook (ground truth)
	// over screen-quiescence inference; the hook makes idle both faster (no
	// quiescence wait) and authoritative. Quiescence remains the path for
	// non-hook agents and the backstop if a turn_end hook is dropped. See
	// arcmux-u1c (quiescence) and arcmux-hc3 (hook-authoritative idle).
	if snap.State == session.StateWorking {
		switch m.hookDecision(snap, prof) {
		case idleConfirmed:
			// The agent reported turn_end for this turn. Trust it now,
			// regardless of what the screen is still rendering.
			sess.SetState(session.StateIdle)
			sess.ResetNudge()
			m.emit(HealthEvent{
				SessionID: snap.ID,
				Type:      EventIdleDetected,
				Reason:    "hook:turn_end",
				Timestamp: now,
			})
			return
		case idleWorking:
			// The agent reports it is still working. Do not let screen
			// quiescence declare a false idle on a long silent think/tool
			// call. Backstop: if the screen has also been completely static
			// for well beyond the quiescence window, a turn_end hook was
			// likely dropped — declare idle so the session can't wedge.
			if now.Sub(rs.lastChangeAt) >= hookStaleBackstop(m.interval) {
				sess.SetState(session.StateIdle)
				sess.ResetNudge()
				m.emit(HealthEvent{
					SessionID: snap.ID,
					Type:      EventIdleDetected,
					Reason:    "output quiescent (no turn_end hook)",
					Timestamp: now,
				})
			}
			return
		case idleUndecided:
			// No trustworthy hook signal — fall through to quiescence.
		}
	}

	// Screen-quiescence inference: a working pane whose visible output has
	// been unchanged for the quiescence window — with no working indicator on
	// screen and no stuck pattern — has finished its turn.
	workingVisible := prof.WorkingIndicator != "" &&
		strings.Contains(strings.ToLower(result.Output), strings.ToLower(prof.WorkingIndicator))
	if quiescentIdle(snap.State, result.StuckMatch, workingVisible, now.Sub(rs.lastChangeAt), idleQuiescence(m.interval)) {
		sess.SetState(session.StateIdle)
		sess.ResetNudge()
		m.emit(HealthEvent{
			SessionID: snap.ID,
			Type:      EventIdleDetected,
			Reason:    "output quiescent",
			Timestamp: now,
		})
		return
	}

	// Agent is producing output — mark as working
	if snap.State == session.StateIdle && !result.IsIdle {
		sess.SetState(session.StateWorking)
		sess.ResetNudge()
	}
}

func (m *Monitor) emit(event HealthEvent) {
	select {
	case m.events <- event:
	default:
		// drop if channel is full — caller should drain
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
