package screenstitch

import (
	"reflect"
	"strings"
	"testing"
)

func TestStripUniversal(t *testing.T) {
	// CSI color codes stripped, trailing whitespace trimmed, text preserved.
	got := StripUniversal("\x1b[31mhello\x1b[0m world   ")
	if got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestTranscriptRegionDropsComposer(t *testing.T) {
	frame := []string{
		"real transcript line one",
		"real transcript line two",
		"────────────────────────────────────────", // composer top rule (>=30 ─)
		"❯ type here",
		"────────────────────────────────────────",
	}
	got := TranscriptRegion(frame)
	want := []string{"real transcript line one", "real transcript line two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestScrollDeltaIdleTick(t *testing.T) {
	prev := []string{
		"the bug is a missing await in auth",
		"patched the logout handler as well",
		"working 1m9s 3.4k tokens",
	}
	cap := []string{
		"the bug is a missing await in auth",
		"patched the logout handler as well",
		"working 1m14s 3.4k tokens",
	}
	d, ok, _ := ScrollDelta(prev, cap)
	if !ok || d != 0 {
		t.Fatalf("idle tick: got d=%d ok=%v, want 0,true", d, ok)
	}
	if got := NewLines(prev, cap); len(got) != 0 {
		t.Fatalf("idle tick should yield no new lines, got %#v", got)
	}
}

func TestNewLinesScroll(t *testing.T) {
	prev := []string{"alpha alpha alpha line", "bravo bravo bravo line", "charlie charlie line"}
	cap := []string{"bravo bravo bravo line", "charlie charlie line", "delta delta delta line"}
	got := NewLines(prev, cap)
	want := []string{"delta delta delta line"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestMaskVolatileAndStatus(t *testing.T) {
	if got := MaskVolatile("same line here", "same line here"); got != "same line here" {
		t.Fatalf("equal lines unchanged, got %q", got)
	}
	m := MaskVolatile("Thinking (12s)", "Thinking (17s)")
	if !strings.Contains(m, "§") || !strings.HasPrefix(m, "Thinking (") {
		t.Fatalf("expected masked span with stable prefix, got %q", m)
	}
	if !isStatusUpdate("Working (1m9s 3.4k tokens)", "Working (1m14s 3.7k tokens)") {
		t.Fatal("ticking status line should be a status update")
	}
	if isStatusUpdate("Working (1m9s 3.4k tokens)", "Let me look at the auth module instead") {
		t.Fatal("a different line is not a status update")
	}
}
