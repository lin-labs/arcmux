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
//
// Recording flags (all optional; empty leaves the field untouched). These make
// the turn-contract an accurate record, not a steer:
//
//	--goal <text>          the latest gauged "Your ask:" (the current sub-task)
//	--overall-goal <text>  the whole-conversation objective (summarizer-refreshed)
//	--last-message <text>  the raw, verbatim last user turn (3-line truncated)
//	--vault-link <path>    where the conversation is saved in the vault
//	--verification <text>  optional, current concrete success check
//	--path <text>          optional, consolidated path taken/planned
//	--contract-source <ev> which native event supplied the recording
//
// When --event is omitted but recording flags are present, only the contract is
// updated (no event is recorded, no counters move) — the write path used by the
// background overall-goal summarizer.
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
		case "--overall-goal":
			contract.OverallGoal, err = next()
		case "--last-message":
			contract.LastUserMessage, err = next()
		case "--vault-link":
			contract.VaultLink, err = next()
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
	hasContract := contract.Goal != "" || contract.OverallGoal != "" ||
		contract.LastUserMessage != "" || contract.VaultLink != "" ||
		contract.SuccessVerification != "" || contract.Path != ""
	if event == "" && !hasContract {
		return fmt.Errorf("arcmux hook: --event is required (one of %v)", hooks.CanonicalEvents)
	}
	if stateDir == "" {
		return fmt.Errorf("arcmux hook: no state dir (set --state-dir or ARCMUX_SESSION_STATE_DIR)")
	}

	if contract.Source == "" && hasContract {
		contract.Source = event // empty for a contract-only refresh
	}

	// Contract-only refresh (no event): used by the background summarizer to
	// update overall_goal without perturbing the event stream / counters.
	if event == "" {
		if err := hooks.ApplyContractOnly(stateDir, sessionID, agent, contract, time.Now()); err != nil {
			return fmt.Errorf("arcmux hook: %w", err)
		}
		return nil
	}

	if err := hooks.ApplyEventWithContract(stateDir, sessionID, agent, event, tool, contract, time.Now()); err != nil {
		return fmt.Errorf("arcmux hook: %w", err)
	}
	return nil
}
