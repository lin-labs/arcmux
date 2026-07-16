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
			d.tmuxExactPaneExistsHook = func(_ context.Context, target string) (bool, error) {
				if target != managed.TmuxTarget {
					t.Fatalf("verified pane=%q, want %q", target, managed.TmuxTarget)
				}
				return test.paneExists, nil
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

func TestKillCompletesWhenTerminationCommandFailsAfterExactPaneVanished(t *testing.T) {
	d := newCatalogTestDaemon(t, "")
	managed := session.NewSession("source-session", "source", "codex", "/repo")
	managed.SetTransport(profile.TransportTmux)
	managed.SetState(session.StateWorking)
	managed.TmuxSessionName = "arcmux-source"
	managed.TmuxTarget = "%17"
	d.sessions[managed.ID] = managed
	monitorCanceled := false
	d.monitors[managed.ID] = func() { monitorCanceled = true }
	d.killTmuxSessionHook = func(context.Context, string) error {
		return errors.New("can't find session: arcmux-source")
	}
	d.tmuxExactPaneExistsHook = func(_ context.Context, target string) (bool, error) {
		if target != managed.TmuxTarget {
			t.Fatalf("verified pane=%q, want %q", target, managed.TmuxTarget)
		}
		return false, nil
	}

	if err := d.Kill(context.Background(), managed.ID, false, time.Second); err != nil {
		t.Fatalf("Kill returned error after exact pane vanished: %v", err)
	}
	if managed.Snapshot().State != session.StateExited {
		t.Fatalf("successful kill left session state %s", managed.Snapshot().State)
	}
	if _, ok := d.monitors[managed.ID]; ok || !monitorCanceled {
		t.Fatalf("successful kill kept supervision: present=%t canceled=%t", ok, monitorCanceled)
	}
}

func TestKillKeepsSupervisionWhenExactPaneProbeFails(t *testing.T) {
	d := newCatalogTestDaemon(t, "")
	managed := session.NewSession("source-session", "source", "codex", "/repo")
	managed.SetTransport(profile.TransportTmux)
	managed.SetState(session.StateWorking)
	managed.TmuxSessionName = "arcmux-source"
	managed.TmuxTarget = "%17"
	d.sessions[managed.ID] = managed
	monitorCanceled := false
	d.monitors[managed.ID] = func() { monitorCanceled = true }
	d.killTmuxSessionHook = func(context.Context, string) error {
		return errors.New("can't find session: arcmux-source")
	}
	d.tmuxExactPaneExistsHook = func(context.Context, string) (bool, error) {
		return false, errors.New("tmux liveness query unavailable")
	}

	err := d.Kill(context.Background(), managed.ID, false, time.Second)
	if err == nil || !strings.Contains(err.Error(), "tmux liveness query unavailable") {
		t.Fatalf("Kill error=%v, want liveness query failure", err)
	}
	if managed.Snapshot().State != session.StateWorking {
		t.Fatalf("probe failure changed session state to %s", managed.Snapshot().State)
	}
	if _, ok := d.monitors[managed.ID]; !ok || monitorCanceled {
		t.Fatalf("probe failure tore down supervision: present=%t canceled=%t", ok, monitorCanceled)
	}
}
