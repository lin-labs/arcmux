package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
)

// cmdHook implements `arcmux hook` — the single writer of per-session hook
// state. Agent hooks (claude, codex) invoke it on each event; it translates the
// canonical event into a mutation of <state-dir>/<session>.json.
//
// It fails SAFE for the "no arcmux session" case: an empty --session (or
// ARCMUX_SESSION_ID) is NOT an error — it exits 0 silently, so the generic hook
// stays harmless when installed globally and fired by non-arcmux sessions.
//
// Flags:
//
//	--session <id>     (defaults to $ARCMUX_SESSION_ID)
//	--agent <name>     (defaults to $ARCMUX_HOOK_AGENT, optional)
//	--event <canonical event>   prompt_submit|tool_start|tool_end|turn_end|notification
//	--tool <name>      (optional, for tool_* events)
//	--state-dir <dir>  (defaults to $ARCMUX_SESSION_STATE_DIR)
//	--goal <text>      (optional, current concrete goal snapshot)
//	--verification <text> (optional, current concrete success check)
//	--path <text>      (optional, consolidated path taken/planned)
func cmdHook(args []string) error {
	var (
		sessionID = os.Getenv("ARCMUX_SESSION_ID")
		agent     = os.Getenv("ARCMUX_HOOK_AGENT")
		stateDir  = os.Getenv("ARCMUX_SESSION_STATE_DIR")
		event     string
		tool      string
		contract  hooks.TurnContractUpdate
	)

	for i := 0; i < len(args); i++ {
		flag := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("arcmux hook: %s requires a value", flag)
			}
			v := args[i+1]
			if strings.HasPrefix(v, "--") {
				return "", fmt.Errorf("arcmux hook: %s requires a value, got flag %q", flag, v)
			}
			i++
			return v, nil
		}
		var err error
		switch args[i] {
		case "--session":
			sessionID, err = next()
		case "--agent":
			agent, err = next()
		case "--event":
			event, err = next()
		case "--tool":
			tool, err = next()
		case "--state-dir":
			stateDir, err = next()
		case "--goal":
			contract.Goal, err = next()
		case "--verification", "--success-verification":
			contract.SuccessVerification, err = next()
		case "--path":
			contract.Path, err = next()
		case "--contract-source":
			contract.Source, err = next()
		default:
			return fmt.Errorf("arcmux hook: unknown argument %q", args[i])
		}
		if err != nil {
			return err
		}
	}

	// Fail-safe no-op: not an arcmux-spawned session.
	if sessionID == "" {
		return nil
	}
	if event == "" {
		return fmt.Errorf("arcmux hook: --event is required (one of %v)", hooks.CanonicalEvents)
	}
	if stateDir == "" {
		return fmt.Errorf("arcmux hook: no state dir (set --state-dir or ARCMUX_SESSION_STATE_DIR)")
	}

	if contract.Source == "" && (contract.Goal != "" || contract.SuccessVerification != "" || contract.Path != "") {
		contract.Source = event
	}

	if err := hooks.ApplyEventWithContract(stateDir, sessionID, agent, event, tool, contract, time.Now()); err != nil {
		return fmt.Errorf("arcmux hook: %w", err)
	}
	return nil
}
