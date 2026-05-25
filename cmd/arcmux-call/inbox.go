package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// cmdInbox dispatches `arcmux-call inbox <sub>`.
func cmdInbox(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-call inbox push|peek|ack [flags]")
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

func cmdInboxPush(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox push", flag.ContinueOnError)
	verb := fs.String("verb", "", "message verb (required)")
	from := fs.String("from", "", "sender identifier (required)")
	priority := fs.Int("priority", 0, "priority (higher = more urgent)")
	id := fs.String("id", "", "explicit message id (auto-generated when empty)")
	refsRaw := fs.String("refs", "", "refs JSON object (optional)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *verb == "" || *from == "" {
		return fmt.Errorf("inbox push: --verb and --from required")
	}
	if *project == "" {
		return fmt.Errorf("inbox push: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	var refs map[string]any
	if *refsRaw != "" {
		if err := json.Unmarshal([]byte(*refsRaw), &refs); err != nil {
			return fmt.Errorf("inbox push: --refs must be JSON object: %w", err)
		}
	}

	body, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("inbox push: read body from stdin: %w", err)
	}

	msgID := *id
	if msgID == "" {
		msgID, err = store.NewInboxID()
		if err != nil {
			return fmt.Errorf("inbox push: generate id: %w", err)
		}
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	m := store.InboxMsg{
		ID:         msgID,
		Verb:       *verb,
		From:       *from,
		Priority:   *priority,
		Body:       string(body),
		Refs:       refs,
		ReceivedAt: time.Now(),
	}
	if err := db.PushElonInbox(m); err != nil {
		return fmt.Errorf("inbox push: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":          true,
		"id":          m.ID,
		"received_at": m.ReceivedAt.Format(time.RFC3339Nano),
	})
}

func cmdInboxPeek(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox peek", flag.ContinueOnError)
	n := fs.Int("n", 20, "max messages (oldest-first)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("inbox peek: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	msgs, err := db.PeekElonInbox(*n)
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}
	if msgs == nil {
		msgs = []store.InboxMsg{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"messages": msgs})
}

func cmdInboxAck(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox ack", flag.ContinueOnError)
	id := fs.String("id", "", "message id to ack (required)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("inbox ack: --id required")
	}
	if *project == "" {
		return fmt.Errorf("inbox ack: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.AckElonInbox(*id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("inbox ack: id %q not found", *id)
		}
		return fmt.Errorf("inbox ack: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"ok": true, "id": *id})
}

