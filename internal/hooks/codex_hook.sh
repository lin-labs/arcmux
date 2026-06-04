#!/bin/sh
# Translate Codex lifecycle hook JSON into the arcmux canonical hook contract.
# This is intentionally quiet: hooks should not perturb Codex turns.

[ -n "${ARCMUX_SESSION_ID:-}" ] || exit 0

first_arg="${1:-}"
case "$first_arg" in
\{*)
	# Legacy `notify` appends JSON as the final argv argument instead of stdin.
	payload=$first_arg
	native_event=""
	;;
*)
	payload=$(cat 2>/dev/null || printf '')
	native_event="$first_arg"
	;;
esac
tool_name=""
notify_type=""

if command -v python3 >/dev/null 2>&1; then
	parsed=$(
		printf '%s' "$payload" | python3 -c '
import json
import sys

raw = sys.stdin.read()
event = ""
tool = ""
try:
    data = json.loads(raw) if raw.strip() else {}
    if isinstance(data, dict):
        event = data.get("hook_event_name") or ""
        tool = data.get("tool_name") or ""
        notify_type = data.get("type") or ""
except Exception:
    notify_type = ""
print(event)
print(tool)
print(notify_type)
' 2>/dev/null
	)
	json_event=$(printf '%s\n' "$parsed" | sed -n '1p')
	tool_name=$(printf '%s\n' "$parsed" | sed -n '2p')
	notify_type=$(printf '%s\n' "$parsed" | sed -n '3p')
else
	json_event=$(printf '%s' "$payload" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
	tool_name=$(printf '%s' "$payload" | sed -n 's/.*"tool_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
	notify_type=$(printf '%s' "$payload" | sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
fi

[ -n "$native_event" ] || native_event="$json_event"
[ -n "$native_event" ] || native_event="$notify_type"

case "$native_event" in
UserPromptSubmit)
	canonical_event="prompt_submit"
	tool_name=""
	;;
PreToolUse)
	canonical_event="tool_start"
	;;
PostToolUse)
	canonical_event="tool_end"
	;;
Stop|SubagentStop)
	canonical_event="turn_end"
	tool_name=""
	;;
agent-turn-complete)
	canonical_event="turn_end"
	tool_name=""
	;;
approval-requested)
	canonical_event="notification"
	;;
PermissionRequest|SessionStart|SubagentStart|PreCompact|PostCompact)
	canonical_event="notification"
	;;
*)
	exit 0
	;;
esac

if [ -n "${ARCMUX_BIN:-}" ]; then
	arcmux_bin=$ARCMUX_BIN
else
	arcmux_bin=$(command -v arcmux 2>/dev/null || true)
fi

[ -n "$arcmux_bin" ] || exit 0

set -- hook --session "$ARCMUX_SESSION_ID" --agent codex --event "$canonical_event"

if [ -n "$tool_name" ]; then
	set -- "$@" --tool "$tool_name"
fi

if [ -n "${ARCMUX_SESSION_STATE_DIR:-}" ]; then
	set -- "$@" --state-dir "$ARCMUX_SESSION_STATE_DIR"
fi

"$arcmux_bin" "$@" >/dev/null 2>&1 || exit 0
exit 0
