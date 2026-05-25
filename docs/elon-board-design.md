# elon-board — Design Spec

**Status:** Draft (drafted in arcmux repo; will migrate to elonco repo)
**Date:** 2026-05-25
**Author:** Boyan + Claude

---

## 1. Mission

elon-board exists to answer **"what is each of my Elon companies doing, what's blocked, what needs me?"** — federated across every running elonco instance on this machine.

It is the cockpit view *above* the orchestrators, not another orchestrator.

---

## 2. Layout

Single web page at `http://localhost:9090`. Three regions plus chrome.

```
+---------------------------------------------------------------------------+
| :  search...                       [Register Elon +]   [Refresh] [Help ?] |  <- top bar, 40px
+-----------------+---------------------------------------------------------+
| ELON COMPANIES  | RIGHT PANE                                              |
| (left rail      | (overview / team / contract — depends on drill depth)   |
|  240px)         |                                                         |
|                 |                                                         |
| > [9001] arcmux |                                                         |
|   focus: ship   |                                                         |
|   purify branch |                                                         |
|   T:3  IC:7  !2 |                                                         |
|                 |                                                         |
|   [9002] elonco |                                                         |
|   focus: split  |                                                         |
|   T:2  IC:4     |                                                         |
|                 |                                                         |
|   [9003] olymp  |                                                         |
|   focus: BFCL   |                                                         |
|   T:5  IC:12 !1 |                                                         |
+-----------------+---------------------------------------------------------+
| arcmux: up | agents: 23 reg | queues: 4/2/1/0 | elonco: 3 live | 14:22 PT |  <- substrate strip, 28px
+---------------------------------------------------------------------------+
```

**Left rail (240px, scrollable).** One row per discovered elonco instance. Each row:

- Line 1: `[port] project-slug` — `[9001] arcmux`
- Line 2: `focus: <one-line scratchpad excerpt>` (truncated)
- Line 3: `T:<team count>  IC:<ic count>  !<needs-attention count>` (the `!` badge is red when >0)

Selection = arrow keys / `j`/`k` / click. Selected row gets a left border accent.

**Right pane — overview (default after selecting an Elon).** Renders the selected Elon's:

- Mission (1–2 lines, from `charter.md` `mission:` frontmatter)
- Current focus (full scratchpad, not truncated)
- Teams as cards (3-col grid): name, manager initial, IC count, in-flight contracts, blocker count
- "Pending decisions for Boyan" list (from Elon's outbox where audience=ceo)

**Right pane — team drilldown.** Click a team card OR press `l`:

- Manager card (name, model, last journal entry timestamp, current goal)
- IC slots as cards (one per IC): name, status (idle/working/stuck), last screen line, current contract id
- Contracts table: id, IC, verb, status, age, last-update

**Right pane — contract drilldown.** Click a contract row OR `l` from a contract:

- Contract details (verb, body, acceptance criteria, status timeline)
- IC's journal (rendered markdown, latest 50 entries)
- IC's scratchpad (live)
- "Send order to this IC" textarea (gated; sends through the IC's manager, not direct)

**Top bar.** Command palette input (focus with `:` or `/`). Free-text search filters left rail by project slug. "Register Elon +" opens a modal that calls `:register-elon`.

**Bottom strip (substrate health, 28px).**

- arcmux daemon status (color dot)
- registered agent count
- queue depths summary `inbox/outbox/dead/retry`
- live elonco count
- current PT clock

**Keyboard.**

| Key | Action |
|---|---|
| `j` / `k` | Down / up in current list |
| `h` / `l` | Drill up / drill down (overview → team → contract) |
| `:` or `/` | Open command palette |
| `?` | Help overlay |
| `r` | Refresh current pane (force re-poll, ignore cache) |
| `g` / `G` | Top / bottom of list |
| `Esc` | Close palette / drill up one level |
| `1`..`9` | Jump to nth elonco in left rail |

---

## 3. Data Sources

| What | Source | Cadence |
|---|---|---|
| Live elonco discovery | scan `~/data/elonco/*/registration.json` | every 5s + `fsnotify` on dir |
| Project overview (mission, focus, teams) | `GET <elonco>/v1/projects/<slug>` | 3s poll while pane open, longer when backgrounded |
| Agent-level info (status, owner, queue) | `GET <arcmux>/v1/agents?owner=<elon-id>` | 3s poll |
| Journal / charter / scratchpad markdown | direct read from `~obsAgents/Projects/<project>/` | on drilldown + fs watch |
| Live deltas | SSE `<elonco>/v1/events` AND `<arcmux>/v1/events` | persistent |
| Agent screen tail | `GET <arcmux>/v1/agents/<id>/screen?lines=50` | on demand (`:read-screen`) |

**Caching.** All `GET` responses cached in-memory with 1s TTL per Elon to debounce duplicate fetches from concurrent UI components. SSE events invalidate cache keys directly.

**Markdown is the source of truth.** Anything Boyan would read in Obsidian (charter, journal, scratchpad) is read straight from disk, not proxied through HTTP. This avoids stale-cache drift and keeps elonco's API surface small.

---

## 4. Command Palette

Invoked with `:` or `/`. Verbs:

| Command | Effect |
|---|---|
| `:focus <project>` | Select that Elon in left rail |
| `:order <project> <verb> <body>` | `POST <elonco>/v1/orders` — push to Elon's inbox |
| `:revise <project> <body>` | Sugar for `:order <project> revise <body>` |
| `:retract <project> <goal-id>` | `DELETE <elonco>/v1/goals/<id>` — cancel in-flight |
| `:peek <agent-id>` | Show that agent's inbox queue contents (modal) |
| `:read-screen <agent-id>` | Show last 50 lines of agent's terminal (modal) |
| `:health` | Open substrate health overlay (arcmux + each elonco) |
| `:register-elon <slug>` | Run launcher script `elonco-spawn <slug>`; await registration.json |
| `:close-elon <slug>` | `POST <elonco>/v1/shutdown` (graceful) |
| `:help` | Help overlay |

Tab-completion is mandatory for `<project>` and `<agent-id>` args — these come from the discovered set, so completion is trivial and prevents typos. Free-text args (`<body>`) accept anything.

Failed commands surface as toast notifications, not modal blockers.

---

## 5. Tech Choice

**Recommendation: full Python.** FastAPI + Jinja2 templates + vanilla JS + HTMX for partial swaps. No build step, no Node, no bundler.

Reasons:

- elonco is already Python. One project, one venv, one `pyproject.toml`. Adding a Go-embedded frontend means a second toolchain for a UI shell.
- Jinja + HTMX gets us 80% of "live UI" without writing a SPA. The remaining 20% (palette, keybinds, SSE wiring) is ~300 lines of vanilla JS — no React.
- HTMX `hx-sse` is a clean fit for "swap this fragment when an event arrives."

**Live updates: SSE.** WebSocket is overkill. elon-board is read-mostly; the few writes (orders) go through normal `POST` endpoints. SSE multiplexes cleanly: one connection per elonco + one to arcmux, browser handles reconnect.

**Multi-Elon discovery cadence: hybrid.**

- `watchdog` (Python `fsnotify` wrapper) on `~/data/elonco/` — instant reaction to new/stale registrations.
- 5s safety poll in case a watcher misses an event (NFS, iCloud weirdness, etc.).
- Each registration is validated on read by checking the PID is alive and the port responds to `GET /v1/health` within 500ms. Stale registrations (process gone) are pruned and the file deleted.

---

## 6. Where It Lives

**Recommendation: `python -m elonco board` subcommand inside the elonco repo.**

Justification:

- elonco and elon-board share data models (project, team, contract, IC). A separate repo means duplicating Pydantic schemas or vendoring them — both painful.
- Single venv, single install, single version. `pip install elonco` → you get the orchestrator AND the cockpit.
- elon-board is a *view* over elonco semantics. If elonco's API changes, elon-board changes in the same PR. Forced co-evolution is a feature.
- One running elon-board at a time (singleton on port 9090) regardless of N elonco instances. elon-board is the *board room*; the elonco instances are the *companies*. You don't run multiple board rooms.

**Alternative considered: standalone repo `elon-board`.** Rejected — it would need an HTTP client SDK to elonco, which we'd have to keep in lockstep. The cost of a separate repo only pays off if elon-board grows multiple non-elonco data sources, which is explicitly out of scope (see §8).

**Layout inside elonco repo:**

```
elonco/
  __main__.py        # python -m elonco {run,board,...}
  core/              # orchestrator
  board/
    app.py           # FastAPI app
    templates/       # Jinja2
    static/          # vanilla JS + CSS
    discovery.py     # fs scanner + watchdog
    federation.py    # per-elonco client + SSE multiplexer
```

---

## 7. MVP Scope

| Version | Scope |
|---|---|
| **v0.1** | Read-only. Discover elonco instances. Render left rail + overview pane. Journal viewer (file read). No commands, no live updates. Manual `r` to refresh. |
| **v0.2** | Command palette skeleton + `:focus`, `:order`, `:revise`, `:retract`. Toast notifications. Still poll-based. |
| **v0.3** | SSE wiring to each elonco's `/v1/events`. HTMX fragment swaps for left rail rows and the overview pane. |
| **v0.4** | arcmux substrate integration: bottom strip, `:peek`, `:read-screen`, agent cards on team drilldown. SSE to arcmux too. |
| **v0.5** | True multi-elon federation — until now, drilling down assumes one elonco at a time; v0.5 lets aggregated views (e.g. "show all blocked contracts across all Elons") work. `:register-elon`, `:close-elon`. |

Each version ships in a separate branch + tagged release. v0.1 must be usable in isolation — no waiting for v0.4 before Boyan can see his Elons.

---

## 8. Anti-Cleverness — What elon-board Won't Do

1. **No code editing.** It's a viewer. Markdown is read-only in the UI. Edits happen in Obsidian, vim, or wherever — the filesystem watcher picks up changes within ~1s.
2. **No authentication.** Localhost-only. If you want remote access, SSH-tunnel. Adding auth means a session store, a login page, and a threat model — none of which earn their keep on a single-user dev machine.
3. **No agent execution.** elon-board never spawns or signals an agent directly. All actions go through elonco's API (which goes through arcmux). One blast radius, one audit trail.
4. **No replacement for cmux / tmux.** Agent terminals stay in cmux. elon-board reads screens (read-only tail), it does not host TTYs.
5. **No project creation / scaffolding.** `:register-elon` shells out to a launcher script that already exists. elon-board does not own project templates, charter generation, or first-time setup — that belongs in elonco itself.

---

## 9. Open Questions

1. **Singleton enforcement.** If a second `python -m elonco board` starts on a busy 9090, do we fail loudly, attempt a re-bind on 9091, or hand off (kill the old one)? Lean toward "fail loudly with PID of holder."
2. **What counts as "needs-attention"?** Concrete predicate needed. Proposal: `len(outbox where audience=ceo and unread) > 0` OR `any IC stuck > 30min` OR `any contract failed acceptance`. Boyan to confirm.
3. **Drilldown URL routing.** Should `/elon/<slug>/team/<id>/contract/<cid>` be real URLs (bookmarkable, back-button works) or pure client state? Real URLs cost ~30 lines extra, big UX win.
4. **`registration.json` schema authority.** Who owns the schema — elonco core, or a shared `elonco-protocol` package? Matters for v0.5 federation where elon-board may want to validate.
5. **arcmux SSE bus shape.** Does arcmux already emit per-agent events, or only daemon-level events? If the former, elon-board can render live IC status without polling. Needs check against current arcmux purification work.
6. **iPad / phone view.** Out of scope for v0.1–v0.5 — but if Boyan ever wants `/m` mobile route, layout assumptions (240px rail, 3-col team grid) need to be tagged for responsive override now, not retrofitted later.

---

*End of spec. Word count ≈ 1450.*
