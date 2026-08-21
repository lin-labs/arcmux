package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"google.golang.org/grpc"
)

type attachStatusClient interface {
	Status(context.Context, *arcmuxv1.StatusRequest, ...grpc.CallOption) (*arcmuxv1.StatusResponse, error)
}

type attachRunner func(socket, session string, stdin io.Reader, stdout, stderr io.Writer) error

func tmuxAttachArgs(socket, session string) []string {
	return []string{"-L", socket, "attach-session", "-t", "=" + session}
}

func cmdAttach(ctx context.Context, c attachStatusClient, args []string, stdin io.Reader, stdout, stderr io.Writer, run attachRunner) error {
	if len(args) != 1 || args[0] == "" {
		return fmt.Errorf("attach: usage: arcmux-cli attach <session-id>")
	}

	resp, err := c.Status(ctx, &arcmuxv1.StatusRequest{SessionId: args[0]})
	if err != nil {
		return fmt.Errorf("attach: status %s: %w", args[0], err)
	}
	if resp.TmuxSession == "" || resp.TmuxSocket == "" {
		return fmt.Errorf("attach: session %s is not tmux-backed", args[0])
	}
	if err := run(resp.TmuxSocket, resp.TmuxSession, stdin, stdout, stderr); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	return nil
}

func runTmuxAttach(socket, session string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command("tmux", tmuxAttachArgs(socket, session)...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
