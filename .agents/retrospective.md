# Retrospective profile — arcmux

Go daemon running as a long-lived lab service. Retros here usually concern
the lifecycle Makefile, port allocation discipline, and the deploy-to-labs
SSH workflow.

This file is the **meta-process for retrospectives on this project**. Read
by the `retrospective` skill at the start of every retro. Each retro
updates this file with new meta-level lessons.

Outcome logs go to `~obsAgents/Sessions/.../## Session Retro`. Concrete
codified checks live in `.agents/validate.md`.

---

## Project-specific signals to weight extra

- Did the change touch the Makefile lifecycle targets (start/stop/status/
  restart/release)? These must remain idempotent — re-running `make start`
  on a healthy service must NOT spawn a duplicate.
- Did port allocation change without updating the documented port in
  `.agents/validate.md`?
- Did the change touch protobuf definitions? Was `make proto` re-run, and
  was the generated `gen/` content committed?
- Did `make deploy` to labs succeed AND `make status` on labs confirm the
  new binary is the one running? "Built locally" ≠ "deployed."
- Did an agent TUI version change its readiness text? Compare the configured
  pattern with a live pane capture; a visible prompt does not prove arcmux's
  handshake recognized it.

## Recurring failure modes (codified)

- Env bugs can hide behind tmux inheritance. When diagnosing ARCMUX_* values,
  inspect tmux session scope (`show-environment -t <agent-session>`) as well as
  the pane shell; shell-only probes can miss stale or accidentally inherited
  session env.
- **Restarting arcmux kills the deployer.** The agent doing the deploy usually
  runs inside an arcmux-hosted pane; `make restart`/`release` takes down the
  tmux server and that pane with it (hit 2026-07-12: `make release` killed the
  Claude session running it, mid-turn). Prevention: `make build`/`install`
  freely; the restart only with Boyan present and warned, as the LAST command
  of the turn.
- **Best-effort background passes rot silently.** The turn_end goal summarizer
  no-oped for ~3 weeks because grok's auth.json went empty (2026-06-25 →
  2026-07-12); its "never block the turn" design hid it, and unauthenticated
  retries got the box device-code rate-limited (429). Prevention: probe listed
  in `.agents/validate.md` ("Goal summarizer probe"); run it whenever a
  feature built on a best-effort dependency produces mysteriously empty fields.
- **A tracked stop gate is not protection unless startup is disabled and the
  installed binary is verified.** The unauthenticated summarizer storm was
  contained on 2026-07-26 with a P0 saying production must remain stopped, but
  the service restarted 35 minutes later on the stale pre-hardening binary and
  produced a larger Grok/Chrome storm on 2026-07-30. Prevention: while a
  no-restart issue is open, require both `systemctl --user is-active` =
  `inactive` and `systemctl --user is-enabled` = `disabled`; before restart,
  compare installed-binary build markers/version to the verified source and
  satisfy the explicit release gate in `.agents/validate.md`.

## Successful patterns worth reinforcing

- Using the `blin-lab-service` conventions for install path, log path,
  systemd unit, signal handling — never reinventing per service.
- `make release` (build + push + restart) as the single deploy ritual —
  EXCEPT when the deployer is arcmux-hosted; then split install from restart
  (see failure mode above).
- Confirming the deployed binary via remote `make status` rather than
  trusting that the local build is what's running.
- Reproducing TUI compatibility with the isolated live-agent hook gate before
  restarting the pane-preserving service.
- **Self-dogfood verification**: a Claude session running inside arcmux can
  verify arcmux features on itself (2026-07-12: artifact capture, summarize,
  feature-slug tmux rename all proven against the verifying session's own
  state doc). Cheapest possible end-to-end fixture — prefer it.
- **Quiesce the spawner before clearing a process storm.** `SIGSTOP` on the
  confirmed daemon parent halted creation without killing the diagnostic pane;
  exact-name child cleanup plus inspection of systemd `KillMode=process` then
  enabled a clean daemon stop while preserving tmux sessions.

## Where retro findings from this project should land

1. Meta-process about retroing arcmux → this file.
2. arcmux-specific service gotchas → `.agents/validate.md` here.
3. Cross-lab-service workflow → the `blin-lab-service` skill in the agents
   repo (this is the typical destination — other lab services should
   inherit the lesson).
4. Cross-project Go conventions → `AGENTS.shared.md`.

Most arcmux lessons belong in the `blin-lab-service` skill because they
generalize to other lab services. Default there unless the lesson is
arcmux-specific.

## Project-specific retro checklist

- Did the lifecycle Makefile remain idempotent?
- Was the port published in `.agents/validate.md` if it changed?
- Was the protobuf-generated code re-generated and committed?
- Was `make deploy` followed by a remote-side verification?
- For a profile/readiness change, did `make validate-e2e-hooks AGENT=<agent>`
  prove a fresh automatic handshake and first delivery?
- Did the change require updating the `blin-lab-service` skill?

## How to fill this in

This profile is small and project-specific because most arcmux lessons
generalize and belong in the `blin-lab-service` skill. Keep this file
tight; promote durable patterns upward.
