package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lin-labs/arcmux/internal/hooks"
)

func TestOnDemandSummaryUsesTheSameSafeOwnerPathAndNeverLegacyCLI(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	managed := addGoalSummarySession(t, d, "s-on-demand-safe", t.TempDir(), false,
		"Replace unsafe inference with a direct API boundary",
		"RAW-USER-SENTINEL-on-demand", "RAW-LAUNCH-SENTINEL-on-demand")

	markerPath := filepath.Join(t.TempDir(), "legacy-executed")
	fakeProducer := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(fakeProducer, []byte("#!/bin/sh\ntouch \"$FAKE_MARKER_FILE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_GOAL_BIN", fakeProducer)
	t.Setenv("FAKE_MARKER_FILE", markerPath)
	d.goalSummaryRunner = func(_ context.Context, current, goal string) (string, error) {
		if current != "" || goal != "Replace unsafe inference with a direct API boundary" {
			t.Fatalf("owner inputs current=%q goal=%q", current, goal)
		}
		return "Constrain background semantic work to authenticated network calls\nFEATURE: semantic-guardrails", nil
	}

	got, err := d.SummarizeSessionByID(context.Background(), managed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Constrain background semantic work to authenticated network calls" {
		t.Fatalf("summary=%q", got)
	}
	assertGoalSummary(t, d, managed.ID, got, true)
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, managed.ID)
	if err != nil || state.TurnContract.Feature != "semantic-guardrails" {
		t.Fatalf("feature=%q err=%v", state.TurnContract.Feature, err)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("legacy provider executed a process: %v", err)
	}
}

func TestWaitSummarizeReturnsServiceUnavailableWhenBoundedQueueIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newMeshApplicationTestDaemon(t, "ref")
	d.ctx = ctx
	d.goalSummaries = newGoalSummaryPool(goalSummaryGlobalLimit, goalSummaryQueueCapacity)
	d.goalSummaries.start(ctx)
	managed := addGoalSummarySession(t, d, "s-http-queue-full", t.TempDir(), false,
		"Exercise bounded admission", "raw queue user", "raw queue launch")

	release := make(chan struct{})
	block := func(context.Context) error { <-release; return nil }
	for i := 0; i < goalSummaryGlobalLimit+goalSummaryQueueCapacity; i++ {
		if err := d.goalSummaries.enqueue(goalSummaryJob{key: fmt.Sprintf("occupy-%d", i), run: block}); err != nil {
			t.Fatal(err)
		}
	}

	h := &HTTPServer{daemon: d}
	req := httptest.NewRequest(http.MethodPost, "/session/summarize?name="+managed.Name+"&wait=1", nil)
	resp := httptest.NewRecorder()
	h.handleSessionSummarize(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	close(release)
	cancel()
	d.goalSummaries.wait()
}
