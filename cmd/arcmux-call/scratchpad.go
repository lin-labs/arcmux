package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lin-labs/arcmux/internal/manager/scratchpad"
)

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
	if *project == "" {
		return fmt.Errorf("scratchpad read: --project or $ARCMUX_PROJECT required")
	}
	path, err := scratchpad.Path(*dataRoot, *project, *role)
	if err != nil {
		return fmt.Errorf("scratchpad read: %w", err)
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
	if *project == "" {
		return fmt.Errorf("scratchpad write: --project or $ARCMUX_PROJECT required")
	}
	path, err := scratchpad.Path(*dataRoot, *project, *role)
	if err != nil {
		return fmt.Errorf("scratchpad write: %w", err)
	}

	body, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("scratchpad write: read body from stdin: %w", err)
	}
	if err := scratchpad.Write(path, body); err != nil {
		return fmt.Errorf("scratchpad write: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"ok":   true,
		"path": path,
		"size": len(body),
	})
}
