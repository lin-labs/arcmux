package daemon

import (
	"testing"
	"time"
)

func TestComputeFocus_RecentLatestAndLargestAlt(t *testing.T) {
	now := time.Now()
	arts := []artifactEntry{
		{Path: "/old/plan.md", LastSeen: now.Add(-6 * time.Hour), SizeBytes: 90_000},
		{Path: "/recent/big.go", LastSeen: now.Add(-90 * time.Minute), SizeBytes: 50_000},
		{Path: "/recent/latest.md", LastSeen: now.Add(-5 * time.Minute), SizeBytes: 2_000},
	}
	focus, alt := computeFocus(arts, 2*time.Hour, now)
	if focus == nil || focus.Path != "/recent/latest.md" {
		t.Fatalf("focus = %+v, want /recent/latest.md", focus)
	}
	if alt == nil || alt.Path != "/recent/big.go" {
		t.Fatalf("alt = %+v, want /recent/big.go", alt)
	}
}

func TestComputeFocus_FallsBackToAllWhenWindowEmpty(t *testing.T) {
	now := time.Now()
	arts := []artifactEntry{
		{Path: "/a.md", LastSeen: now.Add(-30 * time.Hour), SizeBytes: 10},
		{Path: "/b.md", LastSeen: now.Add(-20 * time.Hour), SizeBytes: 5},
	}
	focus, _ := computeFocus(arts, 2*time.Hour, now)
	if focus == nil || focus.Path != "/b.md" {
		t.Fatalf("focus = %+v, want /b.md (overall fallback)", focus)
	}
}

func TestComputeFocus_Empty(t *testing.T) {
	if f, a := computeFocus(nil, time.Hour, time.Now()); f != nil || a != nil {
		t.Fatalf("empty artifacts should yield nil focus, got %v/%v", f, a)
	}
}
