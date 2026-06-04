# Codex Hooks Findings for arcmux

Date: 2026-06-03 PT
Worker: Codex on `/Users/blin/Projects/arcmux`

## Scope

This document records what was verified about the OpenAI `codex` CLI hook
mechanisms for arcmux's CODEX-SIDE hook integration. The integration target is:

```sh
arcmux hook --session "$ARCMUX_SESSION_ID" --agent codex \
  --event prompt_submit|tool_start|tool_end|turn_end|notification [--tool NAME]
```

The arcmux contract deliberately uses `ARCMUX_SESSION_ID`, not Codex's own
`session_id`, because the arcmux session id is the key for
`~/data/arcmux/sessions/<id>.json`.

## Verified Environment

- Installed CLI: `/Users/blin/.local/bin/codex --version` prints
  `codex-cli 0.136.0`.
- Binary path: `/Users/blin/.local/bin/codex` is a symlink to
  `/Users/blin/.codex/packages/standalone/current/bin/codex`, a Mach-O arm64
  standalone binary.
- `codex --help` includes `--dangerously-bypass-hook-trust`, which confirms this
  installed version knows about hook trust.
- `codex features list` reports `hooks` as `stable` and effectively `true`.
- `/Users/blin/.codex/config.toml` has:

```toml
[features]
hooks = true
```

- The same config contains `[hooks.state]` trust records for
  `/Users/blin/.codex/hooks.json` hook entries including `user_prompt_submit`,
  `pre_tool_use`, `permission_request`, `session_start`, and `stop`.
- `/Users/blin/.codex/hooks.json` currently defines command hooks for
  `Stop`, `PermissionRequest`, `PreToolUse`, `SessionStart`, and
  `UserPromptSubmit`, mostly routed through `cmux-codex-hook-bridge`.
- No `notify = [...]` key is currently present in
  `/Users/blin/.codex/config.toml`; `[tui].notifications` is currently `false`.

## Official Sources Checked

- OpenAI Codex Hooks guide:
  `https://developers.openai.com/codex/hooks`
- OpenAI Codex Advanced Configuration:
  `https://developers.openai.com/codex/config-advanced#notify-vs-tuinotifications`
- OpenAI Codex Configuration Reference:
  `https://developers.openai.com/codex/config-reference#configtoml`
- OpenAI Codex GitHub schemas linked by the hooks guide:
  `https://github.com/openai/codex/tree/main/codex-rs/hooks/schema/generated`
- OpenAI Codex source for legacy `notify`:
  `https://github.com/openai/codex/blob/main/codex-rs/hooks/src/legacy_notify.rs`

The docs page is the release-behavior reference. The generated schemas are from
the `main` branch and may be ahead of the installed release, but they match the
field names exposed by the local 0.136.0 binary strings for the events arcmux
needs.

## Mechanism 1: Lifecycle Hooks

Codex has native lifecycle hooks. They are not limited to desktop
notifications.

Hooks are enabled by `[features].hooks = true` in `~/.codex/config.toml`.
`features.codex_hooks` is documented as a deprecated alias; use `hooks`.

Codex discovers lifecycle hooks from:

- `~/.codex/hooks.json`
- `~/.codex/config.toml` inline `[hooks]` tables
- `<repo>/.codex/hooks.json`
- `<repo>/.codex/config.toml` inline `[hooks]` tables
- plugin-bundled hook config

Project-local hooks load only when the project `.codex/` layer is trusted.
User-level hooks load regardless of project trust. If one layer contains both a
`hooks.json` and inline `[hooks]`, Codex merges both and warns; prefer one
representation per layer.

Non-managed command hooks must be reviewed and trusted before running. Codex
records trust against the hook definition hash. In interactive CLI sessions,
use `/hooks` to inspect and trust hook definitions. Automation can pass
`--dangerously-bypass-hook-trust` after independently vetting hook sources.

Only `type = "command"` handlers run today. `prompt` and `agent` handlers are
parsed but skipped. `async = true` handlers are parsed but skipped. Hook
commands run with the session `cwd`.

### Lifecycle Hook Input Payload

Every command lifecycle hook receives exactly one JSON object on `stdin`. Codex
does not append lifecycle hook JSON as an argv argument; any argv is whatever
the configured `command` string itself includes.

Common fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `session_id` | string | Codex session id, not arcmux session id |
| `transcript_path` | string or null | Codex transcript path if available |
| `cwd` | string | Session working directory |
| `hook_event_name` | string | Native Codex hook event name |
| `model` | string | Active model slug |
| `permission_mode` | string | Present on most turn/session events: `default`, `acceptEdits`, `plan`, `dontAsk`, or `bypassPermissions` |
| `turn_id` | string | Present on turn-scoped hooks |

Event-specific fields arcmux cares about:

| Codex event | Additional input fields |
| --- | --- |
| `UserPromptSubmit` | `prompt` |
| `PreToolUse` | `tool_name`, `tool_use_id`, `tool_input` |
| `PostToolUse` | `tool_name`, `tool_use_id`, `tool_input`, `tool_response` |
| `PermissionRequest` | `tool_name`, `tool_input`, optional `tool_input.description` |
| `Stop` | `stop_hook_active`, `last_assistant_message` |
| `SessionStart` | `source` = `startup`, `resume`, `clear`, or `compact` |
| `PreCompact` / `PostCompact` | `trigger` = `manual` or `auto` |
| `SubagentStart` / `SubagentStop` | `agent_id`, `agent_type`; stop also has `agent_transcript_path`, `stop_hook_active`, `last_assistant_message` |

The generated schemas use filenames like:

- `user-prompt-submit.command.input.schema.json`
- `pre-tool-use.command.input.schema.json`
- `post-tool-use.command.input.schema.json`
- `permission-request.command.input.schema.json`
- `stop.command.input.schema.json`

### Lifecycle Events That Fire

The documented lifecycle events are:

- `SessionStart`
- `SubagentStart`
- `UserPromptSubmit`
- `PreToolUse`
- `PermissionRequest`
- `PostToolUse`
- `PreCompact`
- `PostCompact`
- `SubagentStop`
- `Stop`

Turn-scoped events: `PreToolUse`, `PermissionRequest`, `PostToolUse`,
`PreCompact`, `PostCompact`, `UserPromptSubmit`, `SubagentStop`, and `Stop`.
Thread/start-scoped events: `SessionStart` and `SubagentStart`.

Matcher behavior:

| Event | Matcher filters |
| --- | --- |
| `PreToolUse` | `tool_name`; supports `Bash`, `apply_patch`, MCP names, and `Edit`/`Write` aliases for `apply_patch` |
| `PostToolUse` | `tool_name`; same names and aliases |
| `PermissionRequest` | `tool_name`; same names and aliases |
| `SessionStart` | `source` |
| `PreCompact` / `PostCompact` | `trigger` |
| `SubagentStart` / `SubagentStop` | `agent_type` |
| `UserPromptSubmit` | matcher ignored |
| `Stop` | matcher ignored |

Important limitation: `PreToolUse` and `PostToolUse` currently intercept Bash,
file edits through `apply_patch`, and MCP tool calls. The docs explicitly say
this does not intercept every shell path yet, and it does not intercept
`WebSearch` or other non-shell, non-MCP tools.

### Lifecycle Hook Output

For arcmux, the bridge should normally exit 0 with no stdout. Codex treats that
as success and continues.

Some events support JSON output for blocking, adding context, or continuing a
turn, but arcmux should not use those behaviors for passive state tracking.
`Stop` expects JSON on stdout if any output is produced; plain text is invalid
for that event. Therefore the arcmux bridge should stay quiet.

## Mechanism 2: `notify`

`notify` is separate from lifecycle hooks.

The config key is user-level only:

```toml
notify = ["/path/to/program", "optional", "args"]
```

The configuration reference says `notify` is an array of strings and is ignored
in project-local `.codex/config.toml`; set it in `~/.codex/config.toml`.

Official advanced config distinguishes:

- `notify`: external program for notifications/webhooks/CI hooks.
- `tui.notifications`: built-in TUI notifications, optionally filtered by event
  types such as `agent-turn-complete` and `approval-requested`.

The `openai/codex` source file `codex-rs/hooks/src/legacy_notify.rs` verifies
the current legacy notify wire shape: Codex appends the JSON payload as the
final argv argument to the configured command and uses null stdin/stdout/stderr.

The legacy notify JSON source currently serializes `agent-turn-complete`:

```json
{
  "type": "agent-turn-complete",
  "thread-id": "b5f6c1c2-1111-2222-3333-444455556666",
  "turn-id": "12345",
  "cwd": "/Users/example/project",
  "client": "codex-tui",
  "input-messages": ["Rename `foo` to `bar` and update the callsites."],
  "last-assistant-message": "Rename complete and verified `cargo build` succeeds."
}
```

I did not verify any current external `notify` payload for
`approval-requested`; the current source I checked only defines
`agent-turn-complete` for legacy notify. The docs mention `approval-requested`
as an example for `tui.notifications`, not as a demonstrated external
`notify` payload.

For arcmux prompt-ingestion judging, lifecycle hooks are superior to `notify`
because `UserPromptSubmit` fires before the prompt is sent and carries the raw
prompt. `notify` is turn-complete only in the verified source path.

## Mapping to arcmux Canonical Events

Recommended lifecycle-hook mapping:

| Codex native event | arcmux canonical event | Tool argument | Notes |
| --- | --- | --- | --- |
| `UserPromptSubmit` | `prompt_submit` | none | Ground-truth prompt ingestion signal. Best evidence for HooksJudge. |
| `PreToolUse` | `tool_start` | `--tool "$tool_name"` | Supported tool paths only. |
| `PostToolUse` | `tool_end` | `--tool "$tool_name"` | Supported tool paths only. |
| `Stop` | `turn_end` | none | Turn has completed. Matcher ignored. |
| `SubagentStop` | `turn_end` | none | Reasonable if arcmux tracks subagent turns; otherwise can be omitted. |
| `PermissionRequest` | `notification` | optional `--tool "$tool_name"` | Approval request, not tool execution. Do not mark as tool_start. |
| `SessionStart` | `notification` | none | Daemon should seed session state; this is informational only. |
| `SubagentStart` | `notification` | none | Informational unless arcmux wants subagent state. |
| `PreCompact` | `notification` | none | Informational. |
| `PostCompact` | `notification` | none | Informational. |
| legacy `notify` `agent-turn-complete` | `turn_end` if used as notify-only fallback | none | Do not configure alongside `Stop` for arcmux, or it will duplicate turn-end writes. |

Canonical events Codex lifecycle hooks cannot reliably produce:

- `notification` for every UI notification. Lifecycle hooks have
  `PermissionRequest`, session/compact/subagent lifecycle events, and stop, but
  not every TUI notification event.
- Complete `tool_start`/`tool_end` coverage for every possible Codex capability.
  Current documented coverage is Bash, `apply_patch`, and MCP calls; web search
  and other non-shell, non-MCP tool paths are not covered.

Canonical events Codex can produce well:

- `prompt_submit` via `UserPromptSubmit`.
- `turn_end` via `Stop`.
- `tool_start`/`tool_end` for the supported tool classes above.

## Suggested `~/.codex/config.toml` Snippet

Use inline `[hooks]` tables in user-level `~/.codex/config.toml`, or put the
equivalent JSON in `~/.codex/hooks.json`. The example below passes the native
event as argv for a small fallback path; the script still reads Codex's stdin
JSON to get `tool_name`.

```toml
[features]
hooks = true

[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = 'sh "$HOME/.codex/hooks/arcmux-codex-hook.sh" UserPromptSubmit'
timeout = 5
statusMessage = "Recording arcmux prompt event"

[[hooks.PreToolUse]]
matcher = "*"
[[hooks.PreToolUse.hooks]]
type = "command"
command = 'sh "$HOME/.codex/hooks/arcmux-codex-hook.sh" PreToolUse'
timeout = 5
statusMessage = "Recording arcmux tool start"

[[hooks.PostToolUse]]
matcher = "*"
[[hooks.PostToolUse.hooks]]
type = "command"
command = 'sh "$HOME/.codex/hooks/arcmux-codex-hook.sh" PostToolUse'
timeout = 5
statusMessage = "Recording arcmux tool end"

[[hooks.PermissionRequest]]
matcher = "*"
[[hooks.PermissionRequest.hooks]]
type = "command"
command = 'sh "$HOME/.codex/hooks/arcmux-codex-hook.sh" PermissionRequest'
timeout = 5
statusMessage = "Recording arcmux permission request"

[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = 'sh "$HOME/.codex/hooks/arcmux-codex-hook.sh" Stop'
timeout = 5
statusMessage = "Recording arcmux turn end"
```

Operational notes:

- Install the rendered script at `$HOME/.codex/hooks/arcmux-codex-hook.sh` or
  use another absolute path.
- If the arcmux daemon can inject `ARCMUX_BIN` into spawned Codex sessions, set
  it to the absolute arcmux binary path; otherwise the script falls back to
  `command -v arcmux`.
- If the daemon supports a state directory override, inject
  `ARCMUX_SESSION_STATE_DIR`; the script forwards it as `--state-dir`.
- The provided template also recognizes legacy `notify` JSON when it is passed
  as argv and maps `agent-turn-complete` to `turn_end`. Treat that as a
  fallback, not the preferred path, and do not configure it alongside `Stop`.
- After changing the hook definition, use `/hooks` in Codex to review/trust it,
  or run automation with `--dangerously-bypass-hook-trust` only after the
  hook source has been independently vetted.

## Gaps Versus Claude Hooks for Prompt Ingestion

Codex is strong for prompt ingestion: `UserPromptSubmit` fires at turn scope and
the input includes `prompt`, making it a good ground-truth signal for
`last_prompt_submit_at`.

The remaining gaps are mostly around tool/notification richness, not ingestion:

- `PreToolUse`/`PostToolUse` do not intercept all tool paths yet.
- `notify` is too late for ingestion; verified source only emits after-agent
  `agent-turn-complete`.
- `Stop` can be re-entered when a stop hook asks Codex to continue; arcmux's
  bridge does not request continuation, so this should stay a normal
  `turn_end`, but existing third-party Stop hooks could continue the turn after
  arcmux already marked it ended.
- Lifecycle hooks require trust review before running, so deployment must include
  a trust step or a managed hook path.
