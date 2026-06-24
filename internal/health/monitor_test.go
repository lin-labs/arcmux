package health

import (
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestIdleQuiescence(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{interval: 1 * time.Second, want: 5 * time.Second},  // floored
		{interval: 2 * time.Second, want: 5 * time.Second},  // 2*2=4 -> floored to 5
		{interval: 5 * time.Second, want: 10 * time.Second}, // 2*interval
		{interval: 30 * time.Second, want: 60 * time.Second},
	}
	for _, c := range cases {
		if got := idleQuiescence(c.interval); got != c.want {
			t.Errorf("idleQuiescence(%s) = %s, want %s", c.interval, got, c.want)
		}
	}
}

func TestHookStaleBackstop(t *testing.T) {
	// Always strictly longer than the quiescence window so a legitimately
	// thinking agent is never cut off at the normal idle threshold.
	for _, interval := range []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second} {
		if got, q := hookStaleBackstop(interval), idleQuiescence(interval); got <= q {
			t.Errorf("hookStaleBackstop(%s) = %s, want > quiescence %s", interval, got, q)
		}
	}
}

func TestHookIdleDecision(t *testing.T) {
	// Reference instant the current turn began.
	working := time.Date(2026, 6, 23, 21, 0, 0, 0, time.UTC)
	before := working.Add(-time.Minute) // a previous turn's timestamp
	after := working.Add(time.Minute)   // this turn's timestamp

	cases := []struct {
		name         string
		st           *hooks.SessionState
		workingSince time.Time
		want         idleDecision
	}{
		{
			name:         "nil hook state -> undecided",
			st:           nil,
			workingSince: working,
			want:         idleUndecided,
		},
		{
			name:         "no prompt seen yet -> undecided",
			st:           &hooks.SessionState{},
			workingSince: working,
			want:         idleUndecided,
		},
		{
			name:         "zero WorkingSince anchor -> undecided",
			st:           &hooks.SessionState{LastPromptSubmitAt: before, Working: false, LastTurnEndAt: after},
			workingSince: time.Time{},
			want:         idleUndecided,
		},
		{
			name:         "turn_end after this turn began -> confirmed idle",
			st:           &hooks.SessionState{LastPromptSubmitAt: working, Working: false, LastTurnEndAt: after},
			workingSince: working,
			want:         idleConfirmed,
		},
		{
			name:         "agent reports working -> still working",
			st:           &hooks.SessionState{LastPromptSubmitAt: after, Working: true},
			workingSince: working,
			want:         idleWorking,
		},
		{
			name: "stale turn_end from a previous turn -> NOT idle",
			// not working, but the only turn_end predates this turn's start:
			// a dropped turn_end for the current turn must not mark it idle.
			st:           &hooks.SessionState{LastPromptSubmitAt: working, Working: false, LastTurnEndAt: before},
			workingSince: working,
			want:         idleUndecided,
		},
		{
			name:         "turn_end exactly at WorkingSince -> NOT confirmed (strictly after)",
			st:           &hooks.SessionState{LastPromptSubmitAt: working, Working: false, LastTurnEndAt: working},
			workingSince: working,
			want:         idleUndecided,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hookIdleDecision(c.st, c.workingSince); got != c.want {
				t.Errorf("hookIdleDecision() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestQuiescentIdle(t *testing.T) {
	const q = 10 * time.Second
	cases := []struct {
		name           string
		state          session.State
		stuckMatch     string
		workingVisible bool
		sinceChange    time.Duration
		want           bool
	}{
		{
			name:        "working and quiescent -> idle",
			state:       session.StateWorking,
			sinceChange: 11 * time.Second,
			want:        true,
		},
		{
			name:        "working but screen still changing -> stay working",
			state:       session.StateWorking,
			sinceChange: 3 * time.Second,
			want:        false,
		},
		{
			name:           "working indicator visible -> stay working",
			state:          session.StateWorking,
			workingVisible: true,
			sinceChange:    30 * time.Second,
			want:           false,
		},
		{
			name:        "stuck pattern present -> not idle (stuck handling owns it)",
			state:       session.StateWorking,
			stuckMatch:  "tool denied",
			sinceChange: 30 * time.Second,
			want:        false,
		},
		{
			name:        "already idle -> no-op",
			state:       session.StateIdle,
			sinceChange: 30 * time.Second,
			want:        false,
		},
		{
			name:        "exactly at the window boundary -> idle",
			state:       session.StateWorking,
			sinceChange: q,
			want:        true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := quiescentIdle(c.state, c.stuckMatch, c.workingVisible, c.sinceChange, q)
			if got != c.want {
				t.Errorf("quiescentIdle(%q, %q, %v, %s) = %v, want %v",
					c.state, c.stuckMatch, c.workingVisible, c.sinceChange, got, c.want)
			}
		})
	}
}
