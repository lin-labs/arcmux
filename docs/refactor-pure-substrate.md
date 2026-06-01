# arcmux refactor: pure substrate (strip Elon)

Status: draft plan, no code changes yet.

## 0. Revision summary (after Boyan's answers)

The original plan proposed a parallel `/v1/agents/*` HTTP namespace. Boyan
flagged that gRPC already covers most of the substrate (CreateSession,
SendPrompt, Capture, Status, Kill, ListSessions, StreamOutput, Subscribe)
and that involved consumers (elonco, future orchestrators) prefer it over
HTTP. Existing HTTP `/session/*` is thin (3 routes) but voxtop relies on it
— preserve verbatim.

So the API plan is now:
- **gRPC extended** with the 4 missing capabilities (queued Send, PeekInbox,
  AckInbox, Ready predicate, QueryAudit) + `owner_id` field on Session.
- **HTTP `/session/*` preserved as-is** for voxtop. Optionally extended
  later if elon-board needs browser-friendly endpoints.
- **No `/v1/agents/*` parallel namespace.** Drop §5 of the original plan.

Other directional answers applied:
- arcmux does NOT render role-primed bootstrap scripts. elonco renders its
  own launch script and passes the path (or a pre-created cmux pane's
  screen_ref) via existing `CreateSession`. **`bootstrap.Render` either
  disappears or strips to a generic shell that just exports env.**
- Per-project state.bolt preserved (current model). Add a small global
  index file so multiple projects are programmatically enumerable.
- Existing state.bolt data is dev-time throwaway. No migrator needed.
- Voxtop preservation: keep `/session/*` HTTP routes verbatim; new gRPC
  RPCs are additive (don't break existing message shapes).

This shrinks the blast radius substantially. Revised migration sequence is
in §11 (replaces §6).

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
	- Unsure about what "rendered script" mean? Is that the starting prompt? If so that's elonco's responsibility, or caller's responsibility, arcmux do not own the customized prompts.
- [OPEN] SSE vs WebSocket on `/v1/events`. Default: SSE only.
	- What would this be used for? For pulling one particular screen/agent's events? If so, a lightweight one would make sense because again there will be many screens being monitored
- [OPEN] Keep `/session/*` HTTP routes or fold into `/v1/agents`? Probably
  fold, but verify Codex/cmux integrations first.
	- keep, /v1/agents related might be used in voxtop (checkout the app), if so, yeah needs to keep.
- [OPEN] Per-agent display configs (workspace title, description,
  focus-on-spawn) — pass through `POST /v1/agents` as `screen_opts`? Confirm
  shape before C2.
	- don't understand the question.
- [OPEN] Pulse: per-project (per state.bolt) or one global pulse over a
  shared agents bucket? With multi-tenant `owner_id`, the latter is simpler.
  Default: one global state.bolt per daemon.
	- it's best to have per-project state.bolt. They can have different storage types, but some common interfaces.
	- There could be a global state that maps to different states and can retrieve them programmably.
- [OPEN] Schema-version bump policy: hard-fail v1 on C4 (migrator mandatory)
  or auto-migrate on open? Default: hard-fail with a pointer to the migrator.
	- current data storage don't matter, migration versioning problem does not exist.

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

---

## 11. Revised migration sequence (supersedes §6)

The original §6 ("4 commits") was sized for the bigger /v1/agents/* surface.
With that dropped, the migration becomes tighter — 5 commits, each green:

### C1 — additive: gRPC extensions + owner_id field

- Extend `proto/arcmux/v1/arcmux.proto`:
  - Add `string owner_id` to `CreateSessionRequest`, `SessionSummary`,
    `SessionState` (or wherever Session is canonical).
  - Add new RPCs: `Send` (queueable variant of SendPrompt), `PeekInbox`,
    `AckInbox`, `Ready`, `QueryAudit`.
  - Regenerate `.pb.go` files.
- Add `Session.owner_id` to in-memory + bbolt persistence (additive — empty
  string default is back-compat).
- Wire the new RPC methods to existing daemon internals where possible
  (e.g. `Send` falls back to `SendPrompt` when ready; queues to a new
  `inbox` bucket when not).
- Accept: existing tests green; new gRPC methods callable via the existing
  `arcmux-call` shim or direct grpc client.

### C2 — strip role-file embed; arcmux loses opinions about agent prompts

- Delete `internal/manager/roles/files/*.md` and `roles/seeds.go`. Role
  files become elonco-managed at `$OBS_AGENTS/0Prompts/roles/` (already the
  vault canonical location; arcmux just stops embedding copies).
- Strip role-aware language from `internal/manager/bootstrap/bootstrap.go`
  (rename Options field `Role` to `Tag` or remove; the script writer
  doesn't need to know what role it's launching). OR delete the package
  entirely if elonco can render its own scripts; defer that call to C5.
- Delete `internal/manager/scaffold/project.go`'s scaffolding of
  `0Prompts/roles/` (vault tree creation moves to elonco).
- Accept: `make validate` green; `arcmux manager` still creates a workspace
  (just without role priming — elonco will do that later).

### C3 — remove team/contract/IC concepts from arcmux

- Delete `internal/manager/teamspawn/`, `internal/manager/icspawn/`.
- Delete `internal/manager/store/{teams,contracts,slots}.go`.
- Drop team/contract/slot buckets from `store/db.go::AllBuckets`.
- Drop role-class fields from `internal/manager/pulse/pulse.go` (replace
  `Cadence{Elon,Manager,IC}` with a single per-session `cadence_ms`).
- Delete `cmd/arcmux-call/{team,contract,ic,scratchpad}.go` subcommands.
- Update `cmd/arcmux-call/inbox.go` to use `--to <session-name|session-id>`
  uniformly (drop the `manager:` / `ic:` namespacing).
- Accept: `make validate` green; gRPC tests green; e2e team-spawn-pipeline
  scenario is REMOVED (moves to elonco's eval harness).

### C4 — strip `arcmux manager` subcommand

- Delete `internal/manager/cmd.go`.
- Delete `internal/manager/scratchpad/` (concept moves to elonco).
- Rename `internal/manager/project.go` → `internal/registry/session.go`
  (and the `Project` type → `Session` or `Registration`). This makes the
  name match what the package actually does post-refactor.
- Optional: rename `internal/manager/` → `internal/registry/`. Mechanical
  but invasive; defer to C5 if it slows C4.
- Accept: `make validate` green; `arcmux help` no longer mentions
  Elon/manager/teams/ICs.

### C5 — global index for multi-project enumeration

- Add `~/data/arcmux/_index.bolt` (or `_index.json` — bbolt for consistency
  with other state files). Maps `owner_id → [project-slug]` and tracks the
  per-project state.bolt path.
- Daemon's PulseSupervisor reads this on startup AND watches the data root
  for new state.bolt files (existing behavior; just makes the index
  explicit so callers can query "what projects exist?" without scanning
  the filesystem).
- Add `ListProjects` RPC (returns the index contents).
- Accept: `make validate` green; new gRPC ListProjects returns the live
  set.

### Out of scope for this refactor

- Token auth (deferred; owner_id stays trusted-within-user-scope)
- bbolt → other storage backends (the "common interface" Boyan flagged) —
  add the interface in C1, but don't implement alternative backends yet
- Readiness gate (§F16 from prior task list) — separate slice; this
  refactor unblocks it by making `Ready` a first-class RPC
