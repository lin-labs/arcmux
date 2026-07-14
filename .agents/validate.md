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

- `make validate-e2e-hooks AGENT=grok|claude|codex` — live-agent e2e: an
  isolated daemon (judge=hooks) spawns the real agent in tmux, sends a
  confirmed prompt, and asserts the native hook wrote prompt_submit into the
  per-session state doc plus a prompt_ingested event. grok needs zero manual
  hook registration (drop-in ~/.grok/hooks/arcmux-session.json).
- Headless exec smoke (also exercises class→exec-profile resolution):
  `ARCMUX_SOCKET=<sock> bin/arcmux-cli exec --agent grok "Reply with exactly: pong"`.
- HTTP/gRPC endpoint check after `make start`: hit the documented port and
  confirm a known endpoint returns 200 / a valid proto response.
- Mesh reconnect smoke (isolated ports; never restart the production daemon):
  configure two temporary `mesh.json` registries and daemon configs, expose the
  serving side on `127.0.0.1:7788`, then verify
  `arcmux mesh status --json`, `arcmux mesh ping <peer>`, stop/start only the
  serving daemon, and confirm the dialer transitions disconnected → connected
  without either side's local sessions changing.
- Live tailnet rung: on the stable host run
  `arcmux mesh serve ref --device <host> --url ws://<tailscale-host>:7788/v1/mesh --tailscale-port 7788`
  and pipe the JSON over SSH into
  `arcmux mesh join - --device ref`. Confirm both pairing commands report a
  mesh-only hot reload, `arcmux mesh ping <host>` succeeds, and
  `tailscale serve status` retains unrelated mappings. Never put the invite
  bearer on a command line or in a validation report.
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
- Local control HTTP defaults to `127.0.0.1:7777`; the mesh wire listener is a
  separate loopback-only `127.0.0.1:7788`, normally raw-TCP proxied by
  Tailscale Serve.
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

## Delivery judge ([delivery].judge)

`[delivery].judge` defaults to `auto` — the cascade hooks → typesafe →
heuristic, where hook events are ground truth and always win when the agent
emits them. Pin `typesafe`, `hooks`, or `heuristic` only to bypass tiers
deliberately. Boot proof with a pinned judge (isolated config, tmp
socket/dirs):

```bash
./bin/arcmux start --config /tmp/cfg.toml   # expect log: "delivery judge selected" judge=hooks
# on start it also installs both hooks:
#   <claude_hook_dir>/hooks/arcmux-session-hook.sh   (parses Claude stdin JSON)
#   <codex_hook_dir>/arcmux-codex-hook.sh            (codex lifecycle bridge)
```

The hooks judge reads per-session state at `<session_state_dir>/<id>.json`
(default ~/data/mux/sessions — the protocol state root), mutated by `arcmux hook` from the agent
hooks; archived to `sessions/archived/<id>.json` on unwatch. Codex hook
registration in `~/.codex/hooks.json` is manual + trusted via codex `/hooks`.

Highest rung NOT yet automated for the judge: a live claude/codex session
firing its real hook through a daemon-gated SendPrompt with judge=hooks. Run
via `make validate-e2e` after setting judge=hooks in the e2e daemon config.

## Substrate live-E2E recipe (real claude/codex panes)

Highest-fidelity manual rung for the *substrate* session API (CreateSession /
SendPrompt / Capture / Status). Proves the agent lifecycle end-to-end with real
agents — exercised when validating handshake / state-machine changes.

```bash
# isolated daemon on the default socket the CLI dials; pulse off; tmux backend
cat > /tmp/cfg.toml <<EOF
[daemon]
socket = "$HOME/.config/arcmux/arcmux.sock"
log_dir = "/tmp/arcmux-e2e/logs"
state_path = "/tmp/arcmux-e2e/state.bolt"
http_addr = "127.0.0.1:7777"
[mux]
backend = "tmux"
[pulse]
enabled = false
EOF
./bin/arcmux start --config /tmp/cfg.toml &              # http info: 127.0.0.1:7777
./bin/arcmux-cli create --agent claude --name t-cld --cwd /tmp/wd   # expect state -> idle (NOT failed)
./bin/arcmux-cli create --agent codex  --name t-cdx --cwd /tmp/wd
printf 'What is 17*23? number only.' | ./bin/arcmux-cli send <session_id>   # expect delivered=true
./bin/arcmux-cli status <session_id>                    # working -> idle within ~2*capture_interval
tmux -L <socket_name> capture-pane -t <name> -p | tail  # see the agent's answer
```

Gotchas learned:
- **The substrate `CreateSession` gRPC is tmux-only by design**, regardless of
  `[mux].backend`. `d.mux` (the cmux/tmux workspace backend) is used ONLY by the
  pulse/workspace-orchestration path (`pulse_supervisor.go`) — i.e. the
  elonco/manager layer. `arcmux-cli create` and the `arcmux` skill always make
  raw tmux sessions on the `[tmux].socket_name` server; they never create cmux
  workspaces. **To get agents visible in the cmux (mobile) app, launch via
  elonco, not direct `arcmux-cli create`.**
- `arcmux-cli` (create/send/capture/status/kill) dials the hardcoded default
  socket `~/.config/arcmux/arcmux.sock`; only audit/inbox/ready take `--socket`.
  Point the daemon's `[daemon].socket` at the default for the CLI to work.
- claude ready signal is `"Remote Control active"` (footer from `--remote-control`),
  NOT `">"`; working indicator is `"esc to interrupt"`. codex shows
  `"Working (Ns • esc to interrupt)"`. See arcmux-jwf / arcmux-u1c.

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
