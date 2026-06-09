// cmd/arcmux-cli/exec.go
//
// `arcmux-cli exec` — headless one-shot agent run through the daemon's exec
// transport: create (or reuse) an exec-transport session for an LLM class,
// deliver the prompt, wait for the turn to finish, print the final output.
//
//	arcmux-cli exec --agent grok "summarize this repo"
//	echo "prompt" | arcmux-cli exec --agent codex
//	arcmux-cli exec --agent claude --keep "first turn"   # prints session_id on stderr
//	arcmux-cli exec --session ax-... "follow-up turn"     # resumes the kept session
//
// --agent accepts either the LLM class name ("grok") or the explicit exec
// profile name ("grok_exec"); the class form is resolved via ListAgents.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
)

func cmdExec(c arcmuxv1.AgentRuntimeClient, args []string, out io.Writer) error {
	var (
		agent     = "codex"
		cwd       string
		owner     string
		sessionID string
		keep      bool
		asJSON    bool
		timeout   = 15 * time.Minute
		prompt    string
	)

	for i := 0; i < len(args); i++ {
		flagNeedsValue := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("exec: %s requires a value", args[i])
			}
			i++
			return args[i], nil
		}
		var err error
		switch args[i] {
		case "--agent":
			agent, err = flagNeedsValue()
		case "--cwd":
			cwd, err = flagNeedsValue()
		case "--owner":
			owner, err = flagNeedsValue()
		case "--session":
			sessionID, err = flagNeedsValue()
		case "--keep":
			keep = true
		case "--json":
			asJSON = true
		case "--timeout":
			var v string
			if v, err = flagNeedsValue(); err == nil {
				timeout, err = time.ParseDuration(v)
			}
		default:
			if strings.HasPrefix(args[i], "--") {
				return fmt.Errorf("exec: unknown flag %q", args[i])
			}
			if prompt != "" {
				prompt += " "
			}
			prompt += args[i]
		}
		if err != nil {
			return err
		}
	}

	if prompt == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("exec: read prompt from stdin: %w", err)
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return fmt.Errorf("exec: no prompt (pass as argument or on stdin)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	created := false
	if sessionID == "" {
		profileName, err := resolveExecProfile(ctx, c, agent)
		if err != nil {
			return err
		}
		r, err := c.CreateSession(ctx, &arcmuxv1.CreateSessionRequest{
			Agent:   profileName,
			Cwd:     cwd,
			OwnerId: owner,
			// One-shot runs close out on exit so they don't pile up as dead
			// handles; --keep parks the session at idle for follow-up turns
			// (which resume the same backend thread).
			AutoClose: !keep,
		})
		if err != nil {
			return fmt.Errorf("exec: create session: %w", err)
		}
		sessionID = r.SessionId
		created = true
	}

	if _, err := c.SendPrompt(ctx, &arcmuxv1.SendPromptRequest{
		SessionId: sessionID,
		Text:      prompt,
		WaitIdle:  true, // exec transport: blocks until the subprocess finishes
	}); err != nil {
		return fmt.Errorf("exec: send prompt: %w", err)
	}

	cap, err := c.Capture(ctx, &arcmuxv1.CaptureRequest{SessionId: sessionID})
	if err != nil {
		return fmt.Errorf("exec: capture output: %w", err)
	}

	if asJSON {
		return json.NewEncoder(out).Encode(map[string]any{
			"session_id": sessionID,
			"output":     cap.Output,
			"state":      cap.State,
			"kept":       keep,
		})
	}
	fmt.Fprintln(out, strings.TrimSpace(cap.Output))
	if keep && created {
		fmt.Fprintf(os.Stderr, "session_id: %s (kept — follow up with: arcmux-cli exec --session %s \"...\")\n", sessionID, sessionID)
	}
	return nil
}

// resolveExecProfile maps an LLM class name to its exec-transport profile via
// ListAgents. An exact profile name that is already exec-transport passes
// through unchanged.
func resolveExecProfile(ctx context.Context, c arcmuxv1.AgentRuntimeClient, agent string) (string, error) {
	r, err := c.ListAgents(ctx, &arcmuxv1.ListAgentsRequest{})
	if err != nil {
		return "", fmt.Errorf("exec: list agents: %w", err)
	}
	var classMatch string
	for _, a := range r.Agents {
		if a.Name == agent {
			if a.Transport == "exec" {
				return a.Name, nil
			}
			// Interactive profile named like the class — fall through to the
			// class search for its exec sibling.
		}
		if a.Class == agent && a.Transport == "exec" {
			classMatch = a.Name
		}
	}
	if classMatch != "" {
		return classMatch, nil
	}
	return "", fmt.Errorf("exec: no exec-transport profile for agent %q (see `arcmux-cli agents`)", agent)
}
