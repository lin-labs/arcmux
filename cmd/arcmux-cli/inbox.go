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

// cmdInbox dispatches `arcmux-cli inbox <sub>`.
//
// After C3 the only inbox surface arcmux owns is the per-session inbox
// (BucketSessionInbox). The CLI addresses queues uniformly by session
// name through `--session <name>` (alias `--to <name>` kept for older
// callers). The pre-C3 multi-kind addressing (`elon` / `manager:<slug>` /
// `ic:<slot-id>`) is gone — arcmux no longer knows what role a pane plays.
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

// sessionFlag wires a pair of synonymous flags (--session, --to) so the
// CLI accepts whichever form the caller has memorized. Returns the
// resolved name (whichever is non-empty; --session wins on tie) or an
// error if neither is supplied.
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
	if _, err := paths.Validate(name); err != nil {
		return "", fmt.Errorf("--session %q: %w", name, err)
	}
	return name, nil
}

func cmdInboxPush(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox push", flag.ContinueOnError)
	verb := fs.String("verb", "", "message verb (required)")
	from := fs.String("from", "", "sender identifier (required)")
	priority := fs.Int("priority", 0, "priority (higher = more urgent)")
	id := fs.String("id", "", "explicit message id (auto-generated when empty)")
	refsRaw := fs.String("refs", "", "refs JSON object (optional)")
	var sf sessionFlag
	sf.attach(fs)
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
	sessionName, err := sf.resolve()
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

	// Ensure-then-push: the CLI is the user-facing surface, so a push to a
	// name we've never seen lazily creates the bucket. This matches the C1
	// gRPC Send semantics (push implies ensure when the session isn't
	// ready yet).
	if err := db.EnsureSessionInbox(sessionName); err != nil {
		return fmt.Errorf("inbox push: ensure %q: %w", sessionName, err)
	}

	m := store.InboxMsg{
		ID:         msgID,
		Verb:       *verb,
		From:       *from,
		Priority:   *priority,
		Body:       string(body),
		Refs:       refs,
		ReceivedAt: time.Now(),
	}
	if err := db.PushSessionInbox(sessionName, m); err != nil {
		return fmt.Errorf("inbox push: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":          true,
		"id":          m.ID,
		"session":     sessionName,
		"received_at": m.ReceivedAt.Format(time.RFC3339Nano),
	})
}

func cmdInboxPeek(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("inbox peek", flag.ContinueOnError)
	n := fs.Int("n", 20, "max messages (oldest-first)")
	var sf sessionFlag
	sf.attach(fs)
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
	sessionName, err := sf.resolve()
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	msgs, err := db.PeekSessionInbox(sessionName, *n)
	// A session that's never been pushed to has no inbox yet. From the
	// CLI's perspective that is identical to an empty queue: return
	// {"messages":[]} so polling scripts don't have to special-case the
	// very first poll before a push lands.
	if errors.Is(err, store.ErrSessionInboxMissing) {
		msgs, err = nil, nil
	}
	if err != nil {
		return fmt.Errorf("inbox peek: %w", err)
	}
	if msgs == nil {
		msgs = []store.InboxMsg{}
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
	sessionName, err := sf.resolve()
	if err != nil {
		return fmt.Errorf("inbox ack: %w", err)
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.AckSessionInbox(sessionName, *id); err != nil {
		if errors.Is(err, store.ErrSessionInboxMissing) {
			return fmt.Errorf("inbox ack: session %q has no inbox", sessionName)
		}
		return fmt.Errorf("inbox ack: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":      true,
		"id":      *id,
		"session": sessionName,
	})
}
