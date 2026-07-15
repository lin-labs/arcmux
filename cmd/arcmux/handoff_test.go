package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/handoff"
)

func TestReadHandoffGoalFromStdinAndNoFollowFile(t *testing.T) {
	goal, err := readHandoffGoal("-", strings.NewReader("  continue from stdin  \n"))
	if err != nil || goal != "continue from stdin" {
		t.Fatalf("stdin goal=%q err=%v", goal, err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "goal.txt")
	if err := os.WriteFile(path, []byte("original safe goal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if goal, err := readHandoffGoal(path, strings.NewReader("")); err != nil || goal != "original safe goal" {
		t.Fatalf("file goal=%q err=%v", goal, err)
	}

	symlink := filepath.Join(dir, "goal-link.txt")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readHandoffGoal(symlink, strings.NewReader("")); err == nil {
		t.Fatal("symlink goal file accepted")
	}

	replacement := filepath.Join(dir, "replacement.txt")
	if err := os.WriteFile(replacement, []byte("API_KEY=sk_replacementsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readHandoffGoalFile(path, func() {
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil || string(data) != "original safe goal" {
		t.Fatalf("path swap changed opened content=%q err=%v", data, err)
	}
}

func TestHandoffPrepareRoutesGoalFileAndRejectsInlineGoal(t *testing.T) {
	var captured handoffPrepareInput
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mesh/handoffs" {
			t.Fatalf("request %s %s", r.Method, r.URL.RequestURI())
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"handoff_id":"handoff-1","state":"remote_prepared"}` + "\n"))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	var out bytes.Buffer
	err := cmdHandoff([]string{
		"prepare", "devbox", "root", "session-1", "--project", "demo", "--agent", "codex",
		"--goal-file", "-", "--history", "history.md", "--conversation", "conversation-1", "--config", cfg,
	}, strings.NewReader("Continue the exact branch.\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Goal != "Continue the exact branch." || captured.TargetPeer != "devbox" || captured.ProfileScope != "root" || captured.SessionID != "session-1" {
		t.Fatalf("captured request = %#v", captured)
	}
	if !strings.Contains(out.String(), "remote_prepared") {
		t.Fatalf("output=%s", out.String())
	}
	if err := cmdHandoff([]string{
		"prepare", "devbox", "root", "session-1", "--project", "demo", "--agent", "codex", "--goal", "shell history leak", "--config", cfg,
	}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("inline --goal was accepted")
	}
}

func TestHandoffPrepareWaitPollsWithoutImplicitRetry(t *testing.T) {
	var mu sync.Mutex
	requests := make([]string, 0)
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"handoff_id":"handoff-1","state":"retry_wait"}` + "\n"))
			return
		}
		gets++
		state := "retry_wait"
		if gets >= 2 {
			state = "remote_prepared"
		}
		_, _ = w.Write([]byte(`{"handoff_id":"handoff-1","state":"` + state + `"}` + "\n"))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	var out bytes.Buffer
	err := cmdHandoff([]string{
		"prepare", "devbox", "root", "session-1", "--project", "demo", "--agent", "codex",
		"--goal-file", "-", "--wait", "2s", "--config", cfg,
	}, strings.NewReader("Continue after runtime retry."), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "remote_prepared") {
		t.Fatalf("wait output=%s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range requests {
		if strings.Contains(request, "/retry") {
			t.Fatalf("--wait implicitly retried: %v", requests)
		}
	}
}

func TestHandoffWaitDoesNotAdvanceOfflineRetryWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/retry") {
			t.Fatal("wait called explicit retry endpoint")
		}
		_, _ = w.Write([]byte(`{"handoff_id":"handoff-offline","state":"retry_wait"}` + "\n"))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	err := cmdHandoff([]string{
		"prepare", "devbox", "root", "session-1", "--project", "demo", "--agent", "codex",
		"--goal-file", "-", "--wait", "20ms", "--config", cfg,
	}, strings.NewReader("Wait for reconnect."), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("offline wait err=%v", err)
	}
}

func TestHandoffLaunchWaitPollsWithoutImplicitRetry(t *testing.T) {
	var requests []string
	var mu sync.Mutex
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"handoff_id":"handoff-1","state":"launch_retry_wait"}` + "\n"))
			return
		}
		gets++
		state := "launch_retry_wait"
		if gets >= 2 {
			state = "accepted"
		}
		_, _ = w.Write([]byte(`{"handoff_id":"handoff-1","state":"` + state + `"}` + "\n"))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	var out bytes.Buffer
	if err := cmdHandoff([]string{"launch", "handoff-1", "--wait", "2s", "--config", cfg}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"state":"accepted"`) {
		t.Fatalf("launch output=%s", out.String())
	}
	for _, request := range requests {
		if strings.Contains(request, "/retry") {
			t.Fatalf("launch wait implicitly retried: %v", requests)
		}
	}
}

func TestHandoffReceiveReadsOwnerLocalInstructionsByMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(t.TempDir(), "mux")
	store, err := handoff.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := handoffCommandManifest()
	record, _, err := store.ReceiveTarget(manifest)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.TransitionTarget(manifest.HandoffID, record.Revision, handoff.TargetValidating, handoff.Transition{})
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.TransitionTarget(manifest.HandoffID, record.Revision, handoff.TargetPrepared, handoff.Transition{})
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.TransitionTarget(manifest.HandoffID, record.Revision, handoff.TargetLaunching, handoff.Transition{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "handoff-"+manifest.HandoffID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"goal":"reply HANDOFF_OK","history":"/private/history.md"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "launch-instructions.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	marker := handoff.LaunchMarker(manifest.HandoffID, record.Digest)
	if err := handoff.PublishLaunchRendezvous(handoff.DefaultLaunchRendezvousRoot(), marker, root); err != nil {
		t.Fatal(err)
	}
	if err := cmdHandoff([]string{"receive", marker}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("receive output = %q, want %q", out.String(), want)
	}
}

func TestHandoffListShowRetryAndMainRouting(t *testing.T) {
	var mu sync.Mutex
	requests := make([]string, 0, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
	}))
	defer server.Close()
	cfg, _ := meshDataTestConfig(t, server.URL)
	if err := cmdHandoff([]string{"list", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := cmdHandoff([]string{"show", "handoff-1", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := cmdHandoff([]string{"retry", "handoff-1", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := cmdHandoff([]string{"launch", "handoff-1", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// run cannot inject stdout, but reaching the HTTP server proves the main
	// dispatcher recognizes the handoff command.
	if err := run([]string{"handoff", "list", "--config", cfg}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"GET /mesh/handoffs", "GET /mesh/handoffs?id=handoff-1", "POST /mesh/handoffs/retry?id=handoff-1",
		"POST /mesh/handoffs/launch?id=handoff-1", "GET /mesh/handoffs",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests=%v", requests)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Fatalf("request[%d]=%q want %q", i, requests[i], want[i])
		}
	}
}

func TestHandoffGoalBound(t *testing.T) {
	tooLarge := strings.Repeat("x", maxHandoffGoalFile+1)
	if _, err := readHandoffGoal("-", strings.NewReader(tooLarge)); err == nil {
		t.Fatal("oversized stdin goal accepted")
	}
	path := filepath.Join(t.TempDir(), "goal.txt")
	if err := os.WriteFile(path, []byte(tooLarge), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readHandoffGoal(path, strings.NewReader("")); err == nil {
		t.Fatal("oversized file goal accepted")
	}
}

func TestHandoffPrepareWaitRejectsNegativeDuration(t *testing.T) {
	err := cmdHandoff([]string{
		"prepare", "devbox", "root", "session-1", "--project", "demo", "--agent", "codex",
		"--goal-file", "-", "--wait", (-time.Second).String(),
	}, strings.NewReader("goal"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative wait err=%v", err)
	}
}

func TestHandoffLaunchRejectsNegativeDuration(t *testing.T) {
	err := cmdHandoff([]string{"launch", "handoff-1", "--wait", (-time.Second).String()}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative launch wait err=%v", err)
	}
}

func handoffCommandManifest() handoff.Manifest {
	now := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	return handoff.Manifest{
		SchemaVersion: handoff.ManifestVersion,
		HandoffID:     "handoff-command-receive",
		TraceID:       "trace-command-receive",
		Source: handoff.SourceSession{
			DeviceID: "ref", ProfileScope: "root", SessionID: "source-session",
		},
		SourceAgent: "codex",
		Target:      handoff.TargetAgent{DeviceID: "devbox", Profile: "codex"},
		Goal: handoff.GoalSummary{
			Text: "Reply HANDOFF_OK.", Provenance: "explicit_operator", UpdatedAt: now,
		},
		History: handoff.HistoryRef{
			ArtifactID: "history-command", Basename: "history.snapshot", SHA256: strings.Repeat("a", 64), SizeBytes: 128,
		},
		Repository: handoff.RepositorySnapshot{
			ProjectSlug: "arcmux", RepoSlug: "lin-labs/arcmux", Branch: "boyan/handoff",
			SourceHead: strings.Repeat("b", 40), BaseCommit: strings.Repeat("c", 40), TreeDigest: strings.Repeat("d", 40),
			Cleanliness: handoff.RepositoryClean, Transfer: handoff.TransferRemoteBranch,
		},
		Artifacts:  []handoff.ArtifactRef{},
		Validation: handoff.ValidationEvidence{State: handoff.ValidationPassed, RepositoryRevision: strings.Repeat("b", 40), CompletedAt: &now},
		CreatedAt:  now,
	}
}
