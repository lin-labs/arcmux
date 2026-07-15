package daemon

import (
	"context"
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
	now := time.Now().UTC()
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
