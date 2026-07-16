package main

import (
	"fmt"
	"io"
	"os"

	"github.com/lin-labs/arcmux/internal/hooks"
)

// cmdHookEnv implements `arcmux hook-env <profileScope> <sessionID>`. It is the
// tmux loader's first step: it reads the session's profile-scoped rendezvous file,
// validates ownership/permissions, parses the allowlisted KEY=VALUE records,
// and prints shell `export` lines with every value quoted by arcmux itself.
//
// The loader runs `eval "$(arcmux hook-env <scope> <id>)"` — i.e. it evals arcmux's
// OWN validated/quoted output, never sourcing the raw writable file. So this
// command is the trust boundary: it fails SAFE. On any validation error it
// prints NOTHING to stdout (the eval becomes a no-op and the agent still
// launches with no injected env, which makes the generic hook no-op) and
// reports the reason on stderr. It exits 0 so the loader chain proceeds.
func cmdHookEnv(args []string, out io.Writer) error {
	if len(args) != 2 || args[0] == "" || args[1] == "" {
		return fmt.Errorf("usage: arcmux hook-env <profile-scope> <session-id>")
	}
	profileScope := args[0]
	sessionID := args[1]

	exports, err := hooks.LoadSessionEnvExports(hooks.SessionEnvDir, profileScope, sessionID)
	if err != nil {
		// Fail-safe: no exports, non-fatal. The loader's eval is empty.
		fmt.Fprintf(os.Stderr, "arcmux hook-env: %v\n", err)
		return nil
	}
	for _, line := range exports {
		fmt.Fprintln(out, line)
	}
	return nil
}
