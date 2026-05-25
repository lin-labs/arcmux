package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderClaude(t *testing.T) {
	dir := t.TempDir()
	roleFile := filepath.Join(t.TempDir(), "elon.md")
	if err := os.WriteFile(roleFile, []byte("# Elon"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := Render(Options{
		Agent:     "claude",
		Project:   "demo",
		Role:      "elon",
		EphemRoot: dir,
		VaultRoot: "/vault",
		DataRoot:  "/data",
		RoleFile:  roleFile,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantPath := filepath.Join(dir, "bootstrap-elon.sh")
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
		"export ARCMUX_ROLE='elon'",
		"export ARCMUX_AGENT='claude'",
		"export ARCMUX_VAULT='/vault'",
		"export ARCMUX_DATA='/data'",
		"exec claude --dangerously-skip-permissions --append-system-prompt-file",
		roleFile,
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("script missing %q\n---\n%s", want, bs)
		}
	}
}

func TestRenderCodex(t *testing.T) {
	dir := t.TempDir()
	roleFile := filepath.Join(t.TempDir(), "elon.md")
	if err := os.WriteFile(roleFile, []byte("# Elon"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := Render(Options{
		Agent:     "codex",
		Project:   "demo",
		Role:      "elon",
		EphemRoot: dir,
		VaultRoot: "/vault",
		DataRoot:  "/data",
		RoleFile:  roleFile,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "exec codex") {
		t.Errorf("codex bootstrap missing exec codex; got: %s", body)
	}
}

func TestRenderExportsTeamAndContract(t *testing.T) {
	dir := t.TempDir()
	roleFile := filepath.Join(t.TempDir(), "ic-base.md")
	if err := os.WriteFile(roleFile, []byte("# IC"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := Render(Options{
		Agent:      "claude",
		Project:    "demo",
		Role:       "ic-team-a-linus-1",
		Team:       "team-a",
		Contract:   "design-auth",
		ScriptName: "bootstrap-ic-team-a-linus-1.sh",
		EphemRoot:  dir,
		VaultRoot:  "/vault",
		DataRoot:   "/data",
		RoleFile:   roleFile,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body, _ := os.ReadFile(path)
	bs := string(body)
	for _, want := range []string{
		"export ARCMUX_TEAM='team-a'",
		"export ARCMUX_CONTRACT='design-auth'",
		"export ARCMUX_ROLE='ic-team-a-linus-1'",
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("script missing %q\n---\n%s", want, bs)
		}
	}
}

func TestRenderOmitsContractWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	roleFile := filepath.Join(t.TempDir(), "manager.md")
	_ = os.WriteFile(roleFile, []byte("# M"), 0o644)
	path, err := Render(Options{
		Agent: "claude", Project: "demo", Role: "manager", Team: "team-a",
		EphemRoot: dir, VaultRoot: "/v", DataRoot: "/d", RoleFile: roleFile,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "ARCMUX_CONTRACT") {
		t.Errorf("manager bootstrap should not export ARCMUX_CONTRACT; got: %s", body)
	}
}

func TestRenderRejectsBadAgent(t *testing.T) {
	dir := t.TempDir()
	roleFile := filepath.Join(t.TempDir(), "r.md")
	_ = os.WriteFile(roleFile, []byte("x"), 0o644)
	_, err := Render(Options{
		Agent: "bash", Project: "demo", Role: "elon",
		EphemRoot: dir, VaultRoot: "/v", DataRoot: "/d", RoleFile: roleFile,
	})
	if err == nil {
		t.Error("expected error on unsupported agent")
	}
}

func TestRenderRequiresFields(t *testing.T) {
	_, err := Render(Options{Agent: "claude"})
	if err == nil {
		t.Error("expected error when required fields missing")
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
