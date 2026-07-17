package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newCreateSessionTestDaemon builds a daemon with the real
// claude_exec profile so createSessionWithIdempotency exercises the
// production path. Sessions are added to d.sessions but no exec process
// is spawned (CreateSession for exec only spawns on SendPrompt). Bbolt
// state.bolt is opened so audit rows can be written.
func newCreateSessionTestDaemon(t *testing.T) (*Daemon, func()) {
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

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: filepath.Join(dir, "arcmux.sock"),
			LogDir: filepath.Join(dir, "logs"),
		},
		Hooks: config.HooksConfig{
			HookOutputDir: filepath.Join(dir, "hooks"),
		},
		Agents: config.DefaultAgentProfiles(),
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	d := &Daemon{
		cfg:      cfg,
		hooks:    hooks.NewInstaller(cfg.Hooks.HookOutputDir),
		watcher:  hooks.NewWatcher(cfg.Hooks.HookOutputDir, logger),
		profiles: cfg.Agents,
		logger:   logger,
		sessions: make(map[string]*session.Session),
		monitors: make(map[string]context.CancelFunc),
		eventBus: NewEventBus(),
	}
	d.SetState(db)
	d.ctx = context.Background()
	return d, func() { _ = db.Close() }
}

// sendPromptSpy intercepts daemon.SendPrompt calls so tests can observe
// the confirmDelivery argument. Because SendPrompt's actual implementation
// hits the exec/tmux transport, we can't call it directly in a unit test;
// the spy replaces the daemon's profile map with a profile shape whose
// transport branch is short-circuited via this side channel.
//
// In practice we observe via the same observable-failure trick used by
// makeSendObservable: a missing profile makes SendPrompt return Internal
// with confirmDelivery in the error message — but the message doesn't
// echo the bool, so we go through a thin shim instead.
type sendPromptSpy struct {
	mu             sync.Mutex
	confirmHistory []bool
}

func (s *sendPromptSpy) record(confirmDelivery bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmHistory = append(s.confirmHistory, confirmDelivery)
}

func (s *sendPromptSpy) lastConfirm() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.confirmHistory) == 0 {
		return false
	}
	return s.confirmHistory[len(s.confirmHistory)-1]
}

func (s *sendPromptSpy) confirms() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bool, len(s.confirmHistory))
	copy(out, s.confirmHistory)
	return out
}

// installSendPromptSpy wires a profile-less daemon and records every
// SendPrompt confirmDelivery argument by replacing the daemon's
// sendPromptHook. The hook is consulted by SendPrompt at the very top
// before any transport dispatch, so the spy fires deterministically.
func installSendPromptSpy(d *Daemon) *sendPromptSpy {
	spy := &sendPromptSpy{}
	d.sendPromptHook = func(_ context.Context, _ string, _ string, confirm bool, _ bool) error {
		spy.record(confirm)
		return nil
	}
	return spy
}

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

func TestListSessionsBindsProfileScopeAndCanonicalHistory(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	d.cfg.Daemon.ProfileName = "codex"
	d.cfg.Hooks.SessionStateDir = t.TempDir()
	sess := injectSession(t, d, "self-catalog", "owner", session.StateIdle)
	now := time.Now().UTC()
	state := hooks.SessionState{
		SessionID: sess.ID, Agent: "claude", CreatedAt: now, UpdatedAt: now,
		TurnContract: &hooks.TurnContract{
			CanonicalHistory: &hooks.CanonicalHistoryBinding{
				Basename: "2026-07-15-self.md", ConversationID: "native-conversation-123",
				Provenance: hooks.CanonicalHistoryBindingProvenance, UpdatedAt: now,
			},
			UpdatedAt: now,
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooks.SessionStatePath(d.cfg.Hooks.SessionStateDir, sess.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	response, err := srv.ListSessions(context.Background(), &arcmuxv1.ListSessionsRequest{})
	if err != nil || len(response.Sessions) != 1 {
		t.Fatalf("list sessions=%+v err=%v", response, err)
	}
	got := response.Sessions[0]
	if got.GetSessionId() != sess.ID || got.GetProfileScope() != "profile:codex" || got.GetHistoryBasename() != "2026-07-15-self.md" || got.GetCwd() != "/tmp" {
		t.Fatalf("catalog summary=%+v", got)
	}
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

func TestPrivateSessionCreateRedactsCWDFromLogsAndAudit(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	var logs bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	secretCWD := filepath.Join(t.TempDir(), "DO_NOT_LEAK_PRIVATE_WORKTREE")
	if err := os.Mkdir(secretCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	sess, _, err := d.createSessionWithIdempotency(context.Background(), CreateSessionRequest{
		Agent: "claude_exec", CWD: secretCWD, Name: "private-handoff", OwnerID: "arcmux-handoff:test", private: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := NewGRPCServer(d).QueryAudit(context.Background(), &arcmuxv1.QueryAuditRequest{SessionId: sess.Snapshot().ID})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), secretCWD) || strings.Contains(string(audit), secretCWD) {
		t.Fatalf("private cwd leaked logs=%s audit=%s", logs.String(), audit)
	}
}

// TestQueryAudit_BadSince guards the InvalidArgument edge.
func TestQueryAudit_BadSince(t *testing.T) {
	srv, _, _ := newC1TestServer(t)
	_, err := srv.QueryAudit(context.Background(), &arcmuxv1.QueryAuditRequest{Since: "yesterday"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad since: code=%v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// sendPromptCalls counts how many times the test-instrumented SendPrompt
// shim was hit by the daemon's various send paths. Used as the observable
// signal that the readiness retry / force-direct / idle-drain hooks did
// the right thing without depending on a real tmux/exec transport.
type sendPromptObserver struct {
	called   chan string
	failNext bool
}

// installSendPromptObserver replaces d.SendPrompt's effective behavior by
// wiring the daemon's profile map to point at an "unknown agent" sentinel.
// SendPrompt(unknown agent) returns "unknown agent profile" — that error
// has codes.Internal at the gRPC boundary. So the test can distinguish:
//
//	queue path  → resp.Queued=true, err=nil
//	direct path → err=Internal "send prompt: unknown agent profile: X"
//
// The error WITHOUT a profile is the deterministic, transport-free way
// to observe that the direct branch was chosen. Used by all three Send
// race tests below.
func makeSendObservable(d *Daemon) {
	d.profiles = map[string]profile.Profile{}
}

// TestSend_RetriesForBriefStartingWindow drives the pre-deliver readiness
// poll added to close the "first Send right after CreateSession queues
// instead of delivering" race. A session that starts in StateStarting and
// flips to StateIdle within the readiness window must take the direct
// path, not the queue path. We deliberately do NOT register a profile so
// SendPrompt errors with Internal — that's the observable signal that
// the readiness loop saw Idle and chose direct delivery.
func TestSend_RetriesForBriefStartingWindow(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	sess := injectSession(t, d, "warming-up", "elonco", session.StateStarting)
	makeSendObservable(d)

	// Simulate the agent handshake completing ~75ms after the caller
	// fires Send. Well within sendReadinessWindow (2s).
	go func() {
		time.Sleep(75 * time.Millisecond)
		sess.SetState(session.StateIdle)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := srv.Send(ctx, &arcmuxv1.SendRequest{
		SessionName: "warming-up",
		Body:        "kickoff",
		From:        "elonco",
	})

	// We want the DIRECT branch (Internal err from missing profile),
	// NOT the queue branch (nil err + Queued=true).
	if err == nil {
		if resp != nil && resp.Queued {
			t.Errorf("readiness retry didn't fire: got queued=true (resp=%+v); want direct-delivery attempt",
				resp)
		}
		return
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("Send err code = %v, want Internal (direct path with no profile registered); err=%v",
			status.Code(err), err)
	}
	// Belt-and-suspenders: the queue bucket must NOT have grown.
	left, _ := db.PeekSessionInbox("warming-up", 10)
	if len(left) != 0 {
		t.Errorf("queue depth = %d, want 0 (direct path should not have pushed)", len(left))
	}
}

// TestSend_ForceDirectBypassesReadiness pins the --force escape hatch:
// even when the session is in StateWorking (a state sessionReady rejects),
// force_direct=true must attempt the direct path. With no profile
// registered, SendPrompt returns an Internal error — but force_direct's
// fallback is to queue rather than return that error to the caller, so
// we observe a queued=true response with delivered=false. Either way,
// the readiness predicate was bypassed.
func TestSend_ForceDirectBypassesReadiness(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	injectSession(t, d, "busy", "elonco", session.StateWorking)
	makeSendObservable(d)

	resp, err := srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName: "busy",
		Body:        "kickoff",
		From:        "elonco",
		ForceDirect: true,
	})
	if err != nil {
		t.Fatalf("force_direct should not surface direct-deliver error to caller (falls back to queue): %v", err)
	}
	if resp == nil || !resp.Queued || resp.Delivered {
		t.Errorf("force_direct fallback: resp=%+v, want queued=true delivered=false", resp)
	}
	// The fallback queue must hold the body so the caller can recover.
	left, _ := db.PeekSessionInbox("busy", 10)
	if len(left) != 1 || left[0].Body != "kickoff" {
		t.Errorf("fallback queue depth=%d body=%q; want 1, \"kickoff\"", len(left),
			func() string {
				if len(left) > 0 {
					return left[0].Body
				}
				return ""
			}())
	}
}

// TestEmitStateChanged_DrainsInboxOnIdle drives the idle-drain hook: a
// queued message must be delivered on the next state→idle transition.
// We can't reach delivered=true here (no real transport) — but the hook
// MUST fire, and it must not panic. The smoke is: emit a state→idle
// transition and confirm the daemon did not deadlock.
func TestEmitStateChanged_DrainsInboxOnIdle(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	sess := injectSession(t, d, "drainee", "elonco", session.StateWorking)
	d.ctx = context.Background()
	makeSendObservable(d)

	// Queue a message via Send (working session → queue path).
	if _, err := srv.Send(d.ctx, &arcmuxv1.SendRequest{
		SessionName: "drainee",
		Body:        "do the thing",
		From:        "elonco",
	}); err != nil {
		t.Fatalf("Send (queue): %v", err)
	}
	pre, _ := db.PeekSessionInbox("drainee", 10)
	if len(pre) != 1 {
		t.Fatalf("queue depth before drain = %d, want 1", len(pre))
	}

	// Transition to idle. emitStateChanged kicks off drainInboxOnIdle
	// in a goroutine; that goroutine calls SendPrompt, which errors
	// (no profile registered) and logs without panicking. We assert
	// no deadlock by sleeping a beat and confirming the test exits.
	sess.SetState(session.StateIdle)
	d.emitStateChanged(sess.Snapshot().ID, session.StateIdle, "test idle")

	// Give the goroutine time to run + log. We're proving no panic /
	// deadlock — the queue state itself is implementation-defined when
	// SendPrompt fails (current contract: leave the message queued).
	time.Sleep(150 * time.Millisecond)
	after, _ := db.PeekSessionInbox("drainee", 10)
	if len(after) != 1 {
		t.Errorf("queue depth after drain (with failing SendPrompt) = %d, want 1 (safe fallback leaves the msg queued)", len(after))
	}
}

func TestEmitStateChanged_DrainUsesFireAndForget(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	sess := injectSession(t, d, "drain-fire", "elonco", session.StateWorking)
	d.ctx = context.Background()
	spy := installSendPromptSpy(d)

	if _, err := srv.Send(d.ctx, &arcmuxv1.SendRequest{
		SessionName: "drain-fire",
		Body:        "opening prompt",
		From:        "elonco",
	}); err != nil {
		t.Fatalf("Send (queue): %v", err)
	}

	sess.SetState(session.StateIdle)
	d.emitStateChanged(sess.Snapshot().ID, session.StateIdle, "test idle")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		confirms := spy.confirms()
		if len(confirms) > 0 {
			if confirms[0] != false {
				t.Fatalf("drain confirmDelivery = %v, want false", confirms[0])
			}
			left, _ := db.PeekSessionInbox("drain-fire", 10)
			if len(left) != 0 {
				t.Fatalf("queue depth after successful drain = %d, want 0", len(left))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("drain did not call SendPrompt; confirms=%v", spy.confirms())
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

// TestSend_FreshSpawnTreatedAsReady pins the fresh-spawn override added
// for the elonco "register_agent → Send within ms → queued=true forever"
// bug. A session created within freshSpawnWindow whose state is still
// StateStarting (the SessionStart hook hasn't fired yet) must take the
// direct-delivery path even before the state machine catches up to idle.
//
// With no profile registered, the direct path errors Internal — that's
// the observable signal that fresh-spawn override fired, NOT the queue
// path (which would have returned queued=true with err=nil).
func TestSend_FreshSpawnTreatedAsReady(t *testing.T) {
	srv, d, db := newC1TestServer(t)
	// injectSession constructs the session with StartedAt=time.Now()
	// (via session.NewSession), so it's freshly-spawned by definition.
	injectSession(t, d, "just-spawned", "elonco", session.StateStarting)
	makeSendObservable(d)

	resp, err := srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName: "just-spawned",
		Body:        "kickoff",
		From:        "elonco",
	})
	if err == nil && resp != nil && resp.Queued {
		t.Fatalf("fresh-spawn override didn't fire: got queued=true (resp=%+v); want direct attempt",
			resp)
	}
	if err != nil && status.Code(err) != codes.Internal {
		t.Fatalf("Send err code = %v, want Internal (direct path); err=%v", status.Code(err), err)
	}
	left, _ := db.PeekSessionInbox("just-spawned", 10)
	if len(left) != 0 {
		t.Errorf("queue depth after fresh-spawn override = %d, want 0", len(left))
	}
}

// TestSend_FreshSpawnExpired is the negative bound on the override: a
// session whose StartedAt is older than freshSpawnWindow must NOT be
// treated as ready by virtue of being a recent spawn. With Working state
// + old StartedAt, the response is the normal queue path.
func TestSend_FreshSpawnExpired(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	sess := injectSession(t, d, "stale", "elonco", session.StateStarting)
	// Backdate the spawn so the override window is past.
	sess.StartedAt = time.Now().Add(-2 * freshSpawnWindow)

	resp, err := srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName: "stale",
		Body:        "kickoff",
		From:        "elonco",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !resp.Queued {
		t.Errorf("expired fresh spawn: queued=%v, want true (override should not fire)", resp.Queued)
	}
}

// TestSend_FreshSpawnDoesNotPreemptWorking guards the "fresh spawn but
// already busy" edge: when a session is freshly-created AND in
// StateWorking, the override must NOT fire — Working means real downstream
// activity that the inbox queue is meant to protect. We assert the queue
// path was taken.
func TestSend_FreshSpawnDoesNotPreemptWorking(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	injectSession(t, d, "busy-fresh", "elonco", session.StateWorking)

	resp, err := srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName: "busy-fresh",
		Body:        "kickoff",
		From:        "elonco",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !resp.Queued {
		t.Errorf("fresh+working: queued=%v, want true (override must not preempt working sessions)",
			resp.Queued)
	}
}

// TestSend_ConfirmDeliveryThreadsThroughToSendPrompt pins Bug 4. The
// daemon's SendPrompt has a `confirmDelivery` parameter that gates the
// typesafe assessment; the C1 Send RPC used to hardcode this to true.
// We exercise both values and assert the value the caller passed reaches
// daemon.SendPrompt unchanged.
//
// We can't observe the bool from the gRPC response shape (SendResponse
// doesn't echo it), so we install a SendPrompt observer by stubbing the
// daemon's profile map to a transport-less profile that records the
// confirm arg into a side channel. See sendPromptSpyProfile below.
func TestSend_ConfirmDeliveryThreadsThroughToSendPrompt(t *testing.T) {
	srv, d, _ := newC1TestServer(t)
	// Inject a fresh idle session so sessionReady=true and the direct
	// path is taken (sessionReady is the only branch that consults
	// req.ConfirmDelivery before falling into SendPrompt).
	injectSession(t, d, "echoer", "elonco", session.StateIdle)
	spy := installSendPromptSpy(d)

	// Case A: confirm_delivery=false (the new default for fire-and-forget).
	_, _ = srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName:     "echoer",
		Body:            "hi",
		ConfirmDelivery: false,
	})
	if got := spy.lastConfirm(); got != false {
		t.Errorf("confirm_delivery=false: observed %v, want false", got)
	}

	// Case B: explicit confirm_delivery=true.
	_, _ = srv.Send(context.Background(), &arcmuxv1.SendRequest{
		SessionName:     "echoer",
		Body:            "hi2",
		ConfirmDelivery: true,
	})
	if got := spy.lastConfirm(); got != true {
		t.Errorf("confirm_delivery=true: observed %v, want true", got)
	}
}

// TestCreateSession_IdempotentOnNameOwner pins Bug 2. Two CreateSession
// calls with the same (Name, OwnerID) must return the same session_id
// and the second call's response.created must be false. Without
// idempotency, the daemon would spawn a duplicate tmux window — the
// observed elonco "ic spawn called twice → two identical windows"
// failure mode.
//
// We use the exec transport so the test avoids tmux dependency; the
// idempotency check lives upstream of the transport branch so this is
// representative.
func TestCreateSession_IdempotentOnNameOwner(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateSessionRequest{
		Agent:   "claude_exec",
		CWD:     t.TempDir(),
		Name:    "ic:region-b:obsidian:0",
		OwnerID: "elonco:region-b",
	}

	s1, created1, err := d.createSessionWithIdempotency(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	if !created1 {
		t.Errorf("first call: created=false, want true")
	}

	s2, created2, err := d.createSessionWithIdempotency(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	if created2 {
		t.Errorf("second call: created=true, want false (idempotent)")
	}
	if s1.Snapshot().ID != s2.Snapshot().ID {
		t.Errorf("session_id differed across idempotent calls: %q vs %q",
			s1.Snapshot().ID, s2.Snapshot().ID)
	}

	// A third call with the SAME name but a DIFFERENT owner must spawn
	// a fresh session — owner_id scopes the dedupe key.
	reqOther := req
	reqOther.OwnerID = "elonco:region-c"
	s3, created3, err := d.createSessionWithIdempotency(ctx, reqOther)
	if err != nil {
		t.Fatalf("CreateSession 3 (other owner): %v", err)
	}
	if !created3 {
		t.Errorf("third call (other owner): created=false, want true")
	}
	if s3.Snapshot().ID == s1.Snapshot().ID {
		t.Errorf("other-owner call returned same session_id; want fresh spawn")
	}
}

// TestCreateSession_LegacyNoOwnerSkipsIdempotency keeps legacy callers
// (voxtop / arcmux-cli without --owner) on their historical
// "every-call-is-new" semantics. Without owner_id set, name collisions
// should NOT dedupe — that would silently break tools that happen to
// reuse the same generated name.
func TestCreateSession_LegacyNoOwnerSkipsIdempotency(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	ctx := context.Background()

	req := CreateSessionRequest{
		Agent: "claude_exec",
		CWD:   t.TempDir(),
		Name:  "shared-name",
		// no OwnerID
	}
	s1, c1, err := d.createSessionWithIdempotency(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	s2, c2, err := d.createSessionWithIdempotency(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	if !c1 || !c2 {
		t.Errorf("legacy callers: both calls should report created=true (no owner_id → no dedupe); got c1=%v c2=%v",
			c1, c2)
	}
	if s1.Snapshot().ID == s2.Snapshot().ID {
		t.Errorf("legacy callers: returned same session_id (%q); want distinct sessions",
			s1.Snapshot().ID)
	}
}

func TestCreateSession_InitialExecPromptUsesFireAndForget(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	spy := installSendPromptSpy(d)

	_, _, err := d.createSessionWithIdempotency(context.Background(), CreateSessionRequest{
		Agent:  "claude_exec",
		CWD:    t.TempDir(),
		Name:   "fresh-opening-prompt",
		Prompt: "start the work",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		confirms := spy.confirms()
		if len(confirms) > 0 {
			if confirms[0] != false {
				t.Fatalf("initial prompt confirmDelivery = %v, want false", confirms[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("initial prompt was not sent; confirms=%v", spy.confirms())
}
