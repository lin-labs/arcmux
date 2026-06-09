# Centralized arcmux state — subscriber contract

`~/data/arcmux/` is the single, centralized state root any application can
read to observe arcmux-managed agent sessions — arcmux itself, mission-control,
elonco, or future subscribers. Agents' native hooks write here **once**, via
the `arcmux hook` CLI; subscribers read files instead of installing their own
per-app hooks.

## Layout

| Path | What | Writer | Readers |
|---|---|---|---|
| `~/data/arcmux/sessions/<session_id>.json` | Per-session hook state doc (see schema) | `arcmux hook` (locked read-modify-write); seeded/archived by the daemon | hooks judge, mission-control, anyone |
| `~/data/arcmux/sessions/archived/<session_id>.json` | State docs of ended sessions | daemon | anyone |
| `<hook_output_dir>/arcmux-hooks-<session_id>.jsonl` | Raw per-session hook event audit (append-only JSONL) | generic hook script | daemon watcher, anyone |
| `~/data/arcmux/profiles/index.json` | Daemon-profile registry (which daemons exist, sockets) | arcmux manager | anyone |
| `~/data/arcmux/<project>/state.bolt`, `~/data/arcmux/_daemon/state.bolt` | bbolt stores (inbox/audit substrate) | daemon only | via gRPC (`arcmux-cli audit/inbox/ready`), not raw |
| `/tmp/arcmux/<session_id>.env` | Per-session env handoff (0600, ARCMUX_* allowlist) | daemon | `arcmux hook-env` only — never source raw |

`hook_output_dir` defaults to `/tmp/arcmux-hooks` (config `[hooks]
hook_output_dir`); the session state dir defaults to `~/data/arcmux/sessions`
(config `[hooks] session_state_dir`).

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
