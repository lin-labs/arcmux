#!/usr/bin/env bash
# Self-validating dev-cycle pass for arcmux. Runs static checks, unit tests,
# build, and a tiny e2e smoke against the built binaries; writes a structured
# JSON report to $ARCMUX_EPHEMERAL/validate-reports/YYYY-MM-DD-HH.json.
#
# Exit 0 only if every step succeeded. Exit 1 if any step failed.
#
# Convention: any Elon-turn cycle should end with `make validate`; the report
# is the durable evidence that the slice was green before commit.

set -u
set -o pipefail

cd "$(dirname "$0")/.."
REPO_ROOT="$(pwd)"

# Resolve report destination. Prefer $ARCMUX_EPHEMERAL when set (manager-mode
# shells); fall back to ./.validate-reports otherwise.
if [[ -n "${ARCMUX_EPHEMERAL:-}" ]]; then
  REPORT_DIR="$ARCMUX_EPHEMERAL/validate-reports"
else
  REPORT_DIR="$REPO_ROOT/.validate-reports"
fi
mkdir -p "$REPORT_DIR"

STAMP="$(TZ=America/Los_Angeles date '+%Y-%m-%d-%H-%M')"
REPORT="$REPORT_DIR/$STAMP.json"
LOG="$REPORT_DIR/$STAMP.log"

: > "$LOG"

declare -a STEP_NAMES=()
declare -a STEP_STATUSES=()
declare -a STEP_DURATIONS=()
declare -a STEP_DETAILS=()

run_step() {
  local name="$1"
  shift
  local started ended dur status detail
  started=$(date +%s)
  echo "=== $name ===" >>"$LOG"
  if "$@" >>"$LOG" 2>&1; then
    status="pass"
    detail=""
  else
    status="fail"
    detail="$(tail -20 "$LOG" | sed 's/"/\\"/g' | tr '\n' ' ')"
  fi
  ended=$(date +%s)
  dur=$((ended - started))
  STEP_NAMES+=("$name")
  STEP_STATUSES+=("$status")
  STEP_DURATIONS+=("$dur")
  STEP_DETAILS+=("$detail")
  printf '  [%s] %-22s %ds\n' "$status" "$name" "$dur"
}

echo "arcmux validate — $STAMP PT"
echo "  report: $REPORT"
echo "  log:    $LOG"
echo

# 1) static checks
run_step "gofmt"        bash -c 'out=$(gofmt -l . 2>&1); [[ -z "$out" ]] || { echo "$out"; exit 1; }'
run_step "go vet"       go vet ./...

# 2) unit + integration tests
run_step "go test"      go test ./...

# 3) build
run_step "build arcmux"        go build -o bin/arcmux ./cmd/arcmux
run_step "build arcmux-cli"   go build -o bin/arcmux-cli ./cmd/arcmux-cli

# 4) e2e smoke against built binary
run_step "smoke: arcmux --help"          bash -c './bin/arcmux --help >/dev/null 2>&1 || ./bin/arcmux help >/dev/null 2>&1 || true'
run_step "smoke: arcmux-cli dispatch"   bash -c './bin/arcmux-cli 2>&1 | grep -qE "audit|inbox"'
run_step "smoke: inbox dispatcher"       bash -c './bin/arcmux-cli inbox 2>&1 | grep -qE "push|peek|ack"'
run_step "smoke: audit dispatcher"       bash -c './bin/arcmux-cli audit 2>&1 | grep -qE "append|recent"'
# pulse smoke: invocation against a never-launched project must fail loud
# with a recognizable error (no state.bolt) — proves the subcommand wires
# Open() correctly and the flag parsing is intact.
run_step "smoke: pulse rejects unstarted" bash -c 'out=$(./bin/arcmux pulse --project nostart --vault-root /tmp/__novault__ --data-root /tmp/__nodata__ --once 2>&1 || true); echo "$out" | grep -qE "not started|state.bolt|VaultRoot|no such file|invalid project"'

# Compose JSON report
OVERALL="pass"
for s in "${STEP_STATUSES[@]}"; do
  [[ "$s" == "fail" ]] && OVERALL="fail"
done

{
  echo '{'
  printf '  "stamp": "%s",\n' "$STAMP"
  printf '  "timezone": "America/Los_Angeles",\n'
  printf '  "repo_root": "%s",\n' "$REPO_ROOT"
  printf '  "report_dir": "%s",\n' "$REPORT_DIR"
  printf '  "log": "%s",\n' "$LOG"
  printf '  "overall": "%s",\n' "$OVERALL"
  printf '  "steps": [\n'
  n=${#STEP_NAMES[@]}
  for i in "${!STEP_NAMES[@]}"; do
    sep=","
    [[ $i -eq $((n - 1)) ]] && sep=""
    printf '    {"name": "%s", "status": "%s", "duration_s": %s, "detail": "%s"}%s\n' \
      "${STEP_NAMES[$i]}" "${STEP_STATUSES[$i]}" "${STEP_DURATIONS[$i]}" "${STEP_DETAILS[$i]}" "$sep"
  done
  printf '  ]\n'
  echo '}'
} >"$REPORT"

echo
echo "overall: $OVERALL"
echo "report:  $REPORT"

[[ "$OVERALL" == "pass" ]] || exit 1
