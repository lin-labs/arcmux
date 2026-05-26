package scenarios

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/e2e"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// PulseWake proves that `arcmux start` (the daemon) discovers a freshly
// scaffolded project under its configured data_root, runs the pulse
// supervisor against it, and emits audit rows for ticks + attempted
// wakes. We do NOT depend on the wake landing — the project's Elon
// pane ref is a placeholder, so the pulser emits pulse.wake.error
// instead of pulse.wake. Either row counts: both prove the loop walked
// all the way to "would send a wake".
//
// Substrate note (turn 18 risk #2): the daemon holds an exclusive bbolt
// lock on every discovered project's state.bolt for its entire uptime.
// A sibling reader would block on flock. We therefore stop the daemon
// before reading the audit log — a deliberate, documented order of
// operations until §F11 (route arcmux-call through the daemon's gRPC)
// lands. Once §F11 is in, the assertion can poll live without restart.
//
// SETUP: scaffold + state.bolt + ProjectMeta + one Elon inbox message
//
//	so the very first tick sees inbox-grew (depth>prev) and fires a
//	wake immediately rather than waiting for cadence-elapsed.
//
// ACT:   start daemon with 1s cadences; sleep ~5s; stop daemon.
// ASSERT: bbolt-read the audit log; at least one pulse.tick row for
//
//	our project AND at least one pulse.wake / pulse.wake.error row
//	for the elon target.
type PulseWake struct{}

func (PulseWake) Name() string { return "pulse-wake" }

func (PulseWake) Run(ctx context.Context, env *e2e.Env, log io.Writer) error {
	pp := paths.ForProject(env.DataRoot, env.VaultRoot, env.ProjectSlug)

	if err := scaffold.Project(pp); err != nil {
		return fmt.Errorf("scaffold: %w", err)
	}

	if err := func() error {
		db, err := store.Open(pp.StateBolt)
		if err != nil {
			return fmt.Errorf("store open: %w", err)
		}
		defer db.Close()
		if err := db.PutProjectMeta(store.ProjectMeta{
			ElonPaneRef:      e2e.FormatPaneRef(1),
			ElonSurfaceRef:   "surface:99001",
			ElonWorkspaceRef: "workspace:99001",
		}); err != nil {
			return fmt.Errorf("put project meta: %w", err)
		}
		id, err := store.NewInboxID()
		if err != nil {
			return err
		}
		return db.PushElonInbox(store.InboxMsg{
			ID:         id,
			Verb:       "add",
			From:       "user",
			Body:       "wake me",
			ReceivedAt: time.Now(),
		})
	}(); err != nil {
		return err
	}

	if err := env.StartDaemon(ctx, 10*time.Second); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Let pulse run. Discovery interval = 2s; per-pulser interval = 1s;
	// each pulser fires one tick immediately on first run. So a wake
	// should land within ~3s. 6s gives us comfortable headroom.
	soakDeadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(soakDeadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}

	// Stop daemon so we can release the bbolt lock and read the audit.
	env.StopDaemon(5 * time.Second)

	db, err := store.Open(pp.StateBolt)
	if err != nil {
		return fmt.Errorf("assert: store reopen: %w", err)
	}
	defer db.Close()

	entries, err := db.RecentAudit(500)
	if err != nil {
		return fmt.Errorf("assert: recent audit: %w", err)
	}

	// pulse audit subject format: pulse.tick is the project slug;
	// pulse.wake / pulse.wake.error is "<project>/<kind>:<target_id>" —
	// see internal/manager/pulse/pulse.go::audit().
	var wantTick, wantWake bool
	var tickCount, wakeCount, wakeErrCount int
	elonSuffix := "/elon:elon"
	for _, e := range entries {
		switch e.Action {
		case "pulse.tick":
			if e.Subject == env.ProjectSlug {
				wantTick = true
				tickCount++
			}
		case "pulse.wake":
			if strings.HasSuffix(e.Subject, elonSuffix) {
				wantWake = true
				wakeCount++
			}
		case "pulse.wake.error":
			if strings.HasSuffix(e.Subject, elonSuffix) {
				wantWake = true
				wakeErrCount++
			}
		}
	}
	if !wantTick {
		return fmt.Errorf("assert: no pulse.tick row for project=%q (audit=%d entries)",
			env.ProjectSlug, len(entries))
	}
	if !wantWake {
		return fmt.Errorf("assert: no pulse.wake[.error] row for elon (audit=%d entries, ticks=%d)",
			len(entries), tickCount)
	}

	fmt.Fprintf(log, "pulse-wake PASS: ticks=%d wakes=%d wake_errors=%d (audit=%d)\n",
		tickCount, wakeCount, wakeErrCount, len(entries))
	return nil
}
