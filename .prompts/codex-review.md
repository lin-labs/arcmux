You are an INDEPENDENT adversarial code reviewer on the arcmux Go repo
(/Users/blin/Projects/arcmux). A change was just made that:
- splits the delivery Judge into judge.go (core+heuristic), judge_typesafe.go,
  judge_hooks.go;
- adds a per-session hook-state store (internal/hooks/sessionstate.go + lock.go)
  written by a new `arcmux hook` CLI (cmd/arcmux/hookevent.go);
- adds [delivery].judge config selecting exactly one judge;
- rewrites the claude generic hook + adds an embedded codex hook bridge
  (internal/hooks/codex.go, codex_hook.sh) that call `arcmux hook`.

The full staged diff is at /tmp/judge-hooks-review.diff. Read it and, where
needed, open the actual files for context. Be adversarial and specific. Focus on:
1. Correctness bugs (esp. judge_hooks.go time comparison logic, the controller's
   isIngested/shouldSubmit interaction, atomic write + flock RMW races, the
   sh hook scripts' POSIX correctness and quoting/injection).
2. The hooks judge contract: is `LastPromptSubmitAt >= DeliveryStartedAt` a sound
   ingestion proof given clock sources (hook writes via `arcmux hook` time.Now()
   on same host as daemon time.Now())? Any TOCTOU or stale-state hazards?
3. Config/daemon wiring regressions (NewJudge error handling, env file keys,
   archive-on-unwatch, restore path at daemon.go ~1180 which also Watches —
   does it need InitSessionState too?).
4. Anything that breaks existing behavior when judge stays default "typesafe".

Output a concise findings list ranked by severity (CRITICAL/HIGH/MEDIUM/LOW),
each with file:line and a concrete fix. Do not edit files. Write your review to
/tmp/codex-review-result.txt ending with the literal line: DONE
