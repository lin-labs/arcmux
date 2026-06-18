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

func TestMergerCapturesPinnedQuestion(t *testing.T) {
	// A question/menu that appears at the bottom WITHOUT scrolling (delta 0,
	// replacing the composer) must be captured faithfully — the bug that the
	// old scroll-only stitch dropped.
	m := NewMerger(5000)
	base := "alpha anchor line one\nbravo anchor line two\ncharlie anchor line three\n"
	m.Ingest(base + "\xe2\x9d\xaf \n") // composer prompt at the bottom
	got := m.Ingest(base + "Which option do you want?\n  1. build path A\n  2. build path B\nEnter to select\n")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "Which option do you want?") || !strings.Contains(joined, "build path A") {
		t.Fatalf("pinned question should be captured, got %#v", got)
	}
}

func TestMergerSkipsTickingTimer(t *testing.T) {
	// A volatile timer line that ticks each frame must be logged once, not every tick.
	m := NewMerger(5000)
	base := "the real work line is here long\nsecond real work line also long\n"
	m.Ingest(base + "Working (1m9s tokens 3.4k)\n")
	got := m.Ingest(base + "Working (1m14s tokens 3.7k)\n") // only the timer changed
	if len(got) != 0 {
		t.Fatalf("ticking timer should not be re-logged, got %#v", got)
	}
}
