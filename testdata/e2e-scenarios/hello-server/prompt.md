Build a tiny HTTP server in Go in the current working directory.

Requirements:

1. `main.go` — listens on a TCP port given by env var `PORT` (default `8080`)
   on `127.0.0.1`. Serves `GET /` with response body `hello arcmux\n`
   (plain text, HTTP 200). Returns 404 for everything else.

2. `go.mod` — module name `helloserver`. Use the Go version that the local
   toolchain reports (you can run `go version`). No external dependencies;
   standard library only.

3. `Makefile` — two targets:
   - `run` — starts the server in the foreground (`go run ./...`).
   - `test` — runs `go test ./...` which must include an end-to-end test
     that starts the server on a free port, performs `GET /`, and asserts
     the response body contains `hello arcmux`.

4. The end-to-end test must live under a normal Go test path (e.g.
   `main_test.go` or `server_test.go`) and must NOT shell out — it should
   pick a free port itself, start the server via the same code path as
   `main.go` (e.g. by extracting the handler into a function the test can
   call), curl-equivalent it with `net/http`, and assert.

Constraints:

- Bind only to `127.0.0.1`, never `0.0.0.0` or `::`.
- All files go in the current working directory.
- No third-party Go packages; standard library only.
- Do NOT add a README, license, .gitignore, or any extra files. Just the
  three files above (plus the test file).

When you believe you are done, exit. The harness will run `make test` and
will also spin up the server itself and curl it for a final assertion.
