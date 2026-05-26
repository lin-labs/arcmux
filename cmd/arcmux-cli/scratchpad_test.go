package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScratchpadReadMissing(t *testing.T) {
	dataRoot := t.TempDir()
	var out bytes.Buffer
	args := []string{"--project", "sp", "--data-root", dataRoot, "--role", "elon"}
	if err := cmdScratchpadRead(args, &out); err != nil {
		t.Fatalf("read missing: %v", err)
	}
	var got struct {
		Exists  bool   `json:"exists"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (raw: %q)", err, out.String())
	}
	if got.Exists {
		t.Errorf("exists = true, want false")
	}
	if got.Content != "" {
		t.Errorf("content = %q, want empty for missing file", got.Content)
	}
}

func TestScratchpadWriteThenRead(t *testing.T) {
	dataRoot := t.TempDir()
	project := "sp"
	role := "elon"
	body := `{"focus":"plan-2-v2","turn":4}`

	// Write.
	var out bytes.Buffer
	wargs := []string{"--project", project, "--data-root", dataRoot, "--role", role}
	if err := cmdScratchpadWrite(wargs, strings.NewReader(body), &out); err != nil {
		t.Fatalf("write: %v", err)
	}
	var wack struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
		Size int    `json:"size"`
	}
	if err := json.Unmarshal(out.Bytes(), &wack); err != nil {
		t.Fatalf("decode write ack: %v", err)
	}
	if !wack.OK || wack.Size != len(body) {
		t.Errorf("write ack unexpected: %+v (body len %d)", wack, len(body))
	}
	if !strings.HasSuffix(wack.Path, filepath.Join("arcmux", project, "scratchpads", role+".json")) {
		t.Errorf("path = %q, want suffix /arcmux/%s/scratchpads/%s.json", wack.Path, project, role)
	}

	// File actually exists with 0600 perms.
	info, err := os.Stat(wack.Path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 0600", got)
	}

	// Read back — content matches byte-for-byte.
	out.Reset()
	rargs := []string{"--project", project, "--data-root", dataRoot, "--role", role}
	if err := cmdScratchpadRead(rargs, &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	var got struct {
		Exists  bool   `json:"exists"`
		Content string `json:"content"`
		Size    int64  `json:"size"`
		Mtime   string `json:"mtime"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode read: %v (raw: %q)", err, out.String())
	}
	if !got.Exists {
		t.Errorf("exists = false, want true")
	}
	if got.Content != body {
		t.Errorf("content mismatch:\n  got:  %q\n  want: %q", got.Content, body)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", got.Size, len(body))
	}
	if got.Mtime == "" {
		t.Errorf("mtime empty")
	}
}

func TestScratchpadWriteOverwrites(t *testing.T) {
	dataRoot := t.TempDir()
	project := "sp"
	role := "elon"
	wargs := []string{"--project", project, "--data-root", dataRoot, "--role", role}

	// First write.
	var out bytes.Buffer
	if err := cmdScratchpadWrite(wargs, strings.NewReader(`{"v":1}`), &out); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Second write — fully replaces.
	out.Reset()
	if err := cmdScratchpadWrite(wargs, strings.NewReader(`{"v":2,"more":"data"}`), &out); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	// Read — sees v2 only.
	out.Reset()
	if err := cmdScratchpadRead(wargs, &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	var got struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Content != `{"v":2,"more":"data"}` {
		t.Errorf("content = %q, want v2 payload", got.Content)
	}
}

func TestScratchpadWriteEmptyBody(t *testing.T) {
	// Empty stdin should produce an empty file, not an error — useful for
	// "clear my scratchpad" semantics.
	dataRoot := t.TempDir()
	var out bytes.Buffer
	args := []string{"--project", "sp", "--data-root", dataRoot, "--role", "elon"}
	if err := cmdScratchpadWrite(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	out.Reset()
	if err := cmdScratchpadRead(args, &out); err != nil {
		t.Fatalf("read empty: %v", err)
	}
	var got struct {
		Exists  bool   `json:"exists"`
		Content string `json:"content"`
		Size    int64  `json:"size"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Exists || got.Content != "" || got.Size != 0 {
		t.Errorf("empty write/read: %+v, want exists=true content='' size=0", got)
	}
}

func TestScratchpadRejectsBadInput(t *testing.T) {
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		fn   func([]string, *bytes.Buffer) error
		args []string
	}{
		{
			"read missing role",
			func(a []string, b *bytes.Buffer) error { return cmdScratchpadRead(a, b) },
			[]string{"--project", "p", "--data-root", t.TempDir()},
		},
		{
			"read missing project",
			func(a []string, b *bytes.Buffer) error { return cmdScratchpadRead(a, b) },
			[]string{"--data-root", t.TempDir(), "--role", "elon"},
		},
		{
			"read bad project slug",
			func(a []string, b *bytes.Buffer) error { return cmdScratchpadRead(a, b) },
			[]string{"--project", "../etc", "--data-root", t.TempDir(), "--role", "elon"},
		},
		{
			"read bad role slug",
			func(a []string, b *bytes.Buffer) error { return cmdScratchpadRead(a, b) },
			[]string{"--project", "p", "--data-root", t.TempDir(), "--role", "../etc"},
		},
		{
			"read empty role",
			func(a []string, b *bytes.Buffer) error { return cmdScratchpadRead(a, b) },
			[]string{"--project", "p", "--data-root", t.TempDir(), "--role", ""},
		},
		{
			"write missing role",
			func(a []string, b *bytes.Buffer) error {
				return cmdScratchpadWrite(a, strings.NewReader("x"), b)
			},
			[]string{"--project", "p", "--data-root", t.TempDir()},
		},
		{
			"write bad role slug with slash",
			func(a []string, b *bytes.Buffer) error {
				return cmdScratchpadWrite(a, strings.NewReader("x"), b)
			},
			[]string{"--project", "p", "--data-root", t.TempDir(), "--role", "ic/escape"},
		},
		{
			"write bad project slug",
			func(a []string, b *bytes.Buffer) error {
				return cmdScratchpadWrite(a, strings.NewReader("x"), b)
			},
			[]string{"--project", "../etc", "--data-root", t.TempDir(), "--role", "elon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := tc.fn(tc.args, &out); err == nil {
				t.Fatalf("expected error, got nil; stdout=%q", out.String())
			}
		})
	}
}

func TestScratchpadDispatch(t *testing.T) {
	// Sanity: the dispatcher routes the two subcommands and rejects unknown.
	dataRoot := t.TempDir()
	common := []string{"--project", "sp", "--data-root", dataRoot, "--role", "elon"}

	var out bytes.Buffer
	if err := cmdScratchpad(append([]string{"write"}, common...), strings.NewReader(`{"k":"v"}`), &out); err != nil {
		t.Fatalf("dispatch write: %v", err)
	}
	out.Reset()
	if err := cmdScratchpad(append([]string{"read"}, common...), strings.NewReader(""), &out); err != nil {
		t.Fatalf("dispatch read: %v", err)
	}
	out.Reset()
	if err := cmdScratchpad([]string{"bogus"}, strings.NewReader(""), &out); err == nil {
		t.Fatalf("expected error for bogus subcommand")
	}
	out.Reset()
	if err := cmdScratchpad(nil, strings.NewReader(""), &out); err == nil {
		t.Fatalf("expected error for missing subcommand")
	}
}
