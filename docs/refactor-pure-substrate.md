# arcmux refactor: pure substrate (strip Elon)

Status: draft plan, no code changes yet.

## 1. Motivation

arcmux becomes a **pure librarian** of screens, agents, tmuxes, cmuxes:
register, send, read, ready-state, close, pulse, audit, respawn. All Elon
orchestration (teams, contracts, ICs, slots, charters, coach, forward plans,
role files) moves up to a separate **elonco** Python service that uses arcmux
as one of many substrates. arcmux should not know what a "team" is — only
agents.

## 2. What stays in arcmux (file-by-file)

22 non-test Go files under `internal/manager/`. Verdict legend: K = keep
(possibly rename), M = move concept to elonco, R = remove outright, N = rename.

| File | Verdict | Rationale |
|---|---|---|
| manager/cmd.go | R | `arcmux manager` CLI — replaced by `arcmux register` |
| manager/project.go | N | `Project`→`Registry`. Drop Elon scratchpad/role/mission seed; keep open-store + bootstrap + cmux-pane wiring |
| manager/open.go | K | Generic open helper |
| manager/paths/paths.go | N | Drop `GlobalRolesDir`/`ElonDir`/team-IC subtrees; keep `EphemeralRoot`, `StateBolt`, slug validation |
| manager/bootstrap/bootstrap.go | K | Already parameterized; strip any role-name hardcoding |
| manager/cmuxcli/client.go | K | cmux client |
| manager/cmuxcli/notify.go | K | Notification primitive |
| manager/pulse/pulse.go | N | Replace `Cadence{Elon,Manager,IC}` with per-agent `cadence_ms`; keep loop |
| manager/scaffold/project.go | M | `elon/`, `teams/`, `principles/`, `0Prompts/roles/` scaffolding moves to elonco; arcmux only mkdirs `EphemeralRoot` |
| manager/scaffold/time.go | K | Pure helper |
| manager/scratchpad/scratchpad.go | M | Scratchpad is an Elon artifact; agents that want one write their own |
| manager/roles/* | R | Embedded role markdown — vault-only in elonco |
| manager/store/db.go | N | Replace bucket list with agents/inbox/audit/meta; bump schema |
| manager/store/types.go | N | Keep `InboxMsg`, `AuditEntry`; remove `Team`/`Contract`/`Slot`/`Validation`/state consts; add `Agent` |
| manager/store/teams.go | R | Team CRUD |
| manager/store/contracts.go | R | Contract CRUD |
| manager/store/slots.go | R | Slot CRUD |
| manager/store/inbox.go | N | Replace `PushElon/Manager/ICInbox` with `PushInbox(agentID, msg)` |
| manager/store/audit.go | K | Universal |
| manager/store/meta.go | N | Drop `ElonPaneRef`/`ElonWorkspaceRef`; keep schema-version + generic kv |
| manager/teamspawn/teamspawn.go | R | Team spawn |
| manager/icspawn/icspawn.go | R | IC spawn |

Daemon and cmds:

| File | Verdict | Rationale |
|---|---|---|
| daemon/daemon.go | K | Core |
| daemon/http.go | N+ | Add `/v1/...` agent API; keep `/session/*` for back-compat |
| daemon/grpc.go | K | gRPC session ops |
| daemon/pulse_supervisor.go | N | Drop role-class cadence map |
| daemon/{events,exec_transport,handshake,prompt_delivery}.go | K | Substrate primitives |
| cmd/arcmux/main.go | N | Remove `manager`; add `register`; keep `start`/`pulse`/`version` |
| cmd/arcmux/pulse.go | K | Debug pulse shim |
| cmd/arcmux-call/main.go | N | Drop team/contract/ic dispatch |
| cmd/arcmux-call/{team,contract,ic,scratchpad}.go | R | Elon concepts |
| cmd/arcmux-call/inbox.go | N | `--to <agent-id>` |
| cmd/arcmux-call/audit.go | K | Generic |
| cmd/arcmux-e2e/main.go | N | Update to register-agent flow |
| cmd/arcmux-eval/main.go | M | Elon scenarios — moves to elonco |

## 3. What moves to elonco (concepts)

- Three-tier Elon/Manager/IC company model
- Role files (`elon.md`, `manager.md`, `ic-base.md`, `validator.md`,
  `coach.md`) and the `0Prompts/roles/` global library
- Teams, contracts, slots, contract state machine, HC caps, Validator gate
- Inbox verb semantics (`add`/`revise`/`retract`/`consult`/`escalate`)
- Scratchpad shape + per-turn journal/decisions/forward-plan/coach-reports
  vault tree
- "manager-mode" launch ceremony (mission seeding, vault scaffolding)
- Semantic addressing `elon` / `manager:<slug>` / `ic:<slot>` — elonco's CLI
  maps these names to arcmux agent IDs

## 4. New bbolt schema

`CurrentSchemaVersion = 2`. Buckets:

- `agents` — key=`agent-id`, value=JSON `Agent`
- `inbox` — parent bucket; per-agent nested sub-bucket keyed by `agent-id`,
  inside which keys are time-sortable msg IDs and values are JSON `InboxMsg`
- `audit` — append-only; `AuditEntry` gains optional `agent_id` + `owner_id`
- `meta` — schema version + generic kv

`Agent` JSON shape:

```
{
  "id":             "ag_01H...",           // generated, ULID-ish
  "owner_id":       "elonco:my-project",   // free-form caller tag (§8)
  "agent_type":     "claude|codex|shell|…",
  "display_name":   "elon: my-project",
  "screen_ref":     "cmux://workspaces/<uuid>/panes/<uuid>",
  "tmux_target":    "arcmux:1.%17",
  "bootstrap_path": "/Users/blin/data/arcmux/.../bootstrap.sh",
  "env":            { "FOO": "bar" },
  "cadence_ms":     30000,
  "ready":          { "ready": true, "reason": "stop-hook", "at": "…" },
  "state":          "registered|ready|busy|closed",
  "created_at":     "...",
  "updated_at":     "..."
}
```

No team/contract/slot indices. Caller-side indexing lives in elonco.

## 5. New HTTP API surface

Mounted on `localhost:8080` (configurable). JSON. Wrapped internals in parens.

- `POST /v1/agents` — `{owner_id, agent_type, display_name, screen_ref?,
  bootstrap_path?, env?, cadence_ms?}` → `{agent_id}`. If `screen_ref`
  omitted, arcmux creates a cmux workspace+pane via `cmuxcli.NewWorkspace`
  and `bootstrap.Render`. (wraps `project.Start` minus Elon seed)
- `GET /v1/agents?owner=<id>` → `{agents: […]}` (`store.ListAgents`)
- `GET /v1/agents/{id}` → `Agent` (`store.GetAgent`)
- `DELETE /v1/agents/{id}` → `{closed: true}` — closes pane, marks
  `state=closed`, keeps row for audit. (new `Close`)
- `POST /v1/agents/{id}/send` — `{body}` → `{msg_id, queued}`. Sends direct
  if ready, else queues. (`store.PushInbox` + `daemon.deliverPrompt`)
- `GET /v1/agents/{id}/inbox` → `{messages: […]}` (`store.PeekInbox`)
- `POST /v1/agents/{id}/inbox/{msg_id}/ack` → `{acked: true}`
  (`store.AckInbox`)
- `GET /v1/agents/{id}/ready` → `{ready, reason, last_signal_at}`
- `GET /v1/agents/{id}/screen` → `{text}` (`daemon.Capture`)
- `GET /v1/audit?owner=&since=&agent_id=` → `{entries: […]}`
  (`store.RecentAudit`)
- `GET /v1/events` — SSE of `{type, agent_id, at, detail}` for
  `registered|ready|inbox-grew|state-changed|closed`. (new pubsub fed from
  store writes + pulse + hooks)

Hooks (`UserPromptSubmit`, `Stop`, `Notification`) still update ReadyState;
they now write by `agent_id` instead of role label.

## 6. Migration order (4 commits, each green)

1. **C1 — additive schema, no removals.** Add `agents` + new `inbox` bucket
   layout alongside existing buckets. Bump schema to 2 with tolerant init
   (reads v1 or v2). Add `Agent` type and `store.PutAgent`/`GetAgent`/
   `ListAgents`. No callers yet. **Accept:** all existing tests green; v1
   fixtures still open.
2. **C2 — new HTTP API + `arcmux register`, gated.** Implement `/v1/agents*`,
   `/v1/audit`, `/v1/events`. Leave `arcmux manager` and team/contract/ic
   code in place. **Accept:** new e2e registers via HTTP, sends, peeks,
   acks, closes; manager-mode e2e still passes.
3. **C3 — port arcmux-call inbox/audit to agent IDs.** `inbox push --to
   <agent-id>`. Deprecate `--to elon|manager:X|ic:Y` (warning; still works
   for one cycle via legacy buckets). **Accept:** both flag forms work;
   deprecation warning visible.
4. **C4 — remove the Elon surface.** Delete `cmd.go`, `roles/`, `teamspawn/`,
   `icspawn/`, `store/{teams,contracts,slots}.go`, `inbox-elon|managers|ics`
   buckets (after running §7 migrator), `arcmux-call team|contract|ic|
   scratchpad`. Rename `project.go` → `registry.go`, `Project` → `Registry`.
   Drop role-class fields on Cadence. **Accept:** `go build ./...` green;
   tests green; help text + audit actions no longer mention elon/manager.

Optional C5: rename `internal/manager/` → `internal/registry/`. Mechanical,
do after elonco's first usable release.

## 7. Migration of existing state.bolt

**Recommendation: one-shot migrator that dumps to JSON for elonco to ingest.**

The state is dev-time only, so throwaway (option a) is technically fine. But
the team/contract/audit rows are the only realistic fixture for elonco's
first ingest path — discarding them forces fabricated fixtures.

Build `cmd/arcmux-migrate-v1/main.go`: walks legacy buckets and writes
`migration-<project>-<timestamp>.json` with `{teams, contracts, slots,
inbox_elon, inbox_managers, inbox_ics, audit}`. Run once per project before
C4 drops the buckets. Re-emit audit rows into the v2 audit bucket so
substrate history survives. Read-only, idempotent.

## 8. API authentication

**Minimum: no tokens, `owner_id` required.** Every `POST /v1/agents` carries
a non-empty `owner_id` (e.g. `elonco:my-project`, `elonco:research-cluster`,
`soloist`). Recorded on the agent record and every audit entry. Listing and
audit filter by it. With 5 elonco instances against one arcmux, attribution
stays honest. No allowlist validation yet — substrate is still trusted
within user-scope (localhost-bound, same Unix user). Token auth is the next
commit and would hang off this field.

## 9. Backward-compat aliases

Disappearing subcommands → elonco CLI replacements:

| arcmux-call (gone) | elonco CLI (new) |
|---|---|
| `team list/get/spawn/dissolve` | `elonco team …` |
| `contract create/list/get/transition/deps` | `elonco contract …` |
| `ic list/get/spawn/dissolve` | `elonco ic …` |
| `scratchpad read/write` | `elonco scratchpad …` (vault-backed) |
| `inbox push --to elon\|manager:X\|ic:Y` | `elonco send <name> "<body>"` (resolves to agent id, calls `POST /v1/agents/{id}/send`) |
| `arcmux manager <agent> <project>` | `elonco launch <project> --mission "…"` |

`arcmux-call` itself stays — useful for substrate ops: `inbox push --to
<agent-id>`, `audit recent`, and the existing daemon-mediated `create`/
`send`/`capture`/`status`.

## 10. Open questions

- [OPEN] arcmux owns bootstrap rendering, or elonco hands over a rendered
  script path? Default: keep, parameterized.
- [OPEN] SSE vs WebSocket on `/v1/events`. Default: SSE only.
- [OPEN] Keep `/session/*` HTTP routes or fold into `/v1/agents`? Probably
  fold, but verify Codex/cmux integrations first.
- [OPEN] Per-agent display configs (workspace title, description,
  focus-on-spawn) — pass through `POST /v1/agents` as `screen_opts`? Confirm
  shape before C2.
- [OPEN] Pulse: per-project (per state.bolt) or one global pulse over a
  shared agents bucket? With multi-tenant `owner_id`, the latter is simpler.
  Default: one global state.bolt per daemon.
- [OPEN] Schema-version bump policy: hard-fail v1 on C4 (migrator mandatory)
  or auto-migrate on open? Default: hard-fail with a pointer to the migrator.

---

## Blast radius

`internal/manager/` non-test Go files: **22**.

- KEEP as-is: **5** (cmuxcli/client.go, cmuxcli/notify.go, open.go,
  scaffold/time.go, store/audit.go)
- KEEP with rename / surface trim: **8** (project.go, paths/paths.go,
  bootstrap/bootstrap.go, pulse/pulse.go, store/db.go, store/types.go,
  store/inbox.go, store/meta.go)
- MOVE concept to elonco: **2** (scaffold/project.go, scratchpad/scratchpad.go)
- REMOVE outright: **7** (cmd.go, roles/seeds.go + roles/files/*.md as a unit,
  teamspawn/teamspawn.go, icspawn/icspawn.go, store/teams.go,
  store/contracts.go, store/slots.go)

Net: 7 deleted, 8 modified, 5 untouched, 2 conceptually relocated. Largest
risk is the bbolt schema change — mitigated by additive C1 + the §7 migrator.
