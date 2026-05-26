// cmd/arcmux-cli/main.go
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
	fmt.Fprintln(os.Stderr, "arcmux-cli:", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		die(fmt.Errorf("usage: arcmux-cli create|list|send|capture|status|kill|audit|inbox|ready [args]"))
	}
	cmd := os.Args[1]

	// Post-F11: audit/inbox/ready route through the daemon's gRPC. The
	// dispatch lives inside each subcommand (its own --socket flag, its
	// own dial helper) so it composes cleanly with the rest of the CLI.
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
	case "ready":
		if err := cmdReady(os.Args[2:], os.Stdout); err != nil {
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
			die(fmt.Errorf("send <session_id> (text on stdin) — run `arcmux-cli list` to see session ids"))
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
			die(fmt.Errorf("capture <session_id> — run `arcmux-cli list` to see session ids"))
		}
		r, err := c.Capture(ctx, &arcmuxv1.CaptureRequest{SessionId: os.Args[2], IncludeHistory: true})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"output": r.Output, "state": r.State, "idle_since": r.IdleSince})
	case "status":
		if len(os.Args) < 3 {
			// No session id supplied — fall through to listing everything.
			// This is the common discovery path; `arcmux-cli status` with no
			// args used to be a usage error, but listing is the natural answer.
			r, err := c.ListSessions(ctx, &arcmuxv1.ListSessionsRequest{})
			if err != nil {
				die(err)
			}
			out := make([]map[string]any, 0, len(r.Sessions))
			for _, s := range r.Sessions {
				out = append(out, map[string]any{
					"session_id": s.SessionId,
					"name":       s.SessionName,
					"agent":      s.Agent,
					"state":      s.State,
					"owner_id":   s.OwnerId,
				})
			}
			enc.Encode(map[string]any{"sessions": out, "count": len(out), "hint": "pass <session_id> for per-session detail"})
			return
		}
		r, err := c.Status(ctx, &arcmuxv1.StatusRequest{SessionId: os.Args[2]})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"state": r.State, "health": r.Health, "agent": r.Agent})
	case "list":
		// Optional --owner <id> filter; default lists all sessions.
		var ownerFilter string
		for i := 2; i+1 < len(os.Args); i += 2 {
			if os.Args[i] == "--owner" {
				ownerFilter = os.Args[i+1]
			}
		}
		r, err := c.ListSessions(ctx, &arcmuxv1.ListSessionsRequest{})
		if err != nil {
			die(err)
		}
		out := make([]map[string]any, 0, len(r.Sessions))
		for _, s := range r.Sessions {
			if ownerFilter != "" && s.OwnerId != ownerFilter {
				continue
			}
			out = append(out, map[string]any{
				"session_id":  s.SessionId,
				"name":        s.SessionName,
				"agent":       s.Agent,
				"state":       s.State,
				"owner_id":    s.OwnerId,
				"tmux_target": s.TmuxTarget,
				"cwd":         s.Cwd,
				"started_at":  s.StartedAt,
			})
		}
		enc.Encode(map[string]any{"sessions": out, "count": len(out)})
	case "kill":
		if len(os.Args) < 3 {
			die(fmt.Errorf("kill <session_id> — run `arcmux-cli list` to see session ids"))
		}
		r, err := c.Kill(ctx, &arcmuxv1.KillRequest{SessionId: os.Args[2]})
		if err != nil {
			die(err)
		}
		enc.Encode(map[string]any{"killed": r.Killed, "final_state": r.FinalState})
	default:
		die(fmt.Errorf("unknown subcommand %q", cmd))
	}
}
