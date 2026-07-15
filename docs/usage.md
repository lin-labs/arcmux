# arcmux — Usage

A practical guide for running arcmux's three-tier multi-agent system day-to-day.

This doc lives in-repo at `docs/usage.md` (canonical). Note: arcmux is now a
pure substrate — role structure (Elon/Manager/IC) is owned by callers (elonco),
not the daemon; the mental model below describes the elonco usage pattern, not
daemon-enforced tiers.

## Resilient device mesh

arcmux can maintain a directed, authenticated WebSocket link between two
devices on a tailnet. The mesh listener is separate from the local control API:
it binds only to `127.0.0.1:7788` by default, while Tailscale Serve forwards raw
TCP to it. Losing Wi-Fi, VPN, or the remote machine never stops local sessions;
the peer becomes stale/dead and the dialer retries forever with bounded jitter.

On the machine that stays reachable (for example `labs`), create an invite for
the laptop. Keep the bearer out of shell arguments and logs by piping JSON or
using the owner-only output file:

```bash
arcmux mesh serve ref \
  --device labs \
  --url ws://labs:7788/v1/mesh \
  --tailscale-port 7788 \
  --output ~/.config/arcmux/ref.mesh-invite.json
```

`--tailscale-port` runs the additive command
`tailscale serve --bg --tcp 7788 tcp://127.0.0.1:7788`; it does not reset other
Serve mappings. Copy the invite over a trusted channel, or pipe it over SSH,
then join from the laptop without placing the credential in the process list:

```bash
ssh labs 'arcmux mesh serve ref --device labs --url ws://labs:7788/v1/mesh --tailscale-port 7788' |
  arcmux mesh join - --device ref
```

Both commands hot-reload only the mesh manager in an already-running daemon;
agent panes and local APIs are untouched. If the daemon is offline, the command
reports that the pairing is configured for the next start. Re-running `serve`
is safe and does not re-emit the credential; pass `--rotate` to deliberately
re-enroll that peer.

```bash
arcmux mesh status
arcmux mesh status --json
arcmux mesh ping labs
```

The machine-local registry defaults to `~/.config/arcmux/mesh.json` mode `0600`.
Servers retain only SHA-256 bearer hashes; an outbound dialer retains the raw
credential in that owner-only file. Protocol v1 permits one direction per peer
pair: do not configure the same peer under both inbound accepts and outbound
peers. The socket is bidirectional after connection; session/artifact messages
will layer onto it without a second reverse dial.

Optional transport timing and location overrides belong in `config.toml`:

```toml
[mesh]
enabled = true
listen_addr = "127.0.0.1:7788"
registry_path = "~/.config/arcmux/mesh.json"
heartbeat_interval = "15s"
stale_after = "35s"
dead_after = "60s"
reconnect_min = "500ms"
reconnect_max = "30s"
```

### Explicit cross-device agent handoff

Handoff is a durable two-step control protocol layered onto the mesh. Preparing
a handoff verifies an immutable history snapshot and exact target worktree but
does not launch an agent. Launching requires a second operator action and a
separate peer grant. The source session is never paused, closed, prompted, or
otherwise mutated by either step.

Both devices must register the project in `~/agents/projects.yaml`. The target
entry must name an existing managed worktree root:

```yaml
projects:
  - repo: arcmux
    project: arcmux
    path: ~/Projects/arcmux
    vault: ~/agents/obsProjects/arcmux
    worktrees: ~/data/Projects/arcmux/worktrees
```

On the target, grant preparation while keeping launch disabled. `mesh grant`
replaces the peer's complete grant list, so always repeat every scope that
should remain enabled:

```bash
arcmux mesh grant ref \
  sessions.read artifacts.read events.read handoffs.prepare
```

On the source, prepare from an arcmux-supervised session. The branch must be
clean, pushed, and fetchable at its exact commit; the named history must already
be in the shared history store on both devices. Put the human goal in an
owner-readable file so it does not enter the process list:

```bash
arcmux handoff prepare devbox root <source-session-id> \
  --project arcmux \
  --agent codex \
  --history <session-history-basename> \
  --goal-file ~/.config/arcmux/handoff-goal.txt \
  --wait 30s

arcmux handoff list
arcmux handoff show <handoff-id>
```

After inspecting the prepared record, enable launch on the target and launch
from the source:

```bash
# target: this deliberately replaces the previous grant list
arcmux mesh grant ref \
  sessions.read artifacts.read events.read \
  handoffs.prepare handoffs.launch

# source
arcmux handoff launch <handoff-id> --wait 60s
```

Acceptance returns a stable target session locator. Replaying `launch` returns
the same locator and never creates a second continuation. Offline calls and a
missing launch grant remain retryable; `arcmux handoff retry <handoff-id>` or a
later `launch` resumes the durable record after connectivity or grants recover.

The target agent receives its goal, lineage, history path, checkout, branch,
and exact head only through a target-local `0600` instruction file. Its
confirmed prompt contains a generic marker, private delivery evidence never
leaves the device, and HTTP `/sessions` plus mesh session listings redact its
checkout and prompt-derived context. The owner-local Unix-socket gRPC API
remains a trusted inspection surface. The first release supports clean pushed branches;
stored-patch/bundle materialization for dirty work remains a follow-up, so dirty
or artifact-bearing requests fail closed instead of copying state implicitly.

---

## TL;DR — the minimum to drive Elon

```bash
# One-time per machine: start the daemon (it owns the pulse loop for every project)
./bin/arcmux start &

# Per project: launch the Elon company (note: subcommand name is "manager mode",
# not "spawn a Manager" — see "Naming" section below)
./bin/arcmux manager claude my-project --mission "<your real ask>"
```

A cmux workspace named `elon: my-project` appears with Claude running there as Elon. The daemon's pulse will wake it within 30s.

To ask Elon to do work or check progress, you have two options today (CLI) and one future option (arcmux-board UI, in progress).

---

## Mental model in one paragraph

arcmux runs **one daemon per machine** (`arcmux start`). The daemon discovers projects on disk (`~/data/arcmux/*/state.bolt`) and runs a **pulse loop** that wakes panes when their inbox grows or their cadence elapses. Each project is one **Elon company** (`arcmux manager <agent> <project>` creates it). Elon decides, when work comes in, whether to handle it directly or spawn a **team** — each team gets its own cmux workspace with a Manager pane. Managers create **contracts** and spawn **IC slot panes** inside their team's workspace to execute. All structured state (teams, contracts, inboxes, audit) lives in `~/data/arcmux/<project>/state.bolt`. All durable knowledge (mission, principles, journals, decisions, charters) lives in `~obsAgents/Projects/<project>/`.

---

## When to launch each thing

| Action | When | Frequency |
|---|---|---|
| `arcmux start` | Once at boot (or restart after upgrade) | Long-lived; the daemon is the lab service |
| `arcmux manager <agent> <project>` | Once per new project | One per project, ever (re-running attaches if state.bolt exists) |
| `arcmux pulse --project X --once` | Only as a debug shim if the daemon is down | Rare; canonical path is the daemon |
| `arcmux-call <verb>` | Whenever you want to read/write state from a shell | Ad-hoc; preferred over editing bolt directly |

You **do not** need to launch a Manager or an IC separately. Those are spawned by their parent tier inside cmux when Elon (or you, via CLI) calls `arcmux-call team spawn` and `arcmux-call ic spawn`.

---

## Lifecycle of an Elon company

1. **Daemon discovers the project** — `arcmux start` is running. It scans `~/data/arcmux/*/state.bolt` every `discovery_interval` (default 60s).
2. **You create the project** — `arcmux manager claude my-project --mission "ship the demo"`. This:
   - Validates the project slug.
   - Creates `~/data/arcmux/my-project/` (ephemeral runtime state).
   - Creates `~obsAgents/Projects/my-project/{arcmux,elon,teams,retros}/` (durable knowledge).
   - Seeds the global role library at `~obsAgents/0Prompts/roles/` (only if first time).
   - Opens `state.bolt`, seeds Elon's scratchpad + audit, pushes your mission as the first inbox `add` message.
   - Generates `~/data/arcmux/my-project/bootstrap-elon.sh` exporting `ARCMUX_*` env.
   - Asks cmux to create a workspace named `elon: my-project` running the bootstrap. The Elon claude session boots there with its role primed via `--append-system-prompt-file`.
3. **Daemon's pulse wakes Elon** within Elon's cadence (default 30s). Elon reads its inbox, sees your mission, decides.
4. **Elon dispatches** — if the work fits a team, Elon calls `arcmux-call team spawn` and pushes a charter/vision into the new team's manager inbox. A new cmux workspace `team: <slug>` appears with a Manager pane.
5. **Manager dispatches** — the daemon pulses the new manager pane on cadence (10s default). The manager peeks its inbox, decomposes the vision into IC contracts (`arcmux-call contract create`), spawns IC slot panes (`arcmux-call ic spawn`). IC panes appear as splits inside the team workspace.
6. **ICs execute** — each IC pane has a Claude session with the IC role primed + `ARCMUX_CONTRACT` bound. ICs transition contracts via `arcmux-call contract transition --to working/blocked/validating`.
7. **Validator gates** — at HC≥2 a Validator IC mechanically checks acceptance_criteria, transitions to `completed`.

The daemon's pulse continues throughout; you don't need to manually wake anything.

---

## How to ask Elon to do work

### Option A — type into Elon's cmux pane (interactive)

Switch to the `elon: my-project` workspace tab in cmux. Claude is running there with the elon role primed. Type your request like a normal Claude chat:

> "Spawn a team to design the new login flow."

Elon will:
- Restate the order in one sentence.
- Triage (add / revise / retract).
- Decide: handle directly (e.g. answer a question) or spawn a team.
- Update its journal.
- If team-worthy, call `arcmux-call team spawn` and push the vision into the new manager's inbox.

### Option B — push to Elon's inbox from any shell (preferred for automation, persistence)

```bash
echo "spawn a team to design the new login flow" | \
  arcmux-call inbox push --project my-project --to elon --verb add --from user
```

Why this is preferred over typing in the pane:
- The order **persists** across an Elon respawn — if claude restarts, the new instance reads the inbox and picks up where the old one left off.
- The daemon's pulse will wake Elon within one cadence (default 30s) — no need to be in the cmux tab when you push.
- It's **audited** — `arcmux-call audit recent` shows the push.

### Option C — `arcmux-board` UI (in progress)

When `arcmux-board` ships, you'll open `http://localhost:<port>` in a browser. Left pane lists all projects + teams + ICs; right pane shows the selected role's journal/scratchpad/inbox. A `:` command palette accepts `:add <message>` to push to whichever role you're focused on. This subsumes the CLI for everyday driving — CLI stays as the scripting/automation surface.

### Verbs you can push

`arcmux-call inbox push` accepts `--verb`:

| Verb | When |
|---|---|
| `add` | New order on top of existing work |
| `revise` | Change the scope/priority of an in-flight goal |
| `retract` | Cancel something already in flight |
| `consult` | Manager → Elon escalation (also IC → Manager via `--to manager:<slug>`) |
| `escalate` | When Elon can't decide and needs the user |

---

## How to ask for progress

Pick the level you care about. All of these are read-only and don't disturb running panes.

### Quick glance — Elon's current focus
```bash
cat ~/data/arcmux/my-project/scratchpads/elon.json
```
Rewritten by Elon every activation. Tells you what Elon is thinking about right now.

### Recent activity log — Elon's journal
```bash
tail -100 "$OBS_AGENTS/Projects/my-project/elon/journal.md"
```
Every Elon activation appends a block: trigger, what was read, rationale, decisions, next-expected.

### Project audit trail — every state change across all tiers
```bash
arcmux-call audit recent --project my-project --n 50
```
Manager-mode-started, team-spawned, ic-spawned, contract-transitioned, pulse-wake, etc.

### Teams and their state
```bash
arcmux-call team list --project my-project
arcmux-call team get --project my-project --slug <slug>
```

### Contracts (work items)
```bash
arcmux-call contract list --project my-project
arcmux-call contract list --project my-project --team <slug> --state working
arcmux-call contract get --project my-project --id <contract-id>
arcmux-call contract deps --project my-project --id <contract-id>
```

### ICs (workers)
```bash
arcmux-call ic list --project my-project --team <slug>
arcmux-call ic get --project my-project --team <slug> --slot <n>
```

### Inboxes (what each tier has queued)
```bash
arcmux-call inbox peek --project my-project --to elon --n 10
arcmux-call inbox peek --project my-project --to manager:<slug> --n 10
arcmux-call inbox peek --project my-project --to ic:<slot-id> --n 10
```

### Forward plan (Elon's roadmap)
```bash
cat "$OBS_AGENTS/Projects/my-project/elon/forward-plan.md"
```

### Coach reports (periodic role-file refinement findings)
```bash
ls "$OBS_AGENTS/Projects/my-project/elon/coach-reports/"
```

### Validate reports (every `make validate` run that gated a commit)
```bash
ls ~/data/arcmux/my-project/validate-reports/    # or ./.validate-reports/ when daemon is local-only
```

### Live status (when arcmux-board ships)
Open `http://localhost:<port>` — left pane shows the project tree, right pane drills into any selected role's state in real time (SSE-driven updates).

---

## How to create a new team (without typing into Elon's pane)

If you want a team spawned by hand — bypassing Elon's discretion — use the CLI:

```bash
arcmux-call team spawn \
  --project my-project \
  --slug build-tools \
  --vision "Long-form description of the team's mission"
```

This creates the team's bbolt record, materializes `~obsAgents/Projects/my-project/teams/build-tools/charter.md`, seeds the manager's scratchpad + inbox (with the vision as the first `add` message), generates `bootstrap-manager-build-tools.sh`, and opens a new cmux workspace `team: build-tools` running the bootstrap. The daemon pulses the new manager on its next cadence.

**When to bypass Elon**: rarely. Useful for debugging the team-spawn pipeline or for kicking off a known team without conversational overhead. The preferred path is "tell Elon what you want" and let Elon decide team structure — that's what the role file says.

---

## How to ask a specific Manager or IC to do work

Same pattern as Elon — push to their inbox:

```bash
# To the manager of a team
arcmux-call inbox push --project my-project --to manager:build-tools \
  --verb add --from elon --priority 1 <<< "implement contract C-7 first, others can wait"

# To a specific IC slot
arcmux-call inbox push --project my-project --to ic:build-tools-1 \
  --verb add --from manager-build-tools <<< "switch to the OIDC library on github.com/foo/bar"
```

The daemon's pulse will wake them on their respective cadences (Manager 10s, IC 5s).

---

## Tearing down

```bash
# Close the project's cmux workspaces from any shell
for ws in $(cmux list-workspaces | grep -E "(elon: my-project|team: )" | awk '{print $1}'); do
  cmux close-workspace --workspace $ws
done

# Wipe the project's state (after you're sure)
rm -rf ~/data/arcmux/my-project
rm -rf "$OBS_AGENTS/Projects/my-project"
```

The daemon will notice the missing state.bolt on its next discovery tick (default 60s) and drop the project from its pulse list automatically. The role library at `~obsAgents/0Prompts/roles/` is shared — don't delete it when wiping projects.

---

## Configuration

Config lives at `~/.config/arcmux/config.toml`. The `[pulse]` section controls cadences:

```toml
[pulse]
enabled = true
data_root = ""           # defaults to $HOME/data
interval = "10s"         # how often the supervisor ticks
discovery_interval = "60s"  # how often it rescans for new/removed projects

[pulse.cadence]
elon    = "30s"
manager = "10s"
ic      = "5s"
```

Defaults are sane; override only when you need to slow things down (e.g. budget constraints) or speed them up (debugging).

---

## Naming: a known confusion

`arcmux manager <agent> <project>` does **not** spawn a Manager-role agent. It launches arcmux in **"manager mode"** (the three-tier orchestration mode) and creates the Elon company. The Manager-role agents only get spawned later, by Elon, via `arcmux-call team spawn`.

A rename is under consideration. Current candidates (Boyan decides):

- `arcmux elon <agent> <project>` — most honest; the command launches Elon
- `arcmux launch <agent> <project>` — generic
- `arcmux company <agent> <project>` — matches the "board of Elon companies" framing

Until renamed, `manager` is the working name. When it lands, the old name will alias for one release cycle.

---

## Common workflows

### Start a new piece of work in an existing project
```bash
# From any shell — daemon's pulse will wake Elon within 30s
echo "design the new caching layer; should not increase p99 latency" | \
  arcmux-call inbox push --project my-project --to elon --verb add --from user
```

### Check in on progress without disturbing anyone
```bash
cat ~/data/arcmux/my-project/scratchpads/elon.json
arcmux-call team list --project my-project
arcmux-call contract list --project my-project --state working
arcmux-call contract list --project my-project --state blocked    # who needs help
```

### Course-correct Elon mid-flight
```bash
echo "actually skip the caching work; do the SSO migration first" | \
  arcmux-call inbox push --project my-project --to elon --verb revise --from user --priority 1
```
Elon's role file says: revise → cancel-affected-contracts + reissue. The audit log will show the cascade.

### Read Coach's last refinement findings
```bash
ls -t "$OBS_AGENTS/Projects/my-project/elon/coach-reports/" | head -1 | \
  xargs -I {} cat "$OBS_AGENTS/Projects/my-project/elon/coach-reports/{}"
```

---

## What you almost never need to do manually

- Spawn a Manager directly (Elon owns team formation)
- Spawn an IC directly (the team's Manager owns it)
- Edit `state.bolt` (use `arcmux-call`)
- Write to `~obsAgents/0Prompts/roles/` (Coach surfaces drift; Elon promotes — `--update-roles` flag exists for manual bumps after upgrading the embedded role files)
- Run `arcmux pulse` (the daemon owns it)

---

## Validation tiers — when to run which

arcmux has two canonical validation gates plus a fast iteration path:

| Tier | Command | What it checks | Speed / cost | When to run |
|---|---|---|---|---|
| **Per-commit gate** | `make validate` | Structural (gofmt + vet + test + build + 5 dispatcher smokes) **plus** substrate-behavioral (3 scenarios: bootstrap, pulse-wake, team-spawn-pipeline — real cmux + real daemon, asserts substrate primitives) | ~17s, free | Before every commit |
| **Big-feature gate** | `make validate-e2e` | Real `claude -p` drives real artifact production in a sandboxed workrepo; validation scripts assert the produced artifact actually works | ~1 min/scenario, **costs Anthropic tokens** | **Charter-level merges only**, not per-commit |
| Fast iteration | `make validate-structural` | Structural only (no behavioral) | ~10s, free | Quick local loop while iterating; promote to `make validate` before commit |

### When to invoke `make validate-e2e`

This tier burns real tokens by running `claude -p` against scenario prompts. Use it as a **release-quality gate**, not a fast-iteration loop:

- Before merging a charter-level feature (e.g. arcmux-board v1, daemon rewrites)
- After a substrate refactor that could break agent dispatch (e.g. role-file overhaul, contract DAO changes)
- Before tagging a release
- When investigating a regression that survives `make validate`

Run individually or all:

```bash
make validate-e2e                            # all scenarios
make validate-e2e SCENARIO=hello-server      # one scenario by name
./bin/arcmux-eval --list                     # see available scenarios
```

Reports land at `$ARCMUX_EPHEMERAL/validate-reports/eval-YYYY-MM-DD-HH-MM.json` with per-scenario pass/fail + token usage + wall-time.

### Back-compat aliases

The pre-rename names still work for one release cycle:

- `make validate-all` → `make validate` (the new canonical per-commit gate)
- `make validate-eval` → `make validate-e2e` (the new canonical big-feature gate)

### Adding scenarios

Mechanical: drop a directory under `testdata/eval-scenarios/<name>/` with three files:

- `prompt.md` — initial mission text fed to the agent
- `expected.md` — what good looks like (human-readable contract)
- `validate.sh` — the assertion script the harness runs against the produced workrepo

Then register the scenario in `internal/eval/scenarios/`. The harness picks it up via `--list`.

---

## Pointers

- Architecture spec: `~obsAgents/Projects/arcmux/specs/2026-05-24-claude-manager-design.md`
- Forward plan: `~obsAgents/Projects/arcmux/elon/forward-plan.md` (Elon's roadmap)
- Per-project principles: `~obsAgents/Projects/arcmux/arcmux/principles/<role>.md`
- Global role library: `~obsAgents/0Prompts/roles/{elon,manager,ic-base,validator,coach}.md`
