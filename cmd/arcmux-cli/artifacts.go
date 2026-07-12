package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

// runArtifacts implements `arcmux-cli artifacts <session name or id> [--json]
// [--window 2h]` — the conversation's produced files, in order, with the
// daemon's focus suggestion. This is the voice-agent entry point for "what
// were the artifacts in this conversation?".
func runArtifacts(args []string, httpBase string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: arcmux-cli artifacts <session name or id> [--json] [--window 2h]")
	}
	name := args[0]
	jsonOut := false
	q := url.Values{"name": {name}}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--window":
			if i+1 >= len(args) {
				return fmt.Errorf("--window requires a value (e.g. 2h)")
			}
			i++
			q.Set("window", args[i])
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	m, err := voiceGetJSON("GET", httpBase, "/session/artifacts", q)
	if err != nil {
		return err
	}
	if jsonOut {
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	}

	header := []string{}
	if v, _ := m["project"].(string); v != "" {
		header = append(header, "project="+v)
	}
	if v, _ := m["feature"].(string); v != "" {
		header = append(header, "feature="+v)
	}
	if v, _ := m["session_id"].(string); v != "" {
		header = append(header, "session="+v)
	}
	if len(header) > 0 {
		fmt.Fprintln(out, strings.Join(header, " "))
	}

	arts, _ := m["artifacts"].([]any)
	if len(arts) == 0 {
		fmt.Fprintln(out, "no artifacts recorded")
	}
	// Present newest-last (creation/update order is how the list is stored).
	for _, a := range arts {
		e, ok := a.(map[string]any)
		if !ok {
			continue
		}
		line := fmt.Sprintf("%-6v %s", e["op"], e["path"])
		if edits, ok := e["edits"].(float64); ok && edits > 1 {
			line += fmt.Sprintf("  (%d edits)", int(edits))
		}
		if gs, _ := e["git_status"].(string); gs != "" {
			line += "  [git " + gs + "]"
		}
		fmt.Fprintln(out, line)
	}
	if focus, ok := m["focus"].(map[string]any); ok {
		fmt.Fprintf(out, "focus: %v — %v\n", focus["path"], focus["reason"])
	}
	if alt, ok := m["alt_focus"].(map[string]any); ok {
		fmt.Fprintf(out, "alt:   %v — %v\n", alt["path"], alt["reason"])
	}
	if extra, ok := m["git_extra"].([]any); ok && len(extra) > 0 {
		paths := make([]string, 0, len(extra))
		for _, g := range extra {
			if e, ok := g.(map[string]any); ok {
				paths = append(paths, fmt.Sprintf("%v", e["path"]))
			}
		}
		sort.Strings(paths)
		fmt.Fprintf(out, "also dirty in repo (not tool-captured): %s\n", strings.Join(paths, ", "))
	}
	return nil
}

// runFeature implements `arcmux-cli feature <session> [<slug>] [--no-pin]`.
// With a slug it sets (and by default pins) the session's feature; without,
// it reports the current feature from the sessions listing.
func runFeature(args []string, httpBase string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: arcmux-cli feature <session name> [<slug>] [--no-pin]")
	}
	name := args[0]
	slug := ""
	pin := true
	for _, a := range args[1:] {
		if a == "--no-pin" {
			pin = false
		} else if !strings.HasPrefix(a, "--") && slug == "" {
			slug = a
		} else {
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	if slug == "" {
		m, err := voiceGetJSON("GET", httpBase, "/sessions", nil)
		if err != nil {
			return err
		}
		sessions, _ := m["sessions"].([]any)
		for _, s := range sessions {
			e, ok := s.(map[string]any)
			if !ok || e["name"] != name {
				continue
			}
			fmt.Fprintf(out, "feature=%v project=%v platform=%v\n", e["feature"], e["project"], e["platform"])
			return nil
		}
		return fmt.Errorf("no session named %q", name)
	}
	q := url.Values{"name": {name}, "feature": {slug}}
	if !pin {
		q.Set("pin", "0")
	}
	m, err := voiceGetJSON("POST", httpBase, "/session/feature", q)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "feature=%v pinned=%v session=%v\n", m["feature"], m["pinned"], m["session_id"])
	return nil
}
