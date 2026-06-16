// Package screenstitch merges overlapping terminal screen captures into a
// deduplicated, append-only transcript. Ported from mission-control's
// src/mc_data/frame_merge.rs: strip ANSI, drop bottom chrome, recover the
// scroll delta by anchor voting, append only the lines that scrolled in.
package screenstitch

import (
	"strings"
	"unicode"
)

const (
	// AnchorMinLen is the minimum trimmed rune length for an alignment anchor.
	AnchorMinLen = 12
	// AnchorMinDistinct is the minimum distinct alphanumeric chars for an anchor.
	AnchorMinDistinct = 6
	// MinAgreeingAnchors is the minimum agreeing anchors to trust a scroll delta.
	MinAgreeingAnchors = 2
	// RuleMinLen is the minimum run of ─ that counts as a composer/box rule.
	RuleMinLen = 30
)

// StripUniversal removes CSI escape sequences and trailing whitespace.
// Everything UI-specific (timers, spinners) is learned later by diff, not here.
func StripUniversal(line string) string {
	var b strings.Builder
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == '\x1b' {
			if i+1 < len(runes) && runes[i+1] == '[' {
				i += 2
				for i < len(runes) {
					n := runes[i]
					if n >= '@' && n <= '~' {
						break
					}
					i++
				}
			}
			continue
		}
		b.WriteRune(c)
	}
	return strings.TrimRight(b.String(), " \t\r\n")
}

// Normalize splits a raw capture into stripped lines.
func Normalize(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = StripUniversal(l)
	}
	return out
}

// isRule reports whether the line is a composer/box horizontal rule (a run of ─).
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	if len([]rune(t)) < RuleMinLen {
		return false
	}
	for _, c := range t {
		if c != '─' && c != ' ' {
			return false
		}
	}
	return true
}

// TranscriptRegion drops the bottom UI chrome (input composer box + tmux status
// bar). The composer is bracketed by ─ rules around a ❯ prompt; everything from
// the rule above that prompt downward is chrome and is removed.
func TranscriptRegion(frame []string) []string {
	prompt := -1
	for i := len(frame) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(frame[i], " \t"), "❯") {
			prompt = i
			break
		}
	}
	cut := len(frame)
	if prompt >= 0 {
		cut = prompt
		for i := prompt - 1; i >= 0; i-- {
			if isRule(frame[i]) {
				cut = i
				break
			}
		}
	} else {
		start := len(frame) * 2 / 3
		for i := len(frame) - 1; i >= start; i-- {
			if isRule(frame[i]) {
				cut = i
				break
			}
		}
	}
	out := make([]string, cut)
	copy(out, frame[:cut])
	return out
}

// isAnchor: a good alignment anchor is long and high-entropy. Status/spinner
// lines never qualify, so volatile lines can't corrupt the alignment.
func isAnchor(line string) bool {
	t := strings.TrimSpace(line)
	if len([]rune(t)) < AnchorMinLen {
		return false
	}
	distinct := map[rune]struct{}{}
	for _, c := range t {
		if c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
			distinct[unicode.ToLower(c)] = struct{}{}
		}
	}
	return len(distinct) >= AnchorMinDistinct
}
