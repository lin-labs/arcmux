package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditAppendThenRecent(t *testing.T) {
	dataRoot := t.TempDir()
	project := "elontest"

	// Append two entries.
	appendArgs := func(action, actor, subject string) []string {
		return []string{
			"--project", project,
			"--data-root", dataRoot,
			"--action", action,
			"--actor", actor,
			"--subject", subject,
		}
	}

	var out bytes.Buffer
	if err := cmdAuditAppend(appendArgs("team-created", "elon", "team-a"), &out); err != nil {
		t.Fatalf("append #1: %v", err)
	}
	var ack1 map[string]any
	if err := json.Unmarshal(out.Bytes(), &ack1); err != nil {
		t.Fatalf("decode ack #1: %v (raw: %q)", err, out.String())
	}
	if ack1["ok"] != true {
		t.Errorf("ack #1 ok=%v, want true", ack1["ok"])
	}
	if _, hasTS := ack1["ts"].(string); !hasTS {
		t.Errorf("ack #1 missing ts string: %v", ack1)
	}

	out.Reset()
	withDetail := append(appendArgs("ic-spawned", "manager-a", "ic-1"),
		"--rule-id", "r-spawn-ic",
		"--detail", `{"role":"validator","priority":1}`,
	)
	if err := cmdAuditAppend(withDetail, &out); err != nil {
		t.Fatalf("append #2: %v", err)
	}

	// Recent.
	out.Reset()
	if err := cmdAuditRecent([]string{"--project", project, "--data-root", dataRoot, "--n", "10"}, &out); err != nil {
		t.Fatalf("recent: %v", err)
	}
	var recent struct {
		Entries []struct {
			Action  string         `json:"action"`
			Actor   string         `json:"actor"`
			Subject string         `json:"subject"`
			RuleID  string         `json:"rule_id"`
			Detail  map[string]any `json:"detail"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(out.Bytes(), &recent); err != nil {
		t.Fatalf("decode recent: %v (raw: %q)", err, out.String())
	}
	if len(recent.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(recent.Entries))
	}
	// Newest-first.
	if recent.Entries[0].Action != "ic-spawned" {
		t.Errorf("entries[0].Action = %q, want %q", recent.Entries[0].Action, "ic-spawned")
	}
	if recent.Entries[0].RuleID != "r-spawn-ic" {
		t.Errorf("entries[0].RuleID = %q, want %q", recent.Entries[0].RuleID, "r-spawn-ic")
	}
	if got, want := recent.Entries[0].Detail["role"], "validator"; got != want {
		t.Errorf("entries[0].Detail.role = %v, want %v", got, want)
	}
	if recent.Entries[1].Action != "team-created" {
		t.Errorf("entries[1].Action = %q, want %q", recent.Entries[1].Action, "team-created")
	}
}

func TestAuditAppendRejectsMissingFields(t *testing.T) {
	// Isolate from the ambient shell — running inside `arcmux manager` exports
	// ARCMUX_PROJECT/ARCMUX_DATA, which would mask the "missing project" case.
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		args []string
	}{
		{"missing action", []string{"--project", "p", "--data-root", t.TempDir(), "--actor", "elon", "--subject", "x"}},
		{"missing actor", []string{"--project", "p", "--data-root", t.TempDir(), "--action", "x", "--subject", "x"}},
		{"missing subject", []string{"--project", "p", "--data-root", t.TempDir(), "--action", "x", "--actor", "x"}},
		{"missing project", []string{"--data-root", t.TempDir(), "--action", "x", "--actor", "x", "--subject", "x"}},
		{"bad project slug", []string{"--project", "../etc", "--data-root", t.TempDir(), "--action", "x", "--actor", "x", "--subject", "x"}},
		{"bad detail json", []string{"--project", "p", "--data-root", t.TempDir(), "--action", "x", "--actor", "x", "--subject", "x", "--detail", "not-json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdAuditAppend(tc.args, &out); err == nil {
				t.Fatalf("expected error, got nil; stdout=%q", out.String())
			}
		})
	}
}

func TestAuditRecentEmpty(t *testing.T) {
	dataRoot := t.TempDir()
	var out bytes.Buffer
	if err := cmdAuditRecent([]string{"--project", "fresh", "--data-root", dataRoot, "--n", "5"}, &out); err != nil {
		t.Fatalf("recent on empty: %v", err)
	}
	var got struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (raw: %q)", err, out.String())
	}
	if len(got.Entries) != 0 {
		t.Errorf("expected empty entries, got %d", len(got.Entries))
	}

	// state.bolt should have been created in the ephemeral root.
	if _, err := os.Stat(filepath.Join(dataRoot, "arcmux", "fresh", "state.bolt")); err != nil {
		t.Errorf("expected state.bolt to be created: %v", err)
	}
}
