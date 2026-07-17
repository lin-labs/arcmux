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
| `~/data/mux/sessions/profiles/<profile>/<session_id>.json` | Profile-scoped state for IDs that may duplicate root/other profiles | profile `arcmux hook` + profile daemon | session catalog, mission-control, anyone |
| `~/data/mux/sessions/archived/<session_id>.json` | State docs of ended sessions | daemon | anyone |
| `~/data/mux/hook-output/arcmux-hooks-<session_id>.jsonl` | Raw per-session hook event audit (append-only JSONL) | generic hook script | daemon watcher, anyone |
| `~/data/mux/hook-output/profiles/<profile>/arcmux-hooks-<session_id>.jsonl` | Profile-scoped raw event audit | generic hook script | profile daemon watcher, anyone |
| `/tmp/arcmux/<base64(profile_scope)>--<session_id>.env` | Exact profile/session env handoff (0600, ARCMUX_* allowlist) | daemon | `arcmux hook-env <scope> <id>` only — never source raw |

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

Written atomically under a per-profile/session lock; safe to poll-read. Session
IDs are unique only inside their profile scope, so consumers must keep the root
and `profiles/<name>/` namespaces distinct.

```json
{
  "revision": 4,
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
    "overall_goal_provenance": "hook.overall_goal_summarizer.v1",
    "overall_goal_updated_at": "2026-06-09T13:36:09-07:00",
    "last_user_message": "now also record where the convo is saved\n…",
    "canonical_history": {
      "basename": "2026-06-09-arcmux-history.md",
      "conversation_id": "native-conversation-123",
      "provenance": "hook.canonical_history_frontmatter.v1",
      "updated_at": "2026-06-09T13:36:08-07:00"
    },
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
  evolving**. Seeded from the launch prompt as untrusted display state, then
  re-derived after completed turns by a daemon-owned, tool-less OpenAI or xAI
  HTTPS call. Its only semantic inputs are the exact session-id-keyed gauged
  goal and any prior trusted summary; raw user messages, launch seeds, and
  history files are never model inputs. Only the daemon writer can stamp
  `overall_goal_provenance: hook.overall_goal_summarizer.v1`.
- `last_user_message` — the **raw** last user turn, verbatim, truncated to 3
  lines. Recorded alongside the gauged goal, never as a substitute.
- `canonical_history` — an exact producer-supplied basename plus native
  conversation identity. `arcmux hook` with `--history-basename <name>` and
  `--history-conversation-id <id>` accepts the pair only when one regular,
  non-symlink Markdown file directly under `ARCMUX_HISTORY_ROOT` has the exact
  `conversation_id` in its frontmatter. The verifier stops at the closing
  frontmatter fence and stamps `hook.canonical_history_frontmatter.v1`; it does
  not read transcript body text or search by cwd, host, title, or mtime.
- `vault_link` — optional legacy/external-producer metadata. The generic hook
  does not infer it from cwd/host because that heuristic can confuse concurrent
  conversations in the same checkout. It is never projected as canonical
  history by `session self`, the daemon catalog, or mesh APIs.

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
`--last-message` recording flags) to mutate the state doc. The
per-session identity comes from `ARCMUX_SESSION_ID` / `ARCMUX_SESSION_STATE_DIR`
env injected at launch; the script no-ops for non-arcmux sessions, so global
registration is safe.

Canonical Markdown is owned by the agent-history producer, not inferred by
arcmux. After that producer atomically creates or updates the exact destination,
it may bind the result from inside the supervised session:

```bash
arcmux hook \
  --history-basename "$(basename "$canonical_history")" \
  --history-conversation-id "$native_conversation_id"
```

Both values are required. The daemon injects the canonical history root into
the session as `ARCMUX_HISTORY_ROOT`; callers cannot select an alternate root by
CLI flag. A failed, ambiguous, mismatched, symlinked, or missing file leaves the
prior binding untouched.

The daemon observes `turn_end` and schedules a **background, best-effort**
summary, bounded to two concurrent calls globally and one per session. The
result is committed only if the exact state revision and complete input snapshot
are unchanged; a concurrent hook update makes the response stale. The generic
hook and public CLI expose no way to stamp trusted provenance. Missing providers
or rejected output simply omit mesh `current_work` until a safe summary exists.

Managed services do not source interactive shell files. Persist provider choice
and the credential-file path in owner-local `~/.config/arcmux/config.toml`; keep
the credential itself out of TOML:

```toml
[current_work]
provider = "openai" # openai | xai | legacy-cli
model = "gpt-5.4-mini"
api_key_file = "~/.config/arcmux/openai-api-key" # 0600, current-uid regular file
```

The environment variables `ARCMUX_GOAL_PROVIDER`, `ARCMUX_GOAL_MODEL`,
`OPENAI_API_KEY` / `XAI_API_KEY`, and `OPENAI_API_KEY_FILE` /
`XAI_API_KEY_FILE` remain runtime overrides. The fixed HTTPS requests declare no
tools, explicitly disable OpenAI response storage, and size-bound response
bodies. `ARCMUX_GOAL_BIN` or `[current_work].legacy_bin` remains an explicit
`legacy-cli` compatibility path and is never auto-discovered from an agent
binary.

A subscriber that wants "is session X working? when did it last finish a
turn?" should read `sessions/<id>.json`. A subscriber that wants the full
event stream should tail the JSONL. Do not write to either — `arcmux hook` is
the event/recording writer and the daemon is the trusted summary writer.
