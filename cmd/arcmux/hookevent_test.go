package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCmdHookBindsVerifiedCanonicalHistoryWithoutReadingTranscriptBody(t *testing.T) {
	dir := t.TempDir()
	historyRoot := t.TempDir()
	basename := "2026-07-16-exact-session.md"
	content := "---\nagent: codex\nconversation_id: native-conversation-123\n---\n\nRAW-PRIVATE-TRANSCRIPT-SENTINEL\n"
	if err := os.WriteFile(filepath.Join(historyRoot, basename), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_SESSION_ID", "s-history")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", dir)
	t.Setenv("ARCMUX_HOOK_AGENT", "codex")
	t.Setenv("ARCMUX_HISTORY_ROOT", historyRoot)

	if err := cmdHook([]string{
		"--history-basename", basename,
		"--history-conversation-id", "native-conversation-123",
	}); err != nil {
		t.Fatal(err)
	}
	state, err := hooks.ReadSessionState(dir, "s-history")
	if err != nil || state == nil || state.TurnContract == nil || state.TurnContract.CanonicalHistory == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	encoded := state.TurnContract.CanonicalHistory.Basename + state.TurnContract.CanonicalHistory.ConversationID
	if strings.Contains(encoded, "RAW-PRIVATE-TRANSCRIPT-SENTINEL") {
		t.Fatal("canonical history binding leaked transcript body")
	}
}

func TestCmdHookRejectsMismatchedCanonicalHistoryWithoutMutatingState(t *testing.T) {
	dir := t.TempDir()
	historyRoot := t.TempDir()
	basename := "2026-07-16-other-session.md"
	if err := os.WriteFile(filepath.Join(historyRoot, basename), []byte("---\nconversation_id: other-native-conversation\n---\nprivate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_SESSION_ID", "s-history-mismatch")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", dir)
	t.Setenv("ARCMUX_HOOK_AGENT", "codex")
	t.Setenv("ARCMUX_HISTORY_ROOT", historyRoot)

	err := cmdHook([]string{
		"--history-basename", basename,
		"--history-conversation-id", "native-conversation-123",
	})
	if err == nil || !strings.Contains(err.Error(), "conversation identity does not match") {
		t.Fatalf("cmdHook error=%v", err)
	}
	state, readErr := hooks.ReadSessionState(dir, "s-history-mismatch")
	if readErr != nil || state != nil {
		t.Fatalf("rejected binding mutated state: state=%+v err=%v", state, readErr)
	}
}

func TestCmdHookRejectsCanonicalHistoryMixedWithUnverifiedContractOrEvent(t *testing.T) {
	dir := t.TempDir()
	historyRoot := t.TempDir()
	basename := "2026-07-16-exact-session.md"
	if err := os.WriteFile(filepath.Join(historyRoot, basename), []byte(
		"---\nconversation_id: exact-conversation\n---\nprivate\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_SESSION_ID", "s-history-mixed")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", dir)
	t.Setenv("ARCMUX_HOOK_AGENT", "codex")
	t.Setenv("ARCMUX_HISTORY_ROOT", historyRoot)

	for _, extra := range [][]string{
		{"--event", "turn_end"},
		{"--goal", "caller-controlled goal"},
		{"--vault-link", "/legacy/unverified.md"},
		{"--contract-source", "caller-controlled"},
	} {
		args := []string{
			"--history-basename", basename,
			"--history-conversation-id", "exact-conversation",
		}
		args = append(args, extra...)
		err := cmdHook(args)
		if err == nil || !strings.Contains(err.Error(), "history-only") {
			t.Fatalf("mixed canonical history args %v error=%v", extra, err)
		}
	}
	state, err := hooks.ReadSessionState(dir, "s-history-mixed")
	if err != nil || state != nil {
		t.Fatalf("rejected mixed updates mutated state: state=%+v err=%v", state, err)
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

func TestPaneCallableHookCannotAssertTrustedProvenance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARCMUX_SESSION_ID", "")
	t.Setenv("ARCMUX_SESSION_STATE_DIR", "")
	args := []string{
		"--session", "s-summary", "--agent", "claude", "--state-dir", dir,
		"--overall-goal", "caller-controlled raw text",
	}
	if err := cmdHook(args); err != nil {
		t.Fatal(err)
	}
	st, err := hooks.ReadSessionState(dir, "s-summary")
	if err != nil || st == nil || st.TurnContract == nil {
		t.Fatalf("state=%+v err=%v", st, err)
	}
	if st.TurnContract.OverallGoalProvenance != "" {
		t.Fatalf("caller-controlled text gained provenance: %+v", st.TurnContract)
	}
	if err := cmdHook([]string{"--session", "s-summary", "--state-dir", dir, "--overall-goal-provenance", hooks.OverallGoalSummarizerProvenance}); err == nil {
		t.Fatal("public hook caller was allowed to assert provenance")
	}
	if err := run([]string{"hook-summary", "--overall-goal", "caller-controlled"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("trusted summary writer remained pane-callable: %v", err)
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
