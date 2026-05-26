#!/usr/bin/env bash
# Self-validating dev-cycle pass for arcmux. Runs structural checks (gofmt,
# vet, go test, build) plus substrate-behavioral e2e scenarios that spawn
# isolated daemons and assert observable substrate effects; writes a
# structured JSON report to $ARCMUX_EPHEMERAL/validate-reports/YYYY-MM-DD-HH.json.
#
# Six steps total (~12s wall): gofmt, go vet, go test, make build,
# e2e:bootstrap, e2e:pulse-wake, e2e:grpc-rt.
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

# 3) build (all 4 binaries via make)
run_step "make build"   make build

# 4) substrate-behavioral e2e scenarios against built binaries
run_step "e2e: bootstrap"      ./bin/arcmux-e2e --scenario bootstrap
run_step "e2e: pulse-wake"     ./bin/arcmux-e2e --scenario pulse-wake
run_step "e2e: grpc-rt"        ./bin/arcmux-e2e --scenario grpc-rt

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
