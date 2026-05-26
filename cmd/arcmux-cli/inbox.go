package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// cmdInbox dispatches `arcmux-cli inbox <sub>`.
//
// Post-F11 the CLI no longer opens state.bolt directly. The daemon owns
// the bbolt write lock for its uptime, so the inbox subcommands now
// route through gRPC: push -> Send, peek -> PeekInbox, ack -> AckInbox.
//
// The C1 gRPC Send is queueable: if the named session is ready (idle /
// safely interruptible), it delivers synchronously and returns
// delivered=true; otherwise it queues onto the per-session inbox and
// returns queued=true. This subsumes the old "always-queue" inbox push.
func cmdInbox(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-cli inbox push|peek|ack [flags]")
	}
	switch args[0] {
	case "push":
		return cmdInboxPush(args[1:], stdin, stdout)
	case "peek":
		return cmdInboxPeek(args[1:], stdout)
	case "ack":
		return cmdInboxAck(args[1:], stdout)
	default:
		return fmt.Errorf("unknown inbox subcommand %q (want push|peek|ack)", args[0])
	}
}

// sessionFlag wires --session / --to as synonyms. --session wins on tie.
type sessionFlag struct {
	session string
	to      string
}

func (sf *sessionFlag) attach(fs *flag.FlagSet) {
	fs.StringVar(&sf.session, "session", "", "target session name (required; also accepts --to)")
	fs.StringVar(&sf.to, "to", "", "alias for --session; target session name")
}

func (sf *sessionFlag) resolve() (string, error) {
	name := strings.TrimSpace(sf.session)
	if name == "" {
		name = strings.TrimSpace(sf.to)
	}
	if name == "" {
		return "", fmt.Errorf("--session (or --to) required")
	}
	return name, nil
}

// dialDaemon opens a Unix-socket gRPC client to the daemon.
func dialDaemon(sock string) (*grpc.ClientConn, arcmuxv1.AgentRuntimeClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", sock, err)
	}
	return conn, arcmuxv1.NewAgentRuntimeClient(conn), nil
}

func cmdInboxPush(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox push", flag.ContinueOnError)
	from := fs.String("from", "", "sender identifier (optional but recommended)")
	body := fs.String("body", "", "message body (if empty, reads from stdin)")
	// --force skips the daemon's readiness predicate and asks for direct
	// delivery via SendPrompt regardless of state. Escape hatch when the
	// readiness check mistargets (e.g. immediately after CreateSession
	// the agent is alive but the state machine still says StateStarting).
	force := fs.Bool("force", false, "skip readiness check; deliver directly even if state != idle")
	var sf sessionFlag
	sf.attach(fs)
	sock := fs.String("socket", socketPath(), "daemon socket path")
	// Accept-and-ignore legacy flags so old callers don't break in lockstep.
	// The daemon now owns project scope (per-daemon data_root), and the
	// gRPC Send contract doesn't expose verb/priority/refs/explicit-id.
	_ = fs.String("verb", "", "(deprecated; ignored, daemon doesn't surface verb in Send)")
	_ = fs.Int("priority", 0, "(deprecated; ignored)")
	_ = fs.String("id", "", "(deprecated; daemon generates msg_id)")
	_ = fs.String("refs", "", "(deprecated; ignored)")
	_ = fs.String("project", "", "(deprecated; ignored)")
	_ = fs.String("data-root", "", "(deprecated; ignored)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessionName, err := sf.resolve()
	if err != nil {
		return fmt.Errorf("inbox push: %w", err)
	}

	bodyStr := *body
	if bodyStr == "" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("inbox push: read body from stdin: %w", err)
		}
		bodyStr = string(b)
	}
	if strings.TrimSpace(bodyStr) == "" {
		return fmt.Errorf("inbox push: body required (--body or stdin)")
	}

	conn, c, err := dialDaemon(*sock)
	if err != nil {
		return fmt.Errorf("inbox push: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Send(ctx, &arcmuxv1.SendRequest{
		SessionName: sessionName,
		Body:        bodyStr,
		From:        *from,
		ForceDirect: *force,
	})
	if err != nil {
		return fmt.Errorf("inbox push: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":        true,
		"id":        resp.MsgId,
		"session":   sessionName,
		"delivered": resp.Delivered,
		"queued":    resp.Queued,
	})
}

func cmdInboxPeek(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox peek", flag.ContinueOnError)
	n := fs.Int("n", 20, "max messages (oldest-first)")
	var sf sessionFlag
	sf.attach(fs)
	sock := fs.String("socket", socketPath(), "daemon socket path")
	_ = fs.String("project", "", "(deprecated; ignored)")
	_ = fs.String("data-root", "", "(deprecated; ignored)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessionName, err := sf.resolve()
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}

	conn, c, err := dialDaemon(*sock)
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.PeekInbox(ctx, &arcmuxv1.PeekInboxRequest{
		SessionName: sessionName,
		N:           int32(*n),
	})
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}
	msgs := make([]map[string]any, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		msgs = append(msgs, map[string]any{
			"id":          m.Id,
			"body":        m.Body,
			"from":        m.From,
			"received_at": m.ReceivedAt,
		})
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"messages": msgs,
		"session":  sessionName,
	})
}

func cmdInboxAck(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox ack", flag.ContinueOnError)
	id := fs.String("id", "", "message id to ack (required)")
	var sf sessionFlag
	sf.attach(fs)
	sock := fs.String("socket", socketPath(), "daemon socket path")
	_ = fs.String("project", "", "(deprecated; ignored)")
	_ = fs.String("data-root", "", "(deprecated; ignored)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("inbox ack: --id required")
	}
	sessionName, err := sf.resolve()
	if err != nil {
		return fmt.Errorf("inbox ack: %w", err)
	}

	conn, c, err := dialDaemon(*sock)
	if err != nil {
		return fmt.Errorf("inbox ack: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.AckInbox(ctx, &arcmuxv1.AckInboxRequest{
		SessionName: sessionName,
		MsgId:       *id,
	})
	if err != nil {
		return fmt.Errorf("inbox ack: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":      resp.Acked,
		"acked":   resp.Acked,
		"id":      *id,
		"session": sessionName,
	})
}
