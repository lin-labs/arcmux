package hooks

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// codexHookScript is the canonical codex lifecycle-hook bridge. It is embedded
// so the arcmux binary is the single source of truth — the on-disk
// codex_hook.sh file is the editable source, and `go generate`-free embedding
// keeps the installed script byte-identical to the repo.
//
// Codex registration (the [hooks] tables in ~/.codex/config.toml or
// ~/.codex/hooks.json, plus the one-time trust step) is intentionally NOT
// automated here: it mutates the user's existing codex config (which may
// already carry other hook bridges) and requires a trust review. See
// docs/codex-hooks-findings.md for the exact registration snippet.
//
//go:embed codex_hook.sh
var codexHookScript string

// codexHookName is the installed filename of the codex bridge script.
const codexHookName = "arcmux-codex-hook.sh"

// CodexHookPath returns the absolute path of the codex bridge script inside
// codexHookDir (typically ~/.codex/hooks).
func CodexHookPath(codexHookDir string) string {
	return filepath.Join(codexHookDir, codexHookName)
}

// EnsureCodexHook writes the codex bridge script into codexHookDir idempotently
// (skips the write when the content already matches). It only materializes the
// script file; it does not register or trust the hook in codex config. Mirrors
// EnsureGenericHook for claude.
func (i *Installer) EnsureCodexHook(codexHookDir string) error {
	if !filepath.IsAbs(codexHookDir) {
		return fmt.Errorf("codex hook dir must be absolute, got %q", codexHookDir)
	}
	path := CodexHookPath(codexHookDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create codex hook dir: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == codexHookScript {
		return nil
	}
	if err := os.WriteFile(path, []byte(codexHookScript), 0o755); err != nil {
		return fmt.Errorf("write codex hook script: %w", err)
	}
	return nil
}
