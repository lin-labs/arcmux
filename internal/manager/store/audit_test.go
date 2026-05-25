package store

import (
	"testing"
	"time"
)

func TestAuditAppendAndRange(t *testing.T) {
	db := openTestDB(t)

	e1 := AuditEntry{Timestamp: time.Now(), Action: "team-created", Actor: "elon", Subject: "team-a"}
	e2 := AuditEntry{Timestamp: time.Now().Add(time.Millisecond), Action: "ic-spawned", Actor: "manager-a", Subject: "ic-1"}

	if err := db.AppendAudit(e1); err != nil {
		t.Fatalf("AppendAudit e1: %v", err)
	}
	if err := db.AppendAudit(e2); err != nil {
		t.Fatalf("AppendAudit e2: %v", err)
	}

	all, err := db.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d entries, want 2", len(all))
	}
	if all[0].Action != "ic-spawned" {
		t.Errorf("recent[0].Action = %q, want %q", all[0].Action, "ic-spawned")
	}
	if all[1].Action != "team-created" {
		t.Errorf("recent[1].Action = %q, want %q", all[1].Action, "team-created")
	}
}
