package hooks

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// SessionEnvDir is the world-shared rendezvous directory where the session
// creator drops a profile/session-scoped env file and the tmux loader reads it back
// before launching the agent. It lives under /tmp by design (the agent pane
// shell and the daemon may be different processes), so every access is
// permission- and ownership-checked — see the safety notes on each function.
const SessionEnvDir = "/tmp/arcmux"

// envKeyAllowPrefix gates which keys may live in a session env file. Only
// arcmux-owned vars are accepted, so a hostile writer cannot smuggle in PATH,
// LD_PRELOAD, etc.
const envKeyAllowPrefix = "ARCMUX_"

var (
	envKeyRe      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	sessionIDRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$|^[a-z0-9]$`)
)

// SessionEnvFilePath returns the env file path for an exact (profile scope,
// session ID) locator under dir. Session IDs are unique only inside one
// profile, so scope is part of the rendezvous key rather than metadata in a
// session-ID-only file. Base64 is a bijective, path-safe scope encoding.
func SessionEnvFilePath(dir, profileScope, sessionID string) (string, error) {
	if err := validateProfileScope(profileScope); err != nil {
		return "", err
	}
	if !sessionIDRe.MatchString(sessionID) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	scopeKey := base64.RawURLEncoding.EncodeToString([]byte(profileScope))
	return filepath.Join(dir, scopeKey+"--"+sessionID+".env"), nil
}

// WriteSessionEnvFile writes a per-session env file containing the allowlisted
// ARCMUX_* key=value pairs in env. The directory is created 0700 and the file
// 0600, both owned by the current user. Keys are validated against the
// allowlist; values may not contain NUL or newline (one record per line).
// Returns the file path.
//
// This is a DATA file, not a shell script — it is never executed. The loader
// (`arcmux hook-env`) parses and re-quotes it; see LoadSessionEnvExports.
func WriteSessionEnvFile(dir, profileScope, sessionID string, env map[string]string) (string, error) {
	path, err := SessionEnvFilePath(dir, profileScope, sessionID)
	if err != nil {
		return "", err
	}
	if err := ensureOwnedDir(dir, 0o700); err != nil {
		return "", err
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic file content (idempotent writes)

	var b strings.Builder
	for _, k := range keys {
		if !envKeyRe.MatchString(k) || !strings.HasPrefix(k, envKeyAllowPrefix) {
			return "", fmt.Errorf("disallowed env key %q (must match %s* and [A-Z_][A-Z0-9_]*)", k, envKeyAllowPrefix)
		}
		v := env[k]
		if strings.ContainsAny(v, "\n\x00") {
			return "", fmt.Errorf("env value for %q contains newline or NUL", k)
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}

	// O_TRUNC so a re-create overwrites; O_NOFOLLOW makes a pre-planted
	// symlink fail closed instead of truncating its target; 0600 so only the
	// owner can read/write.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("open session env file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("session env file is not a regular file")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("session env file is not owned by the current uid")
	}
	// Re-assert mode in case a pre-existing file had looser perms.
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("chmod session env file: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return "", fmt.Errorf("write session env file: %w", err)
	}
	return path, nil
}

// LoadSessionEnvExports validates the rendezvous dir + the session env file and
// returns shell `export KEY='VALUE'` lines, with every value single-quote
// escaped by arcmux itself. The caller is expected to `eval "$(arcmux
// hook-env <scope> <id>)"` — i.e. eval ARCMUX's OWN quoted output, NOT source the raw
// file. That is the core safety property: a hostile value round-trips as a
// literal string and cannot inject shell.
//
// Safety checks (any failure => error, no exports returned):
//   - dir and file must be real (non-symlink), owned by the current uid,
//     and have no group/world permission bits (dir 0700, file 0600).
//   - every line must be KEY=VALUE with an allowlisted ARCMUX_* key.
func LoadSessionEnvExports(dir, profileScope, sessionID string) ([]string, error) {
	path, err := SessionEnvFilePath(dir, profileScope, sessionID)
	if err != nil {
		return nil, err
	}
	if err := checkOwnedSecure(dir, true); err != nil {
		return nil, fmt.Errorf("rendezvous dir %q unsafe: %w", dir, err)
	}
	if err := checkOwnedSecure(path, false); err != nil {
		return nil, fmt.Errorf("session env file %q unsafe: %w", path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session env file: %w", err)
	}

	var out []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("malformed env line (no key=value): %q", line)
		}
		key := line[:eq]
		val := line[eq+1:]
		if !envKeyRe.MatchString(key) || !strings.HasPrefix(key, envKeyAllowPrefix) {
			return nil, fmt.Errorf("disallowed env key %q", key)
		}
		if strings.ContainsRune(val, '\x00') {
			return nil, fmt.Errorf("env value for %q contains NUL", key)
		}
		out = append(out, fmt.Sprintf("export %s=%s", key, shellSingleQuote(val)))
	}
	return out, nil
}

func validateProfileScope(profileScope string) error {
	if profileScope == "root" {
		return nil
	}
	const prefix = "profile:"
	if strings.HasPrefix(profileScope, prefix) && profileNameRe.MatchString(strings.TrimPrefix(profileScope, prefix)) {
		return nil
	}
	return fmt.Errorf("invalid profile scope %q", profileScope)
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes
// the POSIX way ('\”), so the result is a single safe shell token. arcmux
// produces this; eval-ing it cannot execute the value's content.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureOwnedDir creates dir (and parents) if absent, enforces mode, and
// verifies it is a real directory owned by the current uid.
func ensureOwnedDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create %q: %w", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("chmod %q: %w", dir, err)
	}
	return checkOwnedSecure(dir, true)
}

// checkOwnedSecure verifies that path is a non-symlink owned by the current
// uid with no group/world permission bits. isDir selects dir-vs-regular-file
// expectations.
func checkOwnedSecure(path string, isDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("is a symlink")
	}
	if isDir {
		if !info.IsDir() {
			return fmt.Errorf("not a directory")
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("permissions %#o are too open (group/world bits set)", info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine ownership")
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not current uid %d", st.Uid, os.Getuid())
	}
	return nil
}
