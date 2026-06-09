# Review Request: Babysitter Voice Mode design

You are a senior engineer doing a read-only review. Do NOT write or edit any files.

## What was built
A design doc for "babysitter voice mode": a project-scoped voice control plane.
arcmux mints an ephemeral, project-scoped "call context" (the project's panes +
plan docs + repo cwd + which server to connect to); a push notification reaches
the phone; tapping it makes the Voxtop iOS app (or a browser prototype) open a
WebSocket to a dedicated voxtop-server; that server's existing xAI realtime relay
comes up scoped to the project with tools to read/redirect/dispatch arcmux panes,
read/write the plan, and CRUD Beads issues — all with confirm-readback on writes.

## Files to review
1. PRIMARY: docs/superpowers/specs/2026-06-03-babysitter-voice-mode-design.md (the design)
2. arcmux HTTP control plane today: docs/http-api.md  (verify endpoints, roadmap claims, port, session shape)
3. arcmux gRPC surface: proto/arcmux/v1/arcmux.proto  (verify ListSessions/Status/Subscribe exist; is project a field?)
4. arcmux daemon HTTP impl: internal/daemon/http.go  (verify how sessions are registered/listed; is there a project association?)
5. arcmux usage / mental model: docs/usage.md  (verify the project->panes mapping: Elon company, teams, ICs, state.bolt; does a "project" tag exist on sessions or is it implicit via cmux workspace name?)
6. voxtop relay + MODES registry: ~/Projects/voxtop/VoxtopServer/realtime_voice.py  (verify a new mode is just a registry entry; verify tool schema mechanism; verify ?token= auth; verify the converse WS protocol)
7. voxtop server entry: ~/Projects/voxtop/VoxtopServer/main.py  (verify /v1/realtime/converse?token= and how token maps to auth/api-key)
8. Vox iOS arcmux client: ~/Projects/voxtop/Vox/Services/ArcmuxService.swift  (verify baseURL/serverHost coupling, the authorize() hook)
9. Vox deep-link + server host: ~/Projects/voxtop/Vox/VoxApp.swift and ~/Projects/voxtop/Vox/Models/AppState.swift (verify vox:// scheme + single serverHost string that C must generalize)

## What to check
### A. Architectural soundness
- Is "reuse voxtop relay; arcmux exposes pane tools + mints context" actually the lowest-risk split given what these two codebases already do?
- Does the call-context concept have a viable home? Where should the context be persisted (bolt? in-memory? a new store?) and how does the relay fetch it cross-process — is GET /babysit/context realistic given http.go's current structure?
- Is the project->panes resolution actually available today? Critically: does arcmux currently associate a tmux session/pane with a *project*, or is project only implied by cmux workspace name / state.bolt directory? If the association does not exist, that is a hidden dependency the spec understates.

### B. Accuracy of claims about the existing code
- Are the endpoint names, port (7777), session JSON shape, and "capture/send are roadmapped not built" claims correct per docs/http-api.md and internal/daemon/http.go?
- Is "a new mode is just a MODES registry entry" true, and does the tool-dispatch loop support adding arbitrary HTTP-backed tools + subprocess (bd) tools cleanly?
- Is ?token= really the auth mechanism, and can a minted context token ride it without breaking existing API-key auth?

### C. Sequencing & risk
- Is B->A->A.5->C->D the right order? Any hidden coupling that breaks the claim that C is independent and that A is testable with a manually-minted token?
- Biggest unknowns / where will this actually get stuck?

### D. Safety
- Is confirm-readback sufficient for send_to_pane/spawn_agent/bead writes? Any auth gap once the server is reachable off-localhost (D introduces remote reach; arcmux :7777 currently has NO auth per ArcmuxService authorize() no-op)?

## Output format
```
## Critical Issues (must fix)
- [C1] ...
## Accuracy Issues (factual errors in paths, commands, descriptions)
- [A1] ...
## Missing Coverage (gaps that cause confusion or incomplete results)
- [M1] ...
## Minor / Polish
- [P1] ...
## Verified Correct
- ...
## Summary
One paragraph.
```
Cite file names and line numbers. Do NOT propose fixes — identify issues only.
