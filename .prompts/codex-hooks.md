You are a Codex worker on the arcmux repo (/Users/blin/Projects/arcmux), Go project.
Your single task: implement arcmux's CODEX-SIDE hook integration. Claude is concurrently
building the Go foundation (an `arcmux hook` CLI + a HooksJudge). To avoid merge conflicts
you MUST only CREATE NEW FILES — do NOT edit existing Go files (internal/hooks/hooks.go,
internal/daemon/*.go, internal/profile/profile.go, cmd/arcmux/*.go). Leave integration of
your artifacts to Claude.

READ FIRST: docs/superpowers/specs/2026-06-03-judge-split-and-hooks-judge-design.md
That spec defines the contract you build against.

THE CONTRACT (already being implemented by Claude as `arcmux hook`):
  arcmux hook --session "$ARCMUX_SESSION_ID" --agent codex \
    --event prompt_submit|tool_start|tool_end|turn_end|notification [--tool NAME]
  - Reads/writes per-session JSON state at ~/data/arcmux/sessions/<id>.json
  - No-ops (exit 0) when ARCMUX_SESSION_ID is empty.

YOUR DELIVERABLES (new files only):
1. docs/codex-hooks-findings.md — authoritative research on codex CLI's NATIVE hook
   mechanism. Boyan asserts "codex does have hooks very much." Determine precisely what
   codex (the OpenAI `codex` CLI, /Users/blin/.local/bin/codex) supports: the `notify`
   program config, any `[hooks]`/experimental hooks, what events fire (e.g.
   agent-turn-complete), the exact JSON/argv payload codex passes to the hook program,
   and where it's configured (~/.codex/config.toml). Verify against the actual installed
   codex version (`codex --version`, inspect `codex --help`, `~/.codex/config.toml`,
   and codex docs). Cite what you verified vs. assumed.
2. A concrete mapping table: codex native event -> our canonical event
   (prompt_submit/tool_start/tool_end/turn_end/notification). If codex only emits
   turn-complete, say so and map it to turn_end; note which canonical events codex
   CANNOT produce.
3. internal/hooks/codex_hook.sh.tmpl (or similar new file) — the codex hook program/script
   that translates codex's payload into the right `arcmux hook ...` invocation(s). Must
   no-op without ARCMUX_SESSION_ID. Keep it POSIX sh.
4. A ~/.codex/config.toml snippet (in the findings doc) showing how to register the hook.
5. Note any gaps where codex hooks are weaker than claude hooks for judging prompt
   *ingestion* specifically.

When done, write a short summary to /tmp/codex-hooks-result.txt ending with the literal
line: DONE
Do not run `git commit`. Do not edit existing files. Stay within the repo.
