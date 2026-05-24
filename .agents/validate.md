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

## Highest fidelity rung available

- [x] Static / typecheck (`go vet`, `gofmt -l`)
- [x] Unit (`go test ./...`)
- [x] Integration (in-tree `_test.go` files)
- [x] Real-deps E2E (local `make start` + endpoint hit; or `make deploy` to
      labs and remote curl)
- [ ] Manual user flow (N/A — this is a daemon, no UI)
