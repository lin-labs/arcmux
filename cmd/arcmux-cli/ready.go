package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
)

// cmdReady implements `arcmux-cli ready --session NAME [--socket PATH]`.
//
// It calls the daemon's Ready RPC and emits a JSON object:
//
//	{
//	  "ready": bool,
//	  "reason": string,         // "ready:idle", "not-ready:working",
//	                            // "no-such-session", ...
//	  "state": string,          // raw session-state label
//	  "last_signal_at": string, // RFC3339
//	  "session": string         // echoed for ergonomics
//	}
//
// Designed for orchestrators (elonco, pulse loops, polling scripts) that
// want to decide between "send now" and "queue" without having to
// speculatively call Send and then interpret the queued/delivered split.
func cmdReady(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	var sf sessionFlag
	sf.attach(fs)
	sock := fs.String("socket", socketPath(), "daemon socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessionName, err := sf.resolve()
	if err != nil {
		return fmt.Errorf("ready: %w", err)
	}

	conn, c, err := dialDaemon(*sock)
	if err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Ready(ctx, &arcmuxv1.ReadyRequest{SessionName: sessionName})
	if err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ready":          resp.Ready,
		"reason":         resp.Reason,
		"state":          resp.State,
		"last_signal_at": resp.LastSignalAt,
		"session":        sessionName,
	})
}
