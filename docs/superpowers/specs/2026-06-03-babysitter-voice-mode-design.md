# Babysitter Voice Mode — Design

**Date:** 2026-06-03
**Status:** Approved (brainstorm complete; first sub-project → writing-plans)
**Repos touched:** `arcmux` (keystone), `voxtop` (relay + iOS), `voice-controlled-pm` (prototype client)

## Summary

A voice control plane over arcmux's multi-agent system. You summon a
project-scoped voice call from your phone (or a browser), and a requirements /
standup brain comes up **already knowing the project** — its running agents
(panes), its plan/PRD, and its issue graph — and lets you, by voice:

- hear what each execution agent is doing or blocked on,
- redirect a running agent or dispatch new work,
- review and edit the project's plan/PRD in place,
- CRUD the project's Beads issues,

all with a **confirm-readback** before anything writes to a live agent or the
issue graph.

The deliverable of "babysit `<project>`" is **not a document** — it is a
prepared, ephemeral **call context** that, the moment a client connects, brings
up a tool-ready xAI realtime voice call scoped to that project.

## Why this shape

Both halves already mostly exist; we connect them rather than rebuild:

- **arcmux** owns the project→panes mapping (one "Elon company" per project;
  Manager/IC panes tracked in `~/data/arcmux/<project>/state.bolt`) and exposes
  an HTTP control plane at `127.0.0.1:7777` plus a gRPC API
  (`ListSessions/Status/Subscribe`). `/session/capture` (read pane) and
  `/session/send` (prompt delivery) are already on its roadmap.
- **voxtop's relay** (`VoxtopServer/realtime_voice.py`) already runs the xAI
  realtime audio + tool-dispatch loop, with a `MODES` registry
  (`interview`/`research`). A new mode is a registry entry. The WS authenticates
  via a `?token=` query param.
- **The Vox iOS app** already has `ArcmuxService.swift` (talks to `:7777`, with
  an `authorize()` hook stubbed for auth) and a `vox://` URL scheme with an
  `.onOpenURL` handler.

**Decision: the relay stays the home of the voice call; the per-project
intelligence is baked into the call context arcmux hands it.** This reuses the
proven Python relay and keeps arcmux's new surface small.

## The central concept: the babysit call context

An ephemeral, project-scoped session record **minted by arcmux**:

```
context_id          opaque id
token               short-lived; rides in the WS ?token= param
project             "voxtop"
panes[]             resolved tmux targets for the project (Elon + managers + ICs)
plan_refs[]         the project's PRD/plan doc paths
repo_cwd            the project's repo path (for plan read/write + bd)
server              which voxtop-server host the client should connect to
created_at / expires_at
```

### End-to-end flow

1. **Trigger** (you via CLI, a cron, or an agent): `POST /babysit/new?project=voxtop` on arcmux.
2. arcmux resolves panes + plan refs + repo cwd, persists the context, and
   returns a **connect handle**:
   `wss://<host>/v1/realtime/converse?mode=babysit&token=<ctx>`.
3. arcmux fires an **APNs push + writes an in-app tray entry**; the payload
   carries the connect handle, the project label, and a one-line "why now."
4. You **tap the notification** (or a tray row) → `vox://babysit?...` deep link →
   the app sets the active server and opens the WS.
5. On connect, the relay calls arcmux `GET /babysit/context?token=…` to load the
   scope, then configures the xAI session: **babysit instructions + tools
   pre-bound to that project's panes/repo.**
6. The call is live and tool-ready. The brain **opens by reading back the scope**
   ("Babysitting voxtop — 1 Elon, 2 ICs, one's blocked. Where do you want to
   start?").

## Babysit toolset

Tools live in the relay; reads are free, **writes require confirm-readback**
(the brain speaks back the exact action and waits for a yes before firing):

| Tool | Kind | Backed by |
|------|------|-----------|
| `list_panes` | read | arcmux `/sessions` (project-filtered) |
| `capture_pane` | read | arcmux `/session/capture` (new) |
| `send_to_pane` | **write** | arcmux `/session/send` (new) |
| `spawn_agent` | **write** | arcmux `/session/new` |
| `read_plan` | read | filesystem (`plan_refs` / `repo_cwd`) |
| `write_plan` | **write** | filesystem |
| `bead_list` | read | `bd list` in `repo_cwd` |
| `bead_create` | **write** | `bd create` in `repo_cwd` |
| `bead_update` | **write** | `bd update` in `repo_cwd` |
| `bead_close` | **write** | `bd close` in `repo_cwd` |
| `save_doc` | write | existing relay tool |

The plan doc doubles as the checklist: open items get checked off by editing the
doc as decisions are made, so progress survives across calls with no separate
state store.

## Sub-projects (each gets its own spec → plan → build)

| | Sub-project | Scope | Depends on |
|---|---|---|---|
| **B** | **arcmux babysit subsystem** *(keystone)* | `/babysit/new` + `/babysit/context` (mint/lookup), `/session/capture` + `/session/send`, project→panes filter, push trigger hook | — |
| **A** | **voxtop babysit mode** | `MODES["babysit"]` brain (scope readback, phase arc, confirm-readback) + tools calling B's endpoints + Beads tools; fetch context on connect | B |
| **A.5** | **prototype gate** | minimal web frontend in `~/Projects/voice-controlled-pm/` (browser mic ↔ babysit WS); validate the whole loop before iOS work | A |
| **C** | **iOS multi-server** | saved server list + active selection + picker + migration, so a notification's `server` is honored | — |
| **D** | **iOS push + tray + connect** | APNs registration, notification handling, in-app tray UI, `vox://babysit` deep link, connect-with-context | B, C |

**Build order: B → A → A.5 (gate) → C → D.**

- **B first** — keystone; both the voice tools and the push depend on it, and
  `capture`/`send` are independently useful and already roadmapped.
- **A** — makes the voice call real; testable with a manually-minted token.
- **A.5** — talk to the babysitter from a browser and verify the full loop
  (scope readback → capture → send-with-readback → spawn → CRUD a bead → write a
  plan) **before** investing in the iOS pieces.
- **C** — small, isolated, unblocks "different servers."
- **D** — the summon-by-notification polish; last because A+B validate without it.

## Safety

- **Confirm-readback** on every write to a live agent or the issue graph
  (`send_to_pane`, `spawn_agent`, `bead_*` writes): the brain restates the exact
  action and target and waits for spoken confirmation before firing.
- **Short-lived context tokens** with expiry; the token is the WS credential.
- arcmux's existing `authorize()` hook (currently a no-op) is the place to add
  token auth on the `:7777` endpoints when D introduces remotely-reachable hosts.

## Open items (resolve during sub-project specs)

- Exact `panes[]` resolution rule (all live sessions whose project == slug —
  Elon + every team manager + IC — confirmed as the default).
- Push transport details for D (APNs key/cert, device-token registration,
  payload schema) — sized as net-new in Vox.
- Prototype frontend home layout under `~/Projects/voice-controlled-pm/`.
