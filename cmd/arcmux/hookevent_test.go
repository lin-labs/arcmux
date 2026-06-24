package main

import (
	"testing"

	"github.com/lin-labs/arcmux/internal/hooks"
)

func TestCmdHookAppliesEvent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCMUX_SESSION_ID", "")
	t.Setenv("ARCMUX_HOOK_AGENT", "")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", "")

	args := []string{
		"--session", "s-cli", "--agent", "claude",
		"--event", "prompt_submit", "--state-dir", dir,
	}
	if err := cmdHook(args); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	st, err := hooks.ReadSessionState(dir, "s-cli")
	if err != nil || st == nil {
		t.Fatalf("read: %v st=%v", err, st)
	}
	if !st.Working || st.LastPromptSubmitAt.IsZero() {
		t.Fatalf("state not mutated: %+v", st)
	}
}

func TestCmdHookNoopWithoutSession(t *testing.T) {
	t.Setenv("ARCMUX_SESSION_ID", "")
	// No --session and no env => fail-safe no-op, exit 0, no error.
	if err := cmdHook([]string{"--event", "prompt_submit", "--state-dir", t.TempDir()}); err != nil {
		t.Fatalf("expected no-op success, got %v", err)
	}
}

func TestCmdHookReadsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCMUX_SESSION_ID", "s-env")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", dir)
	t.Setenv("ARCMUX_HOOK_AGENT", "codex")

	if err := cmdHook([]string{"--event", "turn_end"}); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	st, err := hooks.ReadSessionState(dir, "s-env")
	if err != nil || st == nil {
		t.Fatalf("read: %v", err)
	}
	if st.Agent != "codex" || st.Working {
		t.Fatalf("env-driven event wrong: %+v", st)
	}
}

func TestCmdHookUpdatesTurnContract(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCMUX_SESSION_ID", "")
	t.Setenv("ARCMUX_HOOK_AGENT", "")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", "")

	args := []string{
		"--session", "s-contract", "--agent", "codex",
		"--event", "prompt_submit", "--state-dir", dir,
		"--goal", "Install arcmux turn contract",
		"--verification", "state JSON has turn_contract.goal, success_verification, and path",
		"--path", "Patch arcmux hook state, CLI flags, and hook scripts.",
		"--contract-source", "UserPromptSubmit",
	}
	if err := cmdHook(args); err != nil {
		t.Fatalf("cmdHook: %v", err)
	}
	st, err := hooks.ReadSessionState(dir, "s-contract")
	if err != nil || st == nil {
		t.Fatalf("read: %v st=%v", err, st)
	}
	if st.TurnContract == nil {
		t.Fatal("turn contract missing")
	}
	if st.TurnContract.Goal != "Install arcmux turn contract" {
		t.Fatalf("goal = %q", st.TurnContract.Goal)
	}
	if st.TurnContract.SuccessVerification != "state JSON has turn_contract.goal, success_verification, and path" {
		t.Fatalf("verification = %q", st.TurnContract.SuccessVerification)
	}
	if st.TurnContract.Path != "Patch arcmux hook state, CLI flags, and hook scripts." {
		t.Fatalf("path = %q", st.TurnContract.Path)
	}
	if st.TurnContract.Source != "UserPromptSubmit" {
		t.Fatalf("source = %q", st.TurnContract.Source)
	}
}

func TestCmdHookRequiresEventAndStateDir(t *testing.T) {
	t.Setenv("ARCMUX_SESSION_ID", "")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", "")
	if err := cmdHook([]string{"--session", "s-1", "--state-dir", t.TempDir()}); err == nil {
		t.Fatal("expected error when --event missing")
	}
	if err := cmdHook([]string{"--session", "s-1", "--event", "turn_end"}); err == nil {
		t.Fatal("expected error when no state dir")
	}
}

func TestCmdHookUnknownArg(t *testing.T) {
	t.Setenv("ARCMUX_SESSION_ID", "")
	if err := cmdHook([]string{"--bogus"}); err == nil {
		t.Fatal("expected error for unknown arg")
	}
}

func TestCmdHookFlagWithoutValue(t *testing.T) {
	t.Setenv("ARCMUX_SESSION_ID", "")
	// A valued flag must not silently swallow the following flag.
	if err := cmdHook([]string{"--session", "--event", "prompt_submit"}); err == nil {
		t.Fatal("expected error: --session consumed --event as its value")
	}
	if err := cmdHook([]string{"--event"}); err == nil {
		t.Fatal("expected error: --event with no value")
	}
}
