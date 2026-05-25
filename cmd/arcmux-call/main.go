// cmd/arcmux-call/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func socketPath() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "arcmux", "arcmux.sock")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "arcmux-call:", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		die(fmt.Errorf("usage: arcmux-call create|send|capture|status|audit|inbox|scratchpad|team|contract [args]"))
	}
	cmd := os.Args[1]

	// State-substrate subcommands open state.bolt directly; no daemon required.
	switch cmd {
	case "audit":
		if err := cmdAudit(os.Args[2:], os.Stdout); err != nil {
			die(err)
		}
		return
	case "inbox":
		if err := cmdInbox(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			die(err)
		}
		return
	case "scratchpad":
		if err := cmdScratchpad(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			die(err)
		}
		return
	case "team":
		if err := cmdTeam(os.Args[2:], os.Stdout); err != nil {
			die(err)
		}
		return
	case "contract":
		if err := cmdContract(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			die(err)
		}
		return
	}

	// Daemon-mediated subcommands.
	conn, err := grpc.NewClient(
		"unix://"+socketPath(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		die(err)
	}
	defer conn.Close()
	c := arcmuxv1.NewAgentRuntimeClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	enc := json.NewEncoder(os.Stdout)

	switch cmd {
	case "create":
		h, _ := os.UserHomeDir()
		fs := map[string]string{"--agent": "codex", "--name": "gmail-codex", "--cwd": h}
		for i := 2; i+1 < len(os.Args); i += 2 {
			fs[os.Args[i]] = os.Args[i+1]
		}
		r, err := c.CreateSession(ctx, &arcmuxv1.CreateSessionRequest{
			Agent: fs["--agent"], Cwd: fs["--cwd"], SessionName: fs["--name"],
		})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"session_id": r.SessionId, "state": r.State, "pid": r.Pid})
	case "send":
		if len(os.Args) < 3 {
			die(fmt.Errorf("send <session_id> (text on stdin)"))
		}
		b, _ := io.ReadAll(os.Stdin)
		r, err := c.SendPrompt(ctx, &arcmuxv1.SendPromptRequest{
			SessionId: os.Args[2], Text: string(b), ConfirmDelivery: true, WaitIdle: true,
		})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"delivered": r.Delivered, "state": r.State})
	case "capture":
		if len(os.Args) < 3 {
			die(fmt.Errorf("capture <session_id>"))
		}
		r, err := c.Capture(ctx, &arcmuxv1.CaptureRequest{SessionId: os.Args[2], IncludeHistory: true})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"output": r.Output, "state": r.State, "idle_since": r.IdleSince})
	case "status":
		if len(os.Args) < 3 {
			die(fmt.Errorf("status <session_id>"))
		}
		r, err := c.Status(ctx, &arcmuxv1.StatusRequest{SessionId: os.Args[2]})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"state": r.State, "health": r.Health, "agent": r.Agent})
	default:
		die(fmt.Errorf("unknown subcommand %q", cmd))
	}
}
