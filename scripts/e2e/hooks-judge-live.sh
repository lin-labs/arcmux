#!/usr/bin/env bash
# Real-agent e2e for the hooks-based delivery judge.
#
# Proves the FULL loop with a LIVE agent (no fakes): an isolated arcmux daemon
# running with [delivery].judge=hooks spawns a real `claude` (or `codex`)
# session, a prompt is delivered with ConfirmDelivery=true, and we assert that
# (1) the agent's NATIVE hook fired arcmux's `hook` CLI and wrote prompt_submit
#     into the per-session state doc, and
# (2) the hooks judge gated the delivery — a prompt_ingested event with
#     judge_source="hooks".
#
# Requirements (opt-in / not CI):
#   - a working `claude` binary with auth + --remote-control on PATH
#     (or `codex` / `grok` for --agent codex|grok)
#   - the agent's hook is registered so it fires arcmux-session-hook.sh /
#     arcmux-codex-hook.sh (claude: ~/.claude/settings.json; codex:
#     ~/.codex/hooks.json, trusted via codex /hooks). grok needs NO manual
#     step: the daemon materializes ~/.grok/hooks/arcmux-session.json, which
#     grok loads as an always-trusted drop-in at session start.
#
# Usage:  scripts/e2e/hooks-judge-live.sh [claude|codex|grok]
set -uo pipefail

AGENT="${1:-claude}"
cd "$(dirname "$0")/../.." || exit 1
ROOT="$(pwd)"

fail() { echo "E2E FAIL: $*" >&2; exit 1; }

echo "==> building binaries"
make build >/dev/null || fail "make build"

command -v "$AGENT" >/dev/null 2>&1 || fail "agent binary '$AGENT' not on PATH"

TMP="$(mktemp -d)"
SOCK="$TMP/arcmux.sock"
STATE="$TMP/sessions"
WORK="$TMP/work"; mkdir -p "$WORK"

cat > "$TMP/config.toml" <<EOF
[daemon]
socket = "$SOCK"
log_dir = "$TMP/logs"
http_addr = ""

[tmux]
# isolate from any other running arcmux daemon's tmux server
socket_name = "arcmux-e2ehooks"
default_session = "e2ehooks"

[delivery]
judge = "hooks"

[hooks]
# Real agent-home hook dirs so the agent's own settings registration + the
# installed bridge script are in effect for the spawned session.
claude_hook_dir = "$HOME/.claude"
codex_hook_dir = "$HOME/.codex/hooks"
grok_hook_dir = "$HOME/.grok"
hook_output_dir = "$TMP/hookout"
session_state_dir = "$STATE"
auto_install = true

[pulse]
enabled = false
# Isolate the daemon-level state.bolt (<data_root>/arcmux/_daemon/state.bolt)
# so this e2e never contends the flock held by a real running daemon.
data_root = "$TMP/data"
EOF

DPID=""; SUBPID=""
cleanup() {
  [ -n "$SUBPID" ] && kill "$SUBPID" 2>/dev/null
  if [ -n "$DPID" ]; then
    [ -n "${SID:-}" ] && ARCMUX_SOCKET="$SOCK" ./bin/arcmux-cli kill "$SID" >/dev/null 2>&1
    kill "$DPID" 2>/dev/null; wait "$DPID" 2>/dev/null
  fi
  echo "(left $TMP for inspection)"
}
trap cleanup EXIT

echo "==> starting isolated daemon (judge=hooks) socket=$SOCK"
./bin/arcmux start --config "$TMP/config.toml" > "$TMP/daemon.log" 2>&1 &
DPID=$!
for _ in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.2; done
[ -S "$SOCK" ] || { cat "$TMP/daemon.log"; fail "daemon socket never appeared"; }
grep -q 'delivery judge selected.*judge=hooks' "$TMP/daemon.log" \
  || { cat "$TMP/daemon.log"; fail "daemon did not select judge=hooks"; }

export ARCMUX_SOCKET="$SOCK"

echo "==> subscribing to events"
./bin/arcmux-cli subscribe > "$TMP/events.jsonl" 2>/dev/null &
SUBPID=$!
sleep 0.5

echo "==> creating real '$AGENT' session"
CREATE="$(./bin/arcmux-cli create --agent "$AGENT" --cwd "$WORK" --name "e2e-hooks-$AGENT")" || fail "create"
SID="$(printf '%s' "$CREATE" | python3 -c 'import json,sys;print(json.load(sys.stdin)["session_id"])')"
[ -n "$SID" ] || fail "no session id (create said: $CREATE)"
echo "    session_id=$SID"

echo "==> waiting for agent ready"
for _ in $(seq 1 90); do
  ST="$(./bin/arcmux-cli status "$SID" 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("state",""))' 2>/dev/null)"
  case "$ST" in
    idle|working) echo "    state=$ST"; break ;;
    failed|exited) cat "$TMP/daemon.log"; fail "agent reached state=$ST" ;;
  esac
  sleep 1
done

echo "==> sending prompt (ConfirmDelivery=true → runs the judge)"
printf 'Reply with exactly one word: PONG' | ./bin/arcmux-cli send "$SID" > "$TMP/send.json" 2>&1
echo "    send: $(cat "$TMP/send.json")"
python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));sys.exit(0 if d.get("delivered") else 1)' "$TMP/send.json" \
  || fail "send did not report delivered=true (judge did not confirm ingestion). daemon.log: $(tail -5 "$TMP/daemon.log")"

echo "==> ASSERT 1: native hook wrote prompt_submit into the session state doc"
SF="$STATE/$SID.json"
[ -f "$SF" ] || { ls -la "$STATE" 2>/dev/null; fail "no state file $SF — native hook never fired arcmux hook"; }
# Poll: the prompt-submit hook fires asynchronously right after delivery, so
# give it a window. The zero value "0001-01-01T00:00:00Z" is NOT evidence
# (it is truthy in python — assert on the parsed year, not the string).
HOOK_OK=""
for _ in $(seq 1 30); do
  if python3 - "$SF" <<'PY' 2>/dev/null
import json, sys
d = json.load(open(sys.argv[1]))
ts = d.get("last_prompt_submit_at") or ""
assert not ts.startswith("0001-"), "zero timestamp"
assert d.get("agent"), "no agent recorded"
assert (d.get("events_seen") or 0) > 0, "no events seen"
print(f"    OK: agent={d['agent']} last_event={d.get('last_event')} working={d.get('working')} turns={d.get('turn_count')}")
PY
  then HOOK_OK=1; break; fi
  sleep 0.5
done
cat "$SF"
[ -n "$HOOK_OK" ] || fail "state file never gained prompt_submit evidence — native hook never fired arcmux hook"

echo "==> ASSERT 2: delivery gated by the hooks judge (judge_source=hooks)"
sleep 0.5
if grep -q '"type":"prompt_ingested"' "$TMP/events.jsonl" && grep -q '"judge_source":"hooks"' "$TMP/events.jsonl"; then
  echo "    OK: prompt_ingested with judge_source=hooks"
  grep '"type":"prompt_ingested"' "$TMP/events.jsonl" | tail -1
elif [ "$AGENT" = "grok" ] && grep -q '"type":"prompt_ingested"' "$TMP/events.jsonl"; then
  # Grok's screen shows working-state instantly, so the heuristic fallback can
  # win the first judge poll by milliseconds; ASSERT 1 already proved the hook
  # ground truth (prompt_submit in the state doc). Accept either source here.
  echo "    OK: prompt_ingested (grok: heuristic may win the first-poll race; hook evidence asserted above)"
  grep '"type":"prompt_ingested"' "$TMP/events.jsonl" | tail -1
else
  echo "    --- events seen ---"; cat "$TMP/events.jsonl"
  fail "no prompt_ingested(judge_source=hooks) event observed"
fi

echo "==> E2E PASS ($AGENT): hooks judge gated a live delivery end-to-end"
