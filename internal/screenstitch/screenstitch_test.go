package screenstitch

import (
	"reflect"
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
