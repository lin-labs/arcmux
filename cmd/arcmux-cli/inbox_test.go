package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestInboxPushPeekAck exercises the full CLI roundtrip against a single
// session inbox: push two messages, peek (oldest-first), ack one, peek
// again, ack last, peek empty. The post-C3 CLI addresses queues uniformly
// by session name via --session.
func TestInboxPushPeekAck(t *testing.T) {
	dataRoot := t.TempDir()
	project := "inboxtest"
	session := "front-desk"

	common := []string{"--project", project, "--data-root", dataRoot, "--session", session}

	// Push #1 (auto-id, no refs).
	var out bytes.Buffer
	args1 := append([]string{}, common...)
	args1 = append(args1, "--verb", "add", "--from", "user", "--priority", "1")
	if err := cmdInboxPush(args1, strings.NewReader("do X"), &out); err != nil {
		t.Fatalf("push #1: %v", err)
	}
	var ack1 struct {
		OK         bool   `json:"ok"`
		ID         string `json:"id"`
		Session    string `json:"session"`
		ReceivedAt string `json:"received_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack1); err != nil {
		t.Fatalf("decode ack #1: %v (raw: %q)", err, out.String())
	}
	if !ack1.OK || ack1.ID == "" || ack1.ReceivedAt == "" || ack1.Session != session {
		t.Errorf("ack #1 unexpected: %+v", ack1)
	}

	// Push #2 (explicit id, refs JSON).
	out.Reset()
	args2 := append([]string{}, common...)
	args2 = append(args2,
		"--verb", "revise", "--from", "user", "--priority", "2",
		"--id", "explicit-2",
		"--refs", `{"ticket":"T-7","linked":["a","b"]}`,
	)
	if err := cmdInboxPush(args2, strings.NewReader("actually Y"), &out); err != nil {
		t.Fatalf("push #2: %v", err)
	}
	var ack2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack2); err != nil {
		t.Fatalf("decode ack #2: %v", err)
	}
	if ack2.ID != "explicit-2" {
		t.Errorf("ack #2 id = %q, want explicit-2", ack2.ID)
	}

	// Peek (oldest-first: #1 then #2).
	out.Reset()
	peekArgs := append([]string{}, common...)
	peekArgs = append(peekArgs, "--n", "10")
	if err := cmdInboxPeek(peekArgs, &out); err != nil {
		t.Fatalf("peek: %v", err)
	}
	var peek struct {
		Messages []struct {
			ID       string         `json:"id"`
			Verb     string         `json:"verb"`
			From     string         `json:"from"`
			Priority int            `json:"priority"`
			Body     string         `json:"body"`
			Refs     map[string]any `json:"refs"`
		} `json:"messages"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(out.Bytes(), &peek); err != nil {
		t.Fatalf("decode peek: %v (raw: %q)", err, out.String())
	}
	if len(peek.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(peek.Messages))
	}
	if peek.Session != session {
		t.Errorf("peek.session = %q, want %q", peek.Session, session)
	}
	if peek.Messages[0].ID != ack1.ID {
		t.Errorf("peek[0].ID = %q, want %q (oldest-first)", peek.Messages[0].ID, ack1.ID)
	}
	if peek.Messages[0].Body != "do X" {
		t.Errorf("peek[0].Body = %q, want %q", peek.Messages[0].Body, "do X")
	}
	if peek.Messages[1].ID != "explicit-2" {
		t.Errorf("peek[1].ID = %q, want explicit-2", peek.Messages[1].ID)
	}
	if got, want := peek.Messages[1].Refs["ticket"], "T-7"; got != want {
		t.Errorf("peek[1].Refs.ticket = %v, want %v", got, want)
	}
	if peek.Messages[1].Priority != 2 {
		t.Errorf("peek[1].Priority = %d, want 2", peek.Messages[1].Priority)
	}

	// Ack the older one.
	out.Reset()
	ackArgs := append([]string{}, common...)
	ackArgs = append(ackArgs, "--id", ack1.ID)
	if err := cmdInboxAck(ackArgs, &out); err != nil {
		t.Fatalf("ack #1: %v", err)
	}
	var ackResp struct {
		OK      bool   `json:"ok"`
		ID      string `json:"id"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(out.Bytes(), &ackResp); err != nil {
		t.Fatalf("decode ack resp: %v", err)
	}
	if !ackResp.OK || ackResp.ID != ack1.ID {
		t.Errorf("ack resp = %+v, want ok=true id=%s", ackResp, ack1.ID)
	}

	// Peek again — only #2 remains.
	out.Reset()
	if err := cmdInboxPeek(peekArgs, &out); err != nil {
		t.Fatalf("peek after ack: %v", err)
	}
	peek.Messages = peek.Messages[:0]
	if err := json.Unmarshal(out.Bytes(), &peek); err != nil {
		t.Fatalf("decode peek after ack: %v", err)
	}
	if len(peek.Messages) != 1 || peek.Messages[0].ID != "explicit-2" {
		t.Errorf("after ack peek = %+v, want [explicit-2]", peek.Messages)
	}

	// Ack last.
	out.Reset()
	ackArgs2 := append([]string{}, common...)
	ackArgs2 = append(ackArgs2, "--id", "explicit-2")
	if err := cmdInboxAck(ackArgs2, &out); err != nil {
		t.Fatalf("ack #2: %v", err)
	}

	// Peek-empty.
	out.Reset()
	if err := cmdInboxPeek(peekArgs, &out); err != nil {
		t.Fatalf("peek empty: %v", err)
	}
	var empty struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out.Bytes(), &empty); err != nil {
		t.Fatalf("decode peek empty: %v", err)
	}
	if len(empty.Messages) != 0 {
		t.Errorf("expected empty messages, got %d", len(empty.Messages))
	}
}

// TestInboxToFlagAlias verifies --to is accepted as a synonym for
// --session, preserving callers that memorized the pre-C3 spelling.
func TestInboxToFlagAlias(t *testing.T) {
	dataRoot := t.TempDir()
	project := "aliastest"

	var out bytes.Buffer
	pushArgs := []string{
		"--project", project, "--data-root", dataRoot,
		"--to", "via-to",
		"--verb", "add", "--from", "user", "--id", "m1",
	}
	if err := cmdInboxPush(pushArgs, strings.NewReader("hello"), &out); err != nil {
		t.Fatalf("push: %v", err)
	}
	var ack struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ack.Session != "via-to" {
		t.Errorf("ack session = %q, want via-to", ack.Session)
	}
}

func TestInboxPushRejectsBadInput(t *testing.T) {
	// Strip ambient env so missing-project case actually triggers.
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		args []string
		body string
	}{
		{"missing verb", []string{"--project", "p", "--data-root", t.TempDir(), "--session", "s", "--from", "u"}, "x"},
		{"missing from", []string{"--project", "p", "--data-root", t.TempDir(), "--session", "s", "--verb", "add"}, "x"},
		{"missing project", []string{"--data-root", t.TempDir(), "--session", "s", "--verb", "add", "--from", "u"}, "x"},
		{"missing session", []string{"--project", "p", "--data-root", t.TempDir(), "--verb", "add", "--from", "u"}, "x"},
		{"bad project slug", []string{"--project", "../etc", "--data-root", t.TempDir(), "--session", "s", "--verb", "add", "--from", "u"}, "x"},
		{"bad session slug", []string{"--project", "p", "--data-root", t.TempDir(), "--session", "../evil", "--verb", "add", "--from", "u"}, "x"},
		{"bad refs json", []string{"--project", "p", "--data-root", t.TempDir(), "--session", "s", "--verb", "add", "--from", "u", "--refs", "not-json"}, "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdInboxPush(tc.args, strings.NewReader(tc.body), &out); err == nil {
				t.Fatalf("expected error, got nil; stdout=%q", out.String())
			}
		})
	}
}

// TestInboxAckMissingID: acking a message that isn't in the queue is
// idempotent (returns ok), matching AckSessionInbox semantics.
func TestInboxAckMissingID(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ackmiss"
	session := "s1"

	// Push one so the bucket exists.
	var out bytes.Buffer
	if err := cmdInboxPush(
		[]string{"--project", project, "--data-root", dataRoot, "--session", session, "--verb", "x", "--from", "u", "--id", "real"},
		strings.NewReader("body"),
		&out,
	); err != nil {
		t.Fatalf("seed push: %v", err)
	}

	out.Reset()
	if err := cmdInboxAck([]string{"--project", project, "--data-root", dataRoot, "--session", session, "--id", "nope"}, &out); err != nil {
		t.Fatalf("ack of missing id should be idempotent ok; got err=%v", err)
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (raw=%q)", err, out.String())
	}
	if !resp.OK {
		t.Errorf("ack of missing id: ok=%v, want true (idempotent)", resp.OK)
	}
}

// TestInboxAckUnknownSession: acking against a session that was never
// pushed to is loud — surfaces ErrSessionInboxMissing.
func TestInboxAckUnknownSession(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ackunknown"
	var out bytes.Buffer
	err := cmdInboxAck(
		[]string{"--project", project, "--data-root", dataRoot, "--session", "ghost", "--id", "anything"},
		&out,
	)
	if err == nil {
		t.Fatalf("ack against unknown session: want error, got nil (out=%q)", out.String())
	}
	if !strings.Contains(err.Error(), "no inbox") {
		t.Errorf("err = %q, want substring 'no inbox'", err)
	}
}

func TestInboxAckRejectsMissingFields(t *testing.T) {
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		args []string
	}{
		{"missing id", []string{"--project", "p", "--data-root", t.TempDir(), "--session", "s"}},
		{"missing project", []string{"--data-root", t.TempDir(), "--session", "s", "--id", "x"}},
		{"missing session", []string{"--project", "p", "--data-root", t.TempDir(), "--id", "x"}},
		{"bad project slug", []string{"--project", "../etc", "--data-root", t.TempDir(), "--session", "s", "--id", "x"}},
		{"bad session slug", []string{"--project", "p", "--data-root", t.TempDir(), "--session", "../evil", "--id", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdInboxAck(tc.args, &out); err == nil {
				t.Fatalf("expected error, got nil; stdout=%q", out.String())
			}
		})
	}
}

// TestInboxPeekEmptyFreshSession: peeking a session that was never pushed
// to returns an empty list rather than an error — the polling-friendly
// contract that cron-style scripts depend on.
func TestInboxPeekEmptyFreshSession(t *testing.T) {
	dataRoot := t.TempDir()
	var out bytes.Buffer
	if err := cmdInboxPeek([]string{"--project", "freshinbox", "--data-root", dataRoot, "--session", "ghost", "--n", "5"}, &out); err != nil {
		t.Fatalf("peek fresh: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (raw: %q)", err, out.String())
	}
	if len(got.Messages) != 0 {
		t.Errorf("expected empty, got %d", len(got.Messages))
	}
}

// TestInboxIsolation: two sessions on the same project keep their queues
// separate. The C3 demolition collapsed the elon/manager/ic kinds into a
// uniform per-session bucket; this test pins that the per-name namespace
// still isolates.
func TestInboxIsolation(t *testing.T) {
	dataRoot := t.TempDir()
	project := "isol"

	for _, s := range []string{"a", "b"} {
		var out bytes.Buffer
		if err := cmdInboxPush(
			[]string{"--project", project, "--data-root", dataRoot, "--session", s, "--verb", "add", "--from", "u", "--id", "msg-" + s},
			strings.NewReader("for "+s),
			&out,
		); err != nil {
			t.Fatalf("push %s: %v", s, err)
		}
	}

	for _, s := range []string{"a", "b"} {
		var out bytes.Buffer
		if err := cmdInboxPeek([]string{"--project", project, "--data-root", dataRoot, "--session", s}, &out); err != nil {
			t.Fatalf("peek %s: %v", s, err)
		}
		var got struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(out.Bytes(), &got)
		if len(got.Messages) != 1 || got.Messages[0].ID != "msg-"+s {
			t.Errorf("session %q peek = %+v, want exactly [msg-%s]", s, got.Messages, s)
		}
	}
}
