package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestSessionSelfValidatesExactDaemonCatalogAndBindsHistory(t *testing.T) {
	env := map[string]string{
		"ARCMUX_SESSION_ID": "s-exact", "ARCMUX_PROFILE_SCOPE": "profile:codex",
		"ARCMUX_DAEMON_SOCKET": "/tmp/arcmux-codex.sock", "ARCMUX_SESSION_STATE_DIR": "/tmp/arcmux-state",
	}
	getenv := func(key string) string { return env[key] }
	catalog := func(socket, id string) (sessionSelfCatalogRecord, error) {
		if socket != "/tmp/arcmux-codex.sock" || id != "s-exact" {
			t.Fatalf("catalog socket=%q id=%q", socket, id)
		}
		basename := "2026-07-15-handoff.md"
		return sessionSelfCatalogRecord{
			ProfileScope: "profile:codex", SessionID: id, Agent: "codex", CWD: "/repo/worktree", HistoryBasename: &basename,
		}, nil
	}
	var out bytes.Buffer
	if err := cmdSessionWithRuntime([]string{"self", "--json"}, &out, getenv, catalog); err != nil {
		t.Fatal(err)
	}
	var got sessionSelfIdentity
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "s-exact" || got.ProfileScope != "profile:codex" || got.Agent != "codex" || got.CWD != "/repo/worktree" || got.HistoryBasename == nil || *got.HistoryBasename != "2026-07-15-handoff.md" || got.Source != "daemon_catalog" {
		t.Fatalf("self identity = %+v", got)
	}
}

func TestSessionSelfRejectsForgedEnvAndEmitsExplicitMissingHistory(t *testing.T) {
	env := map[string]string{
		"ARCMUX_SESSION_ID": "s-forged", "ARCMUX_PROFILE_SCOPE": "root",
		"ARCMUX_DAEMON_SOCKET": "/tmp/arcmux.sock", "ARCMUX_SESSION_STATE_DIR": "/tmp/arcmux-state",
	}
	getenv := func(key string) string { return env[key] }
	missing := func(string, string) (sessionSelfCatalogRecord, error) {
		return sessionSelfCatalogRecord{}, errors.New("not found")
	}
	if err := cmdSessionWithRuntime([]string{"self", "--json"}, &bytes.Buffer{}, getenv, missing); err == nil {
		t.Fatal("forged env identity was accepted without exact catalog record")
	}
	wrongScope := func(_ string, id string) (sessionSelfCatalogRecord, error) {
		return sessionSelfCatalogRecord{ProfileScope: "profile:codex", SessionID: id, Agent: "codex", CWD: "/repo"}, nil
	}
	if err := cmdSessionWithRuntime([]string{"self", "--json"}, &bytes.Buffer{}, getenv, wrongScope); err == nil {
		t.Fatal("forged root scope was accepted against a profile catalog")
	}
	catalog := func(_ string, id string) (sessionSelfCatalogRecord, error) {
		return sessionSelfCatalogRecord{ProfileScope: "root", SessionID: id, Agent: "codex", CWD: "/repo"}, nil
	}
	var out bytes.Buffer
	if err := cmdSessionWithRuntime([]string{"self", "--json"}, &out, getenv, catalog); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	value, present := raw["history_basename"]
	if !present || value != nil {
		t.Fatalf("missing history JSON = %s", out.String())
	}
}
