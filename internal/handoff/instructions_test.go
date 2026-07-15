package handoff

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadLaunchInstructionsResolvesOpaqueMarker(t *testing.T) {
	store, root, start := openInstructionTestStore(t)
	record := createTargetInState(
		t, store, "receive-marker", TargetLaunching,
		start,
		start.Add(time.Minute),
	)
	dir := filepath.Join(root, "handoff-"+record.Manifest.HandoffID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"goal":"continue safely","history":"/private/history.md"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "launch-instructions.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReadLaunchInstructions(LaunchMarker(record.Manifest.HandoffID, record.Digest))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("instructions = %q, want %q", got, want)
	}
}

func TestReadLaunchInstructionsRejectsHardlinkedArtifact(t *testing.T) {
	store, root, start := openInstructionTestStore(t)
	record := createTargetInState(
		t, store, "receive-hardlink", TargetLaunching,
		start,
		start.Add(time.Minute),
	)
	dir := filepath.Join(root, "handoff-"+record.Manifest.HandoffID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "mutable-instructions.json")
	if err := os.WriteFile(outside, []byte(`{"goal":"mutable"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "launch-instructions.json")); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ReadLaunchInstructions(LaunchMarker(record.Manifest.HandoffID, record.Digest)); !errors.Is(err, ErrLaunchInstructionsUnavailable) {
		t.Fatalf("hardlinked instructions error = %v", err)
	}
}

func TestLaunchRendezvousFindsAlternateProtocolRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, root, start := openInstructionTestStore(t)
	record := createTargetInState(
		t, store, "receive-alternate-root", TargetLaunching,
		start,
		start.Add(time.Minute),
	)
	dir := filepath.Join(root, "handoff-"+record.Manifest.HandoffID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"goal":"alternate config"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "launch-instructions.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := LaunchMarker(record.Manifest.HandoffID, record.Digest)
	if err := PublishLaunchRendezvous(DefaultLaunchRendezvousRoot(), marker, root); err != nil {
		t.Fatal(err)
	}

	got, err := ReceiveLaunchInstructions(DefaultLaunchRendezvousRoot(), marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("rendezvous instructions = %q, want %q", got, want)
	}
}

func openInstructionTestStore(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	store, root := openTestStore(t)
	start := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return start }
	return store, root, start
}
