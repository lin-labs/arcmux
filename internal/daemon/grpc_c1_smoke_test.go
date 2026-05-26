package daemon

import (
	"context"
	"testing"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/session"
)

// TestC1_EndToEndSmoke walks the C1 spec's E2E flow against an
// in-process Daemon + GRPCServer, no tmux required:
//
//  1. inject a session with owner_id="testco" (proxy for CreateSession;
//     real CreateSession needs tmux/agent — covered by integration_test
//     and the arcmux-test scenariotest harness).
//  2. Send to it. Session is in StateWorking (not ready) → message is
//     queued and we get back a msg_id with queued=true.
//  3. PeekInbox returns the queued message.
//  4. AckInbox idempotently on first and second call.
//  5. Ready returns the current state.
//  6. QueryAudit filtered by owner_id="testco" returns the rows from
//     steps 1-4 (session.create, inbox.send.queued, inbox.ack).
//
// If any future change breaks the C1 round-trip this single test fails
// loud — it's the canary the C2 commit's contract must keep alive.
func TestC1_EndToEndSmoke(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	ctx := context.Background()

	// (1) CreateSession surrogate. Real wire would set owner_id via the
	// CreateSessionRequest proto field; here we inject directly and
	// write the create audit row the way CreateSession does.
	sess := injectSession(t, d, "testco-session", "testco", session.StateWorking)
	d.auditSessionEvent("session.create", sess, map[string]any{
		"agent": "claude",
		"cwd":   "/tmp",
		"name":  sess.Snapshot().Name,
	})

	// (2) Send → queued (session not ready).
	send, err := srv.Send(ctx, &arcmuxv1.SendRequest{
		SessionName: "testco-session",
		Body:        "do the thing",
		From:        "elonco",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !send.Queued || send.Delivered {
		t.Fatalf("send routing: queued=%v delivered=%v, want queued=true delivered=false",
			send.Queued, send.Delivered)
	}
	if send.MsgId == "" {
		t.Fatal("send returned empty msg_id")
	}

	// (3) PeekInbox shows the message.
	peek, err := srv.PeekInbox(ctx, &arcmuxv1.PeekInboxRequest{
		SessionName: "testco-session",
		N:           10,
	})
	if err != nil {
		t.Fatalf("PeekInbox: %v", err)
	}
	if len(peek.Messages) != 1 {
		t.Fatalf("peek: got %d msgs, want 1", len(peek.Messages))
	}
	if peek.Messages[0].Id != send.MsgId {
		t.Errorf("peek msg_id = %q, want %q", peek.Messages[0].Id, send.MsgId)
	}
	if peek.Messages[0].Body != "do the thing" {
		t.Errorf("peek body = %q", peek.Messages[0].Body)
	}
	if peek.Messages[0].From != "elonco" {
		t.Errorf("peek from = %q, want elonco", peek.Messages[0].From)
	}

	// (4) AckInbox — twice.
	ack1, err := srv.AckInbox(ctx, &arcmuxv1.AckInboxRequest{
		SessionName: "testco-session",
		MsgId:       send.MsgId,
	})
	if err != nil || !ack1.Acked {
		t.Fatalf("first ack: acked=%v err=%v", ack1.Acked, err)
	}
	ack2, err := srv.AckInbox(ctx, &arcmuxv1.AckInboxRequest{
		SessionName: "testco-session",
		MsgId:       send.MsgId,
	})
	if err != nil || !ack2.Acked {
		t.Errorf("second ack (idempotent): acked=%v err=%v", ack2.Acked, err)
	}

	// (5) Ready returns the current state (still working).
	ready, err := srv.Ready(ctx, &arcmuxv1.ReadyRequest{SessionName: "testco-session"})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready.Ready {
		t.Errorf("ready=true; session was injected in working state")
	}
	if ready.State != string(session.StateWorking) {
		t.Errorf("Ready.state = %q, want %q", ready.State, session.StateWorking)
	}

	// (6) QueryAudit owner_id=testco → at least 3 rows
	// (session.create, inbox.send.queued, inbox.ack).
	audit, err := srv.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{
		OwnerId: "testco",
	})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range audit.Entries {
		if e.OwnerId != "testco" {
			t.Errorf("filter leak: owner_id=%q want testco", e.OwnerId)
		}
		seen[e.Action] = true
	}
	for _, must := range []string{"session.create", "inbox.send.queued", "inbox.ack"} {
		if !seen[must] {
			t.Errorf("audit missing action %q; have %v", must, seen)
		}
	}
}
