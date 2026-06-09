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
  "last_turn_end_at": "2026-06-09T13:36:08-07:00"
}
```

Canonical events (`last_event`): `prompt_submit`, `tool_start`, `tool_end`,
`turn_end`, `notification`. `working` is true between `prompt_submit` and
`turn_end`. Zero timestamps render as `0001-01-01T00:00:00Z` — treat as
"never", not as evidence.

## How events get here (one hook, many subscribers)

Each LLM's native lifecycle hooks (claude settings hook, codex bridge, grok
drop-in — see [llm-classes.md](llm-classes.md)) run the same generic script,
which (1) appends the raw event to the JSONL audit and (2) calls
`arcmux hook --agent <agent> --event <canonical>` to mutate the state doc.
The per-session identity comes from `ARCMUX_SESSION_ID` /
`ARCMUX_SESSION_STATE_DIR` env injected into the agent's pane at launch; the
script no-ops for non-arcmux sessions, so global registration is safe.

A subscriber that wants "is session X working? when did it last finish a
turn?" should read `sessions/<id>.json`. A subscriber that wants the full
event stream should tail the JSONL. Do not write to either — `arcmux hook` is
the single writer of the state doc.
