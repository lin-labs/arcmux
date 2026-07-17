package hooks

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// HookEvent represents a structured event written by an agent hook.
type HookEvent struct {
	Event     string    `json:"event"`
	Tool      string    `json:"tool,omitempty"`
	Timestamp time.Time `json:"ts"`
	SessionID string    `json:"session_id"`
	Data      any       `json:"data,omitempty"`
}

// genericHookName is the single, session-agnostic Claude hook script arcmux
// installs. It replaces the old per-session arcmux-<sessionID>.sh files: the
// per-session output path is now derived at runtime from environment
// (ARCMUX_SESSION_ID + ARCMUX_HOOK_OUTPUT_DIR) supplied to the spawned agent,
// so one script serves every session and re-install is idempotent.
const genericHookName = "arcmux-session-hook.sh"

// genericHookScript is the fixed content of the unified hook, embedded from the
// editable session_hook.sh so the arcmux binary is the single source of truth.
// It no-ops unless an arcmux-spawned session provided ARCMUX_SESSION_ID, so the
// single script is safe to install once and reference globally. The JSONL
// filename it derives (arcmux-hooks-<id>.jsonl under ARCMUX_HOOK_OUTPUT_DIR) is
// byte-identical to Installer.OutputPath, so the watcher keeps finding each
// session's file.
//
// ONE script serves every hook-backed class — it understands all input dialects
// (claude stdin, grok snake_case, codex argv+transcript), records the gauged
// goal / raw user message into the exact session-id-keyed state document. The
// daemon observes turn_end and owns the background overall-goal summarizer.
// See session_hook.sh for the full contract.
//
//go:embed session_hook.sh
var genericHookScript string

// ErrHookExternallyManaged means the configured hook path already belongs to
// another producer. Startup should warn, while per-session watchers may still
// observe the standard output path emitted by that registered hook.
var ErrHookExternallyManaged = errors.New("hook path is externally managed")

// Installer auto-configures the generic agent hook for sessions.
type Installer struct {
	OutputDir string
}

// NewInstaller creates a hook installer.
func NewInstaller(outputDir string) *Installer {
	return &Installer{OutputDir: outputDir}
}

// Install ensures the hook artifacts for the profile's hook type exist and
// returns the JSONL output path the watcher should monitor for this session.
// The scripts are session-agnostic and written idempotently; only the returned
// output path is session-specific. Keyed by the profile's HookType — not the
// agent name — so custom/config-defined profiles route correctly.
func (i *Installer) Install(sessionID, hookType, hookDir string) (string, error) {
	outputPath := i.OutputPath(sessionID)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", fmt.Errorf("create hook output dir: %w", err)
	}

	switch hookType {
	case "claude_hooks":
		err := i.installGenericHook(hookDir)
		if errors.Is(err, ErrHookExternallyManaged) {
			return outputPath, nil
		}
		return outputPath, err
	case "grok_hooks":
		// Grok loads drop-in JSON hook files from <hookDir>/hooks (always
		// trusted), so install the generic script there plus the registration
		// file that points grok's lifecycle events at it.
		return outputPath, i.EnsureGrokHook(hookDir)
	case "codex_output":
		return outputPath, nil // codex bridge script is materialized at daemon startup
	default:
		return outputPath, nil // screen_only / structured_output agents don't need hook scripts
	}
}

// OutputPath returns the JSONL file path for a session's hook events.
func (i *Installer) OutputPath(sessionID string) string {
	return filepath.Join(i.OutputDir, fmt.Sprintf("arcmux-hooks-%s.jsonl", sessionID))
}

// GenericHookPath returns the absolute path of the single generic hook script
// inside hookDir.
func GenericHookPath(hookDir string) string {
	return filepath.Join(hookDir, "hooks", genericHookName)
}

// EnsureGenericHook writes the shared, session-aware Claude hook without
// requiring a concrete session. Daemon startup uses this so the global hook is
// present immediately after deploy/restart; per-session Install still returns
// each session's JSONL path and remains the watcher contract.
func (i *Installer) EnsureGenericHook(hookDir string) error {
	return i.installGenericHook(hookDir)
}

// Cleanup removes the per-session JSONL output file. It deliberately does NOT
// remove the generic hook script: that script is shared across every session,
// so tearing down one session must not delete it. Either artifact may be
// absent (e.g. codex sessions have no JSONL yet).
func (i *Installer) Cleanup(sessionID string) error {
	if err := os.Remove(i.OutputPath(sessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// installGenericHook writes the single generic hook script idempotently.
// Re-writing identical content is skipped so concurrent/ repeated installs
// don't churn the file.
func (i *Installer) installGenericHook(hookDir string) error {
	// Defense in depth: a relative hookDir is almost certainly a bug upstream
	// (filepath.Join does not expand "~", and the daemon's cwd is not a
	// meaningful base for user-config paths).
	if !filepath.IsAbs(hookDir) {
		return fmt.Errorf("hook dir must be absolute, got %q (config or profile.HookDir was not resolved at load time)", hookDir)
	}

	path := GenericHookPath(hookDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create hook script dir: %w", err)
	}
	return installManagedHookScript(path)
}

// installManagedHookScript creates arcmux's generic hook only when the path is
// unowned. Existing scripts are accepted exclusively when they are the exact,
// executable arcmux payload. In particular, never follow a symlink here: user
// agent homes commonly link this filename into a separate configuration repo,
// and os.WriteFile would otherwise truncate that repository's richer hook.
func installManagedHookScript(path string) error {
	return installManagedHookScriptWithHooks(path, managedHookInstallHooks{})
}

type managedHookInstallHooks struct {
	afterCreate       func() error
	afterExistingRead func() error
}

func installManagedHookScriptWithHooks(path string, testHooks managedHookInstallHooks) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: refusing to replace symlinked hook %q; configure the owning hook producer instead", ErrHookExternallyManaged, path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: refusing to replace non-regular hook %q", ErrHookExternallyManaged, path)
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fmt.Errorf("inspect existing hook %q: %w", path, err)
		}
		opened, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("inspect opened hook %q: %w", path, err)
		}
		if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = file.Close()
			return fmt.Errorf("%w: hook %q changed while it was being inspected", ErrHookExternallyManaged, path)
		}
		existing, readErr := io.ReadAll(io.LimitReader(file, int64(len(genericHookScript)+1)))
		if readErr != nil {
			_ = file.Close()
			return fmt.Errorf("read existing hook %q: %w", path, readErr)
		}
		if testHooks.afterExistingRead != nil {
			if err := testHooks.afterExistingRead(); err != nil {
				_ = file.Close()
				return fmt.Errorf("after reading existing hook %q: %w", path, err)
			}
		}
		afterFile, fileErr := file.Stat()
		afterPath, pathErr := os.Lstat(path)
		closeErr := file.Close()
		if fileErr != nil || pathErr != nil || afterPath.Mode()&os.ModeSymlink != 0 || !afterPath.Mode().IsRegular() ||
			!os.SameFile(opened, afterFile) || !os.SameFile(opened, afterPath) ||
			afterFile.Size() != opened.Size() || afterPath.Size() != opened.Size() ||
			!afterFile.ModTime().Equal(opened.ModTime()) || !afterPath.ModTime().Equal(opened.ModTime()) {
			return fmt.Errorf("%w: hook %q changed while it was being inspected", ErrHookExternallyManaged, path)
		}
		if closeErr != nil {
			return fmt.Errorf("close existing hook %q: %w", path, closeErr)
		}
		if string(existing) != genericHookScript {
			return fmt.Errorf("%w: refusing to replace non-matching hook %q; preserve or update it through its owning configuration", ErrHookExternallyManaged, path)
		}
		if opened.Mode().Perm()&0o100 == 0 {
			return fmt.Errorf("existing arcmux hook %q is not executable; fix its mode through its owning configuration", path)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect hook script %q: %w", path, err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o755)
	if err != nil {
		return fmt.Errorf("create hook script %q without replacing an existing path: %w", path, err)
	}
	createdInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect newly created hook script %q: %w", path, err)
	}
	removePartial := true
	defer func() {
		if removePartial {
			removeCreatedHookIfUnchanged(path, createdInfo)
		}
	}()
	if testHooks.afterCreate != nil {
		if err := testHooks.afterCreate(); err != nil {
			_ = file.Close()
			return fmt.Errorf("after creating hook script %q: %w", path, err)
		}
	}
	if err := file.Chmod(0o755); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod hook script %q: %w", path, err)
	}
	if _, err := file.WriteString(genericHookScript); err != nil {
		_ = file.Close()
		return fmt.Errorf("write hook script %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close hook script %q: %w", path, err)
	}
	removePartial = false
	return nil
}

func removeCreatedHookIfUnchanged(path string, created os.FileInfo) {
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(created, current) {
		return
	}
	_ = os.Remove(path)
}

// CleanupLegacyScripts removes the per-session hook scripts left behind by the
// old generator (arcmux-s-*.sh under hookDir/hooks). It is the coded migration
// that sweeps the stray files instead of a manual one-off rm: idempotent
// (zero matches is fine), it never touches the generic hook, and it returns the
// number of scripts removed. Safe to call on every daemon startup.
func (i *Installer) CleanupLegacyScripts(hookDir string) (int, error) {
	if !filepath.IsAbs(hookDir) {
		return 0, fmt.Errorf("hook dir must be absolute, got %q", hookDir)
	}
	matches, err := filepath.Glob(filepath.Join(hookDir, "hooks", "arcmux-s-*.sh"))
	if err != nil {
		return 0, fmt.Errorf("glob legacy hook scripts: %w", err)
	}
	generic := GenericHookPath(hookDir)
	removed := 0
	var firstErr error
	for _, m := range matches {
		if m == generic {
			continue // defensive: the glob can't match the generic name, but be explicit
		}
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// ParseHookEvent parses a single line from a hook JSONL file.
func ParseHookEvent(line []byte) (HookEvent, error) {
	var event HookEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return HookEvent{}, err
	}
	return event, nil
}
