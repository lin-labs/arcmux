# hello-server — expected outcome

This scenario tests the baseline question: can an agent, given a precise
mission and a fresh workdir, produce a working Go HTTP server end-to-end?

## Success criteria

1. `workrepo/main.go` exists and compiles.
2. `workrepo/go.mod` exists; module name `helloserver`; no external deps.
3. `workrepo/Makefile` exists with `run` and `test` targets.
4. At least one `*_test.go` file exists.
5. `make test` exits 0.
6. Out-of-band check: harness picks a free port, starts the server with
   `PORT=<port> go run ./...`, waits for the port to bind, performs
   `GET http://127.0.0.1:<port>/`, asserts the response contains
   `hello arcmux`.
7. The server only binds to `127.0.0.1` (no `0.0.0.0`, no IPv6 wildcard).

## What this scenario does NOT yet validate

- That arcmux's Elon→Manager→IC chain can dispatch this work. That is a
  chain-mode scenario (§F15.chain backlog).
- Token-level cost. Direct-dispatch eval caps wall-time, not tokens, in
  v0; cost is observed via the agent's printed usage line only.
- Behavior under amendment (mission change mid-flight). A later scenario
  may inject an inbox message and re-invoke.

## Why this scenario is the floor

`hello-server` is the smallest task that exercises: file creation, Go
toolchain knowledge, HTTP semantics, port handling, test authoring, and
Makefile authoring. If an agent cannot do this, no chain-mode escalation
will save it. Conversely, if direct mode passes but chain mode fails on
the same task, the failure is in the chain — useful signal.
