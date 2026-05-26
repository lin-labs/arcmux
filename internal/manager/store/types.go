package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewInboxID returns a sortable id of the form "<unix-ns>-<6-hex>", suitable
// for InboxMsg.ID. The time prefix gives natural chronological ordering in
// logs and tooling; the hex suffix avoids collisions on identical-nano
// writes from independent processes.
func NewInboxID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:])), nil
}

// AuditEntry is a project-wide audit row.
type AuditEntry struct {
	Timestamp time.Time      `json:"ts"`
	Action    string         `json:"action"`
	Actor     string         `json:"actor"`
	Subject   string         `json:"subject"`
	RuleID    string         `json:"rule_id,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// InboxMsg is a queued message awaiting processing by the addressed session.
type InboxMsg struct {
	ID         string         `json:"id"`
	Verb       string         `json:"verb"`
	From       string         `json:"from"`
	Priority   int            `json:"priority"`
	Body       string         `json:"body"`
	Refs       map[string]any `json:"refs,omitempty"`
	ReceivedAt time.Time      `json:"received_at"`
}
