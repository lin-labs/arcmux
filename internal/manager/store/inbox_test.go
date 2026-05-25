package store

import (
	"errors"
	"testing"
	"time"
)

func TestInboxPushPop(t *testing.T) {
	db := openTestDB(t)

	m1 := InboxMsg{ID: "m1", Verb: "add", From: "user", Priority: 1, Body: "do X", ReceivedAt: time.Now()}
	m2 := InboxMsg{ID: "m2", Verb: "revise", From: "user", Priority: 2, Body: "actually Y", ReceivedAt: time.Now().Add(time.Millisecond)}

	if err := db.PushElonInbox(m1); err != nil {
		t.Fatalf("PushElonInbox m1: %v", err)
	}
	if err := db.PushElonInbox(m2); err != nil {
		t.Fatalf("PushElonInbox m2: %v", err)
	}

	msgs, err := db.PeekElonInbox(10)
	if err != nil {
		t.Fatalf("PeekElonInbox: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2", len(msgs))
	}
	if msgs[0].ID != "m1" {
		t.Errorf("msgs[0].ID = %q, want m1", msgs[0].ID)
	}

	if err := db.AckElonInbox(msgs[0].ID); err != nil {
		t.Fatalf("AckElonInbox: %v", err)
	}
	remaining, _ := db.PeekElonInbox(10)
	if len(remaining) != 1 || remaining[0].ID != "m2" {
		t.Errorf("after ack remaining = %+v, want [m2]", remaining)
	}
}

func TestManagerInboxLifecycle(t *testing.T) {
	db := openTestDB(t)

	// Push before Ensure → ErrManagerInboxMissing.
	err := db.PushManagerInbox("team-a", InboxMsg{ID: "x", Verb: "add", From: "elon", Body: "hi"})
	if !errors.Is(err, ErrManagerInboxMissing) {
		t.Fatalf("push before ensure: err = %v, want ErrManagerInboxMissing", err)
	}
	if db.HasManagerInbox("team-a") {
		t.Errorf("HasManagerInbox before ensure = true, want false")
	}

	// Ensure.
	if err := db.EnsureManagerInbox("team-a"); err != nil {
		t.Fatalf("EnsureManagerInbox: %v", err)
	}
	if !db.HasManagerInbox("team-a") {
		t.Errorf("HasManagerInbox after ensure = false, want true")
	}
	// Re-ensure is idempotent.
	if err := db.EnsureManagerInbox("team-a"); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}

	// Push two messages.
	m1 := InboxMsg{ID: "m1", Verb: "add", From: "elon", Body: "vision", ReceivedAt: time.Now()}
	m2 := InboxMsg{ID: "m2", Verb: "revise", From: "elon", Body: "scope cut", ReceivedAt: time.Now().Add(time.Millisecond)}
	if err := db.PushManagerInbox("team-a", m1); err != nil {
		t.Fatalf("push m1: %v", err)
	}
	if err := db.PushManagerInbox("team-a", m2); err != nil {
		t.Fatalf("push m2: %v", err)
	}

	// Peek oldest-first.
	msgs, err := db.PeekManagerInbox("team-a", 10)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
		t.Errorf("peek = %+v, want [m1, m2]", msgs)
	}

	// Ack m1.
	if err := db.AckManagerInbox("team-a", "m1"); err != nil {
		t.Fatalf("ack m1: %v", err)
	}
	msgs, _ = db.PeekManagerInbox("team-a", 10)
	if len(msgs) != 1 || msgs[0].ID != "m2" {
		t.Errorf("after ack = %+v, want [m2]", msgs)
	}

	// Ack missing.
	if err := db.AckManagerInbox("team-a", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ack missing: err = %v, want ErrNotFound", err)
	}
}

func TestManagerInboxIsolation(t *testing.T) {
	db := openTestDB(t)

	if err := db.EnsureManagerInbox("team-a"); err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if err := db.EnsureManagerInbox("team-b"); err != nil {
		t.Fatalf("ensure b: %v", err)
	}

	mA := InboxMsg{ID: "ma", Verb: "add", From: "elon", Body: "for A"}
	mB := InboxMsg{ID: "mb", Verb: "add", From: "elon", Body: "for B"}
	if err := db.PushManagerInbox("team-a", mA); err != nil {
		t.Fatalf("push A: %v", err)
	}
	if err := db.PushManagerInbox("team-b", mB); err != nil {
		t.Fatalf("push B: %v", err)
	}

	gotA, _ := db.PeekManagerInbox("team-a", 10)
	gotB, _ := db.PeekManagerInbox("team-b", 10)
	if len(gotA) != 1 || gotA[0].ID != "ma" {
		t.Errorf("A inbox = %+v, want [ma]", gotA)
	}
	if len(gotB) != 1 || gotB[0].ID != "mb" {
		t.Errorf("B inbox = %+v, want [mb]", gotB)
	}

	// Elon inbox stays empty — manager pushes do not leak.
	elon, _ := db.PeekElonInbox(10)
	if len(elon) != 0 {
		t.Errorf("Elon inbox leaked: %+v", elon)
	}
}

func TestManagerInboxPeekUnknownTeam(t *testing.T) {
	db := openTestDB(t)
	_, err := db.PeekManagerInbox("nonexistent", 10)
	if !errors.Is(err, ErrManagerInboxMissing) {
		t.Errorf("peek unknown: err = %v, want ErrManagerInboxMissing", err)
	}
}

func TestManagerInboxRejectsEmptyTeam(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureManagerInbox(""); err == nil {
		t.Error("EnsureManagerInbox(empty): want error, got nil")
	}
	if err := db.PushManagerInbox("", InboxMsg{ID: "x"}); err == nil {
		t.Error("PushManagerInbox(empty): want error, got nil")
	}
	if _, err := db.PeekManagerInbox("", 1); err == nil {
		t.Error("PeekManagerInbox(empty): want error, got nil")
	}
	if err := db.AckManagerInbox("", "x"); err == nil {
		t.Error("AckManagerInbox(empty): want error, got nil")
	}
}

func TestManagerInboxRequiresMsgID(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureManagerInbox("team-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := db.PushManagerInbox("team-a", InboxMsg{}); err == nil {
		t.Error("PushManagerInbox without ID: want error, got nil")
	}
}

// TestICInboxLifecycle mirrors TestManagerInboxLifecycle but against the
// per-slot inbox surface. The shape is intentionally identical: spawn-time
// EnsureICInbox followed by push/peek/ack semantics that exactly mirror the
// manager inbox. If the two diverge in subtle ways, the CLI's --to routing
// stops being one mental model.
func TestICInboxLifecycle(t *testing.T) {
	db := openTestDB(t)

	// Push before Ensure → ErrICInboxMissing.
	err := db.PushICInbox("slot-a", InboxMsg{ID: "x", Verb: "consult", From: "manager", Body: "hi"})
	if !errors.Is(err, ErrICInboxMissing) {
		t.Fatalf("push before ensure: err = %v, want ErrICInboxMissing", err)
	}
	if db.HasICInbox("slot-a") {
		t.Errorf("HasICInbox before ensure = true, want false")
	}

	if err := db.EnsureICInbox("slot-a"); err != nil {
		t.Fatalf("EnsureICInbox: %v", err)
	}
	if !db.HasICInbox("slot-a") {
		t.Errorf("HasICInbox after ensure = false, want true")
	}
	// Re-ensure is idempotent.
	if err := db.EnsureICInbox("slot-a"); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}

	m1 := InboxMsg{ID: "m1", Verb: "consult", From: "manager", Body: "use lib X", ReceivedAt: time.Now()}
	m2 := InboxMsg{ID: "m2", Verb: "redirect", From: "manager", Body: "skip step 2", ReceivedAt: time.Now().Add(time.Millisecond)}
	if err := db.PushICInbox("slot-a", m1); err != nil {
		t.Fatalf("push m1: %v", err)
	}
	if err := db.PushICInbox("slot-a", m2); err != nil {
		t.Fatalf("push m2: %v", err)
	}

	msgs, err := db.PeekICInbox("slot-a", 10)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
		t.Errorf("peek = %+v, want [m1, m2]", msgs)
	}

	if err := db.AckICInbox("slot-a", "m1"); err != nil {
		t.Fatalf("ack m1: %v", err)
	}
	msgs, _ = db.PeekICInbox("slot-a", 10)
	if len(msgs) != 1 || msgs[0].ID != "m2" {
		t.Errorf("after ack = %+v, want [m2]", msgs)
	}

	if err := db.AckICInbox("slot-a", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ack missing: err = %v, want ErrNotFound", err)
	}
}

// TestICInboxIsolation ensures one IC's queue cannot leak into another's,
// and that IC pushes never leak into Elon's inbox. Two slots that happen to
// share a name in different teams cannot exist (Slot.ID is project-unique),
// so the test models cross-slot isolation within one project.
func TestICInboxIsolation(t *testing.T) {
	db := openTestDB(t)

	if err := db.EnsureICInbox("slot-a"); err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if err := db.EnsureICInbox("slot-b"); err != nil {
		t.Fatalf("ensure b: %v", err)
	}

	mA := InboxMsg{ID: "ma", Verb: "consult", From: "manager", Body: "for A"}
	mB := InboxMsg{ID: "mb", Verb: "consult", From: "manager", Body: "for B"}
	if err := db.PushICInbox("slot-a", mA); err != nil {
		t.Fatalf("push A: %v", err)
	}
	if err := db.PushICInbox("slot-b", mB); err != nil {
		t.Fatalf("push B: %v", err)
	}

	gotA, _ := db.PeekICInbox("slot-a", 10)
	gotB, _ := db.PeekICInbox("slot-b", 10)
	if len(gotA) != 1 || gotA[0].ID != "ma" {
		t.Errorf("A inbox = %+v, want [ma]", gotA)
	}
	if len(gotB) != 1 || gotB[0].ID != "mb" {
		t.Errorf("B inbox = %+v, want [mb]", gotB)
	}

	elon, _ := db.PeekElonInbox(10)
	if len(elon) != 0 {
		t.Errorf("Elon inbox leaked from IC push: %+v", elon)
	}
	mgr, _ := db.PeekManagerInbox("team-a", 10)
	if len(mgr) != 0 {
		// manager inbox bucket doesn't even exist; we expect missing
		// regardless. The point: pushing to slot-a/slot-b did NOT
		// create a team inbox.
		t.Errorf("manager inbox unexpectedly populated by IC push: %+v", mgr)
	}
}

func TestICInboxPeekUnknownSlot(t *testing.T) {
	db := openTestDB(t)
	_, err := db.PeekICInbox("nonexistent", 10)
	if !errors.Is(err, ErrICInboxMissing) {
		t.Errorf("peek unknown: err = %v, want ErrICInboxMissing", err)
	}
}

func TestICInboxRejectsEmptySlot(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureICInbox(""); err == nil {
		t.Error("EnsureICInbox(empty): want error, got nil")
	}
	if err := db.PushICInbox("", InboxMsg{ID: "x"}); err == nil {
		t.Error("PushICInbox(empty): want error, got nil")
	}
	if _, err := db.PeekICInbox("", 1); err == nil {
		t.Error("PeekICInbox(empty): want error, got nil")
	}
	if err := db.AckICInbox("", "x"); err == nil {
		t.Error("AckICInbox(empty): want error, got nil")
	}
}

func TestICInboxRequiresMsgID(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureICInbox("slot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := db.PushICInbox("slot-a", InboxMsg{}); err == nil {
		t.Error("PushICInbox without ID: want error, got nil")
	}
}

// TestDropICInbox pins three properties that the icspawn.Dissolve flow
// depends on:
//
//  1. Queued-but-unacked messages are purged with the bucket — a respawn
//     under the same slot id sees a genuinely empty queue, not the prior
//     IC's leftover state. This is the "don't silently inherit another
//     IC's inbox" foot-gun guard.
//  2. Drop is idempotent — calling DropICInbox on a slot that was never
//     spawned (or already dissolved) is a no-op, not an error. Dissolve
//     never fails just because the inbox was already torn down.
//  3. After drop, a subsequent EnsureICInbox-then-push works exactly like
//     a fresh spawn (push to a never-existed slot is the loud-error path;
//     here we're checking the re-create-then-push round-trip).
func TestDropICInbox(t *testing.T) {
	db := openTestDB(t)

	// (2) Drop before Ensure — silent no-op.
	if err := db.DropICInbox("ghost"); err != nil {
		t.Errorf("DropICInbox before Ensure: %v (want nil — idempotent)", err)
	}

	// (1) Queue some messages, then drop — peek after drop returns the
	// "missing bucket" error, not stale messages.
	if err := db.EnsureICInbox("slot-x"); err != nil {
		t.Fatalf("EnsureICInbox: %v", err)
	}
	for _, id := range []string{"q1", "q2", "q3"} {
		if err := db.PushICInbox("slot-x", InboxMsg{ID: id, Verb: "consult", Body: "x"}); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	pre, _ := db.PeekICInbox("slot-x", 10)
	if len(pre) != 3 {
		t.Fatalf("pre-drop peek = %d msgs, want 3", len(pre))
	}

	if err := db.DropICInbox("slot-x"); err != nil {
		t.Fatalf("DropICInbox: %v", err)
	}
	if db.HasICInbox("slot-x") {
		t.Errorf("HasICInbox after drop = true, want false")
	}
	if _, err := db.PeekICInbox("slot-x", 10); !errors.Is(err, ErrICInboxMissing) {
		t.Errorf("post-drop peek err = %v, want ErrICInboxMissing", err)
	}

	// Drop again — still silent (idempotent on missing bucket).
	if err := db.DropICInbox("slot-x"); err != nil {
		t.Errorf("second DropICInbox: %v (want nil)", err)
	}

	// (3) Re-Ensure under the same slot id yields a fresh, empty queue.
	if err := db.EnsureICInbox("slot-x"); err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	again, _ := db.PeekICInbox("slot-x", 10)
	if len(again) != 0 {
		t.Errorf("post-respawn peek = %+v, want []; prior IC's messages leaked", again)
	}
	if err := db.PushICInbox("slot-x", InboxMsg{ID: "fresh", Verb: "redirect"}); err != nil {
		t.Errorf("push after respawn: %v", err)
	}
}

func TestDropICInboxRejectsEmpty(t *testing.T) {
	db := openTestDB(t)
	if err := db.DropICInbox(""); err == nil {
		t.Error("DropICInbox(empty): want error, got nil")
	}
}

func TestInboxDepth(t *testing.T) {
	db := openTestDB(t)

	// Elon: empty → 0; push two → 2; ack one → 1.
	if n, err := db.DepthElonInbox(); err != nil || n != 0 {
		t.Fatalf("empty Elon depth: n=%d err=%v", n, err)
	}
	for i, id := range []string{"e1", "e2"} {
		if err := db.PushElonInbox(InboxMsg{ID: id, Verb: "add", ReceivedAt: time.Now().Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	if n, _ := db.DepthElonInbox(); n != 2 {
		t.Errorf("after 2 push, Elon depth = %d, want 2", n)
	}
	_ = db.AckElonInbox("e1")
	if n, _ := db.DepthElonInbox(); n != 1 {
		t.Errorf("after ack, Elon depth = %d, want 1", n)
	}

	// Manager: missing sub-bucket → err; after Ensure → 0; push N → N.
	if _, err := db.DepthManagerInbox("ghost"); !errors.Is(err, ErrManagerInboxMissing) {
		t.Errorf("DepthManagerInbox(unknown): err=%v, want ErrManagerInboxMissing", err)
	}
	if err := db.EnsureManagerInbox("alpha"); err != nil {
		t.Fatalf("ensure alpha: %v", err)
	}
	if n, err := db.DepthManagerInbox("alpha"); err != nil || n != 0 {
		t.Fatalf("alpha empty depth: n=%d err=%v", n, err)
	}
	for i, id := range []string{"a1", "a2", "a3"} {
		if err := db.PushManagerInbox("alpha", InboxMsg{ID: id, Verb: "add", ReceivedAt: time.Now().Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatalf("push alpha %s: %v", id, err)
		}
	}
	if n, _ := db.DepthManagerInbox("alpha"); n != 3 {
		t.Errorf("alpha after 3 push: depth = %d, want 3", n)
	}

	// IC: missing → err; after Ensure → 0; push → 1; drop → err.
	if _, err := db.DepthICInbox("ghost-slot"); !errors.Is(err, ErrICInboxMissing) {
		t.Errorf("DepthICInbox(unknown): err=%v, want ErrICInboxMissing", err)
	}
	_ = db.EnsureICInbox("worker-1")
	if n, _ := db.DepthICInbox("worker-1"); n != 0 {
		t.Errorf("worker-1 empty depth: %d, want 0", n)
	}
	_ = db.PushICInbox("worker-1", InboxMsg{ID: "w1", Verb: "ack"})
	if n, _ := db.DepthICInbox("worker-1"); n != 1 {
		t.Errorf("worker-1 after push depth: %d, want 1", n)
	}
	_ = db.DropICInbox("worker-1")
	if _, err := db.DepthICInbox("worker-1"); !errors.Is(err, ErrICInboxMissing) {
		t.Errorf("after drop, DepthICInbox: err=%v, want ErrICInboxMissing", err)
	}

	// Empty-arg guards.
	if _, err := db.DepthManagerInbox(""); err == nil {
		t.Error("DepthManagerInbox(empty): want error, got nil")
	}
	if _, err := db.DepthICInbox(""); err == nil {
		t.Error("DepthICInbox(empty): want error, got nil")
	}
}
