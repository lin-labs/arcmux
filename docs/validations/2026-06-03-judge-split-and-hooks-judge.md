# Validation — judge split + hooks-based judge

Date: 2026-06-03 PT
Branch: `feat/judge-split-and-hooks-judge`
Spec: docs/superpowers/specs/2026-06-03-judge-split-and-hooks-judge-design.md

## 4.0 Change classification

**internal-behavior**, config-gated. `[delivery].judge` defaults to `typesafe`,
so production delivery is unchanged until the value is flipped to `hooks`.

## 4.1–4.3 Results

| Dimension | Result | Evidence |
|---|---|---|
| Static (gofmt) | PASS | `gofmt -l internal/ cmd/` → empty |
| Static (vet) | PASS | `go vet ./...` → exit 0 |
| Unit + integration | PASS | `go test ./...` → 19/19 packages `ok` (judge_hooks, judge_typesafe, sessionstate, hookevent, config delivery, codex installer, daemon) |
| Regression (substrate e2e) | PASS | `bin/arcmux-test` → bootstrap/pulse-wake/grpc-rt all `[pass]`, overall pass |

## 4.7 E2E rung exercised — real-deps (rung 4)

- **Daemon boot with `judge=hooks`** (real binary, isolated config): logs
  `delivery judge selected judge=hooks`; installs
  `<claude_hook_dir>/hooks/arcmux-session-hook.sh` and
  `<codex_hook_dir>/arcmux-codex-hook.sh`; same wiring applies to the `demo`
  profile. No startup error.
- **Hook → state → judge data path** (real `arcmux` binary):
  - Claude generic hook: stdin `{"hook_event_name":"UserPromptSubmit"}` →
    state `last_event=prompt_submit working=true turn_count=1`.
  - Codex bridge: `UserPromptSubmit`/`PreToolUse(apply_patch)`/`Stop` →
    `last_event=turn_end last_tool=apply_patch working=false`; legacy notify
    `agent-turn-complete` → `turn_end`.
  - `HooksJudge` unit tests confirm `last_prompt_submit_at >= DeliveryStartedAt`
    ⇒ `StateIngested source=hooks`, else heuristic fallback.

## 4.7 Rung NOT exercised — live-agent gating (rung 5)

A real claude/codex remote-control session firing its native hook through a
daemon-gated `SendPrompt` with `judge=hooks` was not run (needs the live
elonco/agent stack). To exercise: set `[delivery].judge="hooks"` in the e2e
daemon config and run `make validate-e2e`.

## 4.8 Ship decision

**PASS for merge.** All static/unit/integration/regression dimensions green;
real-deps E2E (rung 4) exercised. Rung 5 is the deliberate test→prod rollout:
default `typesafe` keeps production unaffected; flipping to `hooks` is the
gated promotion. Independent codex adversarial review completed; all findings
addressed.

## Independent review

Codex reviewed the full diff (no CRITICAL). HIGH/MEDIUM/LOW findings all
addressed: claude stdin-JSON parsing, codex env-loading in the launch path,
archive-under-flock, restore re-init, reject empty SessionStateDir, hardened
CLI flag parsing, documented the timestamp-vs-identity caveat.
