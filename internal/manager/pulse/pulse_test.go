package pulse

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// fakeClock is a monotonic clock the test drives explicitly.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// fakeRunner records every cmux call. Send returns ErrSend if SendErr is set.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string // each entry: ["send", "--target", ref, "--", body], etc.
	SendErr error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dup := make([]string, len(args))
	copy(dup, args)
	f.calls = append(f.calls, dup)
	if len(args) > 0 && args[0] == "send" && f.SendErr != nil {
		return "", f.SendErr
	}
	return "OK ref-stub\n", nil
}

func (f *fakeRunner) sendsTo(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= 3 && c[0] == "send" && c[2] == target {
			n++
		}
	}
	return n
}

func (f *fakeRunner) lastBodyTo(target string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		c := f.calls[i]
		if len(c) >= 5 && c[0] == "send" && c[2] == target {
			return c[4]
		}
	}
	return ""
}

func setup(t *testing.T) (*store.DB, *cmuxcli.Client, *fakeRunner, *fakeClock, *Pulser) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "state.bolt"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fr := &fakeRunner{}
	cli := cmuxcli.NewWithRunnerForTest(fr)
	clk := newFakeClock(time.Date(2026, 5, 25, 4, 0, 0, 0, time.UTC))

	p := New("arcmux-test", db, cli)
	p.Now = clk.Now
	// Tight cadences make the test fast and assertable.
	p.Cadence = Cadence{Elon: 100 * time.Millisecond, Manager: 50 * time.Millisecond, IC: 25 * time.Millisecond}
	return db, cli, fr, clk, p
}

func seedElon(t *testing.T, db *store.DB, paneRef string) {
	t.Helper()
	if err := db.PutProjectMeta(store.ProjectMeta{
		ElonPaneRef:      paneRef,
		ElonSurfaceRef:   paneRef + "-surf",
		ElonWorkspaceRef: "ws-test",
	}); err != nil {
		t.Fatalf("PutProjectMeta: %v", err)
	}
}

func seedTeam(t *testing.T, db *store.DB, id, paneRef string) {
	t.Helper()
	if err := db.PutTeam(store.Team{
		ID: id, State: store.TeamActive, ManagerPane: paneRef, WorkspaceRef: "ws-team-" + id,
	}); err != nil {
		t.Fatalf("PutTeam: %v", err)
	}
	if err := db.EnsureManagerInbox(id); err != nil {
		t.Fatalf("EnsureManagerInbox: %v", err)
	}
}

func seedSlot(t *testing.T, db *store.DB, slotID, team, paneRef string) {
	t.Helper()
	if err := db.PutSlot(store.Slot{
		ID: slotID, Team: team, Role: "ic-base", State: store.SlotActive, PaneRef: paneRef,
	}); err != nil {
		t.Fatalf("PutSlot: %v", err)
	}
	if err := db.EnsureICInbox(slotID); err != nil {
		t.Fatalf("EnsureICInbox: %v", err)
	}
}

// TestTick_NoTargets verifies an empty project ticks cleanly (no panic, no
// wakes) — covers the "freshly-scaffolded but never-launched" case.
func TestTick_NoTargets(t *testing.T) {
	_, _, fr, _, p := setup(t)
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Targets != 0 || rep.Wakes != 0 {
		t.Errorf("empty project: targets=%d wakes=%d, want 0/0", rep.Targets, rep.Wakes)
	}
	if got := len(fr.calls); got != 0 {
		t.Errorf("empty project sent %d cmux calls, want 0", got)
	}
}

// TestTick_FirstSightNoWake verifies the storm-on-restart guard: on first
// sight, we anchor cadence at now and only fire if the inbox already has
// messages (i.e., depth > 0 vs the prev=0 baseline IS a "grew" trigger).
func TestTick_FirstSightNoWake_EmptyInbox(t *testing.T) {
	db, _, fr, _, p := setup(t)
	seedElon(t, db, "pane:elon")
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Targets != 1 || rep.Wakes != 0 {
		t.Errorf("first-sight empty: targets=%d wakes=%d, want 1/0", rep.Targets, rep.Wakes)
	}
	if got := fr.sendsTo("pane:elon"); got != 0 {
		t.Errorf("first-sight empty inbox should not wake; got %d sends", got)
	}
}

// TestTick_FirstSight_InboxGrewFires verifies the spec's trigger (a): a
// pre-existing inbox message at first-sight time is "depth>0 from prev=0"
// and DOES fire a wake.
func TestTick_FirstSight_InboxGrewFires(t *testing.T) {
	db, _, fr, _, p := setup(t)
	seedElon(t, db, "pane:elon")
	if err := db.PushElonInbox(store.InboxMsg{ID: "m1", Verb: "add", From: "user", Body: "mission"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rep.Wakes != 1 {
		t.Fatalf("first-sight with queued message: wakes=%d, want 1", rep.Wakes)
	}
	if rep.Decisions[0].Trigger != TriggerInboxGrew {
		t.Errorf("trigger = %q, want inbox-grew", rep.Decisions[0].Trigger)
	}
	if got := fr.sendsTo("pane:elon"); got != 1 {
		t.Errorf("send to elon pane = %d, want 1", got)
	}
	body := fr.lastBodyTo("pane:elon")
	if !strings.Contains(body, "1 queued inbox message") {
		t.Errorf("wake body missing depth count: %q", body)
	}
	if !strings.Contains(body, "[arcmux pulse]") {
		t.Errorf("wake body missing pulse tag: %q", body)
	}
}

// TestTick_CadenceElapsedFires verifies the spec's trigger (b): once a
// target has been seen, advancing the clock past the cadence wakes it even
// with a flat (zero) inbox.
func TestTick_CadenceElapsedFires(t *testing.T) {
	db, _, fr, clk, p := setup(t)
	seedElon(t, db, "pane:elon")
	// Tick 1: first sight, empty inbox → no wake.
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if fr.sendsTo("pane:elon") != 0 {
		t.Fatal("tick1 should not wake")
	}
	// Advance past Elon cadence (100ms).
	clk.Advance(200 * time.Millisecond)
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if rep.Wakes != 1 {
		t.Fatalf("tick2 after cadence: wakes=%d, want 1", rep.Wakes)
	}
	if rep.Decisions[0].Trigger != TriggerCadenceElapsed {
		t.Errorf("trigger = %q, want cadence-elapsed", rep.Decisions[0].Trigger)
	}
}

// TestTick_NewMessageBetweenTicksFires covers the canonical case: an inbox
// push arrives between two ticks; the second tick fires.
func TestTick_NewMessageBetweenTicksFires(t *testing.T) {
	db, _, _, clk, p := setup(t)
	seedElon(t, db, "pane:elon")
	// Tick 1: empty → no wake.
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	// New message arrives.
	if err := db.PushElonInbox(store.InboxMsg{ID: "m1", Verb: "add", From: "user"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	// Advance less than cadence so ONLY the inbox trigger can fire.
	clk.Advance(5 * time.Millisecond)
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if rep.Wakes != 1 {
		t.Fatalf("tick2 wakes=%d, want 1", rep.Wakes)
	}
	if rep.Decisions[0].Trigger != TriggerInboxGrew {
		t.Errorf("trigger = %q, want inbox-grew", rep.Decisions[0].Trigger)
	}
}

// TestTick_NoDoubleWakeSameTick: even when both triggers fire on the same
// tick we send exactly one cmux Send.
func TestTick_NoDoubleWakeSameTick(t *testing.T) {
	db, _, fr, clk, p := setup(t)
	seedElon(t, db, "pane:elon")
	// Tick 1: first sight, empty → no wake.
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	// Inbox grows AND cadence elapses.
	_ = db.PushElonInbox(store.InboxMsg{ID: "m1", Verb: "add"})
	clk.Advance(200 * time.Millisecond)
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if rep.Wakes != 1 {
		t.Fatalf("expected exactly 1 wake; got %d", rep.Wakes)
	}
	if rep.Decisions[0].Trigger != TriggerBoth {
		t.Errorf("trigger = %q, want inbox-grew+cadence-elapsed", rep.Decisions[0].Trigger)
	}
	if fr.sendsTo("pane:elon") != 1 {
		t.Errorf("send count = %d, want 1 (no double-send for combined trigger)", fr.sendsTo("pane:elon"))
	}
}

// TestTick_AllThreeKinds verifies Elon + Manager + IC are all reached, with
// per-kind cadence honored independently.
func TestTick_AllThreeKinds(t *testing.T) {
	db, _, fr, clk, p := setup(t)
	seedElon(t, db, "pane:elon")
	seedTeam(t, db, "alpha", "pane:mgr-alpha")
	seedSlot(t, db, "worker-1", "alpha", "pane:ic-worker-1")

	// First tick: all three first-sight + empty inboxes → no wakes.
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if rep.Targets != 3 || rep.Wakes != 0 {
		t.Fatalf("tick1: targets=%d wakes=%d, want 3/0", rep.Targets, rep.Wakes)
	}

	// Advance past IC cadence (25ms) but not Manager (50ms) or Elon (100ms).
	clk.Advance(30 * time.Millisecond)
	rep, err = p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if rep.Wakes != 1 {
		t.Fatalf("tick2 wakes=%d, want 1 (only IC cadence elapsed)", rep.Wakes)
	}
	if fr.sendsTo("pane:ic-worker-1") != 1 {
		t.Errorf("IC pane was not woken; sends=%d", fr.sendsTo("pane:ic-worker-1"))
	}
	if fr.sendsTo("pane:mgr-alpha") != 0 || fr.sendsTo("pane:elon") != 0 {
		t.Errorf("manager/elon woken too early; mgr=%d elon=%d",
			fr.sendsTo("pane:mgr-alpha"), fr.sendsTo("pane:elon"))
	}

	// Advance further so all three cadences have elapsed (relative to
	// their respective last-wake-or-first-sight anchors).
	clk.Advance(200 * time.Millisecond)
	rep, err = p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if rep.Wakes != 3 {
		t.Fatalf("tick3 wakes=%d, want 3", rep.Wakes)
	}
}

// TestTick_DissolvedSlotNotPulsed verifies dissolved slots are excluded —
// otherwise pulse would keep trying to wake a dead pane forever.
func TestTick_DissolvedSlotNotPulsed(t *testing.T) {
	db, _, fr, clk, p := setup(t)
	seedTeam(t, db, "alpha", "pane:mgr-alpha")
	seedSlot(t, db, "worker-1", "alpha", "pane:ic-worker-1")
	// Mark slot dissolved.
	s, _ := db.GetSlot("worker-1")
	s.State = store.SlotDissolved
	if err := db.PutSlot(s); err != nil {
		t.Fatalf("dissolve: %v", err)
	}

	// First tick.
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	// Advance past everything.
	clk.Advance(time.Second)
	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if fr.sendsTo("pane:ic-worker-1") != 0 {
		t.Errorf("dissolved slot was pulsed; sends=%d", fr.sendsTo("pane:ic-worker-1"))
	}
	// Manager (still active, empty inbox, cadence elapsed) should fire.
	if fr.sendsTo("pane:mgr-alpha") < 1 {
		t.Errorf("active manager not pulsed; sends=%d", fr.sendsTo("pane:mgr-alpha"))
	}
	_ = rep
}

// TestTick_CmuxSendFailureNonFatal: cmux returning an error must be logged
// (in the Decision) but not abort the tick — other targets must still get
// their evaluation pass.
func TestTick_CmuxSendFailureNonFatal(t *testing.T) {
	db, _, fr, clk, p := setup(t)
	seedElon(t, db, "pane:elon")
	seedTeam(t, db, "alpha", "pane:mgr-alpha")
	fr.SendErr = errors.New("cmux: workspace closed")

	// First tick: first sight, empty → no wake → no error.
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	// Force wakes on next tick.
	_ = db.PushElonInbox(store.InboxMsg{ID: "m1", Verb: "add"})
	_ = db.PushManagerInbox("alpha", store.InboxMsg{ID: "m2", Verb: "add"})
	clk.Advance(10 * time.Millisecond)

	rep, err := p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if rep.Errors != 2 {
		t.Errorf("rep.Errors = %d, want 2", rep.Errors)
	}
	if rep.Wakes != 0 {
		t.Errorf("rep.Wakes = %d on send failure, want 0", rep.Wakes)
	}
	for _, d := range rep.Decisions {
		if d.WakeError == "" {
			t.Errorf("decision %+v missing WakeError", d)
		}
	}

	// On retry (next tick) with cmux healthy again, wakes should land —
	// proving LastWakeAt was NOT advanced on the failed send.
	fr.SendErr = nil
	clk.Advance(10 * time.Millisecond)
	rep, err = p.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if rep.Wakes != 2 {
		t.Errorf("recovery tick wakes = %d, want 2", rep.Wakes)
	}
}

// TestRun_RespectsContextCancel verifies Run returns cleanly on context
// cancellation (the SIGINT/SIGTERM path).
func TestRun_RespectsContextCancel(t *testing.T) {
	_, _, _, _, p := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, 50*time.Millisecond) }()
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancel within 1s")
	}
}

// TestRun_RejectsBadInterval prevents a misconfigured deploy from busy-
// looping.
func TestRun_RejectsBadInterval(t *testing.T) {
	_, _, _, _, p := setup(t)
	if err := p.Run(context.Background(), 0); err == nil {
		t.Error("Run(0) returned nil, want error")
	}
	if err := p.Run(context.Background(), -1*time.Second); err == nil {
		t.Error("Run(-1s) returned nil, want error")
	}
}

// TestTick_AuditRowsRecordTickAndWake — the audit log is the durable proof
// pulse ran. Verify a tick row + a wake row land.
func TestTick_AuditRowsRecordTickAndWake(t *testing.T) {
	db, _, _, _, p := setup(t)
	seedElon(t, db, "pane:elon")
	_ = db.PushElonInbox(store.InboxMsg{ID: "m1", Verb: "add"})
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rows, err := db.RecentAudit(50)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	var sawTick, sawWake bool
	for _, r := range rows {
		switch r.Action {
		case "pulse.tick":
			sawTick = true
		case "pulse.wake":
			sawWake = true
		}
	}
	if !sawTick || !sawWake {
		t.Errorf("audit log missing rows: tick=%v wake=%v (rows=%d)", sawTick, sawWake, len(rows))
	}
}
