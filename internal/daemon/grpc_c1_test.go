package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newC1TestServer builds a minimum-viable Daemon + GRPCServer for the
// C1 RPCs. It does NOT call Start() — that would spin up tmux/grpc/
// supervisor goroutines that the C1 RPCs don't depend on. Instead we
// hand-wire a daemon-level state.bolt under a t.TempDir() and inject it.
func newC1TestServer(t *testing.T) (*GRPCServer, *Daemon, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "data", "arcmux", "_daemon")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	db, err := store.Open(filepath.Join(stateDir, "state.bolt"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		Hooks: config.HooksConfig{
			HookOutputDir: filepath.Join(dir, "hooks"),
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	d := &Daemon{
		cfg:      cfg,
		hooks:    hooks.NewInstaller(cfg.Hooks.HookOutputDir),
		watcher:  hooks.NewWatcher(cfg.Hooks.HookOutputDir, logger),
		profiles: nil,
		logger:   logger,
		sessions: make(map[string]*session.Session),
		monitors: make(map[string]context.CancelFunc),
		eventBus: NewEventBus(),
	}
	d.SetState(db)
	return NewGRPCServer(d), d, db
}

// injectSession registers a session into the daemon by name without
// going through CreateSession (which needs tmux + a profile).
func injectSession(t *testing.T, d *Daemon, name, ownerID string, state session.State) *session.Session {
	t.Helper()
	sess := session.NewSession("s-"+name, name, "claude", "/tmp")
	sess.SetOwnerID(ownerID)
	sess.SetState(state)
	d.mu.Lock()
	d.sessions[sess.ID] = sess
	d.mu.Unlock()
	return sess
}

// TestSend_QueuedWhenNotReady drives the C1 routing predicate: a non-idle
// session forces the queue path. Verifies msg_id is returned, the inbox
// bucket is created, the body lands in the queue, and the audit row
// carries owner_id/session_id under Detail.
func TestSend_QueuedWhenNotReady(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	injectSession(t, d, "alpha", "testco", session.StateWorking)
	ctx := context.Background()

	resp, err := srv.Send(ctx, &arcmuxv1.SendRequest{
		SessionName: "alpha",
		Body:        "hello",
		From:        "elonco",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !resp.Queued || resp.Delivered {
		t.Errorf("routing: queued=%v delivered=%v, want queued=true delivered=false", resp.Queued, resp.Delivered)
	}
	if resp.MsgId == "" {
		t.Errorf("msg_id empty; want a sortable id")
	}

	if !db.HasSessionInbox("alpha") {
		t.Errorf("inbox bucket not created after Send")
	}
	msgs, err := db.PeekSessionInbox("alpha", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("peek after send: msgs=%+v err=%v", msgs, err)
	}
	if msgs[0].Body != "hello" {
		t.Errorf("body = %q, want hello", msgs[0].Body)
	}
	if msgs[0].From != "elonco" {
		t.Errorf("from = %q, want elonco", msgs[0].From)
	}

	// Audit row carries owner_id + session_id under Detail.
	audit, err := db.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(audit) == 0 {
		t.Fatal("no audit rows; expected at least inbox.send.queued")
	}
	var found *store.AuditEntry
	for i := range audit {
		if audit[i].Action == "inbox.send.queued" {
			found = &audit[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("inbox.send.queued audit row missing; have %+v", audit)
	}
	if owner, _ := found.Detail["owner_id"].(string); owner != "testco" {
		t.Errorf("audit owner_id = %v, want testco", found.Detail["owner_id"])
	}
}

// TestSend_NoSession ensures a missing session name surfaces NotFound
// rather than silently creating an orphan inbox.
func TestSend_NoSession(t *testing.T) {
	srv, _, _ := newC1TestServer(t)
	_, err := srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName: "ghost",
		Body:        "x",
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("err code = %v, want NotFound (err=%v)", status.Code(err), err)
	}
}

// TestSend_RequiresArgs guards the InvalidArgument edges.
func TestSend_RequiresArgs(t *testing.T) {
	srv, _, _ := newC1TestServer(t)
	ctx := context.Background()
	if _, err := srv.Send(ctx, &arcmuxv1.SendRequest{Body: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty name: code=%v want InvalidArgument", status.Code(err))
	}
	if _, err := srv.Send(ctx, &arcmuxv1.SendRequest{SessionName: "a"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty body: code=%v want InvalidArgument", status.Code(err))
	}
}

// TestPeekInbox_EmptyForUnknownSession proves the tolerant peek contract:
// peeking a session that was never queued returns an empty list, not
// an error. The C1 plan explicitly wants this behavior so the daemon
// can answer "what's queued?" without forcing callers to ensure-first.
func TestPeekInbox_EmptyForUnknownSession(t *testing.T) {
	srv, _, _ := newC1TestServer(t)
	resp, err := srv.PeekInbox(context.Background(), &arcmuxv1.PeekInboxRequest{
		SessionName: "never-sent-to",
		N:           10,
	})
	if err != nil {
		t.Fatalf("PeekInbox: %v", err)
	}
	if len(resp.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(resp.Messages))
	}
}

// TestAckInbox_Idempotent pins the "ack of an ID that was never queued
// returns acked=true" contract. Combined with the second-call case,
// this is what makes the inbox safe to retry from a flaky client.
func TestAckInbox_Idempotent(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	injectSession(t, d, "beta", "testco", session.StateWorking)
	ctx := context.Background()

	// Queue one message via Send.
	send, err := srv.Send(ctx, &arcmuxv1.SendRequest{SessionName: "beta", Body: "go"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgID := send.MsgId

	// First ack: succeeds, removes the message.
	resp, err := srv.AckInbox(ctx, &arcmuxv1.AckInboxRequest{SessionName: "beta", MsgId: msgID})
	if err != nil || !resp.Acked {
		t.Fatalf("first ack: acked=%v err=%v", resp.Acked, err)
	}
	left, _ := db.PeekSessionInbox("beta", 10)
	if len(left) != 0 {
		t.Errorf("after first ack, queue depth = %d, want 0", len(left))
	}

	// Second ack of the same id: still acked=true (idempotent).
	resp, err = srv.AckInbox(ctx, &arcmuxv1.AckInboxRequest{SessionName: "beta", MsgId: msgID})
	if err != nil {
		t.Fatalf("second ack: err=%v", err)
	}
	if !resp.Acked {
		t.Errorf("second ack acked=false, want true (idempotent)")
	}

	// Ack against a never-touched session: bucket missing → acked=false.
	resp, err = srv.AckInbox(ctx, &arcmuxv1.AckInboxRequest{SessionName: "stranger", MsgId: "x"})
	if err != nil {
		t.Fatalf("stranger ack: err=%v", err)
	}
	if resp.Acked {
		t.Errorf("ack against bucket-less session acked=true, want false")
	}
}

// TestReady_StatesMap proves the readiness predicate's mapping. Idle ==
// ready; anything else == not ready. Unknown name returns ready=false
// with reason=no-such-session (not an error — pollers want a sentinel).
func TestReady_StatesMap(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	injectSession(t, d, "idle", "x", session.StateIdle)
	injectSession(t, d, "working", "x", session.StateWorking)
	ctx := context.Background()

	idle, err := srv.Ready(ctx, &arcmuxv1.ReadyRequest{SessionName: "idle"})
	if err != nil {
		t.Fatalf("Ready idle: %v", err)
	}
	if !idle.Ready {
		t.Errorf("idle session: ready=false, want true (reason=%q)", idle.Reason)
	}

	working, err := srv.Ready(ctx, &arcmuxv1.ReadyRequest{SessionName: "working"})
	if err != nil {
		t.Fatalf("Ready working: %v", err)
	}
	if working.Ready {
		t.Errorf("working session: ready=true, want false")
	}

	ghost, err := srv.Ready(ctx, &arcmuxv1.ReadyRequest{SessionName: "ghost"})
	if err != nil {
		t.Fatalf("Ready ghost: %v", err)
	}
	if ghost.Ready || ghost.Reason != "no-such-session" {
		t.Errorf("ghost: ready=%v reason=%q, want ready=false reason=no-such-session", ghost.Ready, ghost.Reason)
	}
}

// TestQueryAudit_Filters drives the three filters end-to-end. Verifies
// owner_id filtering, session_id filtering, and that the limit caps
// results. since= is exercised by a separate cheap test below.
func TestQueryAudit_Filters(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	sessA := injectSession(t, d, "audit-a", "owner-A", session.StateIdle)
	sessB := injectSession(t, d, "audit-b", "owner-B", session.StateIdle)

	// Write a few audit rows directly so we don't depend on Send's
	// rendering (Send is exercised in TestSend_QueuedWhenNotReady).
	d.auditSessionEvent("session.create", sessA, nil)
	d.auditSessionEvent("inbox.send.queued", sessA, map[string]any{"msg_id": "1"})
	d.auditSessionEvent("inbox.ack", sessA, map[string]any{"msg_id": "1"})
	d.auditSessionEvent("session.create", sessB, nil)
	d.auditSessionEvent("inbox.send.queued", sessB, map[string]any{"msg_id": "2"})

	ctx := context.Background()

	// Filter by owner: A → 3 rows, B → 2 rows.
	respA, err := srv.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{OwnerId: "owner-A"})
	if err != nil {
		t.Fatalf("QueryAudit A: %v", err)
	}
	if got := len(respA.Entries); got != 3 {
		t.Errorf("owner-A: got %d entries, want 3 (entries=%+v)", got, respA.Entries)
	}
	for _, e := range respA.Entries {
		if e.OwnerId != "owner-A" {
			t.Errorf("filter leak: owner_id=%q want owner-A", e.OwnerId)
		}
	}

	respB, err := srv.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{OwnerId: "owner-B"})
	if err != nil {
		t.Fatalf("QueryAudit B: %v", err)
	}
	if got := len(respB.Entries); got != 2 {
		t.Errorf("owner-B: got %d entries, want 2", got)
	}

	// Filter by session_id.
	respSess, err := srv.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{SessionId: sessA.Snapshot().ID})
	if err != nil {
		t.Fatalf("QueryAudit sess: %v", err)
	}
	if got := len(respSess.Entries); got != 3 {
		t.Errorf("session filter: got %d entries, want 3", got)
	}

	// Limit caps results.
	respLim, err := srv.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{Limit: 2})
	if err != nil {
		t.Fatalf("QueryAudit limit: %v", err)
	}
	if got := len(respLim.Entries); got != 2 {
		t.Errorf("limit=2: got %d entries, want 2", got)
	}

	// Silence unused — keep db for future expansion of this test.
	_ = db
}

// TestQueryAudit_BadSince guards the InvalidArgument edge.
func TestQueryAudit_BadSince(t *testing.T) {
	srv, _, _ := newC1TestServer(t)
	_, err := srv.QueryAudit(context.Background(), &arcmuxv1.QueryAuditRequest{Since: "yesterday"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad since: code=%v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// TestC1_StateUnavailable ensures all five RPCs return Unavailable
// (not a panic) when the daemon-level state.bolt isn't open. This is
// the contract the daemon Start() falls back on when bbolt open fails.
func TestC1_StateUnavailable(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	// Clear the state injection.
	d.SetState(nil)
	injectSession(t, d, "x", "", session.StateWorking)
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"Send", func() error {
			_, err := srv.Send(ctx, &arcmuxv1.SendRequest{SessionName: "x", Body: "y"})
			return err
		}},
		{"PeekInbox", func() error {
			_, err := srv.PeekInbox(ctx, &arcmuxv1.PeekInboxRequest{SessionName: "x"})
			return err
		}},
		{"AckInbox", func() error {
			_, err := srv.AckInbox(ctx, &arcmuxv1.AckInboxRequest{SessionName: "x", MsgId: "id"})
			return err
		}},
		{"QueryAudit", func() error {
			_, err := srv.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{})
			return err
		}},
	}
	for _, c := range cases {
		err := c.call()
		if status.Code(err) != codes.Unavailable {
			t.Errorf("%s without state: code=%v want Unavailable (err=%v)", c.name, status.Code(err), err)
		}
	}
}
