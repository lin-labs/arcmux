package hooks

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestApplyEventTransitions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "s-abc"
	now := time.Date(2026, 6, 3, 18, 0, 0, 0, time.UTC)

	if err := ApplyEvent(dir, id, "claude", EventPromptSubmit, "", now); err != nil {
		t.Fatalf("prompt_submit: %v", err)
	}
	st, err := ReadSessionState(dir, id)
	if err != nil || st == nil {
		t.Fatalf("read: %v st=%v", err, st)
	}
	if !st.Working || st.TurnCount != 1 || !st.LastPromptSubmitAt.Equal(now) {
		t.Fatalf("after prompt_submit: %+v", st)
	}
	if st.Agent != "claude" || st.EventsSeen != 1 {
		t.Fatalf("identity/counters wrong: %+v", st)
	}

	if err := ApplyEvent(dir, id, "claude", EventToolStart, "Bash", now.Add(time.Second)); err != nil {
		t.Fatalf("tool_start: %v", err)
	}
	st, _ = ReadSessionState(dir, id)
	if st.LastTool != "Bash" || !st.Working {
		t.Fatalf("after tool_start: %+v", st)
	}

	if err := ApplyEvent(dir, id, "claude", EventTurnEnd, "", now.Add(2*time.Second)); err != nil {
		t.Fatalf("turn_end: %v", err)
	}
	st, _ = ReadSessionState(dir, id)
	if st.Working || st.LastTurnEndAt.IsZero() {
		t.Fatalf("after turn_end: %+v", st)
	}
	if st.EventsSeen != 3 {
		t.Fatalf("events_seen = %d, want 3", st.EventsSeen)
	}
}

func TestApplyEventUnknownEvent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := ApplyEvent(dir, "s-1", "claude", "frobnicate", "", time.Now()); err == nil {
		t.Fatal("expected error for unknown event")
	}
}

func TestApplyEventWithContractConsolidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "s-contract"
	now := time.Date(2026, 6, 24, 18, 0, 0, 0, time.UTC)

	first := TurnContractUpdate{
		Goal:                "  Build   the arcmux hook contract  ",
		SuccessVerification: "go test ./internal/hooks ./cmd/arcmux passes",
		Path:                "Inspect hook state, patch schema, add tests.",
		Source:              "UserPromptSubmit",
	}
	if err := ApplyEventWithContract(dir, id, "codex", EventPromptSubmit, "", first, now); err != nil {
		t.Fatalf("first update: %v", err)
	}
	second := TurnContractUpdate{
		Path:   "Patched schema and CLI; running focused tests.",
		Source: "Stop",
	}
	if err := ApplyEventWithContract(dir, id, "codex", EventTurnEnd, "", second, now.Add(time.Minute)); err != nil {
		t.Fatalf("second update: %v", err)
	}

	st, err := ReadSessionState(dir, id)
	if err != nil || st == nil {
		t.Fatalf("read: %v st=%v", err, st)
	}
	if st.TurnContract == nil {
		t.Fatal("turn contract missing")
	}
	if st.TurnContract.Goal != "Build the arcmux hook contract" {
		t.Fatalf("goal = %q", st.TurnContract.Goal)
	}
	if st.TurnContract.SuccessVerification != "go test ./internal/hooks ./cmd/arcmux passes" {
		t.Fatalf("verification = %q", st.TurnContract.SuccessVerification)
	}
	if st.TurnContract.Path != "Patched schema and CLI; running focused tests." {
		t.Fatalf("path = %q", st.TurnContract.Path)
	}
	if st.TurnContract.Source != "Stop" {
		t.Fatalf("source = %q", st.TurnContract.Source)
	}
	if !st.TurnContract.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("updated_at = %v", st.TurnContract.UpdatedAt)
	}
}

func TestReadSessionStateMissing(t *testing.T) {
	t.Parallel()
	st, err := ReadSessionState(t.TempDir(), "s-none")
	if err != nil {
		t.Fatalf("err = %v, want nil for missing file", err)
	}
	if st != nil {
		t.Fatalf("st = %+v, want nil for missing file", st)
	}
}

func TestArchiveSessionState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "s-arch"
	if err := ApplyEvent(dir, id, "codex", EventPromptSubmit, "", time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ArchiveSessionState(dir, id); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Stat(SessionStatePath(dir, id)); !os.IsNotExist(err) {
		t.Fatalf("live file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(ArchivedSessionStatePath(dir, id)); err != nil {
		t.Fatalf("archived file missing: %v", err)
	}
	// Archiving a missing session is a no-op, not an error.
	if err := ArchiveSessionState(dir, "s-never"); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
}

func TestApplyEventConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := "s-conc"
	if err := InitSessionState(dir, id, "claude", time.Now()); err != nil {
		t.Fatalf("init: %v", err)
	}

	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = ApplyEvent(dir, id, "claude", EventToolEnd, "Bash", time.Now())
		}()
	}
	wg.Wait()

	st, err := ReadSessionState(dir, id)
	if err != nil || st == nil {
		t.Fatalf("read: %v", err)
	}
	if st.EventsSeen != n {
		t.Fatalf("events_seen = %d, want %d (lock should serialize RMW)", st.EventsSeen, n)
	}
}

func TestSessionStatePaths(t *testing.T) {
	t.Parallel()
	if got := SessionStatePath("/d", "s-1"); got != filepath.Join("/d", "s-1.json") {
		t.Fatalf("live path = %s", got)
	}
	if got := ArchivedSessionStatePath("/d", "s-1"); got != filepath.Join("/d", "archived", "s-1.json") {
		t.Fatalf("archived path = %s", got)
	}
}
