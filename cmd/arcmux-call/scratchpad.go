package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/paths"
)

var roleSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// cmdScratchpad dispatches `arcmux-call scratchpad <sub>`.
func cmdScratchpad(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: arcmux-call scratchpad read|write [flags]")
	}
	switch args[0] {
	case "read":
		return cmdScratchpadRead(args[1:], stdout)
	case "write":
		return cmdScratchpadWrite(args[1:], stdin, stdout)
	default:
		return fmt.Errorf("unknown scratchpad subcommand %q (want read|write)", args[0])
	}
}

func cmdScratchpadRead(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("scratchpad read", flag.ContinueOnError)
	role := fs.String("role", "", "role identifier (required, e.g. elon, manager-foo, ic-bar-1)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := scratchpadPath(*dataRoot, *project, *role, "read")
	if err != nil {
		return err
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return json.NewEncoder(stdout).Encode(map[string]any{"exists": false})
	}
	if err != nil {
		return fmt.Errorf("scratchpad read: stat %s: %w", path, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("scratchpad read: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"exists":  true,
		"content": string(body),
		"size":    info.Size(),
		"mtime":   info.ModTime().Format(time.RFC3339Nano),
		"path":    path,
	})
}

func cmdScratchpadWrite(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("scratchpad write", flag.ContinueOnError)
	role := fs.String("role", "", "role identifier (required, e.g. elon, manager-foo, ic-bar-1)")
	project := fs.String("project", os.Getenv("ARCMUX_PROJECT"), "project slug (default $ARCMUX_PROJECT)")
	dataRoot := fs.String("data-root", defaultDataRoot(), "ephemeral data root (default $ARCMUX_DATA or $HOME/data)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := scratchpadPath(*dataRoot, *project, *role, "write")
	if err != nil {
		return err
	}

	body, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("scratchpad write: read body from stdin: %w", err)
	}

	if err := atomicWrite(path, body); err != nil {
		return fmt.Errorf("scratchpad write: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":   true,
		"path": path,
		"size": len(body),
	})
}

// scratchpadPath validates inputs, ensures the scratchpads dir exists, and
// returns the canonical file path. label is the subcommand name used in
// error messages.
func scratchpadPath(dataRoot, project, role, label string) (string, error) {
	if role == "" {
		return "", fmt.Errorf("scratchpad %s: --role required", label)
	}
	if !roleSlug.MatchString(role) {
		return "", fmt.Errorf("scratchpad %s: invalid --role %q (must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63})", label, role)
	}
	if project == "" {
		return "", fmt.Errorf("scratchpad %s: --project or $ARCMUX_PROJECT required", label)
	}
	if _, err := paths.Validate(project); err != nil {
		return "", err
	}
	p := paths.ForProject(dataRoot, "", project)
	if err := os.MkdirAll(p.Scratchpads, 0o700); err != nil {
		return "", fmt.Errorf("scratchpad %s: ensure %s: %w", label, p.Scratchpads, err)
	}
	return filepath.Join(p.Scratchpads, role+".json"), nil
}

// atomicWrite writes body to path via a sibling tmp file + rename, with
// fsync on both file and parent dir to survive crashes between rename and
// flush. Permissions are 0600 (machine-local ephemeral state).
func atomicWrite(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// Best-effort parent dir fsync so the rename survives a power cut.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
