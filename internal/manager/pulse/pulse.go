// Package pulse drives the per-project wake loop. Without it, every agent
// in the system (Elon, every Manager, every IC) waits for Boyan to type at
// it. Pulse turns "queued inbox message" and "review cadence elapsed" into
// actual `cmux send` calls so an actor wakes up, peeks its inbox, and acts.
//
// Design choices:
//   - One Pulser per project. Each project has its own state.bolt; a single
//     per-project process matches the store ownership model and keeps blast
//     radius small. Cross-project orchestration is explicitly out of scope
//     (forward-plan.md anti-roadmap).
//   - Triggers are OR-ed: (a) inbox depth grew since last tick OR (b) the
//     per-role review cadence has elapsed since the last wake. Either fires
//     ONE wake send (we don't multi-wake the same target in the same tick).
//   - State is in-memory. A restart effectively resets the cadence clock —
//     each target gets its first wake one cadence after the pulser starts,
//     not immediately. This avoids storm-on-restart while keeping the
//     substrate stateless (no on-disk "last_pulse_at" to keep in sync with
//     the bolt store).
//   - Send failures are logged but never abort the tick. cmux can be flaky,
//     a pane can be dead, a workspace can be closed — Pulse must outlive
//     all of those. Auditing the failure is the durable record; the next
//     tick will retry.
//   - The wake target is a cmux pane ref. icspawn.go already calls
//     `Cmux.Send(ctx, slot.PaneRef, …)`, so the same target works here.
package pulse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// Cadence holds per-role review intervals. A wake fires for a target when
// `now - lastWakeAt >= cadence`, independent of inbox depth.
type Cadence struct {
	Elon    time.Duration
	Manager time.Duration
	IC      time.Duration
}

// DefaultCadence matches the lab-service rhythm Boyan set as canonical when
// pulse moved into the arcmux daemon: Elon 30s, Manager 10s, IC 5s. These
// are the "how often does an idle actor self-review" intervals; the
// inbox-depth trigger is independent and fires regardless of cadence.
//
// Production wiring overrides these via [pulse.cadence] in
// ~/.config/arcmux/config.toml, so these defaults only apply when (a) the
// pulse package is used directly (e.g. tests, the `arcmux pulse` debug
// shim with no overrides) or (b) the user has not set the config file.
func DefaultCadence() Cadence {
	return Cadence{
		Elon:    30 * time.Second,
		Manager: 10 * time.Second,
		IC:      5 * time.Second,
	}
}

// Kind identifies the actor class behind a wake target.
type Kind string

const (
	KindElon    Kind = "elon"
	KindManager Kind = "manager"
	KindIC      Kind = "ic"
)

// Target is one pane the pulser may wake on a given tick.
type Target struct {
	Kind    Kind
	ID      string // "elon" | team slug | slot id; stable identity across ticks
	PaneRef string // cmux target for Send (pane ref; surface refs also accepted by cmux)
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
	Cmux    *cmuxcli.Client
	Cadence Cadence
	Now     func() time.Time
	Log     *slog.Logger

	mu  sync.Mutex
	mem map[string]*state // key: kind+":"+id
}

// New constructs a Pulser with default cadence and time sources.
func New(project string, db *store.DB, c *cmuxcli.Client) *Pulser {
	return &Pulser{
		Project: project,
		DB:      db,
		Cmux:    c,
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
// callers can distinguish clean shutdown (context.Canceled) from a deeper
// failure.
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

	// One audit row per tick captures the aggregate; a row per wake is the
	// fine-grained record. Keeping both lets a future operator answer "did
	// pulse run at all" without scanning every wake.
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

// collectTargets enumerates all wake-eligible panes from the store. Elon is
// a singleton (project meta); managers come from active teams; ICs come
// from active slots.
func (p *Pulser) collectTargets() ([]Target, error) {
	var out []Target

	// Elon (singleton). Absent ProjectMeta means manager-mode never ran for
	// this project; that's a configuration error, not a tick-time error —
	// log and continue with the team/IC scan so a partially-bootstrapped
	// project still pulses what it has.
	if meta, err := p.DB.GetProjectMeta(); err == nil {
		if meta.ElonPaneRef != "" {
			out = append(out, Target{
				Kind:    KindElon,
				ID:      "elon",
				PaneRef: meta.ElonPaneRef,
				Cadence: p.Cadence.Elon,
			})
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("get project meta: %w", err)
	}

	// Managers (one per active team).
	teams, err := p.DB.ListTeams(store.TeamActive)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	for _, t := range teams {
		if t.ManagerPane == "" {
			continue
		}
		out = append(out, Target{
			Kind:    KindManager,
			ID:      t.ID,
			PaneRef: t.ManagerPane,
			Cadence: p.Cadence.Manager,
		})
	}

	// ICs (one per active slot across all teams).
	slots, err := p.DB.ListSlots("", store.SlotActive)
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	for _, s := range slots {
		if s.PaneRef == "" {
			continue
		}
		out = append(out, Target{
			Kind:    KindIC,
			ID:      s.ID,
			PaneRef: s.PaneRef,
			Cadence: p.Cadence.IC,
		})
	}

	return out, nil
}

// evaluate computes the trigger for one target and sends a wake if needed.
func (p *Pulser) evaluate(ctx context.Context, tg Target, now time.Time) Decision {
	depth, depthErr := p.inboxDepth(tg)

	p.mu.Lock()
	st, seen := p.mem[stateKey(tg)]
	if !seen {
		// First sight: anchor the cadence at "now". Don't fire a wake just
		// because we've never seen this target — it would storm-wake every
		// pane on pulser restart. The first cadence-trigger fires one
		// `cadence` later. An inbox-grew trigger still fires immediately
		// if the bucket already has content (prev=0 vs current>0).
		st = &state{LastInboxDepth: 0, LastWakeAt: now}
		p.mem[stateKey(tg)] = st
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
		// Inbox bucket missing for an active manager/IC is a substrate bug;
		// for Elon it means the schema bucket is missing. Record and skip —
		// don't crash the whole tick.
		d.WakeError = depthErr.Error()
		return d
	}

	grew := depth > prev
	elapsed := tg.Cadence > 0 && now.Sub(last) >= tg.Cadence
	switch {
	case grew && elapsed:
		d.Trigger = TriggerBoth
	case grew:
		d.Trigger = TriggerInboxGrew
	case elapsed && seen:
		// Only fire cadence-elapsed for targets we've seen on a prior tick.
		// On first sight we anchor at now (see above), so SinceLast is 0.
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

	if err := p.Cmux.Send(ctx, tg.PaneRef, body); err != nil {
		d.WakeError = err.Error()
		// Leave LastInboxDepth and LastWakeAt unchanged — the wake didn't
		// land, so the next tick should re-evaluate against the same
		// baseline and retry. This means a permanent send failure will
		// re-fire forever; that's intentional (visible in the audit log
		// instead of silently swallowed). A dissolved pane will drop out
		// of collectTargets and stop being retried.
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

func (p *Pulser) inboxDepth(tg Target) (int, error) {
	switch tg.Kind {
	case KindElon:
		return p.DB.DepthElonInbox()
	case KindManager:
		return p.DB.DepthManagerInbox(tg.ID)
	case KindIC:
		return p.DB.DepthICInbox(tg.ID)
	default:
		return 0, fmt.Errorf("unknown kind %q", tg.Kind)
	}
}

func (p *Pulser) audit(at time.Time, tg Target, action string, detail map[string]any) {
	detail["kind"] = string(tg.Kind)
	detail["target_id"] = tg.ID
	detail["pane_ref"] = tg.PaneRef
	_ = p.DB.AppendAudit(store.AuditEntry{
		Timestamp: at,
		Action:    action,
		Actor:     "pulse",
		Subject:   p.Project + "/" + string(tg.Kind) + ":" + tg.ID,
		Detail:    detail,
	})
}

func stateKey(tg Target) string { return string(tg.Kind) + ":" + tg.ID }

// buildWakeBody returns the prompt the pulser sends into the pane. Kept
// short on purpose — the actor's own role file describes the bootstrap
// protocol; the wake just signals "you have work, run it."
func buildWakeBody(tg Target, depth int, since time.Duration) string {
	role := string(tg.Kind)
	if tg.Kind == KindManager {
		role = "manager:" + tg.ID
	} else if tg.Kind == KindIC {
		role = "ic:" + tg.ID
	}
	return fmt.Sprintf(
		"[arcmux pulse] %s — you have %d queued inbox message(s); "+
			"%s since last wake. Run your bootstrap protocol: peek your "+
			"inbox, act on what's there, then journal + scratchpad before "+
			"yielding.",
		role, depth, since.Round(time.Second),
	)
}
