# Agent Tmux Runtime Service

**Codename:** `atrs` (or pick something better)
**Date:** 2026-05-16
**Status:** Design

## Problem

Orchestrators (OpenClaw, Hermes, future systems) need to run coding agents
(Claude Code, Codex, Grok) in tmux and reliably:

1. Know when the agent is working, stuck, waiting for input, or done
2. Send prompts and commands at the right time
3. Read output (streaming and final)
4. Not care about tmux mechanics or agent-specific quirks

Today each orchestrator reinvents this: raw `tmux send-keys`, fragile screen
scraping, no hook integration, ad-hoc stuck detection. Orca solves a superset
but is coupled to amux and bundles task/clone/PR management that belongs in the
orchestrator layer.

## What This Service Owns

- tmux session and pane lifecycle
- Agent process startup and handshake
- Prompt delivery with confirmation
- Output capture (live stream + final transcript)
- Health monitoring (stuck detection, idle timeout, crash detection)
- Hook-based state awareness (Claude Code hooks, Codex signals)
- Event emission to callers

## What This Service Does NOT Own

- Clone/worktree management
- Issue tracking or PR management
- Task queuing or scheduling
- AI decision-making
- Which agent to use or what prompt to send

## Architecture

```
┌─────────────────────────────────────────────────────┐
│  Orchestrator (OpenClaw, Hermes, etc.)              │
│                                                     │
│  "Start codex in ~/project, send this prompt,      │
│   tell me when it's done or stuck"                  │
└──────────────┬──────────────────────▲───────────────┘
               │ commands             │ events
               │ (Unix socket / gRPC) │ (stream)
               ▼                      │
┌─────────────────────────────────────────────────────┐
│  atrs daemon                                        │
│                                                     │
│  ┌─────────┐  ┌──────────┐  ┌─────────────────┐   │
│  │ Session  │  │ Health   │  │ Hook Watchers   │   │
│  │ Manager  │  │ Monitor  │  │ (per agent)     │   │
│  └────┬─────┘  └────┬─────┘  └───────┬─────────┘   │
│       │              │                │             │
│       ▼              ▼                ▼             │
│  ┌─────────────────────────────────────────────┐   │
│  │  tmux interface layer                       │   │
│  │  (send-keys, capture-pane, new-session,     │   │
│  │   new-window, kill-pane, pipe-pane)         │   │
│  └─────────────────────────────────────────────┘   │
│                                                     │
│  ┌─────────────────────────────────────────────┐   │
│  │  Agent profiles (codex, claude, grok)       │   │
│  │  - start command, ready pattern, stuck      │   │
│  │    patterns, hook paths, nudge sequence     │   │
│  └─────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────┘
               │
               ▼
┌─────────────────────────────────────────────────────┐
│  tmux server                                        │
│  ┌──────┐ ┌──────┐ ┌──────┐                       │
│  │ pane │ │ pane │ │ pane │  (each runs an agent)  │
│  └──────┘ └──────┘ └──────┘                       │
└─────────────────────────────────────────────────────┘
```

## API Surface

Unix domain socket at `~/.config/atrs/atrs.sock` (or configurable).
Protocol: newline-delimited JSON (request/response + push events), or gRPC if
we want typed clients in multiple languages.

### Commands (Orchestrator → Service)

#### `session.create`

Start an agent in a new tmux pane.

```json
{
  "id": "req-1",
  "method": "session.create",
  "params": {
    "agent": "codex",
    "cwd": "/Users/blin/Projects/myapp",
    "prompt": "Fix the auth bug. TDD. Open a PR when done.",
    "session_name": "agent-codex-fix-auth",
    "env": {"GITHUB_TOKEN": "..."},
    "tmux_session": "agents",
    "tmux_window": "myapp"
  }
}
```

Returns:

```json
{
  "id": "req-1",
  "result": {
    "session_id": "s-abc123",
    "tmux_target": "agents:myapp.%42",
    "pid": 12345,
    "state": "starting"
  }
}
```

The service:
1. Creates the tmux pane (or session+window if needed)
2. Starts the agent with profile-appropriate command
3. Performs the startup handshake (trust prompts, ready detection)
4. Delivers the prompt with confirmation
5. Emits `state_changed: working` on success

#### `session.send`

Send a follow-up prompt or command to a running agent.

```json
{
  "id": "req-2",
  "method": "session.send",
  "params": {
    "session_id": "s-abc123",
    "text": "Actually, also add rate limiting to the endpoint.",
    "confirm_delivery": true
  }
}
```

The service waits for the agent to be idle (if it's working), then delivers.
`confirm_delivery: true` uses agent-specific confirmation (e.g., wait for
Codex "Working" banner after sending).

#### `session.capture`

Read current pane output.

```json
{
  "id": "req-3",
  "method": "session.capture",
  "params": {
    "session_id": "s-abc123",
    "include_history": true
  }
}
```

Returns:

```json
{
  "id": "req-3",
  "result": {
    "output": "...",
    "current_command": "codex",
    "cwd": "/Users/blin/Projects/myapp",
    "state": "working",
    "idle_since": null
  }
}
```

#### `session.status`

Get structured status using both screen state and hook data.

```json
{
  "id": "req-4",
  "method": "session.status",
  "params": {
    "session_id": "s-abc123"
  }
}
```

Returns:

```json
{
  "id": "req-4",
  "result": {
    "state": "working",
    "agent": "codex",
    "tmux_target": "agents:myapp.%42",
    "pid": 12345,
    "started_at": "2026-05-16T15:30:00Z",
    "last_activity_at": "2026-05-16T15:35:12Z",
    "hook_state": {
      "source": "codex_output",
      "last_tool_use": "file_write",
      "files_changed": 3
    },
    "health": "healthy",
    "nudge_count": 0
  }
}
```

#### `session.kill`

Terminate an agent session.

```json
{
  "id": "req-5",
  "method": "session.kill",
  "params": {
    "session_id": "s-abc123",
    "graceful": true,
    "timeout": "30s"
  }
}
```

Graceful: sends Ctrl-C, waits for exit. Hard: kills the pane immediately.

#### `session.list`

List all managed sessions.

#### `session.subscribe`

Subscribe to events for one or all sessions (see Events below).

### Events (Service → Orchestrator)

Pushed over the same socket connection (or a dedicated event stream).

```json
{"event": "state_changed",   "session_id": "s-abc123", "state": "working", "ts": "..."}
{"event": "state_changed",   "session_id": "s-abc123", "state": "idle",    "ts": "...", "idle_since": "..."}
{"event": "stuck_detected",  "session_id": "s-abc123", "reason": "permission prompt", "output_snippet": "Do you want to allow..."}
{"event": "nudge_sent",      "session_id": "s-abc123", "attempt": 1, "max": 3}
{"event": "nudge_exhausted", "session_id": "s-abc123", "reason": "stuck after 3 nudges"}
{"event": "agent_exited",    "session_id": "s-abc123", "exit_code": 0, "final_output": "..."}
{"event": "hook_signal",     "session_id": "s-abc123", "hook": "PostToolUse", "data": {...}}
{"event": "prompt_delivered", "session_id": "s-abc123"}
{"event": "prompt_failed",   "session_id": "s-abc123", "reason": "agent not ready"}
{"event": "crash_detected",  "session_id": "s-abc123", "output_snippet": "..."}
```

The orchestrator reacts to these however it wants — escalate to human, retry,
reassign, etc. The service doesn't decide. It just reports.

## Agent Profiles

```toml
[agents.codex]
start_command = "codex --yolo"
ready_pattern = "›"
trust_prompt_pattern = "do you trust"
trust_response = "Enter"
working_indicator = "Working"
stuck_text_patterns = ["permission prompt", "do you want to allow"]
stuck_timeout = "5m"
idle_timeout = "60s"
nudge_command = "Enter"
max_nudge_retries = 3
hook_type = "codex_output"
# Codex writes structured output to stdout; capture via pipe-pane

[agents.claude]
start_command = "claude --dangerously-skip-permissions"
ready_pattern = ">"
stuck_text_patterns = ["tool denied", "would you like"]
stuck_timeout = "5m"
idle_timeout = "60s"
nudge_command = "Enter"
max_nudge_retries = 3
hook_type = "claude_hooks"
hook_dir = "~/.claude"
# Claude Code has filesystem hooks: PreToolUse, PostToolUse, Notification

[agents.grok]
start_command = "grok --yolo"
ready_pattern = ">"
stuck_text_patterns = ["confirm"]
stuck_timeout = "5m"
idle_timeout = "60s"
nudge_command = "Enter"
max_nudge_retries = 3
hook_type = "screen_only"
```

## Hook Integration (The Key Differentiator)

Screen scraping is fragile. The real signal comes from agent hooks.

### Claude Code Hooks

Claude Code supports shell hooks that fire on events:

- `PreToolUse` / `PostToolUse` — know what tool is running
- `Notification` — Claude emits structured notifications
- `Stop` — agent completed its turn

**Strategy:** Configure Claude Code hooks to write structured JSON to a
known path per session:

```bash
# ~/.claude/hooks/post-tool-use.sh (configured per-session)
echo '{"event":"tool_use","tool":"'$TOOL_NAME'","ts":"'$(date -u +%FT%TZ)'"}' \
  >> /tmp/atrs-hooks-${ATRS_SESSION_ID}.jsonl
```

The service watches this file with `fsnotify` and emits `hook_signal` events
to the orchestrator. This gives ground-truth state without screen scraping.

### Codex Hooks

Codex writes structured output. The service can capture this via `tmux
pipe-pane` redirecting to a file, then parsing the structured segments.

### Fallback: Screen Capture

For agents without hooks (Grok, or when hooks aren't configured), fall back
to periodic `tmux capture-pane` + pattern matching, similar to Orca.

## State Machine (Per Session)

```
                    ┌──────────────────────────────────┐
                    │                                  │
  create ──► starting ──► handshaking ──► idle ◄──► working
                │              │            │          │
                │              │            │          ▼
                │              │            │        stuck
                │              │            │          │
                │              │            │     nudge (N times)
                │              │            │          │
                ▼              ▼            ▼          ▼
              failed        failed      exited    escalated
```

States:
- **starting** — tmux pane created, agent command launched
- **handshaking** — waiting for ready pattern, handling trust prompts
- **idle** — agent is at prompt, waiting for input
- **working** — agent is executing (confirmed via hooks or screen)
- **stuck** — stuck pattern detected or idle timeout exceeded
- **escalated** — nudge retries exhausted, orchestrator must decide
- **exited** — agent process terminated
- **failed** — startup or handshake failed

## tmux Interface

Not a generic tmux wrapper — purpose-built for agent management.

### Session creation

```bash
# Create dedicated session or reuse existing
tmux new-session -d -s "$session" -n "$window" -c "$cwd" 2>/dev/null || \
tmux new-window -t "$session" -n "$window" -c "$cwd"
# Start agent
tmux send-keys -t "$target" "$start_command" Enter
# Set up output capture
tmux pipe-pane -t "$target" -o "cat >> /tmp/atrs-output-${session_id}.log"
```

### Capture

```bash
# Visible screen
tmux capture-pane -t "$target" -p
# Full scrollback
tmux capture-pane -t "$target" -p -S -
# Process info
tmux display-message -t "$target" -p '#{pane_current_command} #{pane_pid}'
```

### Idle detection

```bash
# Check if pane has had recent output (compare capture timestamps)
# OR use tmux's built-in: monitor-activity, activity-action
tmux set-option -t "$target" monitor-activity on
```

### Send keys

```bash
tmux send-keys -t "$target" "$text" Enter
```

## Persistence

Minimal. The service is the runtime, not the record keeper.

- **In-memory:** active sessions, state machines, health counters
- **On-disk:** `~/.config/atrs/sessions.json` — survives daemon restart so it
  can reconnect to existing tmux panes
- **Log files:** per-session output at `/tmp/atrs-output-{id}.log` and hook
  events at `/tmp/atrs-hooks-{id}.jsonl`

On restart, the service scans its session file, verifies which tmux panes
still exist, resumes monitoring for survivors, marks the rest as `exited`.

## Configuration

`~/.config/atrs/config.toml`:

```toml
[daemon]
socket = "~/.config/atrs/atrs.sock"
log_dir = "~/.config/atrs/logs"
grpc_port = 0  # Unix socket only by default; set >0 for TCP

[tmux]
socket_name = "atrs"         # tmux -L atrs (isolated server)
default_session = "agents"   # session name for new panes

[health]
capture_interval = "5s"      # how often to check pane state
idle_timeout_default = "60s"
stuck_timeout_default = "5m"

[hooks]
claude_hook_dir = "~/.claude"
hook_output_dir = "/tmp/atrs-hooks"
auto_install = true          # inject hooks on session.create
```

## Language Choice

Go. Reasons:
- Single binary, no runtime dependencies
- Excellent process management and concurrency
- Fast file watching (fsnotify)
- Easy Unix socket / gRPC server
- Same language as Orca — can extract patterns directly

Could also be Rust for the same reasons if preference demands it.

## Client Usage (from an orchestrator)

Python example (OpenClaw/Hermes) using generated gRPC stubs:

```python
import grpc
from atrs.v1 import atrs_pb2, atrs_pb2_grpc

channel = grpc.insecure_channel("unix:~/.config/atrs/atrs.sock")
client = atrs_pb2_grpc.AgentRuntimeStub(channel)

# Start an agent
resp = client.CreateSession(atrs_pb2.CreateSessionRequest(
    agent="codex",
    cwd="/Users/blin/Projects/myapp",
    prompt="Fix the auth bug. TDD. Open a PR when done.",
))
session_id = resp.session_id

# Stream live output
for chunk in client.StreamOutput(atrs_pb2.StreamOutputRequest(
    session_id=session_id,
)):
    print(chunk.text, end="")

# Subscribe to events
for event in client.Subscribe(atrs_pb2.SubscribeRequest(
    session_id=session_id,
)):
    if event.type == "stuck_detected":
        # orchestrator decides what to do
        client.SendPrompt(atrs_pb2.SendPromptRequest(
            session_id=session_id,
            text="Try a different approach.",
            confirm_delivery=True,
        ))
    elif event.type == "agent_exited":
        final = client.Capture(atrs_pb2.CaptureRequest(
            session_id=session_id, include_history=True
        ))
        break
```

Go example:

```go
conn, _ := grpc.Dial("unix:///Users/blin/.config/atrs/atrs.sock",
    grpc.WithTransportCredentials(insecure.NewCredentials()))
client := atrsv1.NewAgentRuntimeClient(conn)

resp, _ := client.CreateSession(ctx, &atrsv1.CreateSessionRequest{
    Agent:  "claude",
    Cwd:    "/Users/blin/Projects/myapp",
    Prompt: "Refactor the database layer to use connection pooling.",
})

// Stream events
stream, _ := client.Subscribe(ctx, &atrsv1.SubscribeRequest{
    SessionId: resp.SessionId,
})
for {
    event, err := stream.Recv()
    if err != nil { break }
    log.Printf("[%s] %s: %s", event.SessionId, event.Type, event.Message)
}
```

## Comparison With Orca

| Dimension | Orca | atrs |
|-----------|------|------|
| PTY layer | amux (custom) | tmux (standard) |
| Scope | Full task lifecycle (clone, PR, merge queue) | Runtime only (start, monitor, communicate) |
| Form factor | Standalone daemon + CLI per project | Standalone daemon, multi-project |
| State awareness | Screen scraping only | Hooks + screen scraping |
| Consumability | CLI or Unix socket RPC | Unix socket / gRPC, client libs |
| Agent knowledge | Codex-heavy, some Claude | Equal coverage, hook-native |
| Clone/task mgmt | Built-in | Not its job |

## Decisions

1. **Protocol:** gRPC with protobuf service definitions. Typed clients
   generated for Go, Python, TypeScript. Shell scripting via `grpcurl`.
2. **Topology:** One global daemon managing all sessions across all projects.
3. **tmux isolation:** Own server socket (`tmux -L atrs`). Users can still
   `tmux -L atrs attach` to watch panes when needed.
4. **Hook installation:** Auto-configure per session on `session.create`.
   The service generates a hook script that writes to a known JSONL path,
   injects it into the agent's hook config for that session, and cleans up
   on `session.kill`.
5. **Streaming output:** `session.stream` is a gRPC server-stream RPC that
   tails pane output in real-time. Complements the event subscription.
6. **Auth:** Unix socket permissions (single-user). No token layer needed.
