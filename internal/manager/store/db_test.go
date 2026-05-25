package store

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.bolt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenCreatesBuckets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bolt")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open errored: %v", err)
	}
	defer db.Close()

	for _, b := range AllBuckets {
		if !db.HasBucket(b) {
			t.Errorf("expected bucket %q to exist", b)
		}
	}
}

func TestOpenSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bolt")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open errored: %v", err)
	}
	defer db.Close()

	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion errored: %v", err)
	}
	if v != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", v, CurrentSchemaVersion)
	}
}

func TestReopenIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bolt")

	for i := 0; i < 3; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d errored: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close #%d errored: %v", i, err)
		}
	}
}
