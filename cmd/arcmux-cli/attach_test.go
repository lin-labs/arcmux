package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"google.golang.org/grpc"
)

type attachStatusClientStub struct {
	response *arcmuxv1.StatusResponse
	err      error
	request  *arcmuxv1.StatusRequest
}

func (s *attachStatusClientStub) Status(_ context.Context, req *arcmuxv1.StatusRequest, _ ...grpc.CallOption) (*arcmuxv1.StatusResponse, error) {
	s.request = req
	return s.response, s.err
}

func TestCmdAttachUsesExactDaemonTmuxMetadata(t *testing.T) {
	client := &attachStatusClientStub{response: &arcmuxv1.StatusResponse{
		TmuxSocket:  "custom-arcmux",
		TmuxSession: "blin-agent:with-colon",
	}}
	var gotSocket, gotSession string
	run := func(socket, session string, _ io.Reader, _, _ io.Writer) error {
		gotSocket, gotSession = socket, session
		return nil
	}

	err := cmdAttach(context.Background(), client, []string{"s-123"}, strings.NewReader(""), io.Discard, io.Discard, run)
	if err != nil {
		t.Fatalf("cmdAttach: %v", err)
	}
	if client.request == nil || client.request.SessionId != "s-123" {
		t.Fatalf("status request = %+v, want session s-123", client.request)
	}
	if gotSocket != "custom-arcmux" || gotSession != "blin-agent:with-colon" {
		t.Fatalf("attach target = %q/%q, want custom-arcmux/blin-agent:with-colon", gotSocket, gotSession)
	}
}

func TestTmuxAttachArgsUseExactSessionTarget(t *testing.T) {
	got := tmuxAttachArgs("custom-arcmux", "blin-agent:with-colon")
	want := []string{"-L", "custom-arcmux", "attach-session", "-t", "=blin-agent:with-colon"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("tmux args = %q, want %q", got, want)
	}
}

func TestCmdAttachRejectsNonTmuxSession(t *testing.T) {
	client := &attachStatusClientStub{response: &arcmuxv1.StatusResponse{}}
	runCalled := false
	run := func(string, string, io.Reader, io.Writer, io.Writer) error {
		runCalled = true
		return nil
	}

	err := cmdAttach(context.Background(), client, []string{"s-exec"}, strings.NewReader(""), io.Discard, io.Discard, run)
	if err == nil || !strings.Contains(err.Error(), "not tmux-backed") {
		t.Fatalf("error = %v, want non-tmux error", err)
	}
	if runCalled {
		t.Fatal("tmux runner called for non-tmux session")
	}
}

func TestCmdAttachPropagatesStatusError(t *testing.T) {
	want := errors.New("session not found")
	client := &attachStatusClientStub{err: want}

	err := cmdAttach(context.Background(), client, []string{"s-missing"}, strings.NewReader(""), io.Discard, io.Discard, nil)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped status error", err)
	}
}

func TestCmdAttachRequiresOneSessionID(t *testing.T) {
	client := &attachStatusClientStub{}
	for _, args := range [][]string{nil, {}, {"s-1", "s-2"}} {
		err := cmdAttach(context.Background(), client, args, strings.NewReader(""), io.Discard, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("args %v: error = %v, want usage error", args, err)
		}
	}
}
