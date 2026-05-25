package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestInboxPushPeekAck(t *testing.T) {
	dataRoot := t.TempDir()
	project := "inboxtest"

	// Push #1 (auto-id, no refs).
	var out bytes.Buffer
	args1 := []string{
		"--project", project, "--data-root", dataRoot,
		"--verb", "add", "--from", "user", "--priority", "1",
	}
	if err := cmdInboxPush(args1, strings.NewReader("do X"), &out); err != nil {
		t.Fatalf("push #1: %v", err)
	}
	var ack1 struct {
		OK         bool   `json:"ok"`
		ID         string `json:"id"`
		ReceivedAt string `json:"received_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack1); err != nil {
		t.Fatalf("decode ack #1: %v (raw: %q)", err, out.String())
	}
	if !ack1.OK || ack1.ID == "" || ack1.ReceivedAt == "" {
		t.Errorf("ack #1 unexpected: %+v", ack1)
	}

	// Push #2 (explicit id, refs JSON).
	out.Reset()
	args2 := []string{
		"--project", project, "--data-root", dataRoot,
		"--verb", "revise", "--from", "user", "--priority", "2",
		"--id", "explicit-2",
		"--refs", `{"ticket":"T-7","linked":["a","b"]}`,
	}
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
	if err := cmdInboxPeek([]string{"--project", project, "--data-root", dataRoot, "--n", "10"}, &out); err != nil {
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
	}
	if err := json.Unmarshal(out.Bytes(), &peek); err != nil {
		t.Fatalf("decode peek: %v (raw: %q)", err, out.String())
	}
	if len(peek.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(peek.Messages))
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
	if err := cmdInboxAck([]string{"--project", project, "--data-root", dataRoot, "--id", ack1.ID}, &out); err != nil {
		t.Fatalf("ack #1: %v", err)
	}
	var ackResp struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Bytes(), &ackResp); err != nil {
		t.Fatalf("decode ack resp: %v", err)
	}
	if !ackResp.OK || ackResp.ID != ack1.ID {
		t.Errorf("ack resp = %+v, want ok=true id=%s", ackResp, ack1.ID)
	}

	// Peek again — only #2 remains.
	out.Reset()
	if err := cmdInboxPeek([]string{"--project", project, "--data-root", dataRoot, "--n", "10"}, &out); err != nil {
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
	if err := cmdInboxAck([]string{"--project", project, "--data-root", dataRoot, "--id", "explicit-2"}, &out); err != nil {
		t.Fatalf("ack #2: %v", err)
	}

	// Peek-empty.
	out.Reset()
	if err := cmdInboxPeek([]string{"--project", project, "--data-root", dataRoot}, &out); err != nil {
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

func TestInboxPushRejectsBadInput(t *testing.T) {
	// Strip ambient env so missing-project case actually triggers.
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		args []string
		body string
	}{
		{"missing verb", []string{"--project", "p", "--data-root", t.TempDir(), "--from", "u"}, "x"},
		{"missing from", []string{"--project", "p", "--data-root", t.TempDir(), "--verb", "add"}, "x"},
		{"missing project", []string{"--data-root", t.TempDir(), "--verb", "add", "--from", "u"}, "x"},
		{"bad project slug", []string{"--project", "../etc", "--data-root", t.TempDir(), "--verb", "add", "--from", "u"}, "x"},
		{"bad refs json", []string{"--project", "p", "--data-root", t.TempDir(), "--verb", "add", "--from", "u", "--refs", "not-json"}, "x"},
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

func TestInboxAckMissingID(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ackmiss"

	// Push one so the bucket exists.
	var out bytes.Buffer
	if err := cmdInboxPush(
		[]string{"--project", project, "--data-root", dataRoot, "--verb", "x", "--from", "u", "--id", "real"},
		strings.NewReader("body"),
		&out,
	); err != nil {
		t.Fatalf("seed push: %v", err)
	}

	out.Reset()
	err := cmdInboxAck([]string{"--project", project, "--data-root", dataRoot, "--id", "nope"}, &out)
	if err == nil {
		t.Fatalf("expected not-found error, got nil; stdout=%q", out.String())
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want substring 'not found'", err)
	}
}

func TestInboxAckRejectsMissingFields(t *testing.T) {
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		args []string
	}{
		{"missing id", []string{"--project", "p", "--data-root", t.TempDir()}},
		{"missing project", []string{"--data-root", t.TempDir(), "--id", "x"}},
		{"bad project slug", []string{"--project", "../etc", "--data-root", t.TempDir(), "--id", "x"}},
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

func TestInboxPeekEmptyFreshProject(t *testing.T) {
	dataRoot := t.TempDir()
	var out bytes.Buffer
	if err := cmdInboxPeek([]string{"--project", "freshinbox", "--data-root", dataRoot, "--n", "5"}, &out); err != nil {
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

func TestParseQueue(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantSlug string
		wantErr  bool
	}{
		{"", "elon", "", false},
		{"elon", "elon", "", false},
		{"  elon  ", "elon", "", false},
		{"manager:team-a", "manager", "team-a", false},
		{"manager:auth-refactor", "manager", "auth-refactor", false},
		{"manager:", "", "", true},
		{"manager:../etc", "", "", true},
		{"manager:has/slash", "", "", true},
		{"unknown", "", "", true},
		{"ic:0", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			q, err := parseQueue(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (%+v)", q)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if q.Kind != tc.wantKind || q.Slug != tc.wantSlug {
				t.Errorf("parseQueue(%q) = %+v, want kind=%s slug=%s", tc.in, q, tc.wantKind, tc.wantSlug)
			}
		})
	}
}

// TestInboxManagerQueueLifecycle exercises the full push/peek/ack roundtrip
// against a manager queue. The manager bucket has to be created out-of-band
// because the CLI alone has no spawn primitive in this test scope; we
// reach into the store API directly to mimic what teamspawn.Spawn does.
func TestInboxManagerQueueLifecycle(t *testing.T) {
	dataRoot := t.TempDir()
	project := "mgrqueue"

	// Pre-create the manager inbox bucket. In production this happens
	// inside teamspawn.Spawn; here we use openProjectDB directly so the
	// CLI test stays focused on the --to flag plumbing.
	db, _, err := openProjectDB(dataRoot, project)
	if err != nil {
		t.Fatalf("openProjectDB: %v", err)
	}
	if err := db.EnsureManagerInbox("team-a"); err != nil {
		_ = db.Close()
		t.Fatalf("EnsureManagerInbox: %v", err)
	}
	_ = db.Close()

	// Push to manager:team-a.
	var out bytes.Buffer
	pushArgs := []string{
		"--project", project, "--data-root", dataRoot,
		"--to", "manager:team-a",
		"--verb", "add", "--from", "elon", "--id", "mgr-1",
	}
	if err := cmdInboxPush(pushArgs, strings.NewReader("plan auth refactor"), &out); err != nil {
		t.Fatalf("push: %v", err)
	}
	var pushAck struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
		To string `json:"to"`
	}
	if err := json.Unmarshal(out.Bytes(), &pushAck); err != nil {
		t.Fatalf("decode push: %v (raw: %q)", err, out.String())
	}
	if !pushAck.OK || pushAck.ID != "mgr-1" || pushAck.To != "manager:team-a" {
		t.Errorf("push ack = %+v, want ok=true id=mgr-1 to=manager:team-a", pushAck)
	}

	// Peek elon queue stays empty.
	out.Reset()
	if err := cmdInboxPeek([]string{"--project", project, "--data-root", dataRoot}, &out); err != nil {
		t.Fatalf("peek elon: %v", err)
	}
	var elonPeek struct {
		Messages []map[string]any `json:"messages"`
		To       string           `json:"to"`
	}
	if err := json.Unmarshal(out.Bytes(), &elonPeek); err != nil {
		t.Fatalf("decode elon peek: %v", err)
	}
	if len(elonPeek.Messages) != 0 {
		t.Errorf("elon queue not empty (cross-queue leak): %+v", elonPeek.Messages)
	}
	if elonPeek.To != "elon" {
		t.Errorf("elon peek 'to' = %q, want elon", elonPeek.To)
	}

	// Peek manager queue finds the message.
	out.Reset()
	if err := cmdInboxPeek([]string{
		"--project", project, "--data-root", dataRoot, "--to", "manager:team-a",
	}, &out); err != nil {
		t.Fatalf("peek manager: %v", err)
	}
	var mgrPeek struct {
		Messages []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
			From string `json:"from"`
		} `json:"messages"`
		To string `json:"to"`
	}
	if err := json.Unmarshal(out.Bytes(), &mgrPeek); err != nil {
		t.Fatalf("decode mgr peek: %v", err)
	}
	if len(mgrPeek.Messages) != 1 || mgrPeek.Messages[0].ID != "mgr-1" || mgrPeek.Messages[0].Body != "plan auth refactor" {
		t.Errorf("manager peek = %+v, want one mgr-1 message", mgrPeek.Messages)
	}
	if mgrPeek.To != "manager:team-a" {
		t.Errorf("manager peek 'to' = %q, want manager:team-a", mgrPeek.To)
	}

	// Ack manager queue message.
	out.Reset()
	if err := cmdInboxAck([]string{
		"--project", project, "--data-root", dataRoot,
		"--to", "manager:team-a", "--id", "mgr-1",
	}, &out); err != nil {
		t.Fatalf("ack manager: %v", err)
	}

	// Peek manager queue is empty again.
	out.Reset()
	if err := cmdInboxPeek([]string{
		"--project", project, "--data-root", dataRoot, "--to", "manager:team-a",
	}, &out); err != nil {
		t.Fatalf("peek manager after ack: %v", err)
	}
	mgrPeek.Messages = nil
	if err := json.Unmarshal(out.Bytes(), &mgrPeek); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mgrPeek.Messages) != 0 {
		t.Errorf("manager queue not empty after ack: %+v", mgrPeek.Messages)
	}
}

// TestInboxManagerPushUnspawnedTeam confirms the CLI surfaces a clear
// "spawn it first" error when pushing to a team without an inbox.
func TestInboxManagerPushUnspawnedTeam(t *testing.T) {
	dataRoot := t.TempDir()
	project := "unspawned"
	var out bytes.Buffer
	err := cmdInboxPush(
		[]string{
			"--project", project, "--data-root", dataRoot,
			"--to", "manager:ghost",
			"--verb", "add", "--from", "elon",
		},
		strings.NewReader("orphan"),
		&out,
	)
	if err == nil {
		t.Fatalf("want error pushing to unspawned team, got nil; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), "no inbox") || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("err = %q, want substring about missing inbox and team slug", err)
	}
}

// TestInboxManagerPeekUnspawnedReturnsEmpty proves the polling-friendly
// behavior: peeking a never-spawned team's inbox returns {"messages":[]}
// instead of erroring, so cron-like scripts can race with spawn.
func TestInboxManagerPeekUnspawnedReturnsEmpty(t *testing.T) {
	dataRoot := t.TempDir()
	project := "racey"
	var out bytes.Buffer
	if err := cmdInboxPeek([]string{
		"--project", project, "--data-root", dataRoot, "--to", "manager:ghost",
	}, &out); err != nil {
		t.Fatalf("peek: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
		To       string           `json:"to"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (raw: %q)", err, out.String())
	}
	if len(got.Messages) != 0 {
		t.Errorf("expected empty, got %d", len(got.Messages))
	}
	if got.To != "manager:ghost" {
		t.Errorf("to = %q, want manager:ghost", got.To)
	}
}

func TestInboxBadQueueRejected(t *testing.T) {
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")
	dataRoot := t.TempDir()
	project := "qbad"

	cases := []struct {
		name string
		args []string
	}{
		{"push bad queue", []string{"--project", project, "--data-root", dataRoot,
			"--verb", "add", "--from", "u", "--to", "nope"}},
		{"push bad slug", []string{"--project", project, "--data-root", dataRoot,
			"--verb", "add", "--from", "u", "--to", "manager:../evil"}},
		{"peek bad queue", []string{"--project", project, "--data-root", dataRoot, "--to", "ic:0"}},
		{"ack bad queue", []string{"--project", project, "--data-root", dataRoot, "--id", "x", "--to", "nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var err error
			switch {
			case strings.HasPrefix(tc.name, "push"):
				err = cmdInboxPush(tc.args, strings.NewReader("body"), &out)
			case strings.HasPrefix(tc.name, "peek"):
				err = cmdInboxPeek(tc.args, &out)
			case strings.HasPrefix(tc.name, "ack"):
				err = cmdInboxAck(tc.args, &out)
			}
			if err == nil {
				t.Fatalf("expected error, got nil; out=%q", out.String())
			}
		})
	}
}
