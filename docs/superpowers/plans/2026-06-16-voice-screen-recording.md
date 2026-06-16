# Voice Screen Recording (arcmux side) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give arcmux an aggressive per-screen recording capability — a 1-second capture loop that stitches consecutive tmux captures into a deduplicated, append-only transcript file — plus a single-screen babysit context and an `arcmux-cli voice` entry point, so voxtop's babysit voice mode can read the file and talk to one agent screen.

**Architecture:** A new pure package `internal/screenstitch` ports mission-control's `frame_merge.rs` dedup algorithm to Go. A `Recorder` runs a dedicated 1s ticker per session: capture → stitch → append new lines to `~/data/arcmux/sessions/<id>.screen.log`. The daemon owns a `recorders` map with idempotent enable/cancel, decoupled from any voice client (only an explicit cancel or session close stops it). Control is exposed over HTTP (`/voice/record/*`) and the existing `/babysit/new` gains a `name=` single-screen scope plus a `screen_logs` field. `arcmux-cli voice <name>` ties it together: enable recording, mint the context, print the connect handle.

**Tech Stack:** Go 1.26+, standard library only (no new deps), `tmux`, existing arcmux daemon/HTTP/gRPC infrastructure, bbolt state store.

## Global Constraints

- Go 1.26+; standard library only — **no new third-party dependencies**.
- Every file `gofmt`-clean; tests are Go table tests where natural.
- `go test ./...` and `go build ./...` must stay green after every task.
- **Log format contract (binding):** the screen log is a plain UTF-8 text file, one screen line per file line, **append-only, newest content at the bottom**, no headers/framing/markers. This is the entire arcmux→voxtop screen interface.
- **Recording is decoupled from any client:** recording stops ONLY on an explicit cancel or on session close. There must be NO code path where a client/voice/context disconnect or context-TTL expiry stops recording.
- Screen log dir: `<DataRoot>/arcmux/sessions/` (`~/data/arcmux/sessions/` by default). The startup migration sweep only globs `*.json`, so `*.screen.log` files there are safe.
- Recording state lives in the daemon's in-memory registry; it is not persisted across daemon restarts (a restart stops recording — acceptable for MVP).
- Ported constants from `frame_merge.rs`: `AnchorMinLen=12`, `AnchorMinDistinct=6`, `MinAgreeingAnchors=2`, `RuleMinLen=30`.

---

### Task 1: screenstitch — line normalization + transcript region

**Files:**
- Create: `internal/screenstitch/screenstitch.go`
- Test: `internal/screenstitch/screenstitch_test.go`

**Interfaces:**
- Produces: `StripUniversal(line string) string`, `Normalize(raw string) []string`, `TranscriptRegion(frame []string) []string`, and the package constants `AnchorMinLen`, `AnchorMinDistinct`, `MinAgreeingAnchors`, `RuleMinLen`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/screenstitch/ -run 'TestStripUniversal|TestTranscriptRegion' -v`
Expected: FAIL — `undefined: StripUniversal` / `undefined: TranscriptRegion`.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/screenstitch/ -run 'TestStripUniversal|TestTranscriptRegion' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/screenstitch/screenstitch.go internal/screenstitch/screenstitch_test.go
git commit -m "feat(screenstitch): strip + normalize + transcript region (port frame_merge)"
```

---

### Task 2: screenstitch — scroll delta + new lines

**Files:**
- Modify: `internal/screenstitch/screenstitch.go`
- Test: `internal/screenstitch/screenstitch_test.go`

**Interfaces:**
- Consumes: `Normalize`, `isAnchor`, the anchor constants (Task 1).
- Produces: `ScrollDelta(prev, cap []string) (delta int, ok bool, votes int)` and `NewLines(prev, cap []string) []string`. `ok=false` means no confident overlap.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/screenstitch/ -run 'TestScrollDelta|TestNewLines' -v`
Expected: FAIL — `undefined: ScrollDelta` / `undefined: NewLines`.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/screenstitch/ -run 'TestScrollDelta|TestNewLines' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/screenstitch/
git commit -m "feat(screenstitch): anchor-vote scroll delta + new-lines extraction"
```

---

### Task 3: screenstitch — volatile masking + status-line detection

**Files:**
- Modify: `internal/screenstitch/screenstitch.go`
- Test: `internal/screenstitch/screenstitch_test.go`

**Interfaces:**
- Produces: `MaskVolatile(a, b string) string`, and unexported helpers `stableRatio(mask string) float64`, `isStatusUpdate(prevTail, curTail string) bool`, `sameSkeleton(line, skeleton string) bool`, `lastNonblank(lines []string) int` (returns -1 if none). Consumed by the Merger in Task 4.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/screenstitch/ -run TestMaskVolatileAndStatus -v`
Expected: FAIL — `undefined: MaskVolatile` / `undefined: isStatusUpdate`.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/screenstitch/ -run TestMaskVolatileAndStatus -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/screenstitch/
git commit -m "feat(screenstitch): volatile masking + learned status-line detection"
```

---

### Task 4: screenstitch — stateful Merger + golden fixtures

**Files:**
- Modify: `internal/screenstitch/screenstitch.go`
- Test: `internal/screenstitch/merger_test.go`
- Create (copy fixtures): `internal/screenstitch/testdata/frame1.txt` … `frame6.txt`

**Interfaces:**
- Consumes: all of Tasks 1–3.
- Produces: `type Merger struct{...}`, `NewMerger(maxLines int) *Merger`, and `(m *Merger) Ingest(raw string) []string` which returns the genuinely-new transcript lines appended this tick (nil on an idle tick). The Recorder (Task 5) calls `Ingest` and writes the returned lines to the log file.

- [ ] **Step 1: Copy the golden fixtures**

```bash
mkdir -p internal/screenstitch/testdata
cp ~/Tools/mission-control/tests/fixtures/remote_frames/frame{1,2,3,4,5,6}.txt internal/screenstitch/testdata/
ls internal/screenstitch/testdata/
```
Expected: `frame1.txt` … `frame6.txt` listed.

- [ ] **Step 2: Write the failing test**

```go
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/screenstitch/ -run TestMerger -v`
Expected: FAIL — `undefined: NewMerger`.

- [ ] **Step 4: Write minimal implementation**

```go
// Merger is a stateful accumulator: feed it raw captures, it returns only the
// genuinely-new transcript lines and tracks the live status line separately.
type Merger struct {
	prevBody       []string
	havePrev       bool
	prevTail       string
	havePrevTail   bool
	statusSkeleton string
	haveSkeleton   bool
	transcript     []string
	status         string
	maxLines       int
}

// NewMerger returns a Merger retaining at most maxLines transcript lines
// (0 = unbounded).
func NewMerger(maxLines int) *Merger { return &Merger{maxLines: maxLines} }

// Transcript returns the accumulated, deduplicated transcript.
func (m *Merger) Transcript() []string { return m.transcript }

// Status returns the current live status line, if any.
func (m *Merger) Status() string { return m.status }

// Ingest ingests one raw capture and returns the new transcript lines appended
// this tick (nil on an idle tick).
func (m *Merger) Ingest(raw string) []string {
	region := TranscriptRegion(Normalize(raw))
	body, status, haveStatus := m.splitStatus(region)

	var appended []string
	if !m.havePrev {
		appended = append(appended, body...)
	} else {
		appended = NewLines(m.prevBody, body)
	}

	// Drop any stale copy of the live status line that scrolled up into history.
	var kept []string
	for _, l := range appended {
		if m.haveSkeleton && sameSkeleton(l, m.statusSkeleton) {
			continue
		}
		kept = append(kept, l)
	}

	m.transcript = append(m.transcript, kept...)
	m.trim()

	if li := lastNonblank(body); li >= 0 {
		m.prevTail, m.havePrevTail = body[li], true
	} else {
		m.havePrevTail = false
	}
	m.prevBody, m.havePrev = body, true
	m.status = status
	_ = haveStatus
	return kept
}

// splitStatus peels the live status line off the tail using the learned-volatile
// test against the previous frame's tail.
func (m *Merger) splitStatus(region []string) (body []string, status string, ok bool) {
	li := lastNonblank(region)
	if li < 0 {
		return region, "", false
	}
	tail := region[li]
	if m.havePrevTail && isStatusUpdate(m.prevTail, tail) {
		m.statusSkeleton, m.haveSkeleton = MaskVolatile(m.prevTail, tail), true
		return append([]string{}, region[:li]...), tail, true
	}
	return region, "", false
}

func (m *Merger) trim() {
	if m.maxLines > 0 && len(m.transcript) > m.maxLines {
		excess := len(m.transcript) - m.maxLines
		m.transcript = append([]string{}, m.transcript[excess:]...)
	}
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/screenstitch/ -v`
Expected: PASS (all screenstitch tests).

- [ ] **Step 6: Commit**

```bash
git add internal/screenstitch/
git commit -m "feat(screenstitch): stateful Merger + golden fixtures from live captures"
```

---

### Task 5: Recorder — per-session 1s capture loop writing the log

**Files:**
- Create: `internal/daemon/recorder.go`
- Test: `internal/daemon/recorder_test.go`

**Interfaces:**
- Consumes: `screenstitch.NewMerger`, `Merger.Ingest` (Task 4).
- Produces: `type recorder struct{...}` and `func newRecorder(logPath string, capture func(context.Context) (string, error), interval time.Duration, logger *slog.Logger) *recorder` with methods `start(ctx context.Context)` (spawns the loop goroutine) and `stop()` (cancels + closes the file). The capture func is injected so tests need no tmux. The Recorder writes the screen log, one new line per `\n`, append-only.

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecorderAppendsDedupedLines(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "s-test.screen.log")
	var tick int64
	frames := []string{
		"alpha alpha alpha line\nbravo bravo bravo line\n",            // frame 0: 2 new
		"alpha alpha alpha line\nbravo bravo bravo line\n",            // frame 1: idle → 0 new
		"bravo bravo bravo line\ncharlie charlie charlie line\n",     // frame 2: scroll → 1 new
	}
	capture := func(context.Context) (string, error) {
		i := atomic.AddInt64(&tick, 1) - 1
		if int(i) < len(frames) {
			return frames[i], nil
		}
		return frames[len(frames)-1], nil
	}
	r := newRecorder(logPath, capture, 5*time.Millisecond, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	r.start(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()
	r.stop()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{"alpha alpha alpha line", "bravo bravo bravo line", "charlie charlie charlie line"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log got %#v want %#v", got, want)
	}
}
```

Note: if a `testLogger()` helper does not already exist in the package's tests, add `func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }` to this test file (imports `io`, `log/slog`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestRecorderAppendsDedupedLines -v`
Expected: FAIL — `undefined: newRecorder`.

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/blin/arcmux/internal/screenstitch" // adjust to the module's import path
)

// recorder runs a dedicated capture loop for one session: every interval it
// captures the pane, stitches against the previous frame, and appends only the
// genuinely-new lines to the screen log. Decoupled from any client — it stops
// only when its context is cancelled (explicit cancel or session close).
type recorder struct {
	logPath  string
	capture  func(context.Context) (string, error)
	interval time.Duration
	logger   *slog.Logger
	merger   *screenstitch.Merger

	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	cancel context.CancelFunc
	done   chan struct{}

	startedAt time.Time
}

func newRecorder(logPath string, capture func(context.Context) (string, error), interval time.Duration, logger *slog.Logger) *recorder {
	return &recorder{
		logPath:  logPath,
		capture:  capture,
		interval: interval,
		logger:   logger,
		merger:   screenstitch.NewMerger(0),
		done:     make(chan struct{}),
	}
}

func (r *recorder) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.startedAt = time.Now()
	go r.loop(ctx)
}

func (r *recorder) loop(ctx context.Context) {
	defer close(r.done)
	// Truncate/create the log on start (fresh recording per the spec).
	f, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		r.logger.Error("recorder open log failed", "path", r.logPath, "err", err)
		return
	}
	r.mu.Lock()
	r.f, r.w = f, bufio.NewWriter(f)
	r.mu.Unlock()

	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.tick(ctx) // capture immediately, don't wait one interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *recorder) tick(ctx context.Context) {
	raw, err := r.capture(ctx)
	if err != nil {
		r.logger.Debug("recorder capture failed", "path", r.logPath, "err", err)
		return
	}
	lines := r.merger.Ingest(raw)
	if len(lines) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return
	}
	for _, l := range lines {
		r.w.WriteString(l)
		r.w.WriteByte('\n')
	}
	r.w.Flush()
}

func (r *recorder) stop() {
	if r.cancel != nil {
		r.cancel()
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		r.w.Flush()
	}
	if r.f != nil {
		r.f.Close()
		r.f, r.w = nil, nil
	}
}
```

Verify the module import path first: `head -1 go.mod` and replace `github.com/blin/arcmux` accordingly.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestRecorderAppendsDedupedLines -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/recorder.go internal/daemon/recorder_test.go
git commit -m "feat(daemon): per-session screen recorder (1s capture → stitch → append)"
```

---

### Task 6: Daemon recording lifecycle — enable/cancel/status, decoupled

**Files:**
- Modify: `internal/daemon/daemon.go` (add `recorders` map + methods; hook teardown into `Kill`)
- Modify: `internal/config/config.go` (add `ScreenLogDir()` helper)
- Test: `internal/daemon/recording_test.go`
- Test: `internal/config/config_test.go` (append a small case)

**Interfaces:**
- Consumes: `newRecorder` (Task 5), `Daemon.Capture`, `Daemon.GetSession`, `HTTPServer.findByName` pattern.
- Produces on `*Daemon`:
  - `ScreenLogPath(sessionID string) string`
  - `SetRecording(sessionID string, on bool) (logPath string, err error)` — idempotent enable; cancel leaves the file.
  - `RecordingStatus(sessionID string) (on bool, logPath string, since time.Time)`
  - `recordingSessions() []string`
- Produces on `*Config`: `func (c *Config) ScreenLogDir() string`.

- [ ] **Step 1: Write the failing test**

```go
func TestSetRecordingIdempotentAndDecoupled(t *testing.T) {
	d := newTestDaemon(t) // existing helper; see other *_test.go in this pkg
	sid := installFakeSession(t, d, "agents:1") // helper: registers a session whose Capture is stubbed
	d.captureHook = func(_ context.Context, _ string, _ bool) (string, error) {
		return "stable anchor line one\nstable anchor line two\n", nil
	}

	p1, err := d.SetRecording(sid, true)
	if err != nil {
		t.Fatal(err)
	}
	on, p2, _ := d.RecordingStatus(sid)
	if !on || p1 != p2 {
		t.Fatalf("expected recording on with stable path, got on=%v p1=%q p2=%q", on, p1, p2)
	}
	// Idempotent: enabling again returns the same path, no panic/dup loop.
	if p3, err := d.SetRecording(sid, true); err != nil || p3 != p1 {
		t.Fatalf("idempotent enable: p3=%q err=%v", p3, err)
	}
	// There is no client to disconnect; only explicit cancel stops it.
	if _, err := d.SetRecording(sid, false); err != nil {
		t.Fatal(err)
	}
	if on, _, _ := d.RecordingStatus(sid); on {
		t.Fatal("expected recording off after explicit cancel")
	}
	// The log file is kept after stop.
	if _, err := os.Stat(p1); err != nil {
		t.Fatalf("log should be kept after stop: %v", err)
	}
}
```

If `installFakeSession` / `newTestDaemon` helpers don't exist verbatim, mirror the construction used in `internal/daemon/grpc_c1_test.go` (which builds a `*Daemon` and registers sessions) and adapt names; keep the assertions identical.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestSetRecordingIdempotentAndDecoupled -v`
Expected: FAIL — `d.SetRecording undefined`.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`:

```go
// ScreenLogDir is where per-session voice screen-recording logs live:
// <DataRoot>/arcmux/sessions/. The startup migration sweep only moves *.json,
// so *.screen.log files here are untouched.
func (c *Config) ScreenLogDir() string {
	root := c.DataRoot
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "data")
	}
	return filepath.Join(root, "arcmux", "sessions")
}
```

In `internal/daemon/daemon.go`, add to the `Daemon` struct (inside the existing `mu`-guarded block or a new mutex — reuse `mu`):

```go
	recorders map[string]*recorder // sessionID → active screen recorder
```

Initialize `recorders: map[string]*recorder{}` wherever the other maps (`sessions`, `monitors`) are initialized. Then add:

```go
// ScreenLogPath returns the screen-recording log path for a session.
func (d *Daemon) ScreenLogPath(sessionID string) string {
	return filepath.Join(d.cfg.ScreenLogDir(), sessionID+".screen.log")
}

// SetRecording enables (on=true) or cancels (on=false) aggressive screen
// recording for a session. Enable is idempotent. Recording is decoupled from
// any client: it stops only via this cancel or session close — never on a
// client/context disconnect.
func (d *Daemon) SetRecording(sessionID string, on bool) (string, error) {
	if _, ok := d.GetSession(sessionID); !ok {
		return "", fmt.Errorf("session not found: %s", sessionID)
	}
	logPath := d.ScreenLogPath(sessionID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.recorders == nil {
		d.recorders = map[string]*recorder{}
	}
	if on {
		if _, exists := d.recorders[sessionID]; exists {
			return logPath, nil // idempotent
		}
		if err := os.MkdirAll(d.cfg.ScreenLogDir(), 0o755); err != nil {
			return "", err
		}
		capture := func(ctx context.Context) (string, error) {
			return d.Capture(ctx, sessionID, false)
		}
		r := newRecorder(logPath, capture, time.Second, d.logger)
		r.start(d.ctx)
		d.recorders[sessionID] = r
		d.logger.Info("voice recording started", "session_id", sessionID, "log", logPath)
		return logPath, nil
	}
	if r, exists := d.recorders[sessionID]; exists {
		delete(d.recorders, sessionID)
		go r.stop() // stop outside the lock-sensitive path; file is kept
		d.logger.Info("voice recording stopped", "session_id", sessionID)
	}
	return logPath, nil
}

// RecordingStatus reports whether a session is being recorded and its log path.
func (d *Daemon) RecordingStatus(sessionID string) (bool, string, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.recorders[sessionID]; ok {
		return true, r.logPath, r.startedAt
	}
	return false, d.ScreenLogPath(sessionID), time.Time{}
}

// recordingSessions returns the IDs of all sessions currently recording.
func (d *Daemon) recordingSessions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.recorders))
	for id := range d.recorders {
		out = append(out, id)
	}
	return out
}
```

In `Daemon.Kill` (after the session is confirmed found, before/around teardown of `monitors`), tear down any recorder:

```go
	d.mu.Lock()
	if r, ok := d.recorders[sessionID]; ok {
		delete(d.recorders, sessionID)
		go r.stop()
	}
	d.mu.Unlock()
```

(Place this beside the existing `monitors` cleanup in `Kill`; match the surrounding locking style — if `Kill` already holds `d.mu` at that point, drop the extra Lock/Unlock.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestSetRecording -v && go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite + build**

Run: `go build ./... && go test ./...`
Expected: PASS / build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/daemon.go internal/config/config.go internal/daemon/recording_test.go internal/config/config_test.go
git commit -m "feat(daemon): recording lifecycle — idempotent enable, client-decoupled cancel, close teardown"
```

---

### Task 7: HTTP control shims — /voice/record/start|stop|status

**Files:**
- Modify: `internal/daemon/http.go` (register routes + handlers)
- Test: `internal/daemon/http_voice_test.go`

**Interfaces:**
- Consumes: `Daemon.SetRecording`, `Daemon.RecordingStatus`, `Daemon.recordingSessions`, `HTTPServer.findByName` (existing), `writeJSON`, `errorResponse` (existing).
- Produces: routes `POST /voice/record/start`, `POST /voice/record/stop`, `GET /voice/record/status` (all `?name=<session>`; status with no name lists all). Response type `voiceRecordResponse{Name, SessionID, Recording bool, LogPath string, Since string}`.

- [ ] **Step 1: Write the failing test**

```go
func TestVoiceRecordStartStop(t *testing.T) {
	h, _, sessName := newTestHTTPServerWithSession(t) // mirror http_capture_send_test.go setup
	h.daemon.captureHook = func(context.Context, string, bool) (string, error) {
		return "anchor line alpha one\nanchor line bravo two\n", nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/voice/record/start?name="+sessName, nil)
	h.handleVoiceRecordStart(rec, req)
	if rec.Code != 200 {
		t.Fatalf("start code %d body %s", rec.Code, rec.Body.String())
	}
	var resp voiceRecordResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Recording || resp.LogPath == "" {
		t.Fatalf("expected recording=true with log path, got %+v", resp)
	}

	rec2 := httptest.NewRecorder()
	h.handleVoiceRecordStop(httptest.NewRecorder(), httptest.NewRequest("POST", "/voice/record/stop?name="+sessName, nil))
	_ = rec2

	rec3 := httptest.NewRecorder()
	h.handleVoiceRecordStatus(rec3, httptest.NewRequest("GET", "/voice/record/status?name="+sessName, nil))
	var st voiceRecordResponse
	json.Unmarshal(rec3.Body.Bytes(), &st)
	if st.Recording {
		t.Fatalf("expected recording=false after stop, got %+v", st)
	}
}
```

Use the existing test scaffolding in `internal/daemon/http_capture_send_test.go` to construct `h` (`*HTTPServer`) and a registered session; reuse its helper rather than inventing a new one if present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestVoiceRecordStartStop -v`
Expected: FAIL — `h.handleVoiceRecordStart undefined`.

- [ ] **Step 3: Write minimal implementation**

Register in the route block (near `/session/send`):

```go
	mux.HandleFunc("/voice/record/start", h.handleVoiceRecordStart)
	mux.HandleFunc("/voice/record/stop", h.handleVoiceRecordStop)
	mux.HandleFunc("/voice/record/status", h.handleVoiceRecordStatus)
```

Handlers:

```go
type voiceRecordResponse struct {
	Name      string `json:"name,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Recording bool   `json:"recording"`
	LogPath   string `json:"log_path,omitempty"`
	Since     string `json:"since,omitempty"`
}

func (h *HTTPServer) voiceSetRecording(w http.ResponseWriter, r *http.Request, on bool) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("session not found: %s", name)})
		return
	}
	snap := sess.Snapshot()
	logPath, err := h.daemon.SetRecording(snap.ID, on)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	recOn, _, since := h.daemon.RecordingStatus(snap.ID)
	resp := voiceRecordResponse{Name: name, SessionID: snap.ID, Recording: recOn, LogPath: logPath}
	if !since.IsZero() {
		resp.Since = since.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *HTTPServer) handleVoiceRecordStart(w http.ResponseWriter, r *http.Request) {
	h.voiceSetRecording(w, r, true)
}

func (h *HTTPServer) handleVoiceRecordStop(w http.ResponseWriter, r *http.Request) {
	h.voiceSetRecording(w, r, false)
}

func (h *HTTPServer) handleVoiceRecordStatus(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		// No name → list all recording sessions.
		ids := h.daemon.recordingSessions()
		out := make([]voiceRecordResponse, 0, len(ids))
		for _, id := range ids {
			on, lp, since := h.daemon.RecordingStatus(id)
			vr := voiceRecordResponse{SessionID: id, Recording: on, LogPath: lp}
			if !since.IsZero() {
				vr.Since = since.Format(time.RFC3339)
			}
			out = append(out, vr)
		}
		writeJSON(w, http.StatusOK, map[string]any{"recording": out})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("session not found: %s", name)})
		return
	}
	snap := sess.Snapshot()
	on, lp, since := h.daemon.RecordingStatus(snap.ID)
	resp := voiceRecordResponse{Name: name, SessionID: snap.ID, Recording: on, LogPath: lp}
	if !since.IsZero() {
		resp.Since = since.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestVoiceRecord -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/http.go internal/daemon/http_voice_test.go
git commit -m "feat(http): /voice/record/start|stop|status shims over daemon recording"
```

---

### Task 8: Single-screen babysit context — /babysit/new?name= + screen_logs

**Files:**
- Modify: `internal/daemon/babysit.go` (accept `name=`; add `ScreenLogs` to `BabysitContext` + responses; enable recording on mint)
- Test: `internal/daemon/babysit_test.go` (append cases)

**Interfaces:**
- Consumes: `Daemon.SetRecording`, `Daemon.ScreenLogPath`, `HTTPServer.findByName`, existing `BabysitContext`, `babysitNewResponse`, `randomToken`, `store.PutBabysitContext`.
- Produces: `BabysitContext.ScreenLogs map[string]string` (paneName → log path) + same field on `babysitNewResponse`; `/babysit/new` accepts `name=<session>` as a single-screen alternative to `project=`.

- [ ] **Step 1: Write the failing test**

```go
func TestBabysitNewByName(t *testing.T) {
	h, _, sessName := newTestHTTPServerWithSession(t)
	h.daemon.captureHook = func(context.Context, string, bool) (string, error) { return "x anchor line one two\n", nil }

	rec := httptest.NewRecorder()
	h.handleBabysitNew(rec, httptest.NewRequest("POST", "/babysit/new?name="+sessName, nil))
	if rec.Code != 200 {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	var resp babysitNewResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Panes) != 1 || resp.Panes[0].Name != sessName {
		t.Fatalf("expected exactly the named pane, got %+v", resp.Panes)
	}
	if resp.ScreenLogs[sessName] == "" {
		t.Fatalf("expected a screen_logs entry for %s, got %+v", sessName, resp.ScreenLogs)
	}
	// Minting by name enabled recording (decoupled — stays on).
	if on, _, _ := h.daemon.RecordingStatus(resp.Panes[0].SessionID); !on {
		t.Fatal("expected recording enabled after by-name mint")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestBabysitNewByName -v`
Expected: FAIL — `resp.ScreenLogs undefined` (and the by-name branch missing).

- [ ] **Step 3: Write minimal implementation**

Add the field to `BabysitContext` (in `babysit.go`):

```go
	ScreenLogs map[string]string `json:"screen_logs,omitempty"` // pane name → screen-recording log path
```

Add the same field to `babysitNewResponse`:

```go
	ScreenLogs map[string]string `json:"screen_logs,omitempty"`
```

At the top of `handleBabysitNew`, before the project branch, handle `name=`:

```go
	if name := r.URL.Query().Get("name"); name != "" {
		sess := h.findByName(name)
		if sess == nil {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("session not found: %s", name)})
			return
		}
		snap := sess.Snapshot()
		// Enable recording for this screen (decoupled from any voice client).
		logPath, err := h.daemon.SetRecording(snap.ID, true)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: fmt.Sprintf("enable recording: %v", err)})
			return
		}
		st := h.daemon.State()
		if st == nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "daemon state store unavailable"})
			return
		}
		ttl := DefaultBabysitTTL
		if v := r.URL.Query().Get("ttl"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				ttl = time.Duration(secs) * time.Second
			}
		}
		token, err := randomToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "token generation failed"})
			return
		}
		now := time.Now()
		pane := BabysitPane{Name: snap.Name, SessionID: snap.ID, TmuxTarget: snap.TmuxTarget, State: string(snap.State), CWD: snap.CWD}
		ctx := BabysitContext{
			ContextID:  "ctx-" + token[:12],
			Token:      token,
			Project:    "screen:" + snap.Name,
			RepoCWD:    snap.CWD,
			Panes:      []BabysitPane{pane},
			ScreenLogs: map[string]string{snap.Name: logPath},
			Server:     r.URL.Query().Get("server"),
			CreatedAt:  now,
			ExpiresAt:  now.Add(ttl),
		}
		blob, err := json.Marshal(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "marshal context failed"})
			return
		}
		if err := st.PutBabysitContext(token, blob); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: fmt.Sprintf("persist context: %v", err)})
			return
		}
		h.daemon.logger.Info("babysit context minted (single screen)", "screen", snap.Name, "context_id", ctx.ContextID, "log", logPath)
		writeJSON(w, http.StatusOK, babysitNewResponse{
			ContextID:  ctx.ContextID,
			Token:      token,
			Project:    ctx.Project,
			ConnectURL: connectURL(ctx.Server, token),
			RepoCWD:    ctx.RepoCWD,
			PlanRefs:   []string{},
			Panes:      ctx.Panes,
			ScreenLogs: ctx.ScreenLogs,
			ExpiresAt:  ctx.ExpiresAt.Format(time.RFC3339),
		})
		return
	}
```

(The existing project branch below is unchanged. Project-scoped contexts leave `ScreenLogs` nil for now — auto-recording project panes is a post-MVP open item.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run 'TestBabysit' -v`
Expected: PASS (new case + existing babysit cases still green).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/babysit.go internal/daemon/babysit_test.go
git commit -m "feat(babysit): single-screen context (?name=) + screen_logs; enable recording on mint"
```

---

### Task 9: arcmux-cli `voice` subcommand + handoff

**Files:**
- Create: `cmd/arcmux-cli/voice.go`
- Modify: `cmd/arcmux-cli/main.go` (dispatch `voice` to the new handler)
- Test: `cmd/arcmux-cli/voice_test.go`

**Interfaces:**
- Consumes: the HTTP endpoints from Tasks 7–8 (`/voice/record/{start,stop,status}`, `/babysit/new?name=`). The CLI calls the daemon's HTTP server (default `127.0.0.1:7777`, overridable via `--http`/env, mirroring how the babysit/HTTP base URL is configured elsewhere).
- Produces: `func runVoice(args []string, httpBase string, out io.Writer) error` dispatching subcommands `start <name>`, `stop <name>`, `status [<name>]`, and the bare `voice <name>` (start recording + mint context + print connect handle).

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunVoiceStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/voice/record/start" || r.URL.Query().Get("name") != "agents:1" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": "agents:1", "recording": true, "log_path": "/tmp/x.screen.log"})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runVoice([]string{"start", "agents:1"}, srv.URL, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/tmp/x.screen.log") {
		t.Fatalf("expected log path in output, got %q", out.String())
	}
}

func TestRunVoiceConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/voice/record/start":
			json.NewEncoder(w).Encode(map[string]any{"recording": true, "log_path": "/tmp/x.screen.log"})
		case "/babysit/new":
			json.NewEncoder(w).Encode(map[string]any{"connect_url": "ws://h/v1/realtime/converse?context=tok", "context_id": "ctx-abc"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	if err := runVoice([]string{"agents:1"}, srv.URL, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "converse?context=tok") {
		t.Fatalf("expected connect URL in output, got %q", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/arcmux-cli/ -run TestRunVoice -v`
Expected: FAIL — `undefined: runVoice`.

- [ ] **Step 3: Write minimal implementation**

`cmd/arcmux-cli/voice.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

func voiceGetJSON(method, base, path string, q url.Values) (map[string]any, error) {
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return m, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return m, nil
}

// runVoice dispatches the `arcmux-cli voice` subcommands.
func runVoice(args []string, httpBase string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: arcmux-cli voice <name> | start <name> | stop <name> | status [<name>]")
	}
	switch args[0] {
	case "start", "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: arcmux-cli voice %s <name>", args[0])
		}
		path := "/voice/record/start"
		if args[0] == "stop" {
			path = "/voice/record/stop"
		}
		m, err := voiceGetJSON("POST", httpBase, path, url.Values{"name": {args[1]}})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "recording=%v log_path=%v\n", m["recording"], m["log_path"])
		return nil
	case "status":
		q := url.Values{}
		if len(args) >= 2 {
			q.Set("name", args[1])
		}
		m, err := voiceGetJSON("GET", httpBase, "/voice/record/status", q)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	default:
		// Bare `voice <name>`: enable recording, mint a single-screen context,
		// print the connect handle voxtop should open.
		name := args[0]
		if _, err := voiceGetJSON("POST", httpBase, "/voice/record/start", url.Values{"name": {name}}); err != nil {
			return err
		}
		m, err := voiceGetJSON("POST", httpBase, "/babysit/new", url.Values{"name": {name}})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Voice context for %s ready.\n  connect: %v\n  context: %v\n", name, m["connect_url"], m["context_id"])
		fmt.Fprintln(out, "Open this handle in the voxtop voice client to talk to the screen.")
		return nil
	}
}
```

In `cmd/arcmux-cli/main.go`, add a `case "voice":` to the top-level command switch that resolves the HTTP base (reuse the existing daemon-address resolution; default `http://127.0.0.1:7777`) and calls `runVoice(args, httpBase, os.Stdout)`. Match the file's existing dispatch style.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/arcmux-cli/ -run TestRunVoice -v`
Expected: PASS.

- [ ] **Step 5: Full build + suite**

Run: `go build ./... && go test ./...`
Expected: build clean, all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/arcmux-cli/voice.go cmd/arcmux-cli/main.go cmd/arcmux-cli/voice_test.go
git commit -m "feat(cli): arcmux-cli voice — start/stop/status + bare connect handoff"
```

---

## Deferred to a follow-up (not in this plan)

- **`--voice` flag on spawn/attach** — auto-enable recording when starting a
  screen. Small addition once the `voice` command lands; left out to keep this
  plan focused on the recording substrate. (Tracked under the same Beads issue.)
- **voxtop companion work** (separate repo, separate plan): `read_screen`
  line-window tool over the log file, single-screen scope handling in
  `babysit_tools.py` / `realtime_voice.py`, extend `scripts/smoke-babysit-ws.py`.
- Post-MVP: persist recording intent across daemon restarts; log rotation / size
  caps; auto-record project-scoped babysit panes; optional log timestamps.

## Self-Review

- **Spec coverage:** screenstitch port (Tasks 1–4) ✓; 1s dedicated capture loop +
  append log (Task 5) ✓; recording lifecycle decoupled from client, idempotent,
  close teardown, kept-on-stop (Task 6) ✓; control surface HTTP + CLI (Tasks 7,9)
  ✓; single-screen babysit context + `screen_logs` (Task 8) ✓; log-format
  contract enforced by the recorder writing bare lines (Task 5) ✓. gRPC RPCs were
  in the spec as one of three transport options — this plan ships HTTP + CLI
  (matching the existing babysit pattern) and omits new gRPC RPCs to avoid a
  proto/regen cycle; noted here as an intentional scope decision. `--voice` flag
  is deferred above (explicitly called out).
- **Placeholders:** none — every code step has complete code. Test-helper names
  (`newTestDaemon`, `installFakeSession`, `newTestHTTPServerWithSession`) are
  flagged to be matched against the existing `internal/daemon/*_test.go`
  scaffolding rather than assumed verbatim.
- **Type consistency:** `SetRecording(sessionID string, on bool) (string, error)`,
  `RecordingStatus(...) (bool, string, time.Time)`, `ScreenLogPath`,
  `recorder`/`newRecorder`, `screenstitch.Merger`/`NewMerger`/`Ingest`,
  `voiceRecordResponse`, `BabysitContext.ScreenLogs`, `runVoice` — all used
  consistently across the tasks that produce and consume them.
