# Per-Agent Voice via Babysit + arcmux Screen Recording — Design

**Date:** 2026-06-16
**Status:** Proposed (brainstorm complete, awaiting spec review → plan).

## Summary

A user can run a **voice conversation against one particular agent screen** (a
named arcmux session) — to hear what the agent is doing and, after confirmation,
send it keystrokes.

**This is not a new feature. It is an evolution of the existing `babysit`
feature**, which already implements a voice control plane over arcmux panes:

- **arcmux** already mints a `BabysitContext` (`/babysit/new`) and resolves it
  (`/babysit/context`); the context carries panes, `repo_cwd`, `plan_refs`, and
  a `connect_url` = `wss://<host>/v1/realtime/converse?context=<token>`.
- **voxtop** already hosts the whole voice stack: `MODES["babysit"]` in
  `realtime_voice.py`, the toolset in `babysit_tools.py` (`list_panes`,
  `capture_pane`, `send_to_pane`, `write_plan`, `bead_*`) with a two-phase
  confirm gate, and a **websocket test client** at `scripts/smoke-babysit-ws.py`.

Today babysit is **project-scoped** and reads panes by **live HTTP polling**
(`capture_pane` → arcmux `/session/capture`). This design makes two coordinated
changes across the two repos:

1. **Single-screen scope.** Babysit can be scoped to one named agent screen, not
   just a project. `arcmux-cli voice <name>` is the entry point.
2. **Recording becomes the screen source.** arcmux gains an aggressive recording
   capability: while a screen is in voice mode, arcmux captures it every second,
   stitches consecutive captures into a **deduplicated, append-only transcript**
   (the mission-control algorithm, ported to Go), and writes it to a per-session
   log file. **voxtop owns none of the capture mechanics** — no capture "types,"
   no history flags, no live polling. It treats the file as a plain, ever-growing
   text transcript (newest lines at the bottom) and reads whatever **line window**
   it needs: the tail for "what's on screen now," earlier ranges to understand
   how the agent got there. voxtop holds no live screen connection to arcmux.

Keystrokes continue to flow through arcmux's existing `/session/send` (what
`send_to_pane` already calls). arcmux adds **recording**; everything voice stays
in voxtop's babysit mode.

## Relationship to babysit (binding: one feature, not two)

| | Today (babysit) | After this design |
|---|---|---|
| Scope | project (`/babysit/new?project=`) | project **or** single screen (`/babysit/new?name=`) |
| Screen read | live HTTP `/session/capture` per call | read a **line window** of arcmux's deduped log file (no capture types, no live polling) |
| Screen freshness | one frame per poll | continuous 1s deduped transcript while recording; newest at the bottom |
| Send keys | arcmux `/session/send` | unchanged |
| Confirm gate | two-phase in voxtop | unchanged |
| Test client | `voxtop/scripts/smoke-babysit-ws.py` | extend it for single-screen contexts |
| Voice stack | voxtop | voxtop |

We do **not** add a parallel "voice mode." Per-agent voice is babysit with a
one-pane scope and recording turned on for that pane.

## Scope boundary

| Concern | Owner |
|---|---|
| `internal/screenstitch` dedup engine (Go port of mission-control) | **arcmux** |
| Aggressive 1s capture → stitch → append log | **arcmux** |
| Recording lifecycle (enable/cancel/status), decoupled from any client | **arcmux** |
| Single-screen babysit context (`/babysit/new?name=`) + screen-log path in context | **arcmux** |
| `arcmux-cli voice …` + `--voice` (start recording, mint context, hand off) | **arcmux** |
| Screen-read tool: read a **line window** of the log file (tail / range); single-screen scope handling | voxtop (babysit) |
| Voice call, relay, converse WS, command rewrite, confirm gate, sanitization | voxtop (babysit) |
| WebSocket test client | voxtop — extend `scripts/smoke-babysit-ws.py` |
| Sending keys | arcmux's **existing** `/session/send` (unchanged) |

## Why this shape

- **The voice loop already works in voxtop's babysit mode.** Re-using it (rather
  than building a second voice path) is the whole point of your directive.
- **A file is the simplest integration surface, and decoupled.** voxtop reads a
  local file; arcmux stays unaware of voxtop and of any voice client. Recording
  has no dependency on a client being connected.
- **A deduped transcript beats single-frame polling.** The brain can summarize
  what *happened* across the last minute, not just the current screen.
- **Recording is independently useful** (review, debugging, future
  summarization) regardless of voice.

## Components

### 1. `internal/screenstitch` — the dedup engine (new arcmux package)

A direct port of mission-control's `src/mc_data/frame_merge.rs`
(`~/Tools/mission-control`). Pure functions, no I/O, exhaustively table-tested
against golden claude/codex captures. The riskiest unit; built and verified
first.

| Go function | Rust origin | Purpose |
|---|---|---|
| `StripUniversal(line) string` | `strip_universal` | Strip ANSI/CSI escapes + trailing whitespace. |
| `Normalize(raw) []string` | `normalize` | Split a raw capture into stripped lines. |
| `TranscriptRegion(frame) []string` | `transcript_region` | Drop bottom chrome (composer box bracketed by `─` rules around `❯`, tmux status bar) so it can't fight the scroll vote. |
| `ScrollDelta(prev, cap) (delta *int, votes int)` | `scroll_delta` | Anchor-vote the scroll offset. Unique high-entropy lines (`AnchorMinLen=12`, `AnchorMinDistinct=6`) vote `d = i_prev − j_cap`; consensus wins (`MinAgreeingAnchors=2`). Spinners/timers don't anchor. |
| `NewLines(prev, cap) []string` | `new_lines` | `0` → nothing new; `d>0` → last `d` lines of `cap`; no overlap → whole frame (a gap). |
| `MaskVolatile(a, b) string` | `mask_volatile` | Mask the differing span between two proven-same lines (spinner/timer), learned by diff. Peels the live status line off the tail. |

Tuning constants ported as exposed package constants.

**Stitch contract per tick:** normalize capture → `TranscriptRegion` → `NewLines`
vs. the previous tick's region → mask the volatile tail → append new lines. An
idle tick yields zero new lines.

### 2. Recording loop (new, in the daemon)

One goroutine per recorded session:

- A **dedicated 1-second ticker** (independent of the pulse loop — predictable
  cadence over minimal load, per the capture-timer decision).
- Each tick: `daemon.Capture(sessionID, history=false)` → stitch contract →
  append new lines to the log.
- Previous tick's normalized region held in memory per session.
- Append-only.

**Log file:** `<session-state-dir>/<session-id>.screen.log`, beside the
`s-<id>.json` records (i.e. `~/data/arcmux/sessions/`), resolved via the existing
session-state-dir resolver. **Kept on stop**; a fresh `start` truncates. Pruning
is manual / out of scope.

**Log format contract (what voxtop depends on):** a plain UTF-8 text file, one
screen line per file line, **append-only with the newest content at the bottom**.
No headers, no framing, no per-tick markers — just the deduped transcript, so any
consumer can `tail`/seek/slice it by line with zero knowledge of how it was
produced. This is the *entire* arcmux→voxtop screen interface. (Whether to
interleave lightweight timestamps is a deferred open item; the MVP is bare
lines.)

### 3. Recording lifecycle — decoupled from any client (key constraint)

Recording is a **server-side capability owned by arcmux**, unaware of voxtop or
any voice client:

- **Enable / cancel are explicit.** Enable starts the loop; cancel stops it and
  leaves the log.
- **A voice client disconnecting does NOT stop recording.** arcmux never learns
  that a client connected or dropped.
- **Context expiry does NOT stop recording.** The babysit context TTL governs the
  WS credential, not the recording loop.
- **Enable is idempotent** (returns the existing log path).
- **Auto-stop only on session close** (the pane is gone).
- **Status is queryable** (recording? log path? since when? bytes?).
- Recording state lives in the daemon's in-memory session registry; not persisted
  across daemon restarts in the MVP (a restart stops recording).

### 4. Single-screen babysit context (extend existing arcmux endpoints)

- `POST /babysit/new` accepts **`name=<session>`** as an alternative to
  `project=<slug>`. A by-name context resolves to exactly one pane (the named
  session). Project scope is unchanged.
- `BabysitContext` (and the `/babysit/new` + `/babysit/context` responses) gains
  a **`screen_logs` field** mapping each pane → its recording-log path, so voxtop
  knows which file to read. For project scope this lists every recorded pane; for
  single-screen scope, one entry.
- Minting a by-name context (or `arcmux-cli voice <name>`) **enables recording**
  on the resolved pane(s). Per §3, recording then outlives the context.

### 5. Control surface (new arcmux-cli `voice` subcommand)

Thin wrappers over a daemon `SetRecording` / `RecordingStatus`, exposed via gRPC
(`SetVoiceRecording`/`VoiceRecordingStatus`) + HTTP shims
(`/voice/record/start|stop|status`, bearer-auth/loopback rules as today):

- `arcmux-cli voice start <name>` → enable recording; print log path.
- `arcmux-cli voice stop <name>` → cancel recording.
- `arcmux-cli voice status [<name>]` → recording sessions + log paths.
- `arcmux-cli voice <name>` (**headline**) → enable recording, mint a
  single-screen babysit context, and **hand off to voxtop** (open the voice
  client / print the `connect_url`). Handoff mechanism resolved in the plan.
- `--voice` on spawn/attach → auto-enable recording for that screen + same handoff.

## Data flow (end to end, across both repos)

```
arcmux-cli voice <name>
  ├─ arcmux: SetRecording(session, true)
  │    └─ goroutine: every 1s  Capture → screenstitch → append
  │         └─ ~/data/arcmux/sessions/<id>.screen.log   (deduped, append-only)
  ├─ arcmux: POST /babysit/new?name=<name>  → context {token, pane, screen_logs, repo_cwd}
  └─ hand off connect_url = wss://<host>/v1/realtime/converse?context=<token>

voxtop babysit mode (single-screen scope)
  ├─ on connect: GET /babysit/context?context=<token>   (resolve scope + screen_logs)
  ├─ opens xAI realtime voice call
  ├─ read_screen(tail N | lines A..B) → reads  <id>.screen.log   ← summarize "what's happening" (no live arcmux conn)
  └─ on a spoken command:  rewrite → keys → two-phase confirm → sanitize
       └─ send_to_pane → arcmux POST /session/send?name=<name>&text=…&confirm=1   (EXISTING)
            └─ keys land → next 1s tick records the result
```

## Testing strategy

- **`screenstitch` unit tests** (highest fidelity sans live agent). Golden
  fixtures from real consecutive claude/codex captures: idle → 0 new lines;
  scroll-by-N → exactly N; gap/clear → whole frame; volatile timer masked, not
  duplicated. Port mission-control's `tests/mc_data_frame_merge.rs`.
- **Recording loop tests.** Stubbed `Capture` returning a scripted frame
  sequence; assert log contents = expected deduped transcript; idle ticks append
  nothing.
- **Lifecycle tests.** Idempotent enable; cancel stops appends and keeps the
  file; session close tears down; context expiry does **not** stop recording;
  assert there is **no** client-disconnect path that stops recording.
- **Context tests.** `/babysit/new?name=` resolves one pane; `screen_logs`
  populated; project scope still works.
- **Control-surface tests.** gRPC + HTTP start/stop/status; CLI wiring.
- **End-to-end (voxtop, via the existing test client).** Extend
  `scripts/smoke-babysit-ws.py` to connect a single-screen context against a
  running claude/codex pane (typed turns): screen summary → "what's happening" →
  command → confirm → keys land → next tick records the result. This is the true
  acceptance test for the whole feature.

## Build order

1. **`internal/screenstitch`** — port + golden tests in isolation.
2. **Recording loop** — dedicated 1s timer, stitch, append; loop tests with a
   stubbed `Capture`.
3. **Lifecycle** — enable/cancel/status; idempotency; close teardown; the
   client-/context-decoupling tests.
4. **Single-screen context** — `/babysit/new?name=` + `screen_logs` field.
5. **Control surface** — gRPC RPCs, HTTP shims, `arcmux-cli voice …` + `--voice`.
6. **voxtop babysit changes** (companion repo): replace `capture_pane`'s
   capture-flag semantics with a `read_screen` tool that reads a line window of
   the log file (tail / range); single-screen scope; extend `smoke-babysit-ws.py`.

## Open items (resolve in the plan / during build)

- Confirm the session-state dir for the log (`~/data/arcmux/sessions/` vs. the
  protocol dir `~/data/mux/sessions/`; the `s-<id>.json` records are the anchor).
- voxtop `read_screen` tool surface: which line-window params to expose to the
  voice model (e.g. `tail N` and `lines from..to`) and sensible defaults so the
  brain opens by reading the tail, then pages backward on demand. (Semantics
  settled: line windows over the log; no live capture, no capture types.)
- Whether to interleave lightweight timestamps in the log (MVP: bare lines).
- Handoff mechanism for `arcmux-cli voice <name>` → voxtop.
- Whether `--voice` exists on `exec`/attach paths or only interactive spawns.
- Post-MVP: persist recording intent across daemon restarts; log rotation / size
  caps; auto-start recording for project-scoped babysit panes too.
