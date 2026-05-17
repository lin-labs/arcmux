package session

import (
	"testing"
	"time"
)

func TestNewSession(t *testing.T) {
	s := NewSession("s-123", "test-session", "codex", "/tmp/project")

	if s.ID != "s-123" {
		t.Errorf("ID = %q, want %q", s.ID, "s-123")
	}
	if s.Agent != "codex" {
		t.Errorf("Agent = %q, want %q", s.Agent, "codex")
	}
	if s.State != StateStarting {
		t.Errorf("State = %q, want %q", s.State, StateStarting)
	}
	if s.Health != "healthy" {
		t.Errorf("Health = %q, want %q", s.Health, "healthy")
	}
}

func TestSetState(t *testing.T) {
	s := NewSession("s-1", "test", "claude", "/tmp")

	s.SetState(StateHandshaking)
	snap := s.Snapshot()
	if snap.State != StateHandshaking {
		t.Errorf("State = %q, want %q", snap.State, StateHandshaking)
	}
	if snap.IdleSince != nil {
		t.Error("IdleSince should be nil for non-idle state")
	}

	s.SetState(StateIdle)
	snap = s.Snapshot()
	if snap.State != StateIdle {
		t.Errorf("State = %q, want %q", snap.State, StateIdle)
	}
	if snap.IdleSince == nil {
		t.Error("IdleSince should be set for idle state")
	}

	s.SetState(StateWorking)
	snap = s.Snapshot()
	if snap.IdleSince != nil {
		t.Error("IdleSince should be cleared when leaving idle")
	}
}

func TestNudge(t *testing.T) {
	s := NewSession("s-1", "test", "codex", "/tmp")

	count := s.IncrementNudge()
	if count != 1 {
		t.Errorf("nudge count = %d, want 1", count)
	}

	count = s.IncrementNudge()
	if count != 2 {
		t.Errorf("nudge count = %d, want 2", count)
	}

	s.ResetNudge()
	snap := s.Snapshot()
	if snap.NudgeCount != 0 {
		t.Errorf("nudge count after reset = %d, want 0", snap.NudgeCount)
	}
}

func TestRecordActivity(t *testing.T) {
	s := NewSession("s-1", "test", "codex", "/tmp")
	before := s.Snapshot().LastActivityAt

	time.Sleep(1 * time.Millisecond)
	s.RecordActivity()

	after := s.Snapshot().LastActivityAt
	if !after.After(before) {
		t.Error("LastActivityAt should advance after RecordActivity")
	}
}
