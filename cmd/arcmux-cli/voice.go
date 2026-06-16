package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// voiceHTTPBase returns the HTTP base URL for the daemon's HTTP server.
// ARCMUX_HTTP_ADDR overrides the default; if the value lacks a scheme, "http://" is prepended.
func voiceHTTPBase() string {
	if v := os.Getenv("ARCMUX_HTTP_ADDR"); v != "" {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return "http://" + v
		}
		return v
	}
	return "http://127.0.0.1:7777"
}

// voiceGetJSON makes an HTTP request to the daemon and decodes the JSON response.
func voiceGetJSON(method, base, path string, q url.Values) (map[string]any, error) {
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return m, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return m, nil
}

// runVoice dispatches the `arcmux-cli voice` subcommands.
func runVoice(args []string, httpBase string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: arcmux-cli voice <name> | start <name> | stop <name> | status [<name>]")
	}
	switch args[0] {
	case "start", "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: arcmux-cli voice %s <name>", args[0])
		}
		path := "/voice/record/start"
		if args[0] == "stop" {
			path = "/voice/record/stop"
		}
		m, err := voiceGetJSON("POST", httpBase, path, url.Values{"name": {args[1]}})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "recording=%v log_path=%v\n", m["recording"], m["log_path"])
		return nil
	case "status":
		q := url.Values{}
		if len(args) >= 2 {
			q.Set("name", args[1])
		}
		m, err := voiceGetJSON("GET", httpBase, "/voice/record/status", q)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Fprintln(out, string(b))
		return nil
	default:
		// Bare `voice <name>`: enable recording, mint a single-screen context,
		// print the connect handle voxtop should open.
		name := args[0]
		if _, err := voiceGetJSON("POST", httpBase, "/voice/record/start", url.Values{"name": {name}}); err != nil {
			return err
		}
		m, err := voiceGetJSON("POST", httpBase, "/babysit/new", url.Values{"name": {name}})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Voice context for %s ready.\n  connect: %v\n  context: %v\n", name, m["connect_url"], m["context_id"])
		fmt.Fprintln(out, "Open this handle in the voxtop voice client to talk to the screen.")
		return nil
	}
}
