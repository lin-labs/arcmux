package store

import (
	"errors"
	"testing"
	"time"
)

// TestSessionInboxLifecycle pins the happy path: ensure → push twice →
// peek oldest-first → ack one → peek shows the other. Mirrors
// TestManagerInboxLifecycle's shape so the three nested inbox surfaces
// remain mentally interchangeable.
func TestSessionInboxLifecycle(t *testing.T) {
	db := openTestDB(t)

	// Push before Ensure → ErrSessionInboxMissing.
	err := db.PushSessionInbox("sess-a", InboxMsg{ID: "x", Body: "hi"})
	if !errors.Is(err, ErrSessionInboxMissing) {
		t.Fatalf("push before ensure: err = %v, want ErrSessionInboxMissing", err)
	}
	if db.HasSessionInbox("sess-a") {
		t.Errorf("HasSessionInbox before ensure = true, want false")
	}

	// Ensure once.
	if err := db.EnsureSessionInbox("sess-a"); err != nil {
		t.Fatalf("EnsureSessionInbox: %v", err)
	}
	if !db.HasSessionInbox("sess-a") {
		t.Errorf("HasSessionInbox after ensure = false, want true")
	}
	// Idempotent.
	if err := db.EnsureSessionInbox("sess-a"); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}

	m1 := InboxMsg{ID: "m1", Body: "first", From: "elonco", ReceivedAt: time.Now()}
	m2 := InboxMsg{ID: "m2", Body: "second", From: "elonco", ReceivedAt: time.Now().Add(time.Millisecond)}
	if err := db.PushSessionInbox("sess-a", m1); err != nil {
		t.Fatalf("push m1: %v", err)
	}
	if err := db.PushSessionInbox("sess-a", m2); err != nil {
		t.Fatalf("push m2: %v", err)
	}

	msgs, err := db.PeekSessionInbox("sess-a", 10)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
		t.Fatalf("peek = %+v, want [m1, m2]", msgs)
	}

	if err := db.AckSessionInbox("sess-a", "m1"); err != nil {
		t.Fatalf("ack m1: %v", err)
	}
	msgs, _ = db.PeekSessionInbox("sess-a", 10)
	if len(msgs) != 1 || msgs[0].ID != "m2" {
		t.Errorf("after ack = %+v, want [m2]", msgs)
	}

	// Idempotent ack: a second ack of m1 returns nil (the C1 RPC reports
	// acked=true on the second call).
	if err := db.AckSessionInbox("sess-a", "m1"); err != nil {
		t.Errorf("second ack m1: %v (want nil — idempotent)", err)
	}
	// Ack of a never-seen ID is also nil.
	if err := db.AckSessionInbox("sess-a", "never-existed"); err != nil {
		t.Errorf("ack ghost: %v (want nil)", err)
	}
}

// TestSessionInboxIsolation ensures one session's queue cannot bleed into
// another's, and that a session push never lands in the elon/manager/IC
// inboxes (the C1 inbox is its own parent bucket).
func TestSessionInboxIsolation(t *testing.T) {
	db := openTestDB(t)

	if err := db.EnsureSessionInbox("sess-a"); err != nil {
		t.Fatalf("ensure a: %v", err)
	}
	if err := db.EnsureSessionInbox("sess-b"); err != nil {
		t.Fatalf("ensure b: %v", err)
	}

	if err := db.PushSessionInbox("sess-a", InboxMsg{ID: "ma", Body: "for A"}); err != nil {
		t.Fatalf("push A: %v", err)
	}
	if err := db.PushSessionInbox("sess-b", InboxMsg{ID: "mb", Body: "for B"}); err != nil {
		t.Fatalf("push B: %v", err)
	}

	gotA, _ := db.PeekSessionInbox("sess-a", 10)
	gotB, _ := db.PeekSessionInbox("sess-b", 10)
	if len(gotA) != 1 || gotA[0].ID != "ma" {
		t.Errorf("A inbox = %+v, want [ma]", gotA)
	}
	if len(gotB) != 1 || gotB[0].ID != "mb" {
		t.Errorf("B inbox = %+v, want [mb]", gotB)
	}

	// (Pre-C3 this test also asserted the now-removed Elon inbox stayed
	// untouched. With role-class inboxes deleted, per-session isolation
	// is the only invariant left to pin and we already pinned it above.)
}

// TestSessionInboxPeekUnknown ensures peek on a never-ensured name surfaces
// the "missing" sentinel rather than silently returning an empty list. The
// daemon distinguishes "queue is empty" from "nobody has ever sent here".
func TestSessionInboxPeekUnknown(t *testing.T) {
	db := openTestDB(t)
	_, err := db.PeekSessionInbox("nonexistent", 10)
	if !errors.Is(err, ErrSessionInboxMissing) {
		t.Errorf("peek unknown: err = %v, want ErrSessionInboxMissing", err)
	}
	// Depth: same sentinel.
	if _, err := db.DepthSessionInbox("nonexistent"); !errors.Is(err, ErrSessionInboxMissing) {
		t.Errorf("depth unknown: err = %v, want ErrSessionInboxMissing", err)
	}
}

// TestSessionInboxRejectsEmpty pins the empty-arg guards. Every DAO method
// must refuse "" up front — otherwise a bug in the daemon's session
// lookup would silently write under the empty key.
func TestSessionInboxRejectsEmpty(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureSessionInbox(""); err == nil {
		t.Error("EnsureSessionInbox(empty): want error, got nil")
	}
	if err := db.PushSessionInbox("", InboxMsg{ID: "x"}); err == nil {
		t.Error("PushSessionInbox(empty): want error, got nil")
	}
	if _, err := db.PeekSessionInbox("", 1); err == nil {
		t.Error("PeekSessionInbox(empty): want error, got nil")
	}
	if err := db.AckSessionInbox("", "x"); err == nil {
		t.Error("AckSessionInbox(empty): want error, got nil")
	}
	if _, err := db.DepthSessionInbox(""); err == nil {
		t.Error("DepthSessionInbox(empty): want error, got nil")
	}
}

// TestSessionInboxRequiresMsgID guards against the daemon forgetting to
// stamp NewInboxID() before push.
func TestSessionInboxRequiresMsgID(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureSessionInbox("sess-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := db.PushSessionInbox("sess-a", InboxMsg{}); err == nil {
		t.Error("PushSessionInbox without ID: want error, got nil")
	}
}

// TestSessionInboxDepth covers the depth happy path.
func TestSessionInboxDepth(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureSessionInbox("sess-d"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if n, err := db.DepthSessionInbox("sess-d"); err != nil || n != 0 {
		t.Errorf("empty depth: n=%d err=%v", n, err)
	}
	for i, id := range []string{"d1", "d2", "d3"} {
		if err := db.PushSessionInbox("sess-d", InboxMsg{
			ID:         id,
			Body:       "x",
			ReceivedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	if n, _ := db.DepthSessionInbox("sess-d"); n != 3 {
		t.Errorf("after 3 push, depth = %d, want 3", n)
	}
	_ = db.AckSessionInbox("sess-d", "d2")
	if n, _ := db.DepthSessionInbox("sess-d"); n != 2 {
		t.Errorf("after ack, depth = %d, want 2", n)
	}
}

// TestSessionInboxPeekN ensures the n parameter caps results.
func TestSessionInboxPeekN(t *testing.T) {
	db := openTestDB(t)
	if err := db.EnsureSessionInbox("sess-n"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		if err := db.PushSessionInbox("sess-n", InboxMsg{
			ID:         id,
			ReceivedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	msgs, _ := db.PeekSessionInbox("sess-n", 2)
	if len(msgs) != 2 {
		t.Errorf("peek n=2 returned %d msgs, want 2", len(msgs))
	}
	// n=0 returns all (the daemon uses this to drain).
	all, _ := db.PeekSessionInbox("sess-n", 0)
	if len(all) != 5 {
		t.Errorf("peek n=0 returned %d msgs, want 5 (n<=0 means all)", len(all))
	}
}

// TestSessionInboxBucketCreatedOnOpen guards the AllBuckets registration.
// If somebody trims BucketSessionInbox from AllBuckets the EnsureSessionInbox
// call would still work (it CreateBucketIfNotExists on the parent), but a
// fresh Open should give us the parent bucket without an Ensure first.
func TestSessionInboxBucketCreatedOnOpen(t *testing.T) {
	db := openTestDB(t)
	if !db.HasBucket(BucketSessionInbox) {
		t.Errorf("BucketSessionInbox %q not created on Open — missing from AllBuckets?", BucketSessionInbox)
	}
}
