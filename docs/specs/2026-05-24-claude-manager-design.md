---
project: arcmux
spec: claude-manager / codex-manager mode
date: 2026-05-24
status: draft
authors: [boyan, claude]
related:
  - "[[arcmux/agent-tmux-runtime-service]]"
  - "[[arcmux/http-api]]"
---

# arcmux `claude manager` / `codex manager` — Three-Tier Multi-Agent System

## 1. Summary

A new launch mode for arcmux: `arcmux claude manager <project>` (or `arcmux codex manager <project>`) boots a three-tier multi-agent system inside cmux:

- **Elon** — frontline communicator + system-wide orchestrator. Takes user orders, decides priorities, manages org structure, learns from decisions. **The only globally evolving entity.**
- **Manager** — team-scoped tech lead. Decomposes goals into IC contracts, dispatches, reviews work, manages dependency DAG, escalates only when its own principles can't decide.
- **IC** — individual contributor in a persistent HC slot. Executes one contract at a time against explicit acceptance_criteria. Single inbound channel: its manager.

arcmux is the **substrate**: it owns message routing, comm-graph enforcement, pane lifecycle, crash recovery, notification gating, and durable state. Agents are **fungible role-holders** — identity lives in arcmux's files (principles, journals, contracts), not in the agent's chat scrollback.

This design grounds itself in production multi-agent research: Anthropic's orchestrator-worker contract pattern, LangGraph's queued-injection over hard-interrupts, Cognition's "write-decisions serialized through tech-lead", InterruptBench's add/revise/retract triad, and cmux's primitives-over-prescription philosophy.

## 2. Goals

- Take user orders continuously, with priority awareness and graceful course-correction (`add` / `revise` / `retract` semantics)
- Multi-pane visibility — user sees the right team at the right time
- Quality and speed together — proactive spot-checks, mandatory validator role, crisp acceptance criteria
- Self-improving — learn from every decision; principles grow per-project and globally
- Crash-resilient — Elon and Manager are not single points of failure; fresh instances pick up from durable state
- Honors comm graph mechanically — agents can't bypass the rules even if they "want to"

## 3. Non-Goals

- Cross-project Elon-to-Elon coordination (separate Elons stay isolated)
- Real-time IC-to-IC chat (v2 feature; reserved API surface only)
- Cloud / remote agents (local cmux + arcmux only for v1)
- Replacing arcmux's existing lab-service capabilities (extension, not rewrite)

## 4. Communication Graph

```
                      ┌────────────────┐
                      │      USER      │
                      └─┬──┬──────┬────┘
       preferred down ↓ │  │      │ ↑ primary up (replies, status, deliverables)
                        │  │      │
                  ┌─────┴──┴──────┴───┐
                  │       ELON       │ ←─ consult ── ┐
                  │ (front desk +    │                │
                  │  orchestrator)   │ ── decide ───→ │
                  └─────────┬────────┘                │
              tactical down │ ↑ escalations           │
              ┌─────────────┴──────────────┐          │
              │         MANAGER            │ ─────────┘
              │   (tech lead / dispatcher) │
              └────────────┬───────────────┘
                contracts ↓ ↑ results / blockers
                  ┌──┬──┬──┴──┬──┐
                  │IC│IC│ IC  │IC│   (≤4 ICs per team, includes Validator at HC≥2)
                  └──┴──┴─────┴──┘
                       │   never proactively pushes to user
                       │   user may inspect-pull on demand
```

### Edge contract

| # | Edge | Direction | Allowed? | Trigger / payload |
|---|---|---|---|---|
| 1 | User → Elon | down | **preferred** | New orders, status questions, course-corrections (`add`/`revise`/`retract`) |
| 2 | Elon → User | up | **primary** | Acks, status digests, deliverables, decision asks Elon itself needs |
| 3 | User → Manager | down | allowed back-channel | Tactical details Elon shouldn't filter |
| 4 | Manager → Elon (consult) | up | core control | Manager-to-Elon consult; Elon decides directly OR escalates to user. Manager never directly notifies user under normal flow. |
| 5 | Elon → User (escalation) | up | core | Elon escalates with full context; user replies with decision + reasoning; Elon learns the principle |
| 6 | User → IC | down | allowed (inspect/poke) | Direct chat into a worker pane |
| 7 | IC → User | up | **forbidden** as proactive push | ICs never initiate user notifications |
| 8 | Elon ↔ Manager | both | core control channel | Elon: goal contracts, priority changes, cancellations. Manager: progress digests, consults. |
| 9 | Manager ↔ IC | both | core execution channel | Manager: contract (4-field). IC: artifact / `BLOCKED:` |
| 10 | IC → Manager (escalation) | up | preferred path | All IC upward signal funnels through Manager (manager holds most context) |
| 11 | IC → Elon direct | up | **fallback only** | When Manager unreachable; arcmux opens this channel only on channel-failure detection |

### Notification routing

arcmux is the **only** thing that can fire `cmux notify`. Agents call `arcmux-call notify`; arcmux applies graph rules before deciding what cmux sees. Channel-failure detection (Elon pane unhealthy) opens notify rights one tier down with a `[channel:elon-down]` tag in the message.

| Surface event | cmux primitive | When |
|---|---|---|
| Continuous team progress | `cmux set-progress` | State updates from bbolt |
| Per-pane glanceable state | `cmux set-status` | State transitions |
| Activity log | `cmux log` | Every audit entry |
| "Look at this pane" | `cmux trigger-flash` | Artifact delivered; decision pending |
| User-attention notify | `cmux notify` + OSC9/macOS | Elon escalation; goal complete; hard failure; channel-failure fallback |

### Elon never mutes anyone

Muting is impossible because Elon doesn't own the suppression switch — arcmux does. If Elon's preferred-channel path fails, arcmux opens the downstream channel and tags the message.

## 5. Surface Topology

```
Cmux Window — "arcmux: <project>"
│
├─ Workspace ★ "elon"           (pinned, always present)
│    ├─ Pane: elon              — Claude/Codex with elon role
│    └─ Pane: overview          — read-only digest of all teams
│
├─ Workspace "team:<slug-1>"     (spawned by Elon reactively)
│    ├─ Pane: manager           — persistent until team dissolved
│    ├─ Pane: ic-a              — slot (persistent until HC shrinks)
│    ├─ Pane: ic-b
│    └─ Pane: ic-c
│
├─ Workspace "team:<slug-2>"     ...
└─ ...
```

- Cold start = Elon workspace only
- Teams spawn reactively (only when no existing team can absorb the order)
- Workspace-per-team gives ⌘N team switching
- Multiple missions = multiple cmux windows (alternative surface design A reserved for future)

## 6. Storage

### Global (cross-project, Elon-authored evolving library)

```
~obsAgents/0Prompts/roles/
├── elon.md           ← Elon's soul (only truly evolving entity)
├── manager.md        ← base manager
├── ic-base.md        ← base IC
├── linus.md, jobs.md, turing.md, ...   ← specialized ICs (extend ic-base)
└── <new>.md          ← Elon authors on demand when no profile fits
```

Each role file has `## Stable` and `## Provisional` sections. Promotion from Provisional → Stable for non-Elon roles requires K confirmations across projects; for `elon.md` itself, requires explicit user approval.

### Per-project durable (vault-backed)

```
~obsAgents/Projects/<project>/
├── arcmux/
│   ├── README.md, mission.md
│   ├── playbook.md           ← project-specific overrides to defaults
│   ├── principles/           ← project-specific addendums
│   │   ├── elon.md, manager.md, ic-<role>.md, gotchas.md
│   └── deliverables/
├── elon/
│   ├── journal.md            ← APPEND-ONLY activation log
│   └── decisions.md          ← curated key decisions
├── teams/
│   └── <team-slug>/
│       ├── charter.md        ← team's vision (crystallizes over time)
│       ├── journal.md
│       └── decisions.md
└── retros/
    └── <goal-slug>-YYYY-MM-DD/
        ├── elon.md, manager-<team>.md, ic-<role>-<id>.md, _aggregate.md
```

### Per-project ephemeral (machine-local)

```
~/data/arcmux/<project>/
├── state.bolt                ← all structured coordination (bbolt, single file)
├── scratchpads/
│   ├── elon.json, team-<slug>-manager.json, ic-<contract-id>.json
├── consult_inboxes/          ← file-based for atomic append
└── heartbeats/
```

`state.bolt` buckets:

```
state.bolt
├── teams/                     key=team-slug              value=json(team doc)
├── contracts/                 key=contract-id            value=json(contract doc)
├── idx-team-contracts/        key=team-slug/contract-id  (index)
├── idx-deps-parent/           key=parent/child           (forward DAG)
├── idx-deps-child/            key=child/parent           (reverse DAG)
├── idx-state/                 key=state/contract-id      (state filter)
├── idx-priority/              key=priority/contract-id   (priority order)
├── inbox-elon/                key=ts-uuid                (orders/consults)
├── inbox-manager-<slug>/      key=ts-uuid
├── audit/                     key=ts-uuid                (audit log)
└── meta/                      key=schema-version etc.
```

DB choice: **bbolt** (pure Go, single file, B+tree, MVCC reads, etcd-grade maturity). BadgerDB reserved as upgrade path.

## 7. Elon — Three Activation Modes

### Mode 1 — User Request (interactive, urgent)

- **Trigger**: User types into Elon's pane (UserPromptSubmit hook)
- **Goal**: Clarify against system context, decide priority (or ask), queue
- **Procedure**: Read state.bolt + relevant scratchpads + principles → check conflicts → ask clarifying Q if ambiguous → assign priority → triage `add`/`revise`/`retract` → route or stage spawn
- **Interrupts Review**: yes

### Mode 2 — Escalation Handling (interactive, reactive)

- **Trigger**: Manager (or IC as fallback) wrote consult
- **Goal**: Decide with own judgment OR escalate to user with full context
- **Procedure**: Read consult + state + principles → if covered, decide and write to manager response inbox → if not, compose user-facing escalation; arcmux fires `cmux notify`
- **Latency**: fast (manager blocked)
- **Interrupts Review**: yes

### Mode 3 — Elon Review (proactive, scheduled)

- **Trigger**: Cadence (default 15 min) OR signal-based (team flagged, contract overdue, K confirmations reached)
- **Goal**: Execution rigor. Apply first-principles thinking + retrospective. Don't trust manager reports — verify against artifacts.
- **Procedure**:
  1. For each team: first-principles check (right charter? sound decomposition? crisp contracts?)
  2. Retrospective on recent closed contracts (what worked, what was waste)
  3. Manager sanity check (escalation altitude, autonomous decisions)
  4. **Light retro** — read `~obsAgents/Sessions/*.md` for friction/course-corrections (see §10)
  5. Walk teams for HC, charter, principle promotions
  6. Write findings to project principles (and propose global)
- **Interrupts Review**: yields to Modes 1/2

### Response schema

```json
{
  "tick_id": "<uuid>",
  "decisions": [
    {"verb": "spawn-team|route-order|answer-consult|escalate-to-user|promote-charter|shrink-team|dissolve-team|no-op", ...}
  ],
  "scratchpad_update": "<≤20 lines>",
  "next_tick_hint": "event-driven|after:<N>s"
}
```

User-facing prose precedes the fenced JSON block. arcmux parses and applies; invalid decisions return as next tick events.

## 8. Team Model

| HC | Composition | Validator required? |
|---|---|---|
| 0 (sustained) | manager idle → team archives | n/a |
| 1 | manager + 1 IC | no (solo IC self-validates) |
| 2 | manager + 2 ICs (one is Validator) | **yes** |
| 3 | manager + 3 ICs (Validator + 2 domain) | yes |
| 4 (cap) | manager + 4 ICs (Validator + 3 domain) | yes |

- HC counts ICs only, not manager.
- Max team size: 5 panes per cmux workspace.
- No global cap on number of teams or concurrent agents — HC-utilization is the natural throttle.

### Team formation — reactive-only, crystallizing

| Phase | When |
|---|---|
| **Phase 1 — Creation** | Urgent order arrives, no team fits → spawn team with minimal charter (the one task) |
| **Phase 2 — Crystallization** | Related future orders route into existing team; after K matched orders, Elon promotes the team's charter from "handle X" to "owns X domain" |

**Top-down org design is prohibited.** Elon never spawns teams in anticipation.

### HC lifecycle

```
Charter approved → spawn team @ HC 1
        ↓
Workload arrives → manager justifies +1 IC (within allocated HC) OR escalates HC bump to Elon
        ↓
Utilization < 50% for K ticks → manager requests shrink (Elon decides)
        ↓
HC reaches 0 sustained → dissolve team (close manager pane, archive scratchpad)
```

ICs finish in-flight contracts before being released. Manager dissolves last.

## 9. Manager — Three Modes

### Mode 1 — Intake

- **Trigger**: Elon-dispatched goal lands in team inbox, OR user types directly into manager pane
- **Goal**: Decompose into IC contracts; pick IC profile; dispatch
- **Output**: Contracts written to `state.bolt:contracts/`; dispatch decisions in JSON

### Mode 2 — Escalation (bidirectional)

- **Incoming**: IC consult OR Validator needs-work flag → decide or escalate
- **Outgoing**: Manager's own consult to Elon → wait for next tick

### Mode 3 — Manager Review (proactive)

- **Trigger**: Cadence (default 10 min) OR signal-based
- **Procedure**:
  1. Validator-feedback synthesis (recurring issues → principles)
  2. Contract-quality audit (acceptance_criteria crispness)
  3. **Proactive spot-checks** — read IC artifacts directly, decide: continue / feedback / lateral redistribution / cancel-and-respawn-with-different-profile
  4. HC check + critical-path optimization on DAG
  5. Charter update if domain has shifted
  6. Journal append

### Contract schema (4-field, arcmux-enforced)

```yaml
contract_id: c-3f1a
team: team-auth-7a2
ic_role: linus              # which 0Prompts/roles/*.md to load
priority: 2
objective: >
  Replace JWT verify with OIDC introspection in middleware/auth.go
output_format: >
  PR against main; passes existing auth_test.go suite
tools: [bash, edit, read, grep]
boundaries:
  - Do not modify session cookie format
  - Do not touch frontend
acceptance_criteria:        # Validator checks these mechanically
  - auth_test.go passes
  - middleware/auth.go imports only the new oidc package
  - no new env vars without .env.example update
depends_on: [c-1a]
parallelizable_with: []
capstone: false             # true → triggers heavy retro on validator-pass
deadline: 2026-05-24T18:00 PT
```

arcmux rejects incomplete contracts at write time.

### Dependency DAG

Manager owns a per-team DAG in `state.bolt`. arcmux-enforced rules:

- Contract can't transition `pending → working` until all `depends_on` are `completed`
- DAG cycle detection on every dep-add (cycles rejected)
- Mode 3 reviews critical path; manager can split work to flatten

### Authoring scope

| File | Who can write |
|---|---|
| `0Prompts/roles/manager.md` (global) | **Elon only** |
| `0Prompts/roles/ic-*.md` (global) | **Elon only** |
| `arcmux/principles/manager.md` (project) | Manager |
| `arcmux/principles/ic-<role>.md` (project) | Manager |
| `arcmux/principles/gotchas.md` (project) | Manager + Validator |
| `teams/<slug>/charter.md`, `journal.md`, `decisions.md` | Manager |

Manager flags generalizable wisdom with `propagate-up: true` in journal; Elon picks up at next Review and decides global promotion.

## 10. IC — Base Role + Slot Model

### Slot ≠ contract

- IC pane = persistent HC slot, not per-contract ephemeral
- One contract at a time per slot
- Slot persists across many contracts until HC shrinks the slot
- Each contract: fresh scratchpad, fresh acceptance criteria, no carry-over reasoning

### State machine

```
idle → working → {blocked, complete, cancelling, failed}
blocked → working (after manager reply)
complete → validating → {pass→idle, needs-work→working}
cancelling → idle (after cooperative wrap-up)
```

### Activation paths (no proactive Review)

| Path | Trigger | Behavior |
|---|---|---|
| Execute | Contract spawn or resume from blocked | Work toward acceptance_criteria; update scratchpad per step |
| Handle manager message | Manager posts to IC's inbox | Yield at next safe checkpoint; apply (clarification, scope tweak, cancel, handoff) |
| Escalate | Unresolvable ambiguity / missing context | Write consult to manager; transition to `blocked` |

### Cooperative checkpoint discipline

Between every atomic step:

```
check cancel_flag       → handle and exit cleanly if set
check manager inbox     → read and apply if message
check step budget       → escalate if exceeded
check stuck signal      → escalate if blocked
```

No SIGINT, ever. All cancellation is cooperative.

### IC isolation (arcmux-enforced)

| IC inbound (allowed) | IC inbound (rejected) |
|---|---|
| Initial contract on spawn | Messages from other ICs (v1) |
| Messages from its manager | Messages from Elon directly |
| Direct chat from user (inspect/poke) | Messages from other teams' managers |

IC outbound: deliverables, status updates to its manager, consults to its manager. Never to user, Elon, or other ICs (in v1).

### Base IC role file seed

Lives at `0Prompts/roles/ic-base.md`. Specialized roles declare `extends: ic-base` in frontmatter. Bootstrap loads ic-base + specialized + project-specific principle file.

Operating principles:

1. The contract is your bible. Stay inside boundaries; acceptance criteria are non-negotiable.
2. Update scratchpad after every meaningful step (respawn-friendly).
3. Checkpoint between steps (cancel/inbox/budget/stuck).
4. Don't decide your work is "done" — Validator decides.
5. Escalate early, not late. Sunk-cost pushing-through is a failure mode.
6. Stay focused — don't refactor neighbors. Note follow-ups for manager.

## 11. Retrospectives

### Light retro — every Mode 3 Review

Single-agent. Elon reads `~obsAgents/Sessions/*.md` for itself and active managers, looking for friction / course-corrections / waste vs stated rationale in journal. Findings → journal `### Retro observations`; patterns → Provisional principle.

### Heavy retro — goal-finishline triggered

Multi-agent coordination.

- **Trigger**: capstone-contract validator-pass OR `arcmux-call retro start --goal <slug>`
- **Procedure**:
  1. Elon announces heavy retro; arcmux pauses non-essential contracts on affected teams
  2. Each participant (manager + each IC who worked on the goal) writes their own retro citing session-log turn timestamps
  3. Elon waits, then aggregates → `_aggregate.md`
  4. Generalizable lessons promote up (project → global Provisional)
- **Output**: `retros/<goal-slug>-YYYY-MM-DD/` durable in vault

## 12. arcmux Daemon Architecture

Single long-lived daemon (lab service), multi-tenant by project key. One daemon serves all Elon-companies; each project is fully isolated by directory + bbolt file.

### New packages

```
internal/manager/
├── project.go        ← per-project state machine
├── playbook.go       ← rule loader / evaluator
├── notify.go         ← comm-graph routing + cmux notify gating
├── respawn.go        ← crash detection + respawn ladder
├── scratchpad.go     ← scratchpad read/write API
├── decisions.go      ← decision log + learning loop
├── retro.go          ← light + heavy retro orchestration
├── cmux/
│   ├── client.go     ← cmux CLI client (new-workspace, new-pane, send, ...)
│   └── events.go     ← long-lived cmux events stream subscription
└── store/
    ├── db.go         ← bbolt open/close, bucket creation
    ├── contracts.go  ← contract DAO (CRUD + state transitions)
    ├── teams.go      ← team DAO
    ├── queue.go      ← DAG / dep queries
    ├── inbox.go      ← inbox push/pop
    └── audit.go      ← audit append
```

Existing packages (`session`, `delivery`, `hooks`, `profile`, `tmux`) keep working. `tmux` retires once cmux client covers every case.

### Launch flow

```
arcmux claude manager <project>
    ↓
arcmux-call (CLI) → daemon: "start manager-mode for project=<name>, agent=claude"
    ↓
Daemon scaffolds ~/data/arcmux/<project>/ + ~obsAgents/Projects/<project>/{arcmux,elon,teams}/
    ↓
Daemon opens state.bolt; creates cmux workspace "🎩 elon" (pinned)
    ↓
Daemon spawns one pane, runs `claude` with elon role primed + bootstrap context
    ↓
Daemon starts the project's event-driven activation loop
    ↓
CLI returns; user types to elon
```

`arcmux codex manager <project>` is identical except step "claude" → "codex".

### Ready-to-receive detection (layered)

| Layer | Source | Latency | Role |
|---|---|---|---|
| Hooks (primary) | UserPromptSubmit, Stop, SessionStart | in-band | State machine ground truth |
| cmux events (push) | Single `cmux events` subscription | ~ms | Pane death, surface close, focus changes |
| Screen classifier (backup) | `cmux read-screen` on suspicion | ~100ms | Detects permission prompts, missed hooks |
| Pre-flight (critical) | Synchronous read before sensitive send | ~100ms | First dispatch into fresh pane |

Every `cmux send` goes through a **per-pane send queue** (Go channel, ordered). Drains when state machine says pane is `Idle`. Mid-response sends buffered, not dropped. arcmux is the SMTP-style relay between tiers.

### Crash recovery

Detection: PID gone / health stuck > N ticks / heartbeat miss / unexpected state.

Respawn ladder:
1. Soft prompt (`/resume — re-read state and continue`)
2. Restart-in-place (fresh agent in same cmux surface)
3. Reseat (close surface, new surface, then step 2)
4. Escalate (3 consecutive failures → `cmux notify` user with `[role-respawn-failed]`)

Every pane writes a scratchpad after each LLM turn so respawn can read durable state and resume. Bootstrap prompt for respawned pane says: *"You may be a fresh instance picking up mid-mission. READ FIRST: principles, state, scratchpad, recent decisions. Do not act until you have read these."*

Per-tier criticality:

| Role | Crash impact | Respawn priority |
|---|---|---|
| Elon | High (user-facing) | Fast (< 5s) |
| Manager | Medium (team halt) | Fast, team-scoped |
| IC | Low (contract idempotent) | Manager re-dispatches |

## 13. Concurrency

| Resource | Pressure at 360 entities | Bottleneck |
|---|---|---|
| Goroutines | ~1k | No |
| Hook events / sec | ~50–200 | No |
| cmux events stream | Single subscription | No |
| cmux CLI fork/exec | Solved by events-stream + on-suspicion reads only | No |
| Pane processes (memory) | ~60 GB at full | **Yes** — mitigated by pause-on-idle |
| LLM rate/cost | $$$ at scale | External — HC-utilization is the throttle |

No global cap on concurrent reasoning agents. The HC-utilization controller is the natural throttle. Idle teams shrink and dissolve.

## 14. Playbook Defaults

Default `~obsAgents/Projects/<project>/arcmux/playbook.md`:

```markdown
# arcmux playbook (defaults)

## Team formation
- Reactive-only spawn. Do not create teams in anticipation.
- New order has no team fit AND is time-sensitive → spawn.
- Otherwise enqueue or route to existing team.

## HC
- Validator mandatory at HC ≥ 2.
- Manager spawns within allocated HC freely; HC bumps escalate to Elon.
- Utilization < 50% sustained K ticks → manager requests shrink.
- HC=0 sustained → team dissolves.

## Charter promotion
- After K=3 related orders routed to same team → promote charter from
  "handle X" to "owns X domain".

## Review cadence
- Elon Review: 15 min, signal-driven.
- Manager Review: 10 min, signal-driven.

## Notifications
- IC → user push: forbidden (arcmux drops).
- Manager → user direct: only on channel-failure (Elon unhealthy).
- Elon escalation → user: `cmux notify` + flash on Elon pane.

## Retros
- Light retro embedded in every Elon Review.
- Heavy retro on capstone-contract validator-pass.

## Crash recovery
- Respawn ladder: soft prompt → restart-in-place → reseat → escalate.
- Bootstrap requires durable-state read before any action.
```

User can hand-edit. Project-specific overrides outrank defaults.

## 15. Future Features (Reserved Not Implemented)

- **IC-to-IC communication** via `arcmux-call ic message --to <ic-id> --topic <slug>`. Manager-authorized one-shot channels; all audited. Reserved API surface.
- **Multiple Elons in one window** (surface design A): workspace = company, surface = team, tmux inside surface for manager + ICs. Migration friendly since state is team-keyed not topology-keyed.
- **Project-specific role overrides** at `arcmux/roles/<role>.md` that supersede global. Not needed yet.
- **Cross-project Elon coordination** (multi-mission promotion proposals across Elons). Currently each Elon is fully isolated.

## 16. Tunable Constants (Playbook Defaults)

| Constant | Default | Where |
|---|---|---|
| Elon Review cadence | 15 min | Playbook |
| Manager Review cadence | 10 min | Playbook |
| Heartbeat tick | 5 min (cheap blind tick only) | Playbook |
| HC shrink utilization threshold | 50% | Playbook |
| HC shrink window (K ticks) | 6 ticks (~1 hr at manager cadence) | Playbook |
| Team archive after HC=0 | 3 ticks sustained | Playbook |
| Charter promotion threshold (K routed orders) | 3 | Playbook |
| Provisional → Stable principle confirmations (project) | 3 | Playbook |
| Provisional → Stable principle confirmations (global) | 3 different projects | Playbook |
| Elon `roles/elon.md` Stable promotion | explicit user approval | Hardcoded |
| Respawn ladder steps before escalating to user | 3 | Playbook |
| Validator pass attempts before escalation | 3 | Playbook |
| IC step budget (steps without milestone) | 30 | Playbook |
| Channel-failure detection (Elon mute → fallback) | 2 missed ticks | Playbook |

## 17. Open Questions

- Heavy-retro pause behavior — should arcmux fully halt in-flight contracts or only suspend new dispatches? Lean toward suspend-new-dispatches (in-flight finish naturally) but unconfirmed.
- Per-team Review cadence tunability — should managers be able to request different cadences (e.g. fast-moving teams every 5 min)? Yes architecturally; default 10.
- bbolt hot-snapshot cadence for backup — every Review tick or every N audits? Default Review tick.
