package hooks

import (
	"fmt"
	"os"
	"path/filepath"
)

// Codex no longer has a separate bridge script: the unified session_hook.sh
// (genericHookScript) handles codex natively — it reads codex's native event
// from argv[1], its legacy `notify` JSON payload, and extracts the gauged goal
// / raw user message from the codex transcript. So codex installs the SAME
// script as claude/grok, under the same arcmux-session-hook.sh name.
//
// Codex registration (the [hooks] tables in ~/.codex/config.toml or
// ~/.codex/hooks.json, plus the one-time trust step) is intentionally NOT
// automated here: it mutates the user's existing codex config (which may
// already carry other hook bridges) and requires a trust review. See
// docs/codex-hooks-findings.md for the exact registration snippet.

// CodexHookPath returns the absolute path of the codex hook script. Unlike the
// claude/grok dirs (the agent home, with "hooks/" appended by GenericHookPath),
// CodexHookDir is already the hooks dir (~/.codex/hooks), so the script sits
// directly inside it — under the SAME unified name claude/grok use.
func CodexHookPath(codexHookDir string) string {
	return filepath.Join(codexHookDir, genericHookName)
}

// EnsureCodexHook materializes the unified hook script only when the destination
// is unowned. It accepts an existing executable only when its content already
// matches, and never follows or overwrites a symlink or foreign script. It does
// not register or trust the hook in codex config. Mirrors EnsureGenericHook for
// claude — and now installs the identical script, so codex no longer has a
// divergent bridge.
func (i *Installer) EnsureCodexHook(codexHookDir string) error {
	if !filepath.IsAbs(codexHookDir) {
		return fmt.Errorf("codex hook dir must be absolute, got %q", codexHookDir)
	}
	path := CodexHookPath(codexHookDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create codex hook dir: %w", err)
	}
	return installManagedHookScript(path)
}
