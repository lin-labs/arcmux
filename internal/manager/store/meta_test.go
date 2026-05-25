package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestProjectMeta_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "state.bolt"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Absent → ErrNotFound.
	if _, err := db.GetProjectMeta(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProjectMeta on empty: want ErrNotFound, got %v", err)
	}

	in := ProjectMeta{
		ElonPaneRef:      "pane:elon",
		ElonSurfaceRef:   "surf:elon",
		ElonWorkspaceRef: "ws:project",
	}
	if err := db.PutProjectMeta(in); err != nil {
		t.Fatalf("PutProjectMeta: %v", err)
	}

	out, err := db.GetProjectMeta()
	if err != nil {
		t.Fatalf("GetProjectMeta: %v", err)
	}
	if out.ElonPaneRef != in.ElonPaneRef || out.ElonSurfaceRef != in.ElonSurfaceRef || out.ElonWorkspaceRef != in.ElonWorkspaceRef {
		t.Fatalf("roundtrip mismatch: got %+v", out)
	}
	if out.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt should be set on Put")
	}

	// Overwrite — only the new ref should remain.
	in2 := ProjectMeta{ElonPaneRef: "pane:elon-v2", ElonSurfaceRef: "surf:elon-v2", ElonWorkspaceRef: "ws:project-v2"}
	if err := db.PutProjectMeta(in2); err != nil {
		t.Fatalf("PutProjectMeta v2: %v", err)
	}
	out2, err := db.GetProjectMeta()
	if err != nil {
		t.Fatalf("GetProjectMeta v2: %v", err)
	}
	if out2.ElonPaneRef != "pane:elon-v2" {
		t.Fatalf("v2 upsert lost: got %q", out2.ElonPaneRef)
	}
}
