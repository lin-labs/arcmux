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

## 4.7 Live-agent gating (rung 5) — EXERCISED, PASS

Added a committed, repeatable real-agent e2e: `scripts/e2e/hooks-judge-live.sh`
(`make validate-e2e-hooks`). It starts an isolated real `arcmux` daemon with
`[delivery].judge=hooks`, spawns a **live claude** via `arcmux-cli create`
(real remote-control handshake in real tmux), delivers a prompt with
`ConfirmDelivery=true`, and asserts both seams.

Run 2026-06-03 22:09 PT — **PASS**:

- ASSERT 1 (native hook → contract): `sessions/<id>.json` →
  `last_event=prompt_submit, working=true, turn_count=1`. Claude's own
  `UserPromptSubmit` hook fired `arcmux hook` and wrote the state doc.
- ASSERT 2 (judge gated delivery): `prompt_ingested` event with
  `judge_source=hooks, judge_state=ingested, judge_confidence=0.97,
  work_started_probability=0.97`.

Two real gaps this e2e surfaced and fixed:

1. **Claude hook never registered.** arcmux dropped `arcmux-session-hook.sh`
   but nothing registered it in `~/.claude/settings.json` (no `UserPromptSubmit`
   entry), so a live claude's native hook would never fire. Registered the 4
   events (UserPromptSubmit/PreToolUse/PostToolUse/Stop), preserving existing.
   NOTE: this registration is machine-local, not yet auto-applied by arcmux —
   see follow-up beads issue for optional daemon-side auto-registration.
2. **Claude folder-trust gate not handled.** The claude profile had no
   `TrustPromptPattern`, so the handshake hung at claude's "trust this folder"
   prompt in any untrusted cwd. Added `TrustPromptPattern="trust this folder"`
   + `TrustResponse="Enter"`, and made `waitForReadyPattern` resolve trust
   prompts that render after the one-shot Phase-2 check.

codex rung-5 is the same script with `AGENT=codex`, runnable after trusting the
codex hooks via codex `/hooks` (deferred to the operator).

## 4.8 Ship decision

**PASS for merge.** All static/unit/integration/regression dimensions green;
**real-deps E2E rung 5 exercised with a live claude** (judge_source=hooks).
Default judge stays `typesafe`, so flipping to `hooks` remains the deliberate
test→prod rollout. Independent codex adversarial review completed; all findings
addressed.

## Independent review

Codex reviewed the full diff (no CRITICAL). HIGH/MEDIUM/LOW findings all
addressed: claude stdin-JSON parsing, codex env-loading in the launch path,
archive-under-flock, restore re-init, reject empty SessionStateDir, hardened
CLI flag parsing, documented the timestamp-vs-identity caveat.
