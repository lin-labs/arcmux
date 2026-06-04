# Split judges + hooks-based judging

Date: 2026-06-03
Status: Approved (design), implementation pending
Branch: `fix/claude-handshake-and-working-idle` (or a fresh feature branch)

## Problem

`internal/delivery/judge.go` currently bundles three concerns into one file:
the `Judge` interface + shared types, the screen-scraping `HeuristicJudge`, and
the Typesafe-API `TypesafeJudge`. There is no way to judge prompt ingestion from
the **agent's own hook events**, which are the most reliable signal available
(an agent firing `UserPromptSubmit` is ground truth that the prompt was
ingested, far stronger than guessing from a screen scrape or paying a Typesafe
round-trip).

We want:
1. Typesafe to be **one** judge among several, isolated in its own file.
2. A new **hooks judge** that reads cached per-session state mutated by the
   agents' hooks, with a screen-heuristic fallback for the window before the
   first hook event lands.
3. **Config** to pick exactly one judge. Typesafe stays the default; we flip to
   `hooks` manually once it is proven in tests + a few production runs.
4. **Proper hooks for both claude and codex** that mutate per-session state
   files under `~/data/arcmux/sessions/`.

## Non-goals

- No automatic runtime fallback *between* typesafe and hooks. Selection is an
  explicit single judge chosen by config (a judge MAY still fall back to the
  always-available heuristic internally — that is not "typesafe vs hooks").
- No freshness-window / auto-promotion logic. "Prefer cached once stable" is
  realized by manually switching `delivery.judge = "hooks"` in config.

## Architecture

### A. Judge file split (`internal/delivery/`)

- **`judge.go`** — shared core only: `State`, `Evidence`, `Assessment`, the
  `Judge` interface, `HeuristicJudge`, shared string helpers
  (`normalizeScreen`, `containsFold`, `containsPromptFragment`,
  `promptFragments`, `trimForDecision`), and the config-driven `NewJudge`
  factory.
- **`judge_typesafe.go`** — `TypesafeJudge`, the `evaluator` interface, the
  Typesafe prompt construction, and response→Assessment mapping. Pure move; no
  behavior change.
- **`judge_hooks.go`** — `HooksJudge`: reads the session state file and maps it
  to an `Assessment`; delegates to an injected heuristic fallback when no hook
  state is usable yet.

`Evidence` gains two fields so a stateless judge can locate + time-bound the
session's hook state:
- `SessionID string`
- `DeliveryStartedAt time.Time` — set once by the caller at the top of
  `ensurePromptIngested` (right after the prompt was submitted). The hooks judge
  treats a `last_prompt_submit_at >= DeliveryStartedAt` as proof of ingestion.

### B. Session state files (the "cached data")

arcmux tracks every watched session as a JSON state doc:

- Live: `~/data/arcmux/sessions/<id>.json`
- Archived on unwatch/teardown: `~/data/arcmux/sessions/archived/<id>.json`

Directory derives from the existing `Pulse.DataRoot` (`~/data`) →
`<DataRoot>/arcmux/sessions`, matching the `<DataRoot>/arcmux/<project>/state.bolt`
convention. A new config field `Hooks.SessionStateDir` (default derived, also
tilde-expanded in `expandConfigPaths`) lets it be overridden and keeps the
daemon and the `arcmux hook` CLI agreeing on one path.

**Schema** (`internal/session` or `internal/hooks` — a small typed struct with
atomic read-modify-write):

```json
{
  "session_id": "s-abc123",
  "agent": "claude",
  "created_at": "2026-06-03T18:00:00Z",
  "updated_at": "2026-06-03T18:00:04Z",
  "last_event": "prompt_submit",
  "last_tool": "Bash",
  "working": true,
  "turn_count": 2,
  "events_seen": 17,
  "last_prompt_submit_at": "2026-06-03T18:00:03Z",
  "last_turn_end_at": "2026-06-03T17:59:10Z"
}
```

Writes are atomic: write to `<id>.json.tmp` then `os.Rename`. The CLI is the
single writer; concurrent hook invocations serialize via a per-file lock
(flock) around the read-modify-write.

### C. `arcmux hook` subcommand (single writer)

New subcommand alongside `arcmux hook-env`, dispatched in `cmd/arcmux/main.go`,
implemented in `cmd/arcmux/hookevent.go`. The agent hook scripts call it instead
of raw shell `printf`. It is **agent-agnostic**: it accepts a canonical event
and maps it onto the state doc, so the judge never needs per-agent logic.

```
arcmux hook \
  --session "$ARCMUX_SESSION_ID" \
  --agent claude \
  --event prompt_submit|tool_start|tool_end|turn_end|notification \
  [--tool Bash] \
  [--state-dir <override>]
```

Canonical event → state mutation:

| `--event`       | mutation                                                        |
|-----------------|-----------------------------------------------------------------|
| `prompt_submit` | `working=true`, `last_prompt_submit_at=now`, `turn_count++`     |
| `tool_start`    | `working=true`, `last_tool=<tool>`                              |
| `tool_end`      | `working=true` (still in turn)                                  |
| `turn_end`      | `working=false`, `last_turn_end_at=now`                         |
| `notification`  | record only (`last_event`, `updated_at`)                        |

Every call bumps `events_seen`, sets `last_event` + `updated_at`, and (cheaply)
keeps appending to the existing per-session JSONL audit so nothing is lost.

Missing/empty `ARCMUX_SESSION_ID` → exit 0 silently (so the generic hook stays
safe to install globally, exactly like today).

### Claude hook script

`internal/hooks/hooks.go` `genericHookScript` is rewritten to resolve the arcmux
binary and call `arcmux hook`, translating Claude's `CLAUDE_HOOK_EVENT_TYPE`
(`UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`/`SubagentStop`,
`Notification`) to the canonical events above. Still no-ops without
`ARCMUX_SESSION_ID`. Idempotent install unchanged.

### Codex hook (delegated to an arcmux codex session)

Codex has its own hook mechanism. The codex agent implements the codex-side hook
to call the same `arcmux hook` CLI with `--agent codex`, mapping codex's event
types onto the canonical set (at minimum `prompt_submit` and `turn_end`; tool
events if codex exposes them). Contract = this `arcmux hook` interface + the
state schema above. The default codex profile's `HookType` moves from
`codex_output` toward hook-backed state once verified.

### D. Daemon lifecycle

- On session start (where `hooks.Install` + `watcher.Watch` happen,
  `daemon.go:526`/`1176`): initialize `sessions/<id>.json` with
  `created_at`, `agent`, `session_id`.
- Pass `ARCMUX_SESSION_STATE_DIR` (or reuse existing env plumbing) to the agent
  so the hook CLI writes to the right place; the daemon already injects
  `ARCMUX_SESSION_ID` + `ARCMUX_HOOK_OUTPUT_DIR`.
- On `watcher.Unwatch` / teardown (`daemon.go:820`): move
  `sessions/<id>.json` → `sessions/archived/<id>.json` (best-effort, non-fatal).

### E. Config selector

New `[delivery]` table in `internal/config/config.go`:

```toml
[delivery]
judge = "typesafe"   # "typesafe" | "hooks" | "heuristic"
```

- Default `"typesafe"` (preserves current behavior; `NewJudge` with no key set
  behaves as today: typesafe-if-key-present else heuristic).
- `NewJudge(cfg)` reads the value and returns the one selected judge:
  - `heuristic` → `HeuristicJudge{}`
  - `typesafe`  → `TypesafeJudge` (heuristic fallback inside, as today) or
    `HeuristicJudge` if no API key.
  - `hooks`     → `HooksJudge` wrapping a `HeuristicJudge` fallback.
- Unknown value → error at config load.
- `daemon.go:102` wiring changes from `delivery.NewJudge()` to
  `delivery.NewJudge(d.cfg)` (or a small `JudgeConfig` passed in).

## HooksJudge.Assess logic

```
state, err := readSessionState(stateDir, evidence.SessionID)
if err != nil || state == nil:
    return fallback.Assess(ctx, evidence)        // no hook data yet → heuristic

if state.LastPromptSubmitAt >= evidence.DeliveryStartedAt:
    // ground truth: the agent ingested this delivery
    a := Assessment{State: StateIngested, Source: "hooks", Confidence: 0.97}
    a.WorkStartedProbability = 0.5
    if state.Working { a.WorkStartedProbability = 0.97; a.LastTool != "" }
    return a

// hook state exists but predates this delivery → not yet ingested; let the
// controller keep polling. Use heuristic to decide submit/blocked nuance.
return fallback.Assess(ctx, evidence)
```

This keeps the controller's existing `isIngested` / `shouldSubmit` semantics
intact: a confident `hooks` ingested assessment passes the strict path; the
unclear window before the first event behaves exactly like the heuristic does
today.

## Testing

- `judge_hooks_test.go`: state file (none / pre-delivery / post-delivery /
  working) → expected assessment; fallback path.
- `judge_typesafe_test.go`: moved typesafe tests (no behavior change).
- `judge_test.go`: trimmed to heuristic + factory.
- `NewJudge` config selection test (each value, unknown→error, no-key typesafe).
- `cmd/arcmux/hookevent_test.go`: each canonical event mutates the state doc
  correctly; atomic write; missing session id → no-op exit 0; concurrent calls
  serialize.
- Session lifecycle: init on watch, archive-move on unwatch.
- Existing delivery/daemon integration tests stay green with default
  `judge="typesafe"`.

## Build sequence

1. Split files (mechanical move) → tests green.
2. Add `Evidence.SessionID` + `DeliveryStartedAt`; thread through
   `ensurePromptIngested`.
3. Session state struct + atomic read-modify-write + `SessionStateDir` config.
4. `arcmux hook` subcommand + tests.
5. Rewrite Claude generic hook script to call `arcmux hook`.
6. Daemon lifecycle: init + archive-move.
7. `HooksJudge` + config selector + wiring.
8. (Parallel) arcmux codex session implements the codex hook against the
   contract.
9. Validate end to end; flip config to `hooks` only after it proves out.
