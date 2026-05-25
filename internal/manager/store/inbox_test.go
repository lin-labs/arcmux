package store

import (
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
