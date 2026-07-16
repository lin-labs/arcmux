package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestKillKeepsSupervisionLiveUntilExactTmuxTerminationIsConfirmed(t *testing.T) {
	for _, test := range []struct {
		name       string
		kill       func(context.Context, string) error
		paneExists bool
		wantError  string
	}{
		{
			name: "kill command fails",
			kill: func(context.Context, string) error {
				return errors.New("tmux refused termination")
			},
			paneExists: true,
			wantError:  "tmux refused termination",
		},
		{
			name:       "pane remains alive",
			kill:       func(context.Context, string) error { return nil },
			paneExists: true,
			wantError:  "still alive after termination",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newCatalogTestDaemon(t, "")
			managed := session.NewSession("source-session", "source", "codex", "/repo")
			managed.SetTransport(profile.TransportTmux)
			managed.SetState(session.StateWorking)
			managed.TmuxSessionName = "arcmux-source"
			managed.TmuxTarget = "%17"
			d.sessions[managed.ID] = managed
			monitorCanceled := false
			d.monitors[managed.ID] = func() { monitorCanceled = true }
			d.killTmuxSessionHook = test.kill
			d.tmuxPaneExistsHook = func(_ context.Context, target string) bool {
				if target != managed.TmuxTarget {
					t.Fatalf("verified pane=%q, want %q", target, managed.TmuxTarget)
				}
				return test.paneExists
			}

			err := d.Kill(context.Background(), managed.ID, false, time.Second)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Kill error=%v, want %q", err, test.wantError)
			}
			if managed.Snapshot().State != session.StateWorking {
				t.Fatalf("failed kill changed session state to %s", managed.Snapshot().State)
			}
			if _, ok := d.monitors[managed.ID]; !ok || monitorCanceled {
				t.Fatalf("failed kill tore down supervision: present=%t canceled=%t", ok, monitorCanceled)
			}
		})
	}
}
