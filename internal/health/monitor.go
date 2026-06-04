package health

import (
	"context"
	"strings"
	"time"

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
}

// NewMonitor creates a health monitor.
func NewMonitor(tmuxClient *tmux.Client, interval time.Duration, events chan<- HealthEvent) *Monitor {
	return &Monitor{
		tmux:     tmuxClient,
		interval: interval,
		events:   events,
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

	// Working -> idle: the agent's turn has ended. We infer this from the
	// pane going quiescent — visible output unchanged for the quiescence
	// window — with no working indicator on screen and no stuck pattern.
	// This is the only working->idle path for the tmux transport; without
	// it a session set to "working" on prompt delivery would stay "working"
	// forever (the hook watcher records Stop events but nothing consumed
	// them to drive state). See arcmux-u1c.
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
