package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// cmdAudit dispatches `arcmux-cli audit <sub>`.
//
// Post-F11 the CLI no longer opens state.bolt directly. The daemon owns
// the bbolt write lock for its uptime, so any sibling reader would block
// on flock. Audit reads now route through the daemon's QueryAudit RPC.
//
// The `audit append` subcommand is intentionally gone: audit entries are
// daemon-side side effects of state changes (CreateSession, Send, ...).
// External callers should not be able to inject arbitrary audit rows.
func cmdAudit(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-cli audit recent [flags]")
	}
	switch args[0] {
	case "recent":
		return cmdAuditRecent(args[1:], stdout)
	default:
		return fmt.Errorf("unknown audit subcommand %q (want recent)", args[0])
	}
}

func cmdAuditRecent(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("audit recent", flag.ContinueOnError)
	n := fs.Int("n", 20, "max entries (newest-first)")
	since := fs.String("since", "", "RFC3339 lower bound (optional)")
	ownerID := fs.String("owner-id", "", "filter by owner_id (optional)")
	sessionID := fs.String("session-id", "", "filter by session_id (optional)")
	sock := fs.String("socket", socketPath(), "daemon socket path")
	// Kept for back-compat with older callers; ignored by the gRPC path.
	// These used to address the bbolt directly; the daemon now owns the
	// scope. We accept-and-discard rather than error so existing scripts
	// don't break in lockstep.
	_ = fs.String("project", "", "(deprecated; ignored, daemon owns scope)")
	_ = fs.String("data-root", "", "(deprecated; ignored, daemon owns scope)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := grpc.NewClient(
		"unix://"+*sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("audit recent: dial %s: %w", *sock, err)
	}
	defer conn.Close()
	c := arcmuxv1.NewAgentRuntimeClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.QueryAudit(ctx, &arcmuxv1.QueryAuditRequest{
		OwnerId:   *ownerID,
		SessionId: *sessionID,
		Since:     *since,
		Limit:     int32(*n),
	})
	if err != nil {
		return fmt.Errorf("audit recent: %w", err)
	}

	entries := make([]map[string]any, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, map[string]any{
			"timestamp":  e.Timestamp,
			"action":     e.Action,
			"actor":      e.Actor,
			"subject":    e.Subject,
			"owner_id":   e.OwnerId,
			"session_id": e.SessionId,
			"detail":     e.Detail,
		})
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"entries": entries})
}
