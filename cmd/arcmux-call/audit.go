package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// cmdAudit dispatches `arcmux-call audit <sub>`.
func cmdAudit(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-call audit append|recent [flags]")
	}
	switch args[0] {
	case "append":
		return cmdAuditAppend(args[1:], stdout)
	case "recent":
		return cmdAuditRecent(args[1:], stdout)
	default:
		return fmt.Errorf("unknown audit subcommand %q (want append|recent)", args[0])
	}
}

func cmdAuditAppend(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("audit append", flag.ContinueOnError)
	action := fs.String("action", "", "audit action verb (required)")
	actor := fs.String("actor", "", "actor identifier (required)")
	subject := fs.String("subject", "", "subject identifier (required)")
	ruleID := fs.String("rule-id", "", "playbook rule id (optional)")
	detailRaw := fs.String("detail", "", "detail JSON object (optional)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *action == "" || *actor == "" || *subject == "" {
		return fmt.Errorf("audit append: --action, --actor, --subject required")
	}
	if *project == "" {
		return fmt.Errorf("audit append: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	var detail map[string]any
	if *detailRaw != "" {
		if err := json.Unmarshal([]byte(*detailRaw), &detail); err != nil {
			return fmt.Errorf("audit append: --detail must be JSON object: %w", err)
		}
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	e := store.AuditEntry{
		Timestamp: time.Now(),
		Action:    *action,
		Actor:     *actor,
		Subject:   *subject,
		RuleID:    *ruleID,
		Detail:    detail,
	}
	if err := db.AppendAudit(e); err != nil {
		return fmt.Errorf("audit append: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok": true,
		"ts": e.Timestamp.Format(time.RFC3339Nano),
	})
}

func cmdAuditRecent(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("audit recent", flag.ContinueOnError)
	n := fs.Int("n", 20, "max entries (newest-first)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("audit recent: --project or $ARCMUX_PROJECT required")
	}
	if _, err := paths.Validate(*project); err != nil {
		return err
	}

	db, _, err := openProjectDB(*dataRoot, *project)
	if err != nil {
		return err
	}
	defer db.Close()

	entries, err := db.RecentAudit(*n)
	if err != nil {
		return fmt.Errorf("audit recent: %w", err)
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"entries": entries})
}

// openProjectDB ensures the ephemeral dir exists, opens state.bolt, and
// returns the handle plus resolved paths. Caller closes the DB.
func openProjectDB(dataRoot, project string) (*store.DB, paths.Project, error) {
	p := paths.ForProject(dataRoot, "", project)
	if err := os.MkdirAll(p.EphemeralRoot, 0o700); err != nil {
		return nil, p, fmt.Errorf("ensure %s: %w", p.EphemeralRoot, err)
	}
	db, err := store.Open(p.StateBolt)
	if err != nil {
		return nil, p, err
	}
	return db, p, nil
}

func defaultDataRoot() string {
	if v := os.Getenv("ARCMUX_DATA"); v != "" {
		return v
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "data")
}
