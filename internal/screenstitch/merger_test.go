package screenstitch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFrame(t *testing.T, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "frame"+itoa(n)+".txt"))
	if err != nil {
		t.Fatalf("read frame%d: %v", n, err)
	}
	return string(b)
}

func itoa(n int) string { return string(rune('0' + n)) }

func TestMergerDedupsLiveCaptures(t *testing.T) {
	m := NewMerger(5000)
	total := 0
	for n := 1; n <= 6; n++ {
		total += len(m.Ingest(readFrame(t, n)))
	}
	joined := strings.Join(m.Transcript(), "\n")
	if !strings.Contains(joined, "honest verification that the rename is clean") {
		t.Fatal("expected real scrolled-in content in merged transcript")
	}
	if len(m.Transcript()) >= 400 {
		t.Fatalf("expected dedup to keep transcript compact, got %d lines", len(m.Transcript()))
	}
}

func TestMergerIdleTickAppendsNothing(t *testing.T) {
	m := NewMerger(5000)
	m.Ingest(readFrame(t, 1))
	if got := m.Ingest(readFrame(t, 2)); len(got) != 0 {
		t.Fatalf("frame1→frame2 is an idle tick, expected 0 new lines, got %d: %#v", len(got), got)
	}
}
