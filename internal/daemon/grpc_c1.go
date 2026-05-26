// grpc_c1.go implements the C1 substrate extensions on the AgentRuntime
// service: Send (queueable variant of SendPrompt), PeekInbox, AckInbox,
// Ready, and QueryAudit. These are additive — none of the legacy RPCs
// change behavior, and a client that doesn't know about them keeps
// working unmodified.
//
// All five share two backbones:
//
//  1. Session lookup by name. The C1 RPCs key off the session_name field
//     because elonco-style callers manage naming themselves and want a
//     stable handle that survives across daemon restarts. (The opaque
//     session_id is regenerated each create — fine for in-process work,
//     hostile to cross-process orchestration.)
//
//  2. d.daemon.State() — the daemon-level bbolt store. If it's nil
//     (Start hasn't run, or open failed), every C1 RPC returns
//     codes.Unavailable so clients can fall back instead of hanging.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/manager/store"
	"github.com/lin-labs/arcmux/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Send delivers a body to a session by name. Routing:
//   - session ready (idle / safely interruptible) → deliver synchronously
//     via the existing SendPrompt path; returns delivered=true.
//   - otherwise → push onto the per-session inbox and return queued=true.
//
// On either branch a sortable msg_id is returned so the caller can ack /
// audit the message. The msg_id is generated even on direct deliver
// because elonco wants one audit handle whether or not the body was
// queued — symmetric, easier to log.
func (s *GRPCServer) Send(ctx context.Context, req *arcmuxv1.SendRequest) (*arcmuxv1.SendResponse, error) {
	if req.SessionName == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name required")
	}
	if req.Body == "" {
		return nil, status.Error(codes.InvalidArgument, "body required")
	}

	sess := s.daemon.FindSessionByName(req.SessionName)
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "no session named %q", req.SessionName)
	}

	st := s.daemon.State()
	if st == nil {
		return nil, status.Error(codes.Unavailable, "daemon state.bolt not open; C1 substrate disabled")
	}

	msgID, err := store.NewInboxID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "new inbox id: %v", err)
	}

	// Pre-deliver readiness window: a freshly-spawned session can be in
	// StateStarting / StateHandshaking for ~hundreds of ms before flipping
	// to StateIdle. Without this poll, the FIRST Send right after
	// CreateSession reliably gets queued=true / delivered=false even
	// though the session becomes ready almost immediately.
	//
	// Only wait for the spawn-transient states. A StateWorking /
	// StateStuck session can sit there for minutes; we MUST NOT block
	// the caller on those — that's exactly what the inbox queue is for.
	//
	// force_direct skips the predicate entirely — escape hatch for
	// callers who know the session is alive but the state machine hasn't
	// caught up yet.
	if !req.ForceDirect && isSpawnTransient(sess) {
		waitForSessionReady(ctx, sess, sendReadinessWindow)
	}

	if req.ForceDirect || sessionReady(sess) {
		// Deliver immediately. Same path SendPrompt uses, with the
		// caller-supplied confirm_delivery and wait_idle=false (we
		// already proved ready, or the caller forced direct). The C1
		// Send RPC previously hardcoded confirmDelivery=true which
		// forced every delivery through the typesafe assessment gate;
		// surfacing it lets fire-and-forget callers bypass that gate
		// when they know it would over-reject (e.g. fresh-spawn panes).
		snap := sess.Snapshot()
		if err := s.daemon.SendPrompt(ctx, snap.ID, req.Body, req.ConfirmDelivery, false); err != nil {
			// force_direct: on failure, fall through to the queue path
			// so caller still has a recoverable msg_id handle.
			if !req.ForceDirect {
				return nil, status.Errorf(codes.Internal, "send prompt: %v", err)
			}
			s.daemon.Logger().Warn("session.send.force_direct.failed; falling back to queue",
				"session_id", snap.ID, "name", snap.Name, "error", err)
		} else {
			actionTag := "inbox.send.direct"
			if req.ForceDirect {
				actionTag = "inbox.send.force_direct"
			}
			s.daemon.auditSessionEvent(actionTag, sess, map[string]any{
				"msg_id": msgID,
				"from":   req.From,
			})
			return &arcmuxv1.SendResponse{
				MsgId:     msgID,
				Delivered: true,
				Queued:    false,
			}, nil
		}
	}

	// Queue path. Ensure the inbox bucket exists, then push.
	if err := st.EnsureSessionInbox(req.SessionName); err != nil {
		return nil, status.Errorf(codes.Internal, "ensure inbox: %v", err)
	}
	if err := st.PushSessionInbox(req.SessionName, store.InboxMsg{
		ID:         msgID,
		Body:       req.Body,
		From:       req.From,
		ReceivedAt: time.Now(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "push inbox: %v", err)
	}
	snap := sess.Snapshot()
	s.daemon.Logger().Info("session.send.queued",
		"session_id", snap.ID,
		"name", snap.Name,
		"msg_id", msgID,
		"from", req.From,
		"bytes", len(req.Body),
		"preview", truncatePreview(req.Body, 50),
	)
	s.daemon.auditSessionEvent("inbox.send.queued", sess, map[string]any{
		"msg_id": msgID,
		"from":   req.From,
	})

	return &arcmuxv1.SendResponse{
		MsgId:     msgID,
		Delivered: false,
		Queued:    true,
	}, nil
}

// PeekInbox returns up to n queued messages, oldest-first. Tolerant: a
// session whose inbox bucket was never ensured returns an empty list,
// not an error — that's the "queue is empty" semantic callers want.
func (s *GRPCServer) PeekInbox(ctx context.Context, req *arcmuxv1.PeekInboxRequest) (*arcmuxv1.PeekInboxResponse, error) {
	if req.SessionName == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name required")
	}
	st := s.daemon.State()
	if st == nil {
		return nil, status.Error(codes.Unavailable, "daemon state.bolt not open; C1 substrate disabled")
	}

	limit := int(req.N)
	if limit <= 0 {
		limit = 0 // 0 means "all"
	}

	msgs, err := st.PeekSessionInbox(req.SessionName, limit)
	if err != nil {
		// "Never sent here" is empty, not an error — the substrate
		// answer to "what's queued?" is "nothing".
		if isSessionInboxMissing(err) {
			return &arcmuxv1.PeekInboxResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "peek inbox: %v", err)
	}

	out := &arcmuxv1.PeekInboxResponse{
		Messages: make([]*arcmuxv1.InboxMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, &arcmuxv1.InboxMessage{
			Id:         m.ID,
			Body:       m.Body,
			From:       m.From,
			ReceivedAt: m.ReceivedAt.Format(time.RFC3339Nano),
		})
	}
	return out, nil
}

// AckInbox removes a queued message by ID. Idempotent: a second call
// (or a call for an ID that was never queued) returns acked=true. The
// one false case is "the inbox bucket itself doesn't exist", which
// means the caller is acking against a session that's never been sent
// to — almost certainly a bug, so we tell them.
func (s *GRPCServer) AckInbox(ctx context.Context, req *arcmuxv1.AckInboxRequest) (*arcmuxv1.AckInboxResponse, error) {
	if req.SessionName == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name required")
	}
	if req.MsgId == "" {
		return nil, status.Error(codes.InvalidArgument, "msg_id required")
	}
	st := s.daemon.State()
	if st == nil {
		return nil, status.Error(codes.Unavailable, "daemon state.bolt not open; C1 substrate disabled")
	}

	if err := st.AckSessionInbox(req.SessionName, req.MsgId); err != nil {
		if isSessionInboxMissing(err) {
			return &arcmuxv1.AckInboxResponse{Acked: false}, nil
		}
		return nil, status.Errorf(codes.Internal, "ack inbox: %v", err)
	}

	// Best-effort audit — we don't fail the ack if the audit fails.
	if sess := s.daemon.FindSessionByName(req.SessionName); sess != nil {
		s.daemon.auditSessionEvent("inbox.ack", sess, map[string]any{
			"msg_id": req.MsgId,
		})
	}

	return &arcmuxv1.AckInboxResponse{Acked: true}, nil
}

// Ready reports whether a session is currently safe to deliver into.
// Sources: the in-memory session state machine (StateIdle is ready) +
// the hook watcher's latest signal timestamp. Mirrors the predicate the
// internal sessionReady helper uses, exposed so callers can poll
// instead of speculatively queueing.
func (s *GRPCServer) Ready(ctx context.Context, req *arcmuxv1.ReadyRequest) (*arcmuxv1.ReadyResponse, error) {
	if req.SessionName == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name required")
	}
	sess := s.daemon.FindSessionByName(req.SessionName)
	if sess == nil {
		return &arcmuxv1.ReadyResponse{
			Ready:  false,
			Reason: "no-such-session",
		}, nil
	}
	snap := sess.Snapshot()
	ready := sessionReady(sess)

	// last_signal_at: prefer the latest hook event timestamp; fall back
	// to LastActivityAt so we always emit something useful.
	lastSignal := snap.LastActivityAt
	if events := s.daemon.watcher.LatestEvents(snap.ID); len(events) > 0 {
		if t := events[len(events)-1].Timestamp; !t.IsZero() {
			lastSignal = t
		}
	}

	reason := string(snap.State)
	if ready {
		reason = "ready:" + reason
	} else {
		reason = "not-ready:" + reason
	}
	return &arcmuxv1.ReadyResponse{
		Ready:        ready,
		Reason:       reason,
		LastSignalAt: lastSignal.Format(time.RFC3339Nano),
		State:        string(snap.State),
	}, nil
}

// QueryAudit returns recent audit rows, optionally filtered by owner_id
// / session_id / since timestamp. The current implementation pages
// through RecentAudit and filters in-memory; that's fine at C1 scale
// (bbolt audit bucket is small) and matches the AuditEntry shape we
// already have. When the C3/C4 cleanup lands and AuditEntry grows
// real top-level owner_id/session_id columns, this handler can switch
// to bucket-side filtering.
func (s *GRPCServer) QueryAudit(ctx context.Context, req *arcmuxv1.QueryAuditRequest) (*arcmuxv1.QueryAuditResponse, error) {
	st := s.daemon.State()
	if st == nil {
		return nil, status.Error(codes.Unavailable, "daemon state.bolt not open; C1 substrate disabled")
	}

	limit := int(req.Limit)
	if limit <= 0 {
		limit = 200 // default cap
	}

	var since time.Time
	if s := strings.TrimSpace(req.Since); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t, err = time.Parse(time.RFC3339, s)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "since must be RFC3339: %v", err)
			}
		}
		since = t
	}

	// Pull a generous chunk and filter; the bbolt depth here is bounded.
	raw, err := st.RecentAudit(limit * 4)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "recent audit: %v", err)
	}

	out := &arcmuxv1.QueryAuditResponse{
		Entries: make([]*arcmuxv1.AuditEntry, 0, limit),
	}
	for _, e := range raw {
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		ownerID, sessionID := extractIDs(e)
		if req.OwnerId != "" && req.OwnerId != ownerID {
			continue
		}
		if req.SessionId != "" && req.SessionId != sessionID {
			continue
		}
		detail := map[string]string{}
		for k, v := range e.Detail {
			detail[k] = formatDetailValue(v)
		}
		out.Entries = append(out.Entries, &arcmuxv1.AuditEntry{
			Timestamp: e.Timestamp.Format(time.RFC3339Nano),
			Action:    e.Action,
			Actor:     e.Actor,
			Subject:   e.Subject,
			OwnerId:   ownerID,
			SessionId: sessionID,
			Detail:    detail,
		})
		if len(out.Entries) >= limit {
			break
		}
	}
	return out, nil
}

// sessionReady returns true when the session is in a state we can safely
// deliver into synchronously. Kept conservative: only StateIdle counts.
// Working / handshaking / stuck / etc. force the queue path. This is the
// predicate Ready() and Send() both consult so behavior stays consistent.
//
// Plus an explicit fresh-spawn override: a session that was created
// within the last freshSpawnWindow is treated as ready for any non-failed
// state. Rationale — claude with `--remote-control` doesn't fire its
// SessionStart hook until the user types, so the daemon-side state stays
// in StateStarting/StateHandshaking far longer than the agent is actually
// "not ready". For the first few seconds after spawn we trust the OS
// process being alive as good enough; the typesafe gate inside SendPrompt
// will still catch a truly broken pane and reject delivery downstream.
func sessionReady(sess *session.Session) bool {
	if sess == nil {
		return false
	}
	snap := sess.Snapshot()
	if snap.State == session.StateIdle {
		return true
	}
	if isFreshSpawn(snap) {
		return true
	}
	return false
}

// freshSpawnWindow is how long after CreateSession we keep treating a
// session as "ready enough" even when the state machine hasn't caught up
// to a real readiness signal. 10s comfortably covers a typical
// claude/codex handshake (1–4s) plus the gap before
// `--remote-control` SessionStart fires.
const freshSpawnWindow = 10 * time.Second

// isFreshSpawn reports whether the session was created recently enough
// that a missing readiness signal is more likely "the hook hasn't fired
// yet" than "the agent is genuinely stuck or working". Scoped to the
// spawn-transient states (Starting, Handshaking) so we never preempt a
// session that's already accepted prior work — a Working/Stuck session
// has real activity downstream that the inbox queue is meant to protect.
func isFreshSpawn(snap session.Snapshot) bool {
	switch snap.State {
	case session.StateStarting, session.StateHandshaking:
		// fall through
	default:
		return false
	}
	if snap.StartedAt.IsZero() {
		return false
	}
	return time.Since(snap.StartedAt) <= freshSpawnWindow
}

// sendReadinessWindow is the max time Send polls for StateIdle before
// giving up and falling back to the inbox queue. Chosen to comfortably
// cover the spawn-to-idle transition (handshake + initial pane ready),
// while staying short enough that callers don't notice a hang.
const sendReadinessWindow = 2 * time.Second

// isSpawnTransient reports whether the session is in a "still booting up,
// almost-ready" state where a short readiness wait is justified. Working /
// stuck / escalated sessions are explicitly NOT transient: blocking on
// those would defeat the purpose of the inbox queue.
func isSpawnTransient(sess *session.Session) bool {
	if sess == nil {
		return false
	}
	switch sess.Snapshot().State {
	case session.StateStarting, session.StateHandshaking:
		return true
	default:
		return false
	}
}

// waitForSessionReady polls until sessionReady(sess) is true or window
// elapses. Does not block forever — once window expires we return and
// the caller falls through to the queue path. Cheap busy-wait with a
// small tick; daemon state transitions are coarse (every state change
// goes through SetState in the same process), so a 25ms tick is plenty.
func waitForSessionReady(ctx context.Context, sess *session.Session, window time.Duration) {
	if sessionReady(sess) {
		return
	}
	deadline := time.Now().Add(window)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if sessionReady(sess) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				return
			}
		}
	}
}

// isSessionInboxMissing recognizes the wrapped sentinel from
// sessionInboxBucket. errors.Is handles both the bare sentinel and
// fmt.Errorf("%w: …") wraps.
func isSessionInboxMissing(err error) bool {
	return errors.Is(err, store.ErrSessionInboxMissing)
}

// extractIDs digs owner_id / session_id out of an AuditEntry.Detail map.
// C1 stashes these under Detail (no schema bump); when AuditEntry grows
// real columns this helper goes away.
func extractIDs(e store.AuditEntry) (owner, session string) {
	if e.Detail == nil {
		return "", ""
	}
	if v, ok := e.Detail["owner_id"]; ok {
		owner = formatDetailValue(v)
	}
	if v, ok := e.Detail["session_id"]; ok {
		session = formatDetailValue(v)
	}
	return owner, session
}

// formatDetailValue stringifies a JSON-decoded any value so the C1
// audit RPC can return a flat map[string]string.
func formatDetailValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}
