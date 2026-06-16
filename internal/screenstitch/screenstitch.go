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

// uniqueAnchorIndex maps each anchor line that occurs exactly once → its index.
// Repeated anchors are dropped (they can't cast an unambiguous vote).
func uniqueAnchorIndex(frame []string) map[string]int {
	count := map[string]int{}
	idx := map[string]int{}
	for i, line := range frame {
		if !isAnchor(line) {
			continue
		}
		count[line]++
		idx[line] = i
	}
	for k := range idx {
		if count[k] != 1 {
			delete(idx, k)
		}
	}
	return idx
}

// ScrollDelta returns how many lines cap advanced past prev, by anchor voting.
// ok=false means no confident overlap (a gap, or unrelated frames).
func ScrollDelta(prev, cap []string) (delta int, ok bool, votes int) {
	pidx := uniqueAnchorIndex(prev)
	cidx := uniqueAnchorIndex(cap)
	tally := map[int]int{}
	for line, i := range pidx {
		if j, found := cidx[line]; found {
			tally[i-j]++
		}
	}
	bestD, bestN := 0, 0
	for d, n := range tally {
		if n > bestN {
			bestD, bestN = d, n
		}
	}
	if bestN >= MinAgreeingAnchors {
		return bestD, true, bestN
	}
	return 0, false, 0
}

// NewLines returns the lines that scrolled in. No confident overlap → the whole
// frame is new (a gap). delta 0 → nothing new. delta d>0 → the last d lines.
func NewLines(prev, cap []string) []string {
	d, ok, _ := ScrollDelta(prev, cap)
	if !ok {
		out := make([]string, len(cap))
		copy(out, cap)
		return out
	}
	if d == 0 {
		return nil
	}
	if d > 0 && d <= len(cap) {
		out := make([]string, d)
		copy(out, cap[len(cap)-d:])
		return out
	}
	out := make([]string, len(cap))
	copy(out, cap)
	return out
}

// MaskVolatile masks the differing span between two proven-same logical lines
// with § (prefix§suffix). Equal lines pass through. Learned by diff — never a
// hardcoded \d+s catalog.
func MaskVolatile(a, b string) string {
	if a == b {
		return a
	}
	av := []rune(a)
	bv := []rune(b)
	p := 0
	for p < len(av) && p < len(bv) && av[p] == bv[p] {
		p++
	}
	sa, sb := len(av), len(bv)
	for sa > p && sb > p && av[sa-1] == bv[sb-1] {
		sa--
		sb--
	}
	return string(av[:p]) + "§" + string(av[sa:])
}

// stableRatio: how much of a mask is stable (non-§) alphanumeric content.
func stableRatio(mask string) float64 {
	alnum, total := 0, 0
	for _, c := range mask {
		if !unicode.IsSpace(c) {
			total++
		}
		if c < 128 && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
			alnum++
		}
	}
	if total == 0 {
		total = 1
	}
	return float64(alnum) / float64(total)
}

// isStatusUpdate: cur is the live status line if, vs prev's tail, it changed
// only in volatile spans (shared skeleton).
func isStatusUpdate(prevTail, curTail string) bool {
	if prevTail == curTail {
		return false
	}
	mask := MaskVolatile(prevTail, curTail)
	return strings.Contains(mask, "§") && stableRatio(mask) >= 0.5
}

// lastNonblank returns the index of the last non-blank line, or -1.
func lastNonblank(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

// sameSkeleton reports whether line is another tick of the status line whose
// volatile skeleton is `prefix§suffix`.
func sameSkeleton(line, skeleton string) bool {
	prefix, suffix, found := strings.Cut(skeleton, "§")
	if !found {
		return false
	}
	lt := strings.TrimSpace(line)
	pre := strings.TrimLeft(prefix, " \t")
	suf := strings.TrimRight(suffix, " \t")
	okPrefix := pre == "" || strings.HasPrefix(lt, pre)
	okSuffix := suf == "" || strings.HasSuffix(lt, suf)
	return okPrefix && okSuffix && len([]rune(lt)) >= len([]rune(strings.TrimSpace(prefix)))
}
