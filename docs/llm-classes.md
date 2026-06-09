# LLM Classes — one identity, two run modes

arcmux manages each LLM (codex, claude, grok) as a single **class**
(`internal/profile.Class`) with two profiles derived from it:

| Class | Interactive profile (tmux) | Exec profile (headless one-shot) |
|---|---|---|
| codex | `codex` — `codex --dangerously-bypass-approvals-and-sandbox --no-alt-screen` | `codex_exec` — `codex exec --json` |
| claude | `claude` — `claude --dangerously-skip-permissions --remote-control` | `claude_exec` — `claude -p --output-format stream-json` |
| grok | `grok` — `grok --no-alt-screen --permission-mode bypassPermissions` | `grok_exec` — `grok -p --output-format streaming-json` |

Adding an LLM = adding one `Class` to `profile.DefaultClasses()` (or an
`[agents.<name>]` TOML override). The flat profile map, hook installation,
env-loader wrapping, and exec-driver routing all derive from the class — no
agent-name conditionals anywhere in the daemon.

## Discovery

```bash
arcmux-cli agents          # JSON: name, class, transport, exec_driver, hook_type, hook_backed
```

## Mode 1 — interactive tmux session

```bash
arcmux-cli create --agent grok --cwd ~/Projects/x --name my-grok --owner me
echo "do the thing" | arcmux-cli send <session_id>     # ConfirmDelivery + WaitIdle
arcmux-cli capture <session_id>
tmux -L <tmux-socket-name> attach -t <session-name>     # human attach (socket per daemon profile)
```

The daemon creates a dedicated tmux session under its configured tmux socket,
launches the agent with a fail-safe env loader (`eval "$(arcmux hook-env <id>)"`),
performs the ready/trust handshake, and verifies prompt delivery via the
configured judge. The default `[delivery] judge = "auto"` is a cascade —
**hooks ground truth always wins when the agent emits hook events**; sessions
without a usable hook signal degrade to the typesafe judge, then to the
screen heuristic. Pin `"hooks"`, `"typesafe"`, or `"heuristic"` only to
bypass tiers deliberately.

**Hook-backed delivery verification.** Each LLM's native lifecycle hooks call
`arcmux hook`, the single writer of the per-session state doc the hooks judge
reads (see [state-dir.md](state-dir.md)):

- **claude**: generic script at `~/.claude/hooks/arcmux-session-hook.sh`;
  registration in `~/.claude/settings.json` is manual (see arcmux-urc).
- **codex**: bridge at `~/.codex/hooks/arcmux-codex-hook.sh`; registration in
  codex config is manual (see codex-hooks-findings.md).
- **grok**: fully automatic. The daemon materializes the same generic script
  plus a drop-in registration `~/.grok/hooks/arcmux-session.json`; grok merges
  `~/.grok/hooks/*.json` (always trusted) at session start. The generic script
  parses both payload dialects (claude `hook_event_name`/PascalCase, grok
  `hookEventName`/snake_case).

**Grok leader caveat (load-bearing).** Grok executes hooks in its *leader*
process, not the TUI client. A shared leader spawned earlier by another client
would not carry the session's `ARCMUX_*` env, silently no-opping the hooks. So
the grok profile's `session_start_args` gives every session a private leader
(`--leader-socket ~/.grok/leader-arcmux-<session>.sock`) that inherits the pane
env; the leader exits when its client disconnects, so it cleans up with the
session.

## Mode 2 — headless one-shot exec

```bash
arcmux-cli exec --agent grok "summarize the diff"        # class name resolves to grok_exec
echo "prompt" | arcmux-cli exec --agent codex --cwd ~/Projects/x
arcmux-cli exec --agent claude --keep "first turn"       # session_id on stderr
arcmux-cli exec --session <id> "follow-up"               # resumes the same backend thread
arcmux-cli exec --agent grok --json --timeout 30m "..."  # structured result / long runs
```

`exec` creates an exec-transport session (AutoClose unless `--keep`), delivers
the prompt (the daemon spawns `codex exec` / `claude -p` / `grok -p`, parses
the structured stream, and records the backend session/thread id for resume),
waits for completion, and prints the final assistant message. Successive
exec turns on the same kept session resume the same backend thread (codex
thread id / claude session id / grok sessionId, captured from the stream).

## Validation

```bash
make validate                          # gofmt + vet + go test + build + substrate scenarios
make validate-e2e-hooks AGENT=grok     # live agent: hooks judge gates a real delivery
```
