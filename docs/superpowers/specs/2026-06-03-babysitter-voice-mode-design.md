# Babysitter Voice Mode — Design

**Date:** 2026-06-03
**Status:** Approved at the architecture level; grounded against code after a Codex
review (see "Review resolutions"). Sub-project specs land per-unit before build.
**Repos touched:** `arcmux` (keystone), `voxtop` (relay + iOS), `voice-controlled-pm` (prototype client)

## Summary

A voice control plane over arcmux's agent substrate. You summon a
project-scoped voice call from your phone (or a browser), and a requirements /
standup brain comes up **already knowing the project** — its running agents
(panes), its repo/plan, and its issue graph — and lets you, by voice:

- hear what each execution agent is doing or blocked on,
- redirect a running agent or dispatch new work,
- review and edit the project's plan/PRD in place,
- CRUD the project's Beads issues,

all behind a **mechanical confirm gate** before anything writes to a live agent,
the plan, or the issue graph.

The deliverable of "babysit `<project>`" is **not a document** — it is a
prepared, ephemeral **call context** that, the moment a client connects, brings
up a tool-ready xAI realtime voice call scoped to that project.

## Why this shape

Both halves already mostly exist; we connect them rather than rebuild:

- **arcmux** is a **pure substrate**: it tracks agent sessions (a tmux pane per
  session) with a `cwd` and a free-form `owner_id` caller tag, and exposes an
  HTTP control plane at `127.0.0.1:7777` (`/session/new`, `/sessions`,
  `/session/close`) plus a gRPC API. **Role structure (Elon/Manager/IC) now
  lives in callers (elonco), not the daemon.** gRPC already has `Capture`,
  `Send`, `SendPrompt`, `ListSessions`, `Status`, `Subscribe`.
- **voxtop's relay** (`VoxtopServer/realtime_voice.py`) runs the xAI realtime
  audio + tool-dispatch loop, with a `MODES` registry (`interview`/`research`).
- **The Vox iOS app** has `ArcmuxService.swift` (talks to `:7777`) and a `vox://`
  URL scheme with an `.onOpenURL` handler (today only `quick-note`).

**Decision: the relay stays the home of the voice call; the per-project
intelligence is baked into the call context arcmux hands it.** This reuses the
proven Python relay and keeps arcmux's new surface small.

## Review resolutions (decisions made to close Codex findings)

These are binding decisions for the sub-project specs:

1. **Project→panes (C1/A5).** arcmux gains a **project registry** (config:
   `~/.config/arcmux/projects.toml`, slug → `repo_cwd` + optional `plan_globs`).
   `/sessions?project=<slug>` returns sessions whose `cwd` is under that
   project's `repo_cwd`, with an `owner_id` tag of `project:<slug>` as an
   explicit override. No proto change required for the MVP filter (filtering is
   done daemon-side over existing session fields). A first-class `project` field
   on the session is a later refinement, not in the MVP.
2. **Repo/plan resolution (M1).** Comes from the project registry above —
   `repo_cwd` and `plan_globs` are registry-owned, not derived from
   `~/data/arcmux/<project>/`.
3. **Context store (M2).** The minted call context lives in the **daemon-level
   bbolt** store (the same store C1 already uses), in a `babysit_context`
   bucket, with a TTL/expiry.
4. **Handoff token (C2/A2).** Do **not** overload `?token=` (that is the API
   key, validated against the key DB). The converse WS gains a **distinct
   `?context=<ctx-token>` param**. `main.py` validates `context` against arcmux's
   context store, and on success injects `(mode=babysit, scope)` into
   `run_relay` (whose signature is extended to accept an optional context). The
   server **forces babysit mode + the scoped tools** when a valid context is
   present; the client does not need to send a mode `session.update`.
5. **Mechanical confirm gate (C3/M5).** Write tools are **two-phase**. The model
   calls `propose(action, args)` → the relay returns a human-readable readback +
   a one-shot `confirm_token` and performs **no** side effect. The side effect
   fires only when the model calls `confirm(confirm_token)` (which it does after
   the user says yes aloud). Applies to `send_to_pane`, `spawn_agent`,
   `write_plan`, and `bead_create/update/close`. This is enforced in the
   dispatch loop, not by prompt wording.
6. **arcmux HTTP auth (C4).** Add **server-side bearer auth** to the arcmux HTTP
   server (shared secret from config). Required whenever `http_addr` is
   non-loopback. Vox's `authorize()` then sends it; the babysit `?context=` flow
   carries it server-to-server. Loopback stays open for local dev.
7. **Per-context tool cwd (M3).** Babysit tools run against the context's
   `repo_cwd`, threaded per-session, **not** the process-global
   `VOICE_TOOLS_CWD`.
8. **Beads tools (M5).** Explicit `bead_*` tools with structured, validated args
   (no raw `bd` bash). Reads are free; writes go through the confirm gate.
9. **C scope (M4).** Multi-server means a single resolved-active-host accessor
   consumed uniformly by `ConverseService`, `ServerAPI`, and `ArcmuxService`
   (which derives `:7777` from it) — not just swapping the `AppState.serverHost`
   string.

## The babysit call context

An ephemeral, project-scoped record **minted by arcmux** (bbolt, TTL'd):

```
context_id          opaque id
token               short-lived; rides in the WS ?context= param (NOT ?token=)
project             "voxtop"
panes[]             sessions whose cwd ∈ repo_cwd (or owner_id == project:<slug>)
repo_cwd            from the project registry
plan_globs[]        from the project registry (resolved to plan/PRD paths)
server              which voxtop-server host the client should connect to
created_at / expires_at
```

### End-to-end flow

1. **Trigger** (you via CLI, a cron, or an agent): `POST /babysit/new?project=voxtop` on arcmux.
2. arcmux resolves panes (via the project filter) + repo/plan (via the
   registry), persists the context to bbolt, and returns a **connect handle**:
   `wss://<host>/v1/realtime/converse?context=<ctx-token>`.
3. arcmux fires an **APNs push + writes an in-app tray entry**; payload carries
   the connect handle, the project label, and a one-line "why now."
4. You **tap the notification** (or a tray row) → `vox://babysit?...` deep link →
   the app sets the active server and opens the WS.
5. On connect, `main.py` validates `context` against arcmux's context store, then
   `run_relay` configures the xAI session: **babysit instructions + scoped tools
   bound to that project's panes/repo.**
6. The call is live and tool-ready. The brain **opens by reading back the scope**
   ("Babysitting voxtop — 1 Elon, 2 ICs, one's blocked. Where do you want to
   start?").

## Babysit toolset

Tools live in the relay (each needs a `FUNCTION_SCHEMAS` entry + `TOOLS`
dispatch handler — adding a mode is more than a `MODES` row). Reads are free;
**writes are two-phase via the confirm gate**:

| Tool | Kind | Backed by |
|------|------|-----------|
| `list_panes` | read | arcmux `/sessions?project=` (new filter) |
| `capture_pane` | read | arcmux `/session/capture` (new HTTP shim over gRPC `Capture`) |
| `send_to_pane` | **write** | arcmux `/session/send` (new HTTP shim over gRPC `Send`) |
| `spawn_agent` | **write** | arcmux `/session/new` |
| `read_plan` | read | filesystem (`repo_cwd` + `plan_globs`) |
| `write_plan` | **write** | filesystem (per-context `repo_cwd`) |
| `bead_list` | read | `bd list` (structured args) in `repo_cwd` |
| `bead_create` | **write** | `bd create` (structured args) in `repo_cwd` |
| `bead_update` | **write** | `bd update` (structured args) in `repo_cwd` |
| `bead_close` | **write** | `bd close` (structured args) in `repo_cwd` |
| `save_doc` | write | existing relay tool |

The plan doc doubles as the checklist: open items get checked off by editing the
doc as decisions are made, so progress survives across calls with no separate
state store.

## Sub-projects (each gets its own spec → plan → build)

| | Sub-project | Scope | Depends on |
|---|---|---|---|
| **B** | **arcmux babysit subsystem** *(keystone)* | project registry + `/sessions?project=` filter; `/session/capture` + `/session/send` HTTP shims; `/babysit/new` + `/babysit/context` (mint/lookup in bbolt); server-side bearer auth; push-trigger hook | — |
| **A** | **voxtop babysit mode** | `MODES["babysit"]` + `FUNCTION_SCHEMAS`/`TOOLS` for the toolset; `?context=` validation in `main.py`; `run_relay` accepts context; per-context `repo_cwd`; two-phase confirm gate; Beads tools | B |
| **A.5** | **prototype gate** | minimal web client in `~/Projects/voice-controlled-pm/` (browser mic ↔ babysit WS); validate the full loop before iOS work | A |
| **C** | **iOS multi-server** | resolved-active-host accessor across Converse/ServerAPI/Arcmux; saved server list + picker + migration | — |
| **D** | **iOS push + tray + connect** | APNs registration + device token; arcmux push delivers the connect handle; in-app tray; `vox://babysit` deep link; connect-with-context; sends the arcmux bearer token | B, C |

**Build order: B → A → A.5 (gate) → C → D.**

- **B first** — keystone; both the voice tools and the push depend on it. Start
  with the `/session/capture` + `/session/send` shims (gRPC RPCs already exist),
  then the project filter, then context mint/lookup, then auth.
- **A** — makes the voice call real; testable with a manually-minted context.
- **A.5** — talk to the babysitter from a browser and verify the full loop
  (scope readback → capture → send-with-confirm → spawn → CRUD a bead → write a
  plan) **before** investing in the iOS pieces.
- **C** — small, isolated, unblocks "different servers."
- **D** — the summon-by-notification polish; last because A+B validate without it.

## Safety

- **Mechanical confirm gate** (decision 5) on every write to a live agent, the
  plan, or the issue graph — enforced in the dispatch loop, not prompt wording.
- **Short-lived context tokens** (bbolt TTL); the context token is a WS
  credential distinct from the API key.
- **Server-side bearer auth on arcmux HTTP** (decision 6) before any
  off-localhost reach; loopback stays open for dev.

## Open items (resolve during sub-project specs)

- First-class `project` field on sessions (post-MVP refinement of the cwd/owner_id filter).
- Push transport details for D (APNs key/cert, device-token registration, payload schema).
- Prototype frontend layout under `~/Projects/voice-controlled-pm/`.
