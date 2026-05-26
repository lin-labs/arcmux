package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderBasic(t *testing.T) {
	dir := t.TempDir()

	path, err := Render(Options{
		Project:   "demo",
		EphemRoot: dir,
		VaultRoot: "/vault",
		DataRoot:  "/data",
		Agent:     "claude",
		Command:   "claude --dangerously-skip-permissions --append-system-prompt-file /tmp/elon.md",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantPath := filepath.Join(dir, "bootstrap.sh")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("script not executable: mode=%v", info.Mode())
	}

	body, _ := os.ReadFile(path)
	bs := string(body)
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"export ARCMUX_PROJECT='demo'",
		"export ARCMUX_AGENT='claude'",
		"export ARCMUX_VAULT='/vault'",
		"export ARCMUX_DATA='/data'",
		"export ARCMUX_EPHEMERAL='" + dir + "'",
		"exec claude --dangerously-skip-permissions --append-system-prompt-file /tmp/elon.md",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("script missing %q\n---\n%s", want, bs)
		}
	}
}

func TestRenderEnvTags(t *testing.T) {
	dir := t.TempDir()
	path, err := Render(Options{
		Project:    "demo",
		EphemRoot:  dir,
		VaultRoot:  "/v",
		DataRoot:   "/d",
		Agent:      "claude",
		Command:    "claude --dangerously-skip-permissions --append-system-prompt-file /tmp/ic.md",
		ScriptName: "bootstrap-ic-team-a-linus-1.sh",
		Env: map[string]string{
			"ROLE":     "ic-team-a-linus-1",
			"TEAM":     "team-a",
			"SLOT":     "linus-1",
			"CONTRACT": "design-auth",
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if filepath.Base(path) != "bootstrap-ic-team-a-linus-1.sh" {
		t.Errorf("ScriptName not respected: %q", path)
	}
	body, _ := os.ReadFile(path)
	bs := string(body)
	for _, want := range []string{
		"export ARCMUX_ROLE='ic-team-a-linus-1'",
		"export ARCMUX_TEAM='team-a'",
		"export ARCMUX_SLOT='linus-1'",
		"export ARCMUX_CONTRACT='design-auth'",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("script missing %q\n---\n%s", want, bs)
		}
	}
}

// TestRenderOmitsEmptyTags pins the invariant that an empty-string tag value
// is dropped — callers can pass a partially populated Env map without
// emitting `export ARCMUX_X=”` lines that would mislead readers.
func TestRenderOmitsEmptyTags(t *testing.T) {
	dir := t.TempDir()
	path, err := Render(Options{
		Project:   "demo",
		EphemRoot: dir,
		Command:   "claude",
		Env: map[string]string{
			"ROLE": "manager",
			"SLOT": "",
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "ARCMUX_SLOT") {
		t.Errorf("empty SLOT tag should not be emitted; got: %s", body)
	}
	if !strings.Contains(string(body), "export ARCMUX_ROLE='manager'") {
		t.Errorf("expected ARCMUX_ROLE='manager'; got: %s", body)
	}
}

func TestRenderRequiresFields(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"missing project", Options{EphemRoot: "/tmp", Command: "claude"}},
		{"missing ephem", Options{Project: "demo", Command: "claude"}},
		{"missing command", Options{Project: "demo", EphemRoot: "/tmp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Render(tc.opts); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestShellQuoteEscapes(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"foo", "'foo'"},
		{"foo bar", "'foo bar'"},
		{"foo's bar", `'foo'"'"'s bar'`},
		{"", "''"},
	} {
		got := shellQuote(tc.in)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
