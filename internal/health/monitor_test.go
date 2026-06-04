package health

import (
	"testing"
	"time"

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
