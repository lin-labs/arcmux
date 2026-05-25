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
