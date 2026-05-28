# Validate profile — arcmux

Go daemon (the `arcmux` binary) running as a long-lived lab service. Follows
the `blin-lab-service` conventions: Makefile-driven lifecycle, deploys via
ssh to `labs`.

## Smoke command(s)

```bash
make build         # produces bin/arcmux
make test          # go test ./...
make start         # local lifecycle: start service
make status        # is it running? what PID? listening on which port?
make stop
make restart
make logs          # tail recent logs
```

The fastest "does it boot and respond" smoke:

```bash
make build && ./bin/arcmux --help
make start && sleep 1 && make status
```

## E2E entry points

- HTTP/gRPC endpoint check after `make start`: hit the documented port and
  confirm a known endpoint returns 200 / a valid proto response.
- `make deploy` to labs followed by an SSH-side `make status` and a curl from
  the lab box, confirming the released binary boots in its real environment.

(If a concrete `/healthz` endpoint exists, update this section with the curl
command and expected response.)

## Test entry points

```bash
go test ./... -v                 # unit + integration
go vet ./...                     # static analysis
gofmt -l .                       # format check (empty output = pass)
staticcheck ./... 2>/dev/null    # if installed
```

## Fixtures and corpora

- Proto definitions live in `proto/`; regenerate via `make proto` if changes.
- Test inputs live next to their `*_test.go` files.

## Dev environment

- Toolchain: Go (version per `go.mod`).
- Port: see Makefile `LOG_DIR` / port flag defaults; update this profile with
  the exact value once confirmed.
- Service install path on labs: per `blin-lab-service` conventions.

## Known flakies and quirks

- `make start` is idempotent via the systemd-or-pgrep gate; if a stale process
  exists, prefer `make restart`.
- For tmux environment bugs, verify tmux session scope directly:
  `tmux -L arcmux show-environment -t <agent-session> | grep ARCMUX`. Do not
  rely only on a pane shell's `env`; shared-session inheritance can mask missing
  per-agent env plumbing.

## Highest fidelity rung available

- [x] Static / typecheck (`go vet`, `gofmt -l`)
- [x] Unit (`go test ./...`)
- [x] Integration (in-tree `_test.go` files)
- [x] Real-deps E2E (local `make start` + endpoint hit; or `make deploy` to
      labs and remote curl)
- [ ] Manual user flow (N/A — this is a daemon, no UI)

## PR review checklist — accumulated from real PRs

### Claude hooks / session env handoff (drawn from arcmux-hooks-1)

- **One generic hook, never per-session scripts**: the installer must write a
  single fixed-name `~/.claude/hooks/arcmux-session-hook.sh` (idempotent, fixed
  content) — reject any PR that reintroduces `arcmux-<sessionID>.sh` generation.
  The generic hook must no-op when `ARCMUX_SESSION_ID`/`ARCMUX_HOOK_OUTPUT_DIR`
  are unset (safe to register globally).
- **/tmp/arcmux env handoff is DATA, never sourced**: env reaches the spawned
  agent via `/tmp/arcmux/<id>.env` (a data file), loaded by
  `eval "$(<abs-arcmux> hook-env <id>)"`. The loader must `eval` arcmux's OWN
  re-quoted output, NEVER `source` the raw writable file. Verify in code:
  `LoadSessionEnvExports` does `Lstat` symlink-reject + current-uid ownership +
  `0o077` perm mask (dir 0700 / file 0600) + `ARCMUX_` key allowlist + NUL/
  newline reject, and emits POSIX single-quote-escaped (`'\''`) exports.
  Required test: a malicious value (`'; touch PWNED; '`) round-trips as a
  literal and the marker is never created.
- **Loader uses an absolute binary path**: PATH is not guaranteed inside the
  pane — the loader prefix must use `os.Executable()` (→ `~/.local/bin/arcmux`),
  not bare `arcmux`.
- **hook-env fails safe**: on any validation error it prints NOTHING to stdout
  and exits 0, so the `eval` is a no-op and the agent launches with no injected
  env (the generic hook then no-ops). Don't let a bad/hostile env file block
  agent launch.
- **Watcher unchanged by construction**: `Install` must keep returning
  `OutputPath(sessionID)` and the generic hook must write the byte-identical
  `<HookOutputDir>/arcmux-hooks-<id>.jsonl` filename, so `watcher.Watch` /
  status / ready are unaffected. Assert returned path == watched path.
- **Legacy cleanup is coded + idempotent, not a one-off rm**:
  `CleanupLegacyScripts` globs `arcmux-s-*.sh`, skips the generic hook, runs at
  daemon startup, returns the count, no error on zero. Logs
  `swept legacy per-session hook scripts removed=N`.
- **Runtime-path change ⇒ rebuild + restart required**: any change to the
  daemon launch path needs `make install` (→ `~/.local/bin/arcmux`) + restart
  (`systemctl --user restart arcmux`). arcmux runs as a systemd user service;
  restarting recycles every managed pane (the whole org tier) — drive the live
  restart from OUTSIDE the profile (a `systemd-run --user` transient unit) so
  the deploy survives the restart, and verify post-restart: legacy swept to 0,
  generic hook present, `/tmp/arcmux` 0700 + `<id>.env` 0600.
