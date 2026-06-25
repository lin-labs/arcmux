# Centralized mux state — subscriber contract

`~/data/mux/` is the **protocol** state root: the shared, runtime-agnostic
place where agents' native lifecycle hooks land, readable by any application
— arcmux, mission-control, cmux, or future subscribers. It is deliberately
named after the protocol (mux), not any one application: arcmux is today's
producer, but the contract is the files, not the binary. Agents' hooks write
here **once**, via the `arcmux hook` CLI; subscribers read files instead of
installing their own per-app hooks.

Application-private state stays application-named: `~/data/arcmux/` keeps
arcmux's own substrate (profile registry, bbolt stores) and is not part of
this contract.

## Layout

| Path | What | Writer | Readers |
|---|---|---|---|
| `~/data/mux/sessions/<session_id>.json` | Per-session hook state doc (see schema) | `arcmux hook` (locked read-modify-write) ; seeded/archived by the daemon | hooks judge, mission-control, anyone |
| `~/data/mux/sessions/archived/<session_id>.json` | State docs of ended sessions | daemon | anyone |
| `~/data/mux/hook-output/arcmux-hooks-<session_id>.jsonl` | Raw per-session hook event audit (append-only JSONL) | generic hook script | daemon watcher, anyone |
| `/tmp/arcmux/<session_id>.env` | Per-session env handoff (0600, ARCMUX_* allowlist) | daemon | `arcmux hook-env` only — never source raw |

Config keys: `[hooks] session_state_dir` (default `~/data/mux/sessions`) and
`[hooks] hook_output_dir` (default `~/data/mux/hook-output`).

**Not part of the protocol** (arcmux-private, read via gRPC not raw files):
`~/data/arcmux/profiles/index.json` (daemon-profile registry),
`~/data/arcmux/<project>/state.bolt` and `~/data/arcmux/_daemon/state.bolt`
(inbox/audit substrate — `arcmux-cli audit/inbox/ready`).

**Legacy migration:** state previously lived at `~/data/arcmux/sessions`; the
daemon sweeps any remaining legacy docs into `~/data/mux/sessions` on every
startup (idempotent, never overwrites the new location).

## Session state doc schema (`sessions/<id>.json`)

Written atomically under a per-session lock; safe to poll-read.

```json
{
  "session_id": "s-1781037342173817000",
  "agent": "grok",
  "created_at": "2026-06-09T13:35:42-07:00",
  "updated_at": "2026-06-09T13:36:08-07:00",
  "last_event": "turn_end",
  "last_tool": "run_terminal_command",
  "working": false,
  "turn_count": 1,
  "events_seen": 2,
  "last_prompt_submit_at": "2026-06-09T13:36:02-07:00",
  "last_turn_end_at": "2026-06-09T13:36:08-07:00",
  "turn_contract": {
    "goal": "add the vault link field to the state doc",
    "overall_goal": "Make arcmux's session JSON an accurate per-turn recording",
    "last_user_message": "now also record where the convo is saved\n…",
    "vault_link": "/home/blin/agents/histories/2026-06-25-09-arcmux-hooks.md",
    "source": "Stop",
    "updated_at": "2026-06-09T13:36:08-07:00"
  }
}
```

Canonical events (`last_event`): `prompt_submit`, `tool_start`, `tool_end`,
`turn_end`, `notification`. `working` is true between `prompt_submit` and
`turn_end`. Zero timestamps render as `0001-01-01T00:00:00Z` — treat as
"never", not as evidence.

`turn_contract` is a compact, evolving **recording** for arcmux-parent agent
sessions (recording, not steering — nothing here changes the agent's behavior).
It captures three valued views:

- `goal` — the **latest** gauged goal: the agent's most recent "Your ask:"
  restatement (the AGENTS.md rule), i.e. the current sub-task being steered.
- `overall_goal` — the **whole conversation's** objective, **continuously
  evolving**. Seeded from the launch prompt, then re-derived each turn by a
  background pass that sends the kept user turns + the current overall goal to a
  summarizer (xAI grok). Normally one succinct line; when the conversation has
  clearly split into separate themes, a short checklist with completed/abandoned
  earlier goals checked off (`- [x] …`) and active ones unchecked (`- [ ] …`).
- `last_user_message` — the **raw** last user turn, verbatim, truncated to 3
  lines. Recorded alongside the gauged goal, never as a substitute.
- `vault_link` — best-effort path to where the conversation is saved in the
  vault (`~/agents/histories/<…>.md`), matched by session cwd/host.

Optional `success_verification` / `path` fields are retained for callers that
set them, but the unified hook no longer scrapes them. This is not a transcript
or step log: hook writers replace the snapshot fields, preserving omitted ones
and updating `updated_at`, so subscribers read one current contract.

## How events get here (one hook, many subscribers)

Each LLM's native lifecycle hooks (claude settings hook, codex `hooks.json`,
grok drop-in — see [llm-classes.md](llm-classes.md)) run the **same unified
script** (`arcmux-session-hook.sh`; codex no longer has a divergent bridge),
which (1) appends the raw event to the JSONL audit and (2) calls
`arcmux hook --agent <agent> --event <canonical>` (plus `--goal` /
`--last-message` / `--vault-link` recording flags) to mutate the state doc. The
per-session identity comes from `ARCMUX_SESSION_ID` / `ARCMUX_SESSION_STATE_DIR`
env injected at launch (with `ARCMUX_SESSION_CWD` added for vault-link
resolution); the script no-ops for non-arcmux sessions, so global registration
is safe.

On `turn_end` the hook also fires a **background, best-effort** summarizer that
refreshes `overall_goal` via `arcmux hook --overall-goal <text>` — a
**contract-only** write (no `--event`) that updates the recording without moving
any event counters. It never blocks the turn; if grok is unreachable,
`overall_goal` simply keeps its prior value.

A subscriber that wants "is session X working? when did it last finish a
turn?" should read `sessions/<id>.json`. A subscriber that wants the full
event stream should tail the JSONL. Do not write to either — `arcmux hook` is
the single writer of the state doc.
