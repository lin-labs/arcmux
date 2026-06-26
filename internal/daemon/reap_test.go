package daemon

import (
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestReapCandidate(t *testing.T) {
	const grace = 45 * time.Second
	now := time.Now()
	old := now.Add(-2 * grace)

	base := session.Snapshot{
		ID:         "s-1",
		Transport:  profile.TransportTmux,
		State:      session.StateWorking,
		TmuxTarget: "%1",
		StartedAt:  old,
	}

	cases := []struct {
		name string
		mut  func(s *session.Snapshot)
		want bool
	}{
		{"live tmux past grace is a candidate", func(*session.Snapshot) {}, true},
		{"idle tmux past grace is a candidate", func(s *session.Snapshot) { s.State = session.StateIdle }, true},
		{"failed is a candidate (failed != dead; pane decides)", func(s *session.Snapshot) { s.State = session.StateFailed }, true},
		{"stuck is a candidate", func(s *session.Snapshot) { s.State = session.StateStuck }, true},
		{"exec transport never reaped", func(s *session.Snapshot) { s.Transport = profile.TransportExec }, false},
		{"already exited not reaped", func(s *session.Snapshot) { s.State = session.StateExited }, false},
		{"no pane target not reaped", func(s *session.Snapshot) { s.TmuxTarget = "" }, false},
		{"within startup grace not reaped", func(s *session.Snapshot) { s.StartedAt = now }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := base
			tc.mut(&snap)
			if got := reapCandidate(snap, now, grace); got != tc.want {
				t.Fatalf("reapCandidate = %v, want %v", got, tc.want)
			}
		})
	}
}
