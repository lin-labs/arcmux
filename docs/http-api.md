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

## Authentication

By default the control plane is unauthenticated and bound to loopback. Before
exposing it off-localhost (e.g. over Tailscale), set a shared secret:

```toml
[daemon]
http_addr = "0.0.0.0:7777"
http_auth_token = "<shared-secret>"
```

When `http_auth_token` is set, **non-loopback** requests must present
`Authorization: Bearer <token>` or receive `401`. Loopback callers
(`127.0.0.1`, `::1`, `localhost`) always bypass auth for local dev. When the
token is empty (default), auth is disabled entirely.

The device mesh does **not** reuse this listener. Its authenticated WebSocket
upgrade is on a dedicated loopback listener (default `127.0.0.1:7788`) at
`/v1/mesh`, normally reached through raw-TCP Tailscale Serve. Never expose port
7777 as the mesh transport.

## Mesh administration (local control API)

These endpoints expose no bearer credentials. They are protected by the same
control-plane authentication rules above and are intended for local CLI and
Mission Control use.

### `GET /mesh/status`

Returns deterministic peer status including `peer_id`, direction, state
(`disconnected`, `connecting`, `connected`, `stale`, or `dead`), last seen / last
success, next retry, sanitized last error, negotiated protocol, and round-trip
milliseconds. An offline peer remains visible; local sessions remain usable.

```json
{
  "enabled": true,
  "peers": [{"peer_id":"labs","direction":"outbound","state":"connected","protocol":1,"round_trip_ms":12}]
}
```

### `POST /mesh/ping?peer=<id>`

Sends an application-level ping over the current peer connection and returns
`peer_id` plus `round_trip_ms`. It returns `503` when the mesh or peer is
unavailable.

### `POST /mesh/reload`

Atomically stops and replaces only the mesh manager from the owner-only mesh
registry. The gRPC/HTTP control servers, tmux server, and all agent sessions are
left running. `arcmux mesh serve` and `arcmux mesh join` call this endpoint after
an atomic registry update, so pairing does not require a daemon restart.

## Mesh wire endpoint (dedicated listener)

`GET /v1/mesh` is a WebSocket upgrade requiring:

- `Authorization: Bearer <256-bit invite credential>`;
- `Sec-WebSocket-Protocol: arcmux.mesh.v1`;
- a protocol-v1 text-JSON `hello` whose device ID matches the credential;
- messages no larger than the configured limit (64 KiB default).

The server stores only a SHA-256 hash for accepted credentials. Malformed,
binary, oversized, wrong-version, and wrong-identity frames close only that peer
connection. Protocol v1 carries hello/welcome/ping/pong control envelopes; the
remote-session and artifact vocabulary is intentionally a later layer.

## Supported agents

| agent    | status          | command launched                               |
|----------|-----------------|------------------------------------------------|
| `claude` | implemented     | `cld --remote-control`                         |
| `codex`  | implemented     | codex start command from the profile registry  |
| others   | not implemented | returns 501                                    |

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

### Query parameters

| name      | type   | default | notes                                                                 |
|-----------|--------|---------|-----------------------------------------------------------------------|
| `project` | string | (none)  | When set, return only sessions belonging to the project (see below). An unknown project yields an empty list, not an error. |

**Project scoping.** arcmux is a pure substrate and does not store a "project"
on a session. The `project` filter resolves membership two ways: (1) the
session's `cwd` is within the project's `repo_cwd`, or (2) the session's
`owner_id` tags the project as a colon-delimited component (e.g.
`elonco:voxtop`, `project:voxtop`). `repo_cwd` comes from the project registry
at `~/.config/arcmux/projects.toml`:

```toml
[[project]]
slug = "voxtop"
repo_cwd = "/home/blin/Projects/voxtop"
plan_globs = ["docs/prd-*.md", "docs/plans/*.md"]
```

A missing registry file is fine — owner_id tag matching still works.

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
      "tmux_target": "%42",
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

## `POST|GET /session/capture`

Read a session's pane contents. Thin HTTP shim over the same daemon path the
gRPC `Capture` RPC uses.

### Query parameters

| name      | type   | default | notes                                                  |
|-----------|--------|---------|--------------------------------------------------------|
| `name`    | string | —       | Session name (required).                               |
| `history` | bool   | `false` | `1`/`true` returns full scrollback; default is the visible screen only. |

### Responses

`200 OK`
```json
{
  "name": "alpha",
  "session_id": "s-1779483006940496000",
  "tmux_target": "%1",
  "content": "...pane text..."
}
```

`400` — missing `name`.
`404` — no live session with that name.
`500` — capture failed.

### Example

```bash
curl -s "http://127.0.0.1:7777/session/capture?name=alpha&history=1"
```

---

## `POST|GET /session/send`

Deliver text to a session. Thin HTTP shim over the same daemon path the gRPC
`SendPrompt` RPC uses.

### Query parameters

| name        | type   | default | notes                                                       |
|-------------|--------|---------|-------------------------------------------------------------|
| `name`      | string | —       | Session name (required).                                    |
| `text`      | string | —       | Text to deliver (required).                                 |
| `confirm`   | bool   | `false` | `1`/`true` requests delivery confirmation.                  |
| `wait_idle` | bool   | `false` | `1`/`true` waits for a working agent to go idle before sending. |

### Responses

`200 OK`
```json
{
  "name": "alpha",
  "session_id": "s-1779483006940496000",
  "delivered": true
}
```

`400` — missing `name` or `text`.
`404` — no live session with that name.
`500` — delivery failed.

### Example

```bash
curl -s "http://127.0.0.1:7777/session/send?name=alpha&text=use+JWT&confirm=1"
```

---

## `POST|GET /babysit/new`

Mint an ephemeral, project-scoped **call context** for babysitter voice mode and
return a connect handle. The context is persisted to the daemon bbolt store with
a TTL; the voxtop relay resolves it on connect via `/babysit/context`.

### Query parameters

| name      | type   | default | notes                                                              |
|-----------|--------|---------|--------------------------------------------------------------------|
| `project` | string | —       | Project slug (required). Scopes panes via the same rule as `/sessions?project=`. |
| `server`  | string | (none)  | voxtop-server host (`host:port`) used to build `connect_url`. Loopback → `ws://`, else `wss://`. |
| `ttl`     | int    | `600`   | Context lifetime in seconds.                                       |

### Response

`200 OK`
```json
{
  "context_id": "ctx-ab12cd34ef56",
  "token": "<opaque>",
  "project": "voxtop",
  "connect_url": "wss://labs:5060/v1/realtime/converse?context=<token>",
  "repo_cwd": "/home/blin/Projects/voxtop",
  "plan_refs": ["/home/blin/Projects/voxtop/docs/prd-xai-realtime-voice-chat.md"],
  "panes": [
    {"name": "vox-a", "session_id": "s-..", "tmux_target": "%42", "state": "working", "cwd": "/home/blin/Projects/voxtop/VoxtopServer"}
  ],
  "expires_at": "2026-06-03T16:40:00-07:00"
}
```

`400` — missing `project`.
`503` — daemon state store unavailable.

The connect token rides the WS as `?context=` — distinct from `?token=`, which
remains the voxtop API key.

### Example

```bash
curl -s "http://127.0.0.1:7777/babysit/new?project=voxtop&server=labs:5060"
```

---

## `GET /babysit/context`

Resolve a minted call context by token. Called by the voxtop relay on connect to
load the scope (panes + repo + plan refs) for the session.

### Query parameters

| name      | type   | notes                                  |
|-----------|--------|----------------------------------------|
| `context` | string | Context token (or `token=` alias).     |

### Responses

`200 OK` — the full context JSON (same shape persisted at mint time).
`400` — missing token.
`404` — unknown or expired token (expired tokens are deleted on read).
`503` — daemon state store unavailable.

### Example

```bash
curl -s "http://127.0.0.1:7777/babysit/context?context=<token>"
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
- **Roadmap (not yet implemented):** event stream over HTTP, server-side bearer
  auth (required before any off-localhost exposure of this control plane).
