package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCmdHookEnv_RequiresSessionID(t *testing.T) {
	var out bytes.Buffer
	if err := cmdHookEnv(nil, &out); err == nil {
		t.Error("expected usage error with no locator")
	}
	if err := cmdHookEnv([]string{"s-only"}, &out); err == nil {
		t.Error("expected usage error without profile scope")
	}
	if err := cmdHookEnv([]string{"root", ""}, &out); err == nil {
		t.Error("expected usage error with empty session id")
	}
}

// TestCmdHookEnv_FailSafeOnMissing confirms the loader's safety contract: an
// unresolvable session yields NO stdout and a nil error (exit 0), so the
// loader's `eval "$(arcmux hook-env <scope> <id>)"` is a no-op and the agent still
// launches with no injected env.
func TestCmdHookEnv_FailSafeOnMissing(t *testing.T) {
	var out bytes.Buffer
	// A session id that won't exist under /tmp/arcmux.
	if err := cmdHookEnv([]string{"root", "s-definitely-absent-xyz"}, &out); err != nil {
		t.Fatalf("expected nil error (fail-safe), got %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("expected empty stdout on missing session, got %q", out.String())
	}
}
