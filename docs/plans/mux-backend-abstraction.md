---
title: Mux backend abstraction (cmux + tmux first-class)
date: 2026-05-25
status: in-progress
---

# Mux backend abstraction — Phase 1, 2, 3

## Goal

arcmux supports cmux and tmux as **equal first-class backends**. The choice is
made by a single global config knob (`[mux] backend = "cmux" | "tmux"`). Manager
internals (`icspawn`, `teamspawn`, `pulse`, `health/monitor`, `daemon`) consume
a single `mux.Backend` interface. The backends stay in their own packages and
keep their backend-specific APIs; the shared interface only covers what the
manager pipeline actually uses.

Backend-specific ops that don't translate (e.g. cmux browser panes, multi-surface)
return `mux.ErrUnsupported` when called through the interface.

## Design decisions (locked)

- **Vocabulary**: shared API names on the interface, but each package keeps its
  own native types. cmux native = workspace/pane/surface; tmux native =
  session/window/pane. Interface uses neutral names: `Group` (workspace/session),
  `Pane` (pane/window).
- **Scope**: global only, one daemon = one backend. No per-profile/per-project
  override.
- **Parity contract**: spawn + send + read + close + focus + screen capture
  + idle/health hooks. Excludes browser pane, surfaces-per-pane, identify.
- **Send semantics**: callers pass plain text; backend handles its own Enter
  encoding (cmux: literal `\n`; tmux: `send-keys ... Enter`).

## Interface (target)

```go
// internal/mux/mux.go
package mux

type Group struct{ Ref string }
type Pane  struct{ Ref string; Index int; Focused bool }

type GroupOptions struct {
    Name, Description, CWD, Command string
    Focus bool
}
type PaneOptions struct {
    Group     string
    Direction string // left|right|up|down (cmux); ignored by tmux (window)
    Focus     bool
}

type Backend interface {
    EnsureServer(ctx context.Context) error
    NewGroup(ctx context.Context, opts GroupOptions) (Group, error)
    NewPane(ctx context.Context, opts PaneOptions) (Pane, error)
    Send(ctx context.Context, target, text string) error      // appends Enter
    SendRaw(ctx context.Context, target, text string) error   // no Enter
    ReadScreen(ctx context.Context, target string) (string, error)
    CaptureHistory(ctx context.Context, target string) (string, error)
    Focus(ctx context.Context, target string) error
    ClosePane(ctx context.Context, target string) error
    CloseGroup(ctx context.Context, target string) error
    ListPanes(ctx context.Context, group string) ([]Pane, error)
    PaneExists(ctx context.Context, target string) bool
    WaitIdle(ctx context.Context, target string, timeout, settle time.Duration) error
    PipePaneStart(ctx context.Context, target, outPath string) error
    PipePaneStop(ctx context.Context, target string) error
}

var ErrUnsupported = errors.New("mux: operation not supported by backend")
```

## Phase 1 — interface + dual-backend wiring (no consumer change)

1. Create `internal/mux/mux.go` with the interface, shared types, `ErrUnsupported`.
2. Implement `mux.Backend` in `internal/manager/cmuxcli`:
   - Add adapter methods on `*cmuxcli.Client` matching interface signatures.
   - Map cmux `Workspace`/`Pane` to `mux.Group`/`mux.Pane`.
   - `CaptureHistory` → cmux `read-screen` (visible only today; mark as
     "best-effort same as ReadScreen until cmux gains scrollback").
   - `WaitIdle` → cmux has no native equivalent; loop on `read-screen`
     diff with settle window.
   - `PipePaneStart/Stop` → `mux.ErrUnsupported` on cmux for now.
3. Implement `mux.Backend` in `internal/tmux`:
   - `NewGroup` → `new-session -d`.
   - `NewPane` → `new-window` (Direction ignored, return error if non-empty).
   - `Send` → `send-keys ... Enter`; `SendRaw` → `send-keys -l`.
   - `Close*` → `kill-pane` / `kill-session`.
   - `ListPanes` → `list-windows` JSON.
4. Add `[mux]` config:
   - `MuxConfig{ Backend string }` with default `"cmux"`.
   - Validate values; reject unknown backend.
5. Add `mux.New(cfg *config.Config) (mux.Backend, error)` factory in
   `internal/mux/factory.go`.
6. Tests for interface conformance via small table-driven test that runs
   both backends against an in-process fake/recorder.

## Phase 2 — refactor consumers

Switch concrete client types to `mux.Backend` in:
- `internal/manager/icspawn` (`Cmux *cmuxcli.Client` → `Mux mux.Backend`)
- `internal/manager/teamspawn` (same)
- `internal/manager/pulse` (same)
- `internal/manager/open`
- `internal/manager/project`
- `internal/daemon/pulse_supervisor` (drop the `pulseCmux` ad-hoc shim — use `mux.Backend`)
- `internal/daemon/daemon.go` (construct backend from config, pass through)
- `internal/health/monitor` (`tmux *tmux.Client` → `mux mux.Backend`)

Return types from spawners change from `cmuxcli.Workspace`/`cmuxcli.Pane`
to `mux.Group`/`mux.Pane`. Tests update their fake clients to satisfy
`mux.Backend`.

## Phase 3 — verification (prepare in parallel, run at end)

Verification program:

1. **Build**: `go build ./...` must pass.
2. **Static**: `go vet ./...` clean. If repo uses `golangci-lint` or
   similar, run it.
3. **Unit tests**: `go test ./...` — full suite must pass with both
   backends represented in the suite.
4. **Backend conformance tests** (new):
   - Table-driven test in `internal/mux/conformance_test.go` running
     both backends against the same operation sequence, asserting:
     spawn group, spawn pane, send + read echo, close pane, close group.
   - For cmux-only ops, assert `ErrUnsupported` returned by tmux.
5. **E2E**:
   - Existing tmux e2e (`internal/e2e`) still passes.
   - New cmux smoke parallel: `internal/e2e/cmux_smoke_test.go` (skip if
     cmux binary absent).
   - Manager pipeline e2e under each backend: scaffold project, spawn
     team, spawn IC, send pulse, dissolve. Skip cmux/tmux when binary
     missing.
6. **Config**: load default config → `mux.backend == "cmux"`; load with
   `[mux] backend = "tmux"` → daemon constructs tmux backend.
7. **Negative**: load with `[mux] backend = "bogus"` → `Load` returns
   validation error.
8. **Backwards compat**: existing configs (no `[mux]` section) still
   load and default to cmux.

## Risks

- `WaitIdle` on cmux: no native equivalent. Polling `read-screen` works
  but is slower and noisier. Acceptable for now; document the gap.
- `PipePaneStart` is cmux-unsupported. Anything in health/monitor that
  requires it returns `ErrUnsupported` on cmux.
- `Direction` on pane spawn is a cmux nicety. tmux backend silently
  drops it (no error).
- Tests are heavy. Refactor touched many `fakeRunner` impls in
  icspawn/teamspawn/pulse/manager/CLI. Mechanical but lots of lines.

## Out of scope for this pass

`internal/health/monitor` and `internal/session` are a separate
tmux-rooted subsystem in the daemon (the session/process model is
tmux-shaped: `SnapShot.TmuxTarget`, `SendKeys`, etc.). They are
orthogonal to the cmux-backed manager pipeline and continue to use the
daemon's own `*tmux.Client`. Migrating them to `mux.Backend` would
require modelling sessions in cmux's workspace/pane vocabulary first,
which is a separate piece of work. The two subsystems coexist in the
daemon today and continue to do so after this refactor.

## Open questions / amendments expected

- None right now; locked Send semantics with my proposal.

## Files

- New: `internal/mux/{mux.go,factory.go,errors.go,conformance_test.go}`
- New: `internal/e2e/cmux_smoke_test.go` (mirrors tmux smoke)
- Modified: `internal/config/config.go`, `config_test.go`
- Modified: `internal/manager/cmuxcli/client.go` (+adapter methods)
- Modified: `internal/tmux/client.go` (+ interface impl)
- Modified: `internal/manager/{icspawn,teamspawn,pulse,open,project}/*.go`
- Modified: `internal/health/monitor.go`
- Modified: `internal/daemon/{daemon,pulse_supervisor}.go`
- Modified: tests across the above
