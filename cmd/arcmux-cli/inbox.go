package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// queueRef identifies an inbox queue. Kind is "elon", "manager", or "ic";
// Slug is the team slug for manager queues, the slot id for IC queues,
// and empty for elon.
type queueRef struct {
	Kind string
	Slug string
}

// parseQueue parses a --to value:
//   - "elon" → {Kind:"elon"}
//   - "manager:<slug>" → {Kind:"manager", Slug:<slug>} (slug validated)
//   - "ic:<slot-id>" → {Kind:"ic", Slug:<slot-id>} (slug validated)
//
// Empty input returns {Kind:"elon"} so legacy callers (no --to) keep
// behaving. The three queue kinds are intentionally addressed through the
// same verb so callers learn one mental model rather than three.
func parseQueue(s string) (queueRef, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "elon" {
		return queueRef{Kind: "elon"}, nil
	}
	if strings.HasPrefix(s, "manager:") {
		slug := strings.TrimPrefix(s, "manager:")
		if _, err := paths.Validate(slug); err != nil {
			return queueRef{}, fmt.Errorf("--to manager:<slug>: %w", err)
		}
		return queueRef{Kind: "manager", Slug: slug}, nil
	}
	if strings.HasPrefix(s, "ic:") {
		slug := strings.TrimPrefix(s, "ic:")
		if _, err := paths.Validate(slug); err != nil {
			return queueRef{}, fmt.Errorf("--to ic:<slot-id>: %w", err)
		}
		return queueRef{Kind: "ic", Slug: slug}, nil
	}
	return queueRef{}, fmt.Errorf("--to: unknown queue %q (want 'elon', 'manager:<slug>', or 'ic:<slot-id>')", s)
}

// cmdInbox dispatches `arcmux-cli inbox <sub>`.
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

func cmdInboxPush(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox push", flag.ContinueOnError)
	verb := fs.String("verb", "", "message verb (required)")
	from := fs.String("from", "", "sender identifier (required)")
	priority := fs.Int("priority", 0, "priority (higher = more urgent)")
	id := fs.String("id", "", "explicit message id (auto-generated when empty)")
	refsRaw := fs.String("refs", "", "refs JSON object (optional)")
	to := fs.String("to", "elon", "destination queue: elon | manager:<slug>")
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
	q, err := parseQueue(*to)
	if err != nil {
		return fmt.Errorf("inbox push: %w", err)
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
	switch q.Kind {
	case "elon":
		if err := db.PushElonInbox(m); err != nil {
			return fmt.Errorf("inbox push: %w", err)
		}
	case "manager":
		if err := db.PushManagerInbox(q.Slug, m); err != nil {
			if errors.Is(err, store.ErrManagerInboxMissing) {
				return fmt.Errorf("inbox push: team %q has no inbox (spawn it first)", q.Slug)
			}
			return fmt.Errorf("inbox push: %w", err)
		}
	case "ic":
		if err := db.PushICInbox(q.Slug, m); err != nil {
			if errors.Is(err, store.ErrICInboxMissing) {
				return fmt.Errorf("inbox push: ic %q has no inbox (spawn the slot first)", q.Slug)
			}
			return fmt.Errorf("inbox push: %w", err)
		}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":          true,
		"id":          m.ID,
		"to":          formatQueue(q),
		"received_at": m.ReceivedAt.Format(time.RFC3339Nano),
	})
}

func cmdInboxPeek(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox peek", flag.ContinueOnError)
	n := fs.Int("n", 20, "max messages (oldest-first)")
	to := fs.String("to", "elon", "source queue: elon | manager:<slug>")
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
	q, err := parseQueue(*to)
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	var msgs []store.InboxMsg
	switch q.Kind {
	case "elon":
		msgs, err = db.PeekElonInbox(*n)
	case "manager":
		msgs, err = db.PeekManagerInbox(q.Slug, *n)
		// A team that has never been spawned has no inbox yet. From the
		// CLI's perspective that is identical to an empty queue: return
		// {"messages":[]} so polling scripts don't have to special-case
		// the very first poll before a spawn lands.
		if errors.Is(err, store.ErrManagerInboxMissing) {
			msgs, err = nil, nil
		}
	case "ic":
		msgs, err = db.PeekICInbox(q.Slug, *n)
		// Same silent-empty contract as manager peek: an IC polling its
		// own inbox in a tight loop while spawn is still in flight sees
		// {"messages":[]} instead of an error, which keeps the IC's
		// peek-on-every-loop pattern clean.
		if errors.Is(err, store.ErrICInboxMissing) {
			msgs, err = nil, nil
		}
	}
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}
	if msgs == nil {
		msgs = []store.InboxMsg{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"messages": msgs,
		"to":       formatQueue(q),
	})
}

func cmdInboxAck(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox ack", flag.ContinueOnError)
	id := fs.String("id", "", "message id to ack (required)")
	to := fs.String("to", "elon", "source queue: elon | manager:<slug>")
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
	q, err := parseQueue(*to)
	if err != nil {
		return fmt.Errorf("inbox ack: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	switch q.Kind {
	case "elon":
		err = db.AckElonInbox(*id)
	case "manager":
		err = db.AckManagerInbox(q.Slug, *id)
		if errors.Is(err, store.ErrManagerInboxMissing) {
			return fmt.Errorf("inbox ack: team %q has no inbox", q.Slug)
		}
	case "ic":
		err = db.AckICInbox(q.Slug, *id)
		if errors.Is(err, store.ErrICInboxMissing) {
			return fmt.Errorf("inbox ack: ic %q has no inbox", q.Slug)
		}
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("inbox ack: id %q not found", *id)
		}
		return fmt.Errorf("inbox ack: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok": true,
		"id": *id,
		"to": formatQueue(q),
	})
}

// formatQueue renders a queueRef back to the --to string form, for echoing
// in CLI responses so scripts can confirm routing without re-parsing.
func formatQueue(q queueRef) string {
	switch q.Kind {
	case "manager":
		return "manager:" + q.Slug
	case "ic":
		return "ic:" + q.Slug
	}
	return "elon"
}
