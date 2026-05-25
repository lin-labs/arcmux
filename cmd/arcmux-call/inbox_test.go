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
