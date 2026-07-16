package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestDaemonOwnsTrustedOverallGoalWrite(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	historyRoot := t.TempDir()
	d.goalHistoryRoot = historyRoot
	cwd := t.TempDir()
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	managed := session.NewSession("s-owner-summary", "owner-summary", "codex", cwd)
	d.mu.Lock()
	d.sessions[managed.ID] = managed
	d.mu.Unlock()

	history := "---\nhost: " + host + "\ncwd: " + cwd + "\n---\n\n## user\nBuild exact remote surface identity.\n"
	if err := os.WriteFile(filepath.Join(historyRoot, "session.md"), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-5 * time.Second)
	if err := hooks.ApplyEventWithContract(
		d.cfg.Hooks.SessionStateDir, managed.ID, "codex", hooks.EventPromptSubmit, "",
		hooks.TurnContractUpdate{OverallGoal: "caller-controlled seed"}, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := hooks.ApplyEvent(d.cfg.Hooks.SessionStateDir, managed.ID, "codex", hooks.EventTurnEnd, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	d.goalSummaryRunner = func(_ context.Context, current, conversation string) (string, error) {
		if current != "caller-controlled seed" || !strings.Contains(conversation, "Build exact remote surface identity") {
			t.Fatalf("runner inputs current=%q conversation=%q", current, conversation)
		}
		return "Ship exact native remote identity", nil
	}
	if err := d.refreshOverallGoalOnce(context.Background(), managed.ID); err != nil {
		t.Fatal(err)
	}
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, managed.ID)
	if err != nil || state == nil || state.TurnContract == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if state.TurnContract.OverallGoal != "Ship exact native remote identity" ||
		state.TurnContract.OverallGoalProvenance != hooks.OverallGoalSummarizerProvenance {
		t.Fatalf("trusted daemon summary missing: %+v", state.TurnContract)
	}
	if _, ok := d.goalSummaryCandidate(managed.ID, managed.Snapshot().Agent, managed.Snapshot().CWD); ok {
		t.Fatal("completed turn remained eligible after a trusted summary refresh")
	}
}

func TestRunOverallGoalModelFallsBackToCodex(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	fakeCodex := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s' "$*" > "$FAKE_ARGS_FILE"
out=''
while [ "$#" -gt 0 ]; do
	if [ "$1" = "--output-last-message" ]; then
		shift
		out="$1"
	fi
	shift
done
printf '%s' 'Ship semantic mesh summaries through trusted hook state' > "$out"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ARCMUX_GOAL_BIN", "")
	t.Setenv("ARCMUX_GOAL_MODEL", "gpt-test")
	t.Setenv("FAKE_ARGS_FILE", argsPath)

	got, err := runOverallGoalModel(context.Background(), "", "completed turn")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Ship semantic mesh summaries through trusted hook state" {
		t.Fatalf("summary = %q", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	invocation := string(args)
	for _, required := range []string{"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox read-only", "--output-last-message", "--model gpt-test"} {
		if !strings.Contains(invocation, required) {
			t.Fatalf("codex invocation missing %q: %s", required, invocation)
		}
	}
	if strings.Contains(invocation, "dangerously-bypass") {
		t.Fatalf("codex summarizer bypassed sandbox: %s", invocation)
	}
}

func TestRunOverallGoalModelPreservesExplicitLegacyProducer(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	fakeProducer := filepath.Join(dir, "trusted-summary-wrapper")
	script := `#!/bin/sh
printf '%s' "$*" > "$FAKE_ARGS_FILE"
printf '%s' 'Legacy producer remains compatible'
`
	if err := os.WriteFile(fakeProducer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_GOAL_BIN", fakeProducer)
	t.Setenv("ARCMUX_GOAL_MODEL", "legacy-model")
	t.Setenv("FAKE_ARGS_FILE", argsPath)

	got, err := runOverallGoalModel(context.Background(), "", "completed turn")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Legacy producer remains compatible" {
		t.Fatalf("summary = %q", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if invocation := string(args); !strings.Contains(invocation, "--no-alt-screen") ||
		!strings.Contains(invocation, "--disable-web-search") || !strings.Contains(invocation, "-m legacy-model") {
		t.Fatalf("legacy invocation changed: %s", invocation)
	}
}

func TestRunOverallGoalModelOmitsWhenNoSupportedProducerExists(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ARCMUX_GOAL_BIN", "")
	got, err := runOverallGoalModel(context.Background(), "", "completed turn")
	if !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("summary=%q err=%v, want unavailable", got, err)
	}
	if got != "" {
		t.Fatalf("unavailable producer returned summary %q", got)
	}
}

func TestReadBoundedGoalSummaryRejectsOversizedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", goalSummaryOutputBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedGoalSummary(path); err == nil {
		t.Fatal("oversized producer output was accepted")
	}
}

func TestRefreshOverallGoalRejectsDirectRawOrUnsafeModelOutput(t *testing.T) {
	tests := []struct {
		name    string
		current string
		history string
		output  string
	}{
		{
			name: "raw transcript copy", history: "## user\nRAW-USER-SENTINEL-7391\n",
			output: "RAW-USER-SENTINEL-7391",
		},
		{
			name: "launch seeded goal", current: "RAW-LAUNCH-SEED-2468",
			history: "## assistant\nA different semantic summary.\n", output: "RAW-LAUNCH-SEED-2468",
		},
		{
			name: "credential material", history: "## assistant\nRotate the provider credential.\n",
			output: "OPENAI_API_KEY=sk-proj-abcdefghijklmnop",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := newMeshApplicationTestDaemon(t, "ref")
			history := filepath.Join(t.TempDir(), "history.md")
			if err := os.WriteFile(history, []byte(test.history), 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := hooks.ApplyEventWithContract(
				d.cfg.Hooks.SessionStateDir, "s-output-boundary", "codex", hooks.EventPromptSubmit, "",
				hooks.TurnContractUpdate{OverallGoal: test.current}, now,
			); err != nil {
				t.Fatal(err)
			}
			if err := hooks.ApplyEvent(
				d.cfg.Hooks.SessionStateDir, "s-output-boundary", "codex", hooks.EventTurnEnd, "", now.Add(time.Second),
			); err != nil {
				t.Fatal(err)
			}
			state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, "s-output-boundary")
			if err != nil || state == nil {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			d.goalSummaryRunner = func(context.Context, string, string) (string, error) {
				return test.output, nil
			}
			err = d.refreshOverallGoal(context.Background(), goalSummaryCandidate{
				sessionID: "s-output-boundary", agent: "codex", turnCount: state.TurnCount,
				turnEnd: state.LastTurnEndAt, current: test.current, history: history,
			})
			if err == nil {
				t.Fatal("unsafe producer output was trusted")
			}
			state, err = hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, "s-output-boundary")
			if err != nil || state == nil {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if state.TurnContract != nil && state.TurnContract.OverallGoalProvenance != "" {
				t.Fatalf("unsafe output gained provenance: %+v", state.TurnContract)
			}
		})
	}
}

func TestFindSessionHistoryRequiresExactHostAndCWD(t *testing.T) {
	root := t.TempDir()
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	if err := os.WriteFile(filepath.Join(root, "missing-host.md"), []byte("---\ncwd: /expected\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wrong-host.md"), []byte("---\nhost: another-host\ncwd: /expected\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := findSessionHistory(root, "/expected")
	if err != nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	want := filepath.Join(root, "exact.md")
	if err := os.WriteFile(want, []byte("---\nhost: "+host+"\ncwd: /expected\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err = findSessionHistory(root, "/expected")
	if err != nil || path != want {
		t.Fatalf("path=%q want=%q err=%v", path, want, err)
	}
}

func TestStopCancelsAndWaitsQueuedGoalSummary(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	runCtx, cancel := context.WithCancel(context.Background())
	d.ctx = runCtx
	d.cancel = cancel
	history := filepath.Join(t.TempDir(), "history.md")
	if err := os.WriteFile(history, []byte("conversation"), 0o600); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	finished := make(chan struct{})
	d.goalSummaryRunner = func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(finished)
		return "", ctx.Err()
	}
	if !d.startOverallGoalSummary(runCtx, goalSummaryCandidate{
		sessionID: "s-summary-stop",
		history:   history,
	}) {
		t.Fatal("goal summary was not queued")
	}
	<-started

	d.Stop()

	select {
	case <-finished:
	default:
		t.Fatal("Stop returned before queued goal summary exited")
	}
}
