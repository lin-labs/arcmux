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

## Recurring failure modes (codified)

- Env bugs can hide behind tmux inheritance. When diagnosing ARCMUX_* values,
  inspect tmux session scope (`show-environment -t <agent-session>`) as well as
  the pane shell; shell-only probes can miss stale or accidentally inherited
  session env.

## Successful patterns worth reinforcing

- Using the `blin-lab-service` conventions for install path, log path,
  systemd unit, signal handling — never reinventing per service.
- `make release` (build + push + restart) as the single deploy ritual.
- Confirming the deployed binary via remote `make status` rather than
  trusting that the local build is what's running.

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
- Did the change require updating the `blin-lab-service` skill?

## How to fill this in

This profile is small and project-specific because most arcmux lessons
generalize and belong in the `blin-lab-service` skill. Keep this file
tight; promote durable patterns upward.
