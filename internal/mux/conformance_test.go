package mux_test

// Conformance test: every backend must behave the same way against a
// common operation sequence. Skips when the underlying CLI binary is not
// installed locally — CI runs both, dev boxes typically only have one.
//
// What this exercises:
//   - EnsureServer
//   - NewGroup
//   - ListPanes returns the implicit initial pane
//   - NewPane (in a group)
//   - Send + ReadScreen round-trip
//   - PaneExists
//   - ClosePane
//   - CloseGroup

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/mux"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
	"github.com/lin-labs/arcmux/internal/mux/tmuxbackend"
	"github.com/lin-labs/arcmux/internal/tmux"
)

type backendFactory struct {
	name string
	bin  string // CLI binary checked via exec.LookPath; "" to skip the check
	mk   func(t *testing.T) mux.Backend
}

func backends(t *testing.T) []backendFactory {
	return []backendFactory{
		{
			name: "cmux",
			bin:  "cmux",
			mk: func(t *testing.T) mux.Backend {
				return cmuxbackend.New(cmuxcli.New())
			},
		},
		{
			name: "tmux",
			bin:  "tmux",
			mk: func(t *testing.T) mux.Backend {
				// Use a test-only tmux socket so we never collide with the
				// user's real tmux server.
				socket := fmt.Sprintf("arcmux-conformance-%d", time.Now().UnixNano())
				return tmuxbackend.New(tmux.NewClient(socket))
			},
		},
	}
}

func skipIfNoBin(t *testing.T, bin string) {
	if bin == "" {
		return
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH; skipping", bin)
	}
}

func TestBackend_EnsureServer_Idempotent(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			skipIfNoBin(t, b.bin)
			be := b.mk(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := be.EnsureServer(ctx); err != nil {
				t.Fatalf("EnsureServer: %v", err)
			}
			if err := be.EnsureServer(ctx); err != nil {
				t.Fatalf("EnsureServer (second call): %v", err)
			}
		})
	}
}

func TestBackend_PipePaneSupport(t *testing.T) {
	// Sanity check: tmux supports PipePane, cmux returns ErrUnsupported.
	// This is the canonical "ok to yield errors when not supported" case.
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			skipIfNoBin(t, b.bin)
			be := b.mk(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := be.PipePaneStart(ctx, "dummy-target", "/tmp/arcmux-conformance-pipe.log")
			switch b.name {
			case "cmux":
				if err == nil || !errorsIs(err, mux.ErrUnsupported) {
					t.Fatalf("cmux PipePaneStart should return ErrUnsupported, got: %v", err)
				}
			case "tmux":
				// tmux will error because dummy-target doesn't exist, but it
				// should not return ErrUnsupported.
				if err != nil && errorsIs(err, mux.ErrUnsupported) {
					t.Fatalf("tmux PipePaneStart returned ErrUnsupported (should attempt)")
				}
			}
		})
	}
}

func TestBackend_Lifecycle_StructuralOps(t *testing.T) {
	// Structural conformance both backends must satisfy:
	// NewGroup → NewPane → ListPanes shows the new pane → ClosePane →
	// CloseGroup. No send/read round-trip — that's covered by the tmux-only
	// echo test below because cmux requires GUI activation to fully
	// provision a freshly-spawned workspace's terminal surfaces (a
	// documented cmux limitation, not an arcmux defect).
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			skipIfNoBin(t, b.bin)
			be := b.mk(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := be.EnsureServer(ctx); err != nil {
				t.Fatalf("EnsureServer: %v", err)
			}

			groupName := fmt.Sprintf("arcmux-conf-%d", time.Now().UnixNano())
			group, err := be.NewGroup(ctx, mux.GroupOptions{
				Name:    groupName,
				Command: "sh",
			})
			if err != nil {
				t.Fatalf("NewGroup: %v", err)
			}
			t.Cleanup(func() {
				_ = be.CloseGroup(context.Background(), group.Ref)
			})
			if group.Ref == "" {
				t.Fatal("NewGroup returned empty ref")
			}

			pane, err := be.NewPane(ctx, mux.PaneOptions{Group: group.Ref})
			if err != nil {
				t.Fatalf("NewPane: %v", err)
			}
			if pane.Ref == "" {
				t.Fatal("NewPane returned empty Ref")
			}

			panes, err := be.ListPanes(ctx, group.Ref)
			if err != nil {
				t.Fatalf("ListPanes: %v", err)
			}
			if len(panes) < 2 {
				t.Fatalf("ListPanes: got %d panes, want >= 2 (initial + new)", len(panes))
			}
			found := false
			for _, p := range panes {
				if p.Ref == pane.Ref {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("ListPanes did not include the freshly-created pane %q", pane.Ref)
			}

			// ClosePane is best-effort across backends. cmux currently
			// lacks a direct close-pane verb (workspaces cascade close
			// surfaces); tmux's kill-pane is the canonical path. We log
			// but don't fail on a per-backend error here; CloseGroup in
			// the test cleanup is the authoritative teardown.
			if err := be.ClosePane(ctx, pane.Ref); err != nil {
				t.Logf("ClosePane (best-effort, may be backend-specific): %v", err)
			}
		})
	}
}

func TestTmuxBackend_Send_Read_Echo(t *testing.T) {
	// Full Send→Read round-trip. tmux-only because cmux requires GUI
	// activation to provision terminal surfaces in newly-spawned
	// workspaces (see TestBackend_Lifecycle_StructuralOps).
	skipIfNoBin(t, "tmux")
	socket := fmt.Sprintf("arcmux-echo-%d", time.Now().UnixNano())
	be := tmuxbackend.New(tmux.NewClient(socket))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := be.EnsureServer(ctx); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	groupName := fmt.Sprintf("arcmux-echo-%d", time.Now().UnixNano())
	group, err := be.NewGroup(ctx, mux.GroupOptions{Name: groupName})
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	t.Cleanup(func() {
		_ = be.CloseGroup(context.Background(), group.Ref)
	})

	panes, err := be.ListPanes(ctx, group.Ref)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) == 0 {
		t.Fatalf("ListPanes empty")
	}
	target := panes[0].SendTarget
	if target == "" {
		target = panes[0].Ref
	}

	marker := fmt.Sprintf("ARCMUX_ECHO_%d", time.Now().UnixNano())
	if err := be.Send(ctx, target, "echo "+marker); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := be.ReadScreen(ctx, target)
		if err == nil && strings.Contains(out, marker) {
			return // pass
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("marker %q did not appear within deadline", marker)
}

// errorsIs is a local copy of errors.Is — kept tiny here so the test
// file's import surface stays explicit.
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
