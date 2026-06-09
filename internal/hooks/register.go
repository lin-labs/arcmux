package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var registrationEvents = []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// ClaudeSettingsPath returns the Claude settings.json path for claudeHookDir.
// claudeHookDir is the Claude home directory, typically ~/.claude.
func ClaudeSettingsPath(claudeHookDir string) string {
	return filepath.Join(claudeHookDir, "settings.json")
}

// CodexHooksConfigPath returns the Codex hooks.json path for codexHookDir.
// codexHookDir is the hooks script directory, typically ~/.codex/hooks.
func CodexHooksConfigPath(codexHookDir string) string {
	return filepath.Join(filepath.Dir(codexHookDir), "hooks.json")
}

// RegisterClaudeHooks idempotently registers arcmux's generic Claude hook in
// Claude settings.json. It only appends missing arcmux entries and refuses to
// overwrite malformed JSON.
func RegisterClaudeHooks(claudeHookDir string) (bool, error) {
	if !filepath.IsAbs(claudeHookDir) {
		return false, fmt.Errorf("claude hook dir must be absolute, got %q", claudeHookDir)
	}
	scriptPath := GenericHookPath(claudeHookDir)
	command := guardedHookCommand(scriptPath, "")
	return registerJSONHooks(ClaudeSettingsPath(claudeHookDir), scriptPath, func(string) string {
		return claudeHookEntry(command)
	})
}

// RegisterCodexHooks idempotently registers arcmux's Codex bridge in
// ~/.codex/hooks.json. It mirrors Codex's live hooks.json shape and refuses to
// overwrite malformed JSON.
func RegisterCodexHooks(codexHookDir string) (bool, error) {
	if !filepath.IsAbs(codexHookDir) {
		return false, fmt.Errorf("codex hook dir must be absolute, got %q", codexHookDir)
	}
	scriptPath := CodexHookPath(codexHookDir)
	return registerJSONHooks(CodexHooksConfigPath(codexHookDir), scriptPath, func(event string) string {
		return codexHookEntry(guardedHookCommand(scriptPath, event))
	})
}

func registerJSONHooks(path, scriptPath string, entryForEvent func(event string) string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		content := buildFreshHookConfig(entryForEvent)
		return true, writeHookConfig(path, content, 0o644)
	}
	if err != nil {
		return false, fmt.Errorf("read hook config: %w", err)
	}
	if !json.Valid(data) {
		return false, fmt.Errorf("parse hook config %s: malformed JSON", path)
	}

	updated := append([]byte(nil), data...)
	changed := false
	aliases := hookScriptAliases(scriptPath)
	for _, event := range registrationEvents {
		registered, err := eventHasCommand(updated, event, aliases)
		if err != nil {
			return false, err
		}
		if registered {
			continue
		}
		updated, err = addHookEntry(updated, event, entryForEvent(event))
		if err != nil {
			return false, err
		}
		changed = true
	}
	if !changed {
		return false, nil
	}

	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := writeHookConfig(path, updated, mode); err != nil {
		return false, err
	}
	return true, nil
}

func buildFreshHookConfig(entryForEvent func(event string) string) []byte {
	var b strings.Builder
	b.WriteString("{\n  \"hooks\": {\n")
	for i, event := range registrationEvents {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    %s: [\n%s\n    ]", jsonString(event), entryForEvent(event))
	}
	b.WriteString("\n  }\n}\n")
	return []byte(b.String())
}

func writeHookConfig(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create hook config dir: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.arcmux-%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write temp hook config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace hook config: %w", err)
	}
	return nil
}

func guardedHookCommand(scriptPath, event string) string {
	quoted := shellDoubleQuote(commandDisplayPath(scriptPath))
	if event == "" {
		return fmt.Sprintf("test -f %s || exit 0; sh %s", quoted, quoted)
	}
	return fmt.Sprintf("test -f %s || exit 0; sh %s %s", quoted, quoted, event)
}

func commandDisplayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return path
	}
	return "$HOME/" + filepath.ToSlash(rel)
}

func shellDoubleQuote(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`")
	if !strings.HasPrefix(s, "$HOME/") && s != "$HOME" {
		replacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	}
	return `"` + replacer.Replace(s) + `"`
}

func hookScriptAliases(path string) []string {
	display := commandDisplayPath(path)
	if display == path {
		return []string{path}
	}
	return []string{path, display}
}

func claudeHookEntry(command string) string {
	return fmt.Sprintf(`      {
        "hooks": [
          {
            "type": "command",
            "command": %s,
            "timeout": 5
          }
        ]
      }`, jsonString(command))
}

func codexHookEntry(command string) string {
	return fmt.Sprintf(`      {
        "hooks": [
          {
            "type": "command",
            "command": %s,
            "timeout": 5000,
            "statusMessage": "arcmux session state"
          }
        ]
      }`, jsonString(command))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func eventHasCommand(data []byte, event string, aliases []string) (bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("parse hook config: %w", err)
	}
	hooksRaw, ok := root["hooks"]
	if !ok {
		return false, nil
	}
	var events map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &events); err != nil {
		return false, fmt.Errorf("parse hooks object: %w", err)
	}
	eventRaw, ok := events[event]
	if !ok {
		return false, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(eventRaw, &entries); err != nil {
		return false, fmt.Errorf("parse hooks.%s array: %w", event, err)
	}
	for _, entry := range entries {
		if rawJSONContainsCommand(entry, aliases) {
			return true, nil
		}
	}
	return false, nil
}

func rawJSONContainsCommand(raw json.RawMessage, aliases []string) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return false
	}
	return valueContainsCommand(value, aliases)
}

func valueContainsCommand(value any, aliases []string) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "command" {
				if command, ok := item.(string); ok {
					for _, alias := range aliases {
						if strings.Contains(command, alias) {
							return true
						}
					}
				}
			}
			if valueContainsCommand(item, aliases) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if valueContainsCommand(item, aliases) {
				return true
			}
		}
	}
	return false
}

func addHookEntry(data []byte, event, entry string) ([]byte, error) {
	root, err := parseJSONObject(data)
	if err != nil {
		return nil, err
	}
	hooks, ok := root.member("hooks")
	if !ok {
		hooksObject := fmt.Sprintf("{\n    %s: [\n%s\n    ]\n  }", jsonString(event), entry)
		return insertObjectMember(data, root.start, root.end, "hooks", []byte(hooksObject), "  "), nil
	}
	if data[hooks.valueStart] != '{' {
		return nil, fmt.Errorf("hooks is %q, want object", jsonValueKind(data[hooks.valueStart]))
	}
	hookMembers, _, err := parseObjectMembers(data, hooks.valueStart)
	if err != nil {
		return nil, fmt.Errorf("parse hooks object: %w", err)
	}
	hooksObject := objectInfo{start: hooks.valueStart, end: hooks.valueEnd, members: hookMembers}
	eventMember, ok := hooksObject.member(event)
	if !ok {
		eventArray := fmt.Sprintf("[\n%s\n    ]", entry)
		return insertObjectMember(data, hooks.valueStart, hooks.valueEnd, event, []byte(eventArray), "    "), nil
	}
	if data[eventMember.valueStart] != '[' {
		return nil, fmt.Errorf("hooks.%s is %q, want array", event, jsonValueKind(data[eventMember.valueStart]))
	}
	return insertArrayValue(data, eventMember.valueStart, eventMember.valueEnd, []byte(entry), "    "), nil
}

type objectInfo struct {
	start   int
	end     int
	members []jsonObjectMember
}

type jsonObjectMember struct {
	key        string
	valueStart int
	valueEnd   int
}

func (o objectInfo) member(key string) (jsonObjectMember, bool) {
	for _, member := range o.members {
		if member.key == key {
			return member, true
		}
	}
	return jsonObjectMember{}, false
}

func parseJSONObject(data []byte) (objectInfo, error) {
	start := skipJSONSpace(data, 0)
	if start >= len(data) {
		return objectInfo{}, fmt.Errorf("top-level JSON is empty, want object")
	}
	if data[start] != '{' {
		return objectInfo{}, fmt.Errorf("top-level JSON is %q, want object", jsonValueKind(data[start]))
	}
	members, end, err := parseObjectMembers(data, start)
	if err != nil {
		return objectInfo{}, err
	}
	if skipJSONSpace(data, end) != len(data) {
		return objectInfo{}, fmt.Errorf("trailing content after JSON object")
	}
	return objectInfo{start: start, end: end, members: members}, nil
}

func parseObjectMembers(data []byte, start int) ([]jsonObjectMember, int, error) {
	if start >= len(data) || data[start] != '{' {
		return nil, 0, fmt.Errorf("expected object at offset %d", start)
	}
	var members []jsonObjectMember
	i := skipJSONSpace(data, start+1)
	if i < len(data) && data[i] == '}' {
		return members, i + 1, nil
	}
	for {
		keyStart := skipJSONSpace(data, i)
		keyEnd, err := scanJSONString(data, keyStart)
		if err != nil {
			return nil, 0, err
		}
		var key string
		if err := json.Unmarshal(data[keyStart:keyEnd], &key); err != nil {
			return nil, 0, err
		}
		i = skipJSONSpace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return nil, 0, fmt.Errorf("expected ':' after object key %q", key)
		}
		valueStart := skipJSONSpace(data, i+1)
		valueEnd, err := skipJSONValue(data, valueStart)
		if err != nil {
			return nil, 0, err
		}
		members = append(members, jsonObjectMember{key: key, valueStart: valueStart, valueEnd: valueEnd})
		i = skipJSONSpace(data, valueEnd)
		if i >= len(data) {
			return nil, 0, fmt.Errorf("unterminated object")
		}
		switch data[i] {
		case ',':
			i++
			continue
		case '}':
			return members, i + 1, nil
		default:
			return nil, 0, fmt.Errorf("expected ',' or '}' in object")
		}
	}
}

func insertObjectMember(data []byte, objectStart, objectEnd int, key string, value []byte, indent string) []byte {
	members, _, _ := parseObjectMembers(data, objectStart)
	closeIndex := objectEnd - 1
	var insertAt int
	var insert []byte
	if len(members) == 0 {
		insertAt = closeIndex
		insert = []byte("\n" + indent + jsonString(key) + ": " + string(value) + "\n" + indent[:max(0, len(indent)-2)])
	} else {
		insertAt = previousNonSpace(data, closeIndex) + 1
		insert = []byte(",\n" + indent + jsonString(key) + ": " + string(value))
	}
	return spliceBytes(data, insertAt, insert)
}

func insertArrayValue(data []byte, arrayStart, arrayEnd int, value []byte, closeIndent string) []byte {
	empty := skipJSONSpace(data, arrayStart+1) == arrayEnd-1
	if empty {
		return spliceBytes(data, arrayEnd-1, []byte("\n"+string(value)+"\n"+closeIndent))
	}
	insertAt := previousNonSpace(data, arrayEnd-1) + 1
	return spliceBytes(data, insertAt, []byte(",\n"+string(value)))
}

func spliceBytes(data []byte, at int, insert []byte) []byte {
	out := make([]byte, 0, len(data)+len(insert))
	out = append(out, data[:at]...)
	out = append(out, insert...)
	out = append(out, data[at:]...)
	return out
}

func previousNonSpace(data []byte, before int) int {
	for i := before - 1; i >= 0; i-- {
		switch data[i] {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return i
		}
	}
	return before
}

func skipJSONValue(data []byte, start int) (int, error) {
	start = skipJSONSpace(data, start)
	if start >= len(data) {
		return 0, fmt.Errorf("expected JSON value")
	}
	switch data[start] {
	case '"':
		return scanJSONString(data, start)
	case '{':
		_, end, err := parseObjectMembers(data, start)
		return end, err
	case '[':
		return skipJSONArray(data, start)
	default:
		i := start
		for i < len(data) {
			switch data[i] {
			case ' ', '\n', '\r', '\t', ',', ']', '}':
				if i == start {
					return 0, fmt.Errorf("expected JSON value at offset %d", start)
				}
				return i, nil
			default:
				i++
			}
		}
		return i, nil
	}
}

func skipJSONArray(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '[' {
		return 0, fmt.Errorf("expected array at offset %d", start)
	}
	i := skipJSONSpace(data, start+1)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		end, err := skipJSONValue(data, i)
		if err != nil {
			return 0, err
		}
		i = skipJSONSpace(data, end)
		if i >= len(data) {
			return 0, fmt.Errorf("unterminated array")
		}
		switch data[i] {
		case ',':
			i++
			continue
		case ']':
			return i + 1, nil
		default:
			return 0, fmt.Errorf("expected ',' or ']' in array")
		}
	}
}

func scanJSONString(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, fmt.Errorf("expected string at offset %d", start)
	}
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated string at offset %d", start)
}

func skipJSONSpace(data []byte, start int) int {
	for start < len(data) {
		switch data[start] {
		case ' ', '\n', '\r', '\t':
			start++
		default:
			return start
		}
	}
	return start
}

func jsonValueKind(b byte) string {
	switch b {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	default:
		return string([]byte{b})
	}
}
