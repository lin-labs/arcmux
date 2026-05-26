// Package pulse drives the per-project wake loop. Without it, a pane
// that holds the project's primary session waits for a human to type at
// it. Pulse turns "queued inbox message" and "review cadence elapsed"
// into actual `mux.Send` calls so the pane wakes up, peeks its inbox,
// and acts.
//
// Post-C3 design:
//   - One Pulser per project. Each project has its own state.bolt; a single
//     per-project process matches the store ownership model and keeps blast
//     radius small. Cross-project orchestration is explicitly out of scope.
//   - One target per project: the pane recorded in ProjectMeta. Role-class
//     concepts (Elon / Manager / IC) were demolished in C3 — arcmux no
//     longer enumerates panes by role. Per-session pulse (one wake per
//     active session inbox) is a future slice; today the singleton
//     ProjectMeta pane is the only wake target.
//   - Triggers are OR-ed: (a) inbox depth grew since last tick OR (b) the
//     review cadence has elapsed since the last wake. Either fires ONE
//     wake send (we don't multi-wake the same target in the same tick).
//   - State is in-memory. A restart effectively resets the cadence clock —
//     the target gets its first cadence wake one cadence after the pulser
//     starts, not immediately. This avoids storm-on-restart while keeping
//     the substrate stateless.
//   - Send failures are logged but never abort the tick. cmux can be
//     flaky, a pane can be dead, a workspace can be closed — Pulse must
//     outlive all of those. Auditing the failure is the durable record;
//     the next tick will retry.
package pulse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/mux"
)

// Cadence is the per-target review interval. A wake fires for the
// project's pane when (now - lastWakeAt) >= Cadence, independent of
// inbox depth.
//
// Pre-C3 this struct held one duration per role class (Elon / Manager /
// IC) because arcmux enumerated panes by role. After demolition there
// is one wake target per project, so Cadence collapses to one field.
type Cadence struct {
	Interval time.Duration
}

// DefaultCadence returns 30s — the historical "Elon" cadence that the
// pre-C3 daemon used for the singleton front-desk target. With teams
// and ICs gone, this is the only cadence left and stays at 30s.
//
// Production wiring overrides this via [pulse.cadence] in
// ~/.config/arcmux/config.toml.
func DefaultCadence() Cadence {
	return Cadence{Interval: 30 * time.Second}
}

// Target identifies one pane the pulser may wake on a tick. After C3
// there is exactly one target per project — the pane stored in
// ProjectMeta — so Kind/ID exist only for audit-log readability.
type Target struct {
	// ID is a stable identity for the target (currently always
	// "session" to match the ProjectMeta singleton; reserved for the
	// per-session pulse slice).
	ID string
	// PaneRef is the mux target for Send (pane ref; surface refs are
	// also accepted by cmux).
	PaneRef string
	// Cadence is how often this target wakes on a flat inbox.
	Cadence time.Duration
}

// state is per-target memory the pulser maintains across ticks.
type state struct {
	LastInboxDepth int
	LastWakeAt     time.Time
}

// Pulser orchestrates ticks for one project.
type Pulser struct {
	Project string
	DB      *store.DB
	Mux     mux.Backend
	Cadence Cadence
	Now     func() time.Time
	Log     *slog.Logger

	mu  sync.Mutex
	mem map[string]*state // key: target ID
}

// New constructs a Pulser with default cadence and time sources.
func New(project string, db *store.DB, m mux.Backend) *Pulser {
	return &Pulser{
		Project: project,
		DB:      db,
		Mux:     m,
		Cadence: DefaultCadence(),
		Now:     time.Now,
		Log:     slog.Default(),
		mem:     map[string]*state{},
	}
}

// Trigger explains why a wake fired (or didn't).
type Trigger string

const (
	TriggerInboxGrew      Trigger = "inbox-grew"
	TriggerCadenceElapsed Trigger = "cadence-elapsed"
	TriggerBoth           Trigger = "inbox-grew+cadence-elapsed"
	TriggerNone           Trigger = "none"
)

// Decision records what the pulser decided for one target in one tick.
type Decision struct {
	Target       Target
	InboxDepth   int
	PrevDepth    int
	SinceLast    time.Duration
	Trigger      Trigger
	WakeSent     bool
	WakeError    string
	WakePromptOK string // non-empty when WakeSent — the body that was sent (truncated for the report)
}

// Report aggregates one tick.
type Report struct {
	At        time.Time
	Targets   int
	Wakes     int
	Errors    int
	Decisions []Decision
}

// Run ticks every interval until ctx cancels. Returns the ctx's error so
// callers can distinguish clean shutdown (context.Canceled) from a
// deeper failure.
func (p *Pulser) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("pulse interval must be > 0, got %v", interval)
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Fire one immediate tick so wakes can land before the first interval
	// elapses (otherwise a 30s interval delays the first wake by 30s on
	// every restart).
	if _, err := p.Tick(ctx); err != nil {
		p.Log.Error("pulse tick failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := p.Tick(ctx); err != nil {
				p.Log.Error("pulse tick failed", "err", err)
			}
		}
	}
}

// Tick runs one pass: enumerate targets, evaluate triggers, send wakes,
// audit. Returns the report whether or not a wake was sent so tests and
// observability can assert on each decision.
func (p *Pulser) Tick(ctx context.Context) (Report, error) {
	now := p.Now()
	targets, err := p.collectTargets()
	if err != nil {
		return Report{}, fmt.Errorf("collect targets: %w", err)
	}

	rep := Report{At: now, Targets: len(targets)}
	for _, tg := range targets {
		d := p.evaluate(ctx, tg, now)
		if d.WakeSent {
			rep.Wakes++
		}
		if d.WakeError != "" {
			rep.Errors++
		}
		rep.Decisions = append(rep.Decisions, d)
	}

	// One audit row per tick captures the aggregate; a row per wake is
	// the fine-grained record. Keeping both lets a future operator
	// answer "did pulse run at all" without scanning every wake.
	_ = p.DB.AppendAudit(store.AuditEntry{
		Timestamp: now,
		Action:    "pulse.tick",
		Actor:     "pulse",
		Subject:   p.Project,
		Detail: map[string]any{
			"targets": rep.Targets,
			"wakes":   rep.Wakes,
			"errors":  rep.Errors,
		},
	})
	return rep, nil
}

// collectTargets enumerates wake-eligible panes from the store. Post-C3
// there is exactly one target per project — the pane recorded in
// ProjectMeta. Absent ProjectMeta means the project was scaffolded but
// never registered; we return an empty slice (no panic, no error) so a
// partially-bootstrapped project ticks cleanly.
func (p *Pulser) collectTargets() ([]Target, error) {
	meta, err := p.DB.GetProjectMeta()
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get project meta: %w", err)
	}
	if meta.PaneRef == "" {
		return nil, nil
	}
	return []Target{{
		ID:      "session",
		PaneRef: meta.PaneRef,
		Cadence: p.Cadence.Interval,
	}}, nil
}

// evaluate computes the trigger for one target and sends a wake if
// needed.
func (p *Pulser) evaluate(ctx context.Context, tg Target, now time.Time) Decision {
	depth, depthErr := p.inboxDepth(tg)

	p.mu.Lock()
	st, seen := p.mem[tg.ID]
	if !seen {
		// First sight: anchor the cadence at "now". Don't fire a wake
		// just because we've never seen this target — it would
		// storm-wake on pulser restart. The first cadence-trigger
		// fires one `cadence` later. An inbox-grew trigger still fires
		// immediately if the bucket already has content (prev=0 vs
		// current>0).
		st = &state{LastInboxDepth: 0, LastWakeAt: now}
		p.mem[tg.ID] = st
	}
	prev := st.LastInboxDepth
	last := st.LastWakeAt
	p.mu.Unlock()

	d := Decision{
		Target:     tg,
		InboxDepth: depth,
		PrevDepth:  prev,
		SinceLast:  now.Sub(last),
		Trigger:    TriggerNone,
	}
	if depthErr != nil {
		// Inbox bucket missing for the target is not fatal (the
		// session may not have been pushed to yet). Treat as depth 0
		// and continue — cadence can still fire.
		depth = 0
		d.InboxDepth = 0
		depthErr = nil
	}

	grew := depth > prev
	elapsed := tg.Cadence > 0 && now.Sub(last) >= tg.Cadence
	switch {
	case grew && elapsed:
		d.Trigger = TriggerBoth
	case grew:
		d.Trigger = TriggerInboxGrew
	case elapsed && seen:
		// Only fire cadence-elapsed for targets we've seen on a prior
		// tick. On first sight we anchor at now (see above), so
		// SinceLast is 0.
		d.Trigger = TriggerCadenceElapsed
	}

	if d.Trigger == TriggerNone {
		// Still record the depth so next tick can detect growth.
		p.mu.Lock()
		st.LastInboxDepth = depth
		p.mu.Unlock()
		return d
	}

	body := buildWakeBody(tg, depth, now.Sub(last))
	d.WakePromptOK = body

	if err := p.Mux.Send(ctx, tg.PaneRef, body); err != nil {
		d.WakeError = err.Error()
		// Leave LastInboxDepth and LastWakeAt unchanged — the wake
		// didn't land, so the next tick should re-evaluate against
		// the same baseline and retry. This means a permanent send
		// failure will re-fire forever; that's intentional (visible
		// in the audit log instead of silently swallowed). A
		// dissolved pane will drop out of collectTargets and stop
		// being retried.
		p.audit(now, tg, "pulse.wake.error", map[string]any{
			"trigger":     string(d.Trigger),
			"inbox_depth": depth,
			"prev_depth":  prev,
			"error":       err.Error(),
		})
		return d
	}

	d.WakeSent = true
	p.mu.Lock()
	st.LastInboxDepth = depth
	st.LastWakeAt = now
	p.mu.Unlock()

	p.audit(now, tg, "pulse.wake", map[string]any{
		"trigger":         string(d.Trigger),
		"inbox_depth":     depth,
		"prev_depth":      prev,
		"since_last_wake": now.Sub(last).String(),
	})
	return d
}

// inboxDepth returns the queued count for a target. Post-C3 the inbox
// surface is keyed by session name; the singleton target uses the
// project slug as its session name (matching the historical "elon"
// inbox semantics, just without the role-coded bucket).
//
// A missing bucket is not an error — it means nothing has been pushed
// yet. Callers treat that as depth 0.
func (p *Pulser) inboxDepth(tg Target) (int, error) {
	n, err := p.DB.DepthSessionInbox(p.Project)
	if err != nil {
		if errors.Is(err, store.ErrSessionInboxMissing) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

func (p *Pulser) audit(at time.Time, tg Target, action string, detail map[string]any) {
	detail["target_id"] = tg.ID
	detail["pane_ref"] = tg.PaneRef
	_ = p.DB.AppendAudit(store.AuditEntry{
		Timestamp: at,
		Action:    action,
		Actor:     "pulse",
		Subject:   p.Project + "/" + tg.ID,
		Detail:    detail,
	})
}

// buildWakeBody returns the prompt the pulser sends into the pane. Kept
// short on purpose — the pane's own bootstrap protocol describes what
// to do on wake; the wake just signals "you have work, run it."
func buildWakeBody(tg Target, depth int, since time.Duration) string {
	return fmt.Sprintf(
		"[arcmux pulse] %s — you have %d queued inbox message(s); "+
			"%s since last wake. Run your bootstrap protocol: peek your "+
			"inbox, act on what's there, then journal + scratchpad before "+
			"yielding.",
		tg.ID, depth, since.Round(time.Second),
	)
}
