# arcmux HTTP API

Minimal HTTP control plane for managing agent sessions on the arcmux daemon's
isolated tmux server. Companion to the gRPC API; intended for orchestrators that
want a simple REST surface to spawn / list / close agent sessions.

## Endpoint

Default listen address: `127.0.0.1:7777`. Configurable via:

```toml
# ~/.config/arcmux/config.toml
[daemon]
http_addr = "127.0.0.1:7777"   # set to "" to disable HTTP
```

All responses are `application/json`. Errors use the shape:

```json
{ "error": "<message>" }
```

## Supported agents

| agent    | status         | command launched              |
|----------|----------------|-------------------------------|
| `claude` | implemented    | `cld --remote-control`        |
| `codex`  | not implemented | returns 501                  |
| others   | not implemented | returns 501                  |

`cld` is the user's alias for `claude --dangerously-skip-permissions`. Each
session runs in its own tmux window on the daemon's isolated tmux socket (see
`tmux -L arcmux attach`).

---

## `POST|GET /session/new`

Create a new agent session.

### Query parameters

| name   | type   | default                                | notes                                                                 |
|--------|--------|----------------------------------------|-----------------------------------------------------------------------|
| `agent`| string | `claude`                               | Only `claude` is implemented; others return 501.                      |
| `name` | string | `claude-<nanosec-id>` (auto-generated) | Must match `[A-Za-z0-9_-]{1,64}`. Must be unique across live sessions.|
| `cwd`  | string | empty                                  | Working directory for the tmux pane.                                  |

### Responses

`200 OK`
```json
{
  "session_id": "s-1779483006940496000",
  "name": "alpha",
  "agent": "claude",
  "tmux_target": "%1",
  "command": "cld --remote-control"
}
```

`400` — invalid `name` format.
`409` — `name` already in use by a live session.
`501` — `agent` not implemented.
`500` — tmux pane creation or command dispatch failed.

### Example

```bash
curl -s "http://127.0.0.1:7777/session/new?agent=claude&name=alpha"
```

---

## `GET /sessions`

List all live sessions.

### Response

`200 OK`
```json
{
  "sessions": [
    {
      "session_id": "s-1779483006907958000",
      "name": "claude-1779483006907958000",
      "agent": "claude",
      "state": "idle",
      "tmux_target": "agents:claude-1779483006907958000",
      "cwd": "",
      "started_at": "2026-05-22T13:50:06-07:00"
    }
  ]
}
```

Field semantics:

- `agent` — the agent profile name (`claude`, `codex`, …).
- `state` — session lifecycle: `starting | handshaking | idle | working | stuck | escalated | exited | failed`.
- `tmux_target` — tmux target string usable with `tmux -L arcmux send-keys -t <target> ...`.

### Example

```bash
curl -s "http://127.0.0.1:7777/sessions" | jq
```

---

## `POST|GET /session/close`

Kill the tmux pane for a session and remove it from the registry.

### Query parameters

| name   | type   | required | notes                          |
|--------|--------|----------|--------------------------------|
| `name` | string | yes      | Session name (see `/session/new`). |

### Responses

`200 OK`
```json
{
  "name": "alpha",
  "session_id": "s-1779483006940496000",
  "tmux_target": "%1",
  "closed": true
}
```

`400` — missing `name`.
`404` — no live session with that name.

### Example

```bash
curl -s "http://127.0.0.1:7777/session/close?name=alpha"
```

---

## Notes for implementers

- **Identity:** the canonical identifier is `session_id` (opaque); `name` is the
  human-friendly handle for HTTP callers. Names are unique only across **live**
  sessions; once a session is closed its name can be reused.
- **Concurrency:** the daemon serializes tmux operations internally; callers may
  fire `/session/new` requests concurrently and rely on the 409 collision
  response for de-duplication on name.
- **Observability:** to watch a pane interactively, attach to the isolated tmux
  server: `tmux -L arcmux attach -t agents`.
- **Lifecycle:** sessions also surface via the gRPC `ListSessions` /
  `Status` / `Subscribe` RPCs; the HTTP API is additive, not a replacement.
- **Roadmap (not yet implemented):** `codex` agent support, `/session/send`
  (prompt delivery), `/session/capture` (read pane output), event stream.
