#!/usr/bin/env bash
# validate.sh <workrepo> — assertion script for the hello-server scenario.
#
# Exits 0 on pass, non-zero on fail with a one-line FAIL: reason on stderr.
# Prints structured PASS/FAIL lines to stdout for the harness to capture.
#
# Caller is the eval harness; cwd is irrelevant — workrepo is argv[1].

set -uo pipefail

workrepo="${1:?usage: validate.sh <workrepo>}"
if [[ ! -d "$workrepo" ]]; then
  echo "FAIL: workrepo $workrepo does not exist" >&2
  exit 1
fi

cd "$workrepo"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# --- Static checks --------------------------------------------------------

[[ -f main.go    ]] || fail "missing main.go"
[[ -f go.mod     ]] || fail "missing go.mod"
[[ -f Makefile   ]] || fail "missing Makefile"

if ! grep -q '^module helloserver' go.mod; then
  fail "go.mod module name must be 'helloserver' (got: $(head -1 go.mod))"
fi

# At least one _test.go file (so `make test` has something to run).
if ! ls *_test.go >/dev/null 2>&1 && ! find . -maxdepth 3 -name '*_test.go' | grep -q .; then
  fail "no *_test.go file found (need at least one e2e test)"
fi

# Bind-address sanity: server source must NOT bind 0.0.0.0 or :: wildcard.
if grep -E 'Listen.*"[^"]*0\.0\.0\.0' main.go >/dev/null 2>&1; then
  fail "main.go binds to 0.0.0.0; must bind 127.0.0.1 only"
fi
if grep -E 'Listen.*"[^"]*\[::\]?:' main.go >/dev/null 2>&1; then
  fail "main.go binds to IPv6 wildcard; must bind 127.0.0.1 only"
fi

# No external deps.
if [[ -f go.sum ]] && [[ -s go.sum ]]; then
  fail "go.sum is non-empty; external dependencies are not allowed"
fi

echo "STATIC: pass"

# --- make test ------------------------------------------------------------

if ! make test >/tmp/eval-helloserver-maketest.log 2>&1; then
  echo "FAIL: make test exited non-zero" >&2
  echo "---- make test log (tail 60) ----" >&2
  tail -60 /tmp/eval-helloserver-maketest.log >&2
  exit 1
fi
echo "MAKE_TEST: pass"

# --- Out-of-band server smoke --------------------------------------------

# Pick a free port via python (portable).
port=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
if [[ -z "$port" ]]; then
  fail "could not pick a free port"
fi

server_log=$(mktemp -t eval-helloserver-server.XXXXXX.log)
PORT="$port" go run ./... >"$server_log" 2>&1 &
server_pid=$!

cleanup() {
  if kill -0 "$server_pid" 2>/dev/null; then
    kill "$server_pid" 2>/dev/null || true
    sleep 0.2
    kill -9 "$server_pid" 2>/dev/null || true
  fi
  wait "$server_pid" 2>/dev/null || true
}
trap cleanup EXIT

# Wait up to 8s for port to bind.
bound=0
for i in $(seq 1 80); do
  if nc -z 127.0.0.1 "$port" 2>/dev/null; then
    bound=1
    break
  fi
  sleep 0.1
done
if [[ "$bound" -ne 1 ]]; then
  echo "FAIL: server never bound to 127.0.0.1:$port" >&2
  echo "---- server log (tail 60) ----" >&2
  tail -60 "$server_log" >&2
  exit 1
fi

response=$(curl -fsS --max-time 4 "http://127.0.0.1:$port/" 2>&1 || true)
if [[ "$response" != *"hello arcmux"* ]]; then
  echo "FAIL: GET / response did not contain 'hello arcmux' (got: $response)" >&2
  echo "---- server log (tail 60) ----" >&2
  tail -60 "$server_log" >&2
  exit 1
fi
echo "HTTP_SMOKE: pass (response=$response)"

# 404 on unknown path (defensive — mission says "404 for everything else").
status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 "http://127.0.0.1:$port/does-not-exist" || echo "000")
if [[ "$status" != "404" ]]; then
  # Not a hard fail — some agents may serve 200 here. Note but don't fail.
  echo "NOTE: GET /does-not-exist returned $status (mission asks for 404, downgraded to warning)"
fi

echo "OVERALL: pass"
exit 0
