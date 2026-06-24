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
from pathlib import Path

raw = sys.stdin.read()
event = ""
tool = ""
notify_type = ""
goal = ""
verification = ""
path = ""

def compact(text, limit=600):
    text = " ".join(str(text).split())
    return text[:limit]

def first_text(data, keys):
    for key in keys:
        value = data.get(key)
        if isinstance(value, str) and value.strip():
            return compact(value)
    return ""

def content_text(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for item in content:
            if isinstance(item, dict) and isinstance(item.get("text"), str):
                parts.append(item["text"])
        return "\n".join(parts)
    return ""

def extract_codex_turn(data):
    transcript = data.get("transcript_path")
    turn_id = data.get("turn_id")
    if not isinstance(transcript, str) or not isinstance(turn_id, str):
        return "", "", ""
    assistant = ""
    user = ""
    try:
        with Path(transcript).open(encoding="utf-8") as handle:
            for line in handle:
                try:
                    item = json.loads(line)
                except Exception:
                    continue
                payload = item.get("payload")
                if not isinstance(payload, dict):
                    continue
                metadata = payload.get("metadata")
                if isinstance(metadata, dict) and metadata.get("turn_id") != turn_id:
                    continue
                if payload.get("type") != "message":
                    continue
                text = content_text(payload.get("content"))
                if not text:
                    continue
                role = payload.get("role")
                if role == "user":
                    user = text
                elif role == "assistant":
                    assistant = text
    except Exception:
        return "", "", ""
    found_goal = ""
    found_verification = ""
    found_path = compact(assistant) if assistant else ""
    for line in assistant.splitlines():
        stripped = line.strip()
        lower = stripped.lower()
        if lower.startswith("your ask:"):
            found_goal = compact(stripped.split(":", 1)[1])
        if any(word in lower for word in ("verified", "verification", "tests", "validated", "passed")):
            found_verification = compact(stripped)
            break
    if not found_goal and user:
        found_goal = compact(user)
    return found_goal, found_verification, found_path

try:
    data = json.loads(raw) if raw.strip() else {}
    if isinstance(data, dict):
        event = data.get("hook_event_name") or ""
        tool = data.get("tool_name") or ""
        notify_type = data.get("type") or ""
        goal = first_text(data, ("arcmux_goal", "goal", "objective", "prompt", "message", "text"))
        verification = first_text(data, ("arcmux_success_verification", "success_verification", "verification", "success_check"))
        path = first_text(data, ("arcmux_path", "path", "plan", "approach"))
        if not goal or not verification or not path:
            t_goal, t_verification, t_path = extract_codex_turn(data)
            goal = goal or t_goal
            verification = verification or t_verification
            path = path or t_path
except Exception:
    notify_type = ""
print(event)
print(tool)
print(notify_type)
print(goal)
print(verification)
print(path)
' 2>/dev/null
	)
	json_event=$(printf '%s\n' "$parsed" | sed -n '1p')
	tool_name=$(printf '%s\n' "$parsed" | sed -n '2p')
	notify_type=$(printf '%s\n' "$parsed" | sed -n '3p')
	contract_goal=$(printf '%s\n' "$parsed" | sed -n '4p')
	contract_verification=$(printf '%s\n' "$parsed" | sed -n '5p')
	contract_path=$(printf '%s\n' "$parsed" | sed -n '6p')
else
	json_event=$(printf '%s' "$payload" | sed -n 's/.*"hook_event_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
	tool_name=$(printf '%s' "$payload" | sed -n 's/.*"tool_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
	notify_type=$(printf '%s' "$payload" | sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | sed -n '1p')
	contract_goal=""
	contract_verification=""
	contract_path=""
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

if [ -n "$contract_goal" ]; then
	set -- "$@" --goal "$contract_goal"
fi

if [ -n "$contract_verification" ]; then
	set -- "$@" --verification "$contract_verification"
fi

if [ -n "$contract_path" ]; then
	set -- "$@" --path "$contract_path"
fi

if [ -n "$contract_goal$contract_verification$contract_path" ]; then
	set -- "$@" --contract-source "$native_event"
fi

if [ -n "${ARCMUX_SESSION_STATE_DIR:-}" ]; then
	set -- "$@" --state-dir "$ARCMUX_SESSION_STATE_DIR"
fi

"$arcmux_bin" "$@" >/dev/null 2>&1 || exit 0

if [ "$canonical_event" = "prompt_submit" ] && command -v python3 >/dev/null 2>&1; then
	python3 - <<'PY' 2>/dev/null || true
import json
msg = (
    "Arcmux turn contract: keep the current goal, success verification, and "
    "path concrete. Consolidate path updates instead of appending a log; "
    "treat user or agent steers as edits to one of those three fields."
)
print(json.dumps({
    "systemMessage": msg,
    "hookSpecificOutput": {
        "hookEventName": "UserPromptSubmit",
        "additionalContext": msg,
    },
}))
PY
fi
exit 0
