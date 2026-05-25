package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func newSlotsTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.bolt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPutGetSlotRoundTrip(t *testing.T) {
	db := newSlotsTestDB(t)

	s := Slot{
		ID:             "linus-1",
		Team:           "auth-rewrite",
		Role:           "linus",
		Contract:       "design-auth",
		PaneRef:        "pane:1",
		WorkspaceRef:   "workspace:7",
		ScratchpadPath: "/data/x.json",
		BootstrapPath:  "/data/x.sh",
		Agent:          "claude",
	}
	if err := db.PutSlot(s); err != nil {
		t.Fatalf("PutSlot: %v", err)
	}

	got, err := db.GetSlot("linus-1")
	if err != nil {
		t.Fatalf("GetSlot: %v", err)
	}
	if got.ID != s.ID || got.Team != s.Team || got.Role != s.Role || got.Contract != s.Contract {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.State != SlotActive {
		t.Errorf("default state = %q, want %q", got.State, SlotActive)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", got)
	}
}

func TestGetSlotMissing(t *testing.T) {
	db := newSlotsTestDB(t)
	_, err := db.GetSlot("ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestPutSlotRequiresIDAndTeam(t *testing.T) {
	db := newSlotsTestDB(t)
	if err := db.PutSlot(Slot{Team: "t"}); err == nil {
		t.Error("expected error for empty ID")
	}
	if err := db.PutSlot(Slot{ID: "x"}); err == nil {
		t.Error("expected error for empty Team")
	}
}

func TestListSlotsFilterByTeam(t *testing.T) {
	db := newSlotsTestDB(t)

	for _, s := range []Slot{
		{ID: "a-1", Team: "alpha", Role: "ic-base", Contract: "c1"},
		{ID: "a-2", Team: "alpha", Role: "linus", Contract: "c2"},
		{ID: "b-1", Team: "beta", Role: "ic-base", Contract: "c3"},
	} {
		if err := db.PutSlot(s); err != nil {
			t.Fatalf("PutSlot %s: %v", s.ID, err)
		}
	}

	alpha, err := db.ListSlots("alpha", "")
	if err != nil {
		t.Fatalf("ListSlots alpha: %v", err)
	}
	if len(alpha) != 2 {
		t.Errorf("alpha count = %d, want 2", len(alpha))
	}
	if alpha[0].ID != "a-1" || alpha[1].ID != "a-2" {
		t.Errorf("alpha not sorted by ID: %+v", alpha)
	}

	all, err := db.ListSlots("", "")
	if err != nil {
		t.Fatalf("ListSlots all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all count = %d, want 3", len(all))
	}
}

func TestListSlotsFilterByState(t *testing.T) {
	db := newSlotsTestDB(t)

	for _, s := range []Slot{
		{ID: "live-1", Team: "t", Role: "ic-base", Contract: "c1", State: SlotActive},
		{ID: "dead-1", Team: "t", Role: "ic-base", Contract: "c2", State: SlotDissolved},
		{ID: "idle-1", Team: "t", Role: "ic-base", Contract: "c3", State: SlotIdle},
	} {
		if err := db.PutSlot(s); err != nil {
			t.Fatalf("PutSlot %s: %v", s.ID, err)
		}
	}

	active, err := db.ListSlots("t", SlotActive)
	if err != nil {
		t.Fatalf("ListSlots: %v", err)
	}
	if len(active) != 1 || active[0].ID != "live-1" {
		t.Errorf("active filter wrong: %+v", active)
	}

	dissolved, err := db.ListSlots("t", SlotDissolved)
	if err != nil {
		t.Fatalf("ListSlots: %v", err)
	}
	if len(dissolved) != 1 || dissolved[0].ID != "dead-1" {
		t.Errorf("dissolved filter wrong: %+v", dissolved)
	}
}

func TestPutSlotReassignTeamRebuildsIndex(t *testing.T) {
	db := newSlotsTestDB(t)
	if err := db.PutSlot(Slot{ID: "mover", Team: "old", Role: "ic-base", Contract: "c"}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := db.PutSlot(Slot{ID: "mover", Team: "new", Role: "ic-base", Contract: "c"}); err != nil {
		t.Fatalf("reassign put: %v", err)
	}

	old, err := db.ListSlots("old", "")
	if err != nil {
		t.Fatalf("ListSlots old: %v", err)
	}
	if len(old) != 0 {
		t.Errorf("old team should be empty after move, got %+v", old)
	}
	new_, err := db.ListSlots("new", "")
	if err != nil {
		t.Fatalf("ListSlots new: %v", err)
	}
	if len(new_) != 1 || new_[0].ID != "mover" {
		t.Errorf("new team should have mover, got %+v", new_)
	}
}

func TestBucketsExistAfterOpen(t *testing.T) {
	db := newSlotsTestDB(t)
	for _, name := range []string{BucketSlots, BucketIdxTeamSlot} {
		if !db.HasBucket(name) {
			t.Errorf("bucket %q not created on Open", name)
		}
	}
}
