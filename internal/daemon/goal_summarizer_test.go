package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestDaemonOwnsTrustedOverallGoalWrite(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	managed := addGoalSummarySession(t, d, "s-owner-summary", t.TempDir(), false,
		"Connect Mission Control to exact remote agent state",
		"RAW-USER-SENTINEL-owner", "RAW-LAUNCH-SENTINEL-owner")
	d.goalSummaryRunner = func(_ context.Context, current, goal string) (string, error) {
		if current != "" || goal != "Connect Mission Control to exact remote agent state" {
			t.Fatalf("runner inputs current=%q goal=%q", current, goal)
		}
		return "Project semantic activity through a verified mesh identity", nil
	}
	if err := d.refreshOverallGoalOnce(context.Background(), managed.ID); err != nil {
		t.Fatal(err)
	}
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, managed.ID)
	if err != nil || state == nil || state.TurnContract == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if state.TurnContract.OverallGoal != "Project semantic activity through a verified mesh identity" ||
		state.TurnContract.OverallGoalProvenance != hooks.OverallGoalSummarizerProvenance {
		t.Fatalf("trusted daemon summary missing: %+v", state.TurnContract)
	}
	if _, ok := d.goalSummaryCandidate(managed.Snapshot()); ok {
		t.Fatal("completed turn remained eligible after a trusted summary refresh")
	}
}

func TestGoalSummaryCandidateRequiresExactCanonicalHistoryBinding(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	historyRoot := t.TempDir()
	d.cfg.Hooks.HistoryRoot = historyRoot
	managed := addGoalSummarySessionWithoutHistory(t, d, "s-exact-history", t.TempDir(), false,
		"STATE-GOAL-MUST-NOT-BE-THE-MODEL-INPUT", "RAW-USER-SENTINEL-history", "RAW-LAUNCH-SENTINEL-history")
	if err := hooks.ApplyContractOnly(d.cfg.Hooks.SessionStateDir, managed.ID, "codex", hooks.TurnContractUpdate{
		VaultLink: "/guessed/latest-history.md", Source: "legacy-guess",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.goalSummaryCandidate(managed.Snapshot()); ok {
		t.Fatal("vault_link-only state was accepted as summarization input")
	}

	basename := "2026-07-30-exact-history.md"
	conversationID := "native-conversation-exact-history"
	body := "Canonical conversation semantics from the exact native identity.\n"
	content := "---\nconversation_id: " + conversationID + "\nagent: codex\n---\n" + body
	if err := os.WriteFile(filepath.Join(historyRoot, basename), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hooks.ApplyVerifiedCanonicalHistoryBinding(
		d.cfg.Hooks.SessionStateDir, managed.ID, "codex", historyRoot, basename, conversationID, time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	candidate, ok := d.goalSummaryCandidate(managed.Snapshot())
	if !ok {
		t.Fatal("verified canonical history was not accepted")
	}
	if candidate.goal != body {
		t.Fatalf("model input=%q, want exact canonical body %q", candidate.goal, body)
	}
}

func TestMissingProviderBecomesPersistentTerminalAfterOneAttempt(t *testing.T) {
	for _, key := range []string{
		"ARCMUX_GOAL_PROVIDER", "OPENAI_API_KEY", "OPENAI_API_KEY_FILE", "XAI_API_KEY", "XAI_API_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
	d := newMeshApplicationTestDaemon(t, "ref")
	managed := addGoalSummarySession(t, d, "s-no-provider", t.TempDir(), false,
		"Summarize only when an authenticated provider is configured", "RAW-USER-no-provider", "RAW-LAUNCH-no-provider")
	if err := d.refreshOverallGoalOnce(context.Background(), managed.ID); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("first missing-provider error=%v", err)
	}
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, managed.ID)
	if err != nil || state.GoalSummaryAttempt == nil {
		t.Fatalf("persistent attempt ledger=%+v err=%v", state, err)
	}
	ledger := state.GoalSummaryAttempt
	if ledger.Attempts != 1 || !ledger.Terminal || ledger.FailureClass != "unavailable" {
		t.Fatalf("ledger=%+v", ledger)
	}
	if _, ok := d.goalSummaryCandidate(managed.Snapshot()); ok {
		t.Fatal("terminal turn remained eligible after a simulated daemon restart")
	}
	if err := d.refreshOverallGoalOnce(context.Background(), managed.ID); !errors.Is(err, hooks.ErrOverallGoalTerminal) {
		t.Fatalf("terminal turn retried: %v", err)
	}
	state, _ = hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, managed.ID)
	if state.GoalSummaryAttempt.Attempts != 1 {
		t.Fatalf("terminal turn consumed another attempt: %+v", state.GoalSummaryAttempt)
	}
}

func TestProviderAuthenticationFailureIsPermanentAndCredentialFree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"secret-bearing provider body"}`))
	}))
	defer server.Close()
	_, err := runAPIGoalSummary(context.Background(), goalSummaryProducer{
		kind: goalSummaryProducerOpenAI, model: "gpt-test", apiKey: "must-not-appear", endpoint: server.URL,
	}, "bounded prompt")
	class, permanent := classifyGoalSummaryError(err)
	if class != "auth" || !permanent {
		t.Fatalf("class=%q permanent=%v err=%v", class, permanent, err)
	}
	if strings.Contains(err.Error(), "must-not-appear") || strings.Contains(err.Error(), "secret-bearing") {
		t.Fatalf("credential or response body escaped error boundary: %v", err)
	}
}

func TestSameCWDSessionsUseExactStateAndPrivateSessionIsNeverSummarized(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	cwd := t.TempDir()
	publicA := addGoalSummarySession(t, d, "s-public-a", cwd, false,
		"Coordinate alpha deployment through remote state", "RAW-USER-ALPHA-1111", "RAW-LAUNCH-ALPHA-1111")
	publicB := addGoalSummarySession(t, d, "s-public-b", cwd, false,
		"Validate beta attribution on the mesh", "RAW-USER-BETA-2222", "RAW-LAUNCH-BETA-2222")
	private := addGoalSummarySession(t, d, "s-private", cwd, true,
		"PRIVATE-GOAL-SENTINEL-3333", "PRIVATE-USER-SENTINEL-3333", "PRIVATE-LAUNCH-SENTINEL-3333")

	var mu sync.Mutex
	seen := make(map[string]int)
	d.goalSummaryRunner = func(_ context.Context, _, goal string) (string, error) {
		mu.Lock()
		seen[goal]++
		mu.Unlock()
		switch goal {
		case "Coordinate alpha deployment through remote state":
			return "Advance the first remote rollout with isolated metadata", nil
		case "Validate beta attribution on the mesh":
			return "Confirm the second surface maps to its verified session", nil
		default:
			t.Fatalf("cross-attributed/private input reached producer: %q", goal)
			return "", nil
		}
	}
	d.queueOverallGoalSummaries(context.Background())
	d.goalSummaryWG.Wait()

	mu.Lock()
	if len(seen) != 2 || seen["Coordinate alpha deployment through remote state"] != 1 ||
		seen["Validate beta attribution on the mesh"] != 1 {
		t.Fatalf("producer inputs=%v", seen)
	}
	mu.Unlock()
	assertGoalSummary(t, d, publicA.ID, "Advance the first remote rollout with isolated metadata", true)
	assertGoalSummary(t, d, publicB.ID, "Confirm the second surface maps to its verified session", true)
	assertGoalSummary(t, d, private.ID, "PRIVATE-LAUNCH-SENTINEL-3333", false)
}

func TestDuplicateSessionIDsAcrossProfileScopedStateDoNotCrossAttribute(t *testing.T) {
	root := newMeshApplicationTestDaemon(t, "ref")
	profile := newMeshApplicationTestDaemon(t, "ref")
	stateRoot := filepath.Join(t.TempDir(), "sessions")
	root.cfg.Hooks.SessionStateDir = stateRoot
	profile.cfg.Daemon.ProfileName = "alpha"
	profile.cfg.Hooks.SessionStateDir = filepath.Join(stateRoot, "profiles", "alpha")
	cwd := t.TempDir()
	const duplicateID = "s-profile-duplicate"
	rootSession := addGoalSummarySession(t, root, duplicateID, cwd, false,
		"Coordinate root deployment safely", "RAW-ROOT-USER-1111", "RAW-ROOT-LAUNCH-1111")
	profileSession := addGoalSummarySession(t, profile, duplicateID, cwd, false,
		"Validate isolated profile attribution", "RAW-PROFILE-USER-2222", "RAW-PROFILE-LAUNCH-2222")

	root.goalSummaryRunner = func(_ context.Context, _, goal string) (string, error) {
		if goal != "Coordinate root deployment safely" {
			t.Fatalf("root read profile state: %q", goal)
		}
		return "Advance machine-wide rollout through verified metadata", nil
	}
	profile.goalSummaryRunner = func(_ context.Context, _, goal string) (string, error) {
		if goal != "Validate isolated profile attribution" {
			t.Fatalf("profile read root state: %q", goal)
		}
		return "Confirm the scoped surface uses its own recording", nil
	}
	root.queueOverallGoalSummaries(context.Background())
	profile.queueOverallGoalSummaries(context.Background())
	root.goalSummaryWG.Wait()
	profile.goalSummaryWG.Wait()

	assertGoalSummary(t, root, rootSession.ID, "Advance machine-wide rollout through verified metadata", true)
	assertGoalSummary(t, profile, profileSession.ID, "Confirm the scoped surface uses its own recording", true)
}

func TestToollessOpenAIProviderUsesBoundedHTTPSRequest(t *testing.T) {
	const apiKey = "test-provider-key-never-log"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Errorf("authorization header mismatch")
		}
		body, err := readBoundedGoalSummary(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request map[string]any
		if json.Unmarshal(body, &request) != nil {
			t.Errorf("invalid request: %s", body)
			return
		}
		if _, ok := request["tools"]; ok {
			t.Errorf("tool capability was sent: %s", body)
		}
		if stored, ok := request["store"].(bool); !ok || stored {
			t.Errorf("OpenAI request did not explicitly disable storage: %s", body)
		}
		if !strings.Contains(string(body), "semantic activity") || strings.Contains(string(body), "RAW-USER") {
			t.Errorf("request input mismatch: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"content":[{"type":"output_text","text":"Track verified remote work through exact session state"}]}]}`))
	}))
	defer server.Close()

	producer := goalSummaryProducer{
		kind: goalSummaryProducerOpenAI, model: "gpt-test", apiKey: apiKey, endpoint: server.URL,
	}
	got, err := runOverallGoalModelWithProducer(context.Background(), producer, "", "semantic activity")
	if err != nil || got != "Track verified remote work through exact session state" {
		t.Fatalf("summary=%q err=%v", got, err)
	}
}

func TestAPIGoalProviderDoesNotRedirectCredential(t *testing.T) {
	var redirectedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	producer := goalSummaryProducer{
		kind: goalSummaryProducerOpenAI, model: "gpt-test", apiKey: "redirect-secret", endpoint: source.URL,
	}
	if _, err := runOverallGoalModelWithProducer(context.Background(), producer, "", "semantic activity"); err == nil {
		t.Fatal("redirected API response was accepted")
	}
	if redirectedAuthorization != "" {
		t.Fatalf("provider credential followed redirect: %q", redirectedAuthorization)
	}
}

func TestLegacyProducerConfigurationCanNeverExecuteAProcess(t *testing.T) {
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "executed")
	fakeProducer := filepath.Join(dir, "grok")
	script := `#!/bin/sh
touch "$FAKE_MARKER_FILE"
printf '%s' 'unsafe legacy output'
`
	if err := os.WriteFile(fakeProducer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "legacy-cli")
	t.Setenv("ARCMUX_GOAL_BIN", fakeProducer)
	t.Setenv("FAKE_MARKER_FILE", markerPath)

	if _, err := runOverallGoalModel(context.Background(), "", "semantic goal"); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("legacy provider error=%v, want unavailable", err)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("legacy provider executed a process: %v", err)
	}
}

func TestSummarizerSourcesContainNoProcessLaunchCapability(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	for _, name := range []string{"goal_summarizer.go", "summarize.go"} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"\"os/exec\"", "exec.Command", "ARCMUX_GOAL_BIN", "legacy-cli"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("%s regained forbidden process-launch capability %q", name, forbidden)
			}
		}
	}
}

func TestResolveGoalSummaryProducerUsesOnlyExplicitSafeCapabilities(t *testing.T) {
	for _, key := range []string{
		"ARCMUX_GOAL_PROVIDER", "ARCMUX_GOAL_BIN", "ARCMUX_GOAL_MODEL",
		"OPENAI_API_KEY", "OPENAI_API_KEY_FILE", "XAI_API_KEY", "XAI_API_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
	if _, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{}); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("no-capability error=%v", err)
	}
	t.Setenv("OPENAI_API_KEY", "openai-test")
	t.Setenv("XAI_API_KEY", "xai-test")
	if _, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{}); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("ambient credentials selected an implicit producer: %v", err)
	}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "xai")
	producer, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{})
	if err != nil || producer.kind != goalSummaryProducerXAI {
		t.Fatalf("explicit producer=%+v err=%v", producer, err)
	}
}

func TestProviderAPIKeyFileRequiresPrivateRegularFile(t *testing.T) {
	for _, key := range []string{
		"ARCMUX_GOAL_PROVIDER", "ARCMUX_GOAL_BIN", "OPENAI_API_KEY", "OPENAI_API_KEY_FILE",
		"XAI_API_KEY", "XAI_API_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "openai")
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "openai.key")
	if err := os.WriteFile(keyPath, []byte(" file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY_FILE", keyPath)
	producer, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{})
	if err != nil || producer.kind != goalSummaryProducerOpenAI || producer.apiKey != "file-secret" {
		t.Fatalf("private key file producer kind=%q key-present=%v err=%v", producer.kind, producer.apiKey != "", err)
	}

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{}); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("world-readable key file error=%v", err)
	}
	if err := os.Chmod(keyPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{}); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("non-0600 key file error=%v", err)
	}

	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(dir, "openai-link.key")
	if err := os.Symlink(keyPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY_FILE", symlinkPath)
	if _, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{}); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("symlinked key file error=%v", err)
	}
}

func TestProviderAPIKeyFileRequiresAbsolutePath(t *testing.T) {
	for _, key := range []string{
		"ARCMUX_GOAL_PROVIDER", "ARCMUX_GOAL_BIN", "OPENAI_API_KEY", "OPENAI_API_KEY_FILE",
		"XAI_API_KEY", "XAI_API_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY_FILE", "relative-openai.key")
	if _, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{}); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("relative environment key-file error=%v", err)
	}
}

func TestProviderOverrideCannotReuseDifferentConfiguredProviderCredential(t *testing.T) {
	for _, key := range []string{
		"ARCMUX_GOAL_PROVIDER", "ARCMUX_GOAL_BIN", "OPENAI_API_KEY", "OPENAI_API_KEY_FILE",
		"XAI_API_KEY", "XAI_API_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	openAIPath := filepath.Join(dir, "openai.key")
	if err := os.WriteFile(openAIPath, []byte("openai-config-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := config.CurrentWorkConfig{Provider: "openai", APIKeyFile: openAIPath}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "xai")
	if _, err := resolveGoalSummaryProducer(settings); !errors.Is(err, errGoalSummaryUnavailable) {
		t.Fatalf("xAI override reused configured OpenAI credential: %v", err)
	}

	xAIPath := filepath.Join(dir, "xai.key")
	if err := os.WriteFile(xAIPath, []byte("xai-env-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XAI_API_KEY_FILE", xAIPath)
	producer, err := resolveGoalSummaryProducer(settings)
	if err != nil || producer.kind != goalSummaryProducerXAI || producer.apiKey != "xai-env-secret" {
		t.Fatalf("provider-specific override kind=%q key-present=%v err=%v", producer.kind, producer.apiKey != "", err)
	}
}

type uidFileInfo struct {
	os.FileInfo
	uid uint32
}

func (i uidFileInfo) Sys() any { return &syscall.Stat_t{Uid: i.uid} }

func TestProviderAPIKeyFileRequiresCurrentUIDOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fileInfoOwnedByCurrentUID(uidFileInfo{FileInfo: info, uid: uint32(os.Geteuid())}) {
		t.Fatal("current uid was rejected")
	}
	if fileInfoOwnedByCurrentUID(uidFileInfo{FileInfo: info, uid: uint32(os.Geteuid() + 1)}) {
		t.Fatal("non-owner uid was accepted")
	}
}

func TestResolveGoalSummaryProducerUsesPersistentConfigWithoutServiceEnvironment(t *testing.T) {
	for _, key := range []string{
		"ARCMUX_GOAL_PROVIDER", "ARCMUX_GOAL_BIN", "ARCMUX_GOAL_MODEL",
		"OPENAI_API_KEY", "OPENAI_API_KEY_FILE", "XAI_API_KEY", "XAI_API_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
	keyPath := filepath.Join(t.TempDir(), "openai.key")
	if err := os.WriteFile(keyPath, []byte("configured-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	producer, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{
		Provider: "openai", Model: "configured-model", APIKeyFile: keyPath,
	})
	if err != nil || producer.kind != goalSummaryProducerOpenAI ||
		producer.model != "configured-model" || producer.apiKey != "configured-secret" {
		t.Fatalf("persistent producer kind=%q model=%q key-present=%v err=%v",
			producer.kind, producer.model, producer.apiKey != "", err)
	}
}

func TestRefreshOverallGoalRejectsWrappedEmbeddedAndUnsafeOutput(t *testing.T) {
	candidate := goalSummaryCandidate{forbidden: []string{
		"Please rotate launch private sentinel today",
		"RAW-USER-SENTINEL-7391",
		"RAW-LAUNCH-SEED-2468",
	}}
	unsafe := []string{
		"Current task: RAW-USER-SENTINEL-7391 with additional framing",
		"Focus on rotate launch private sentinel before release",
		"Continue rotate launch safely",
		"Continue RAW-LAUNCH-SEED-2468 safely",
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnop",
	}
	for _, output := range unsafe {
		if _, err := validateGoalSummaryOutput(output, candidate); err == nil {
			t.Fatalf("unsafe producer output accepted: %q", output)
		}
	}
	if got, err := validateGoalSummaryOutput("Renew the provider credential before deployment", candidate); err != nil || got == "" {
		t.Fatalf("safe paraphrase=%q err=%v", got, err)
	}
}

func TestFeatureOutputRejectsCredentialsAndVerbatimTranscriptText(t *testing.T) {
	candidate := goalSummaryCandidate{forbidden: []string{
		"private feature sentinel alpha", "RAW-USER-FEATURE-7391",
	}}
	for _, output := range []string{
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnop",
		"private-feature-sentinel-alpha",
		"RAW-USER-FEATURE-7391",
	} {
		if _, err := validateGoalSummaryFeature(output, candidate); err == nil {
			t.Fatalf("unsafe feature output accepted: %q", output)
		}
	}
	if got, err := validateGoalSummaryFeature("Safe Summary Boundary", candidate); err != nil || got != "safe-summary-boundary" {
		t.Fatalf("feature=%q err=%v", got, err)
	}
}

func TestBoundedGoalReadersRejectOversizedStreamingOutput(t *testing.T) {
	if _, err := readBoundedGoalSummary(strings.NewReader(strings.Repeat("x", goalSummaryOutputBytes+1))); err == nil {
		t.Fatal("oversized API output was accepted")
	}
}

func TestStopCancelsAndWaitsQueuedGoalSummary(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	runCtx, cancel := context.WithCancel(context.Background())
	d.ctx = runCtx
	d.cancel = cancel
	started := make(chan struct{})
	finished := make(chan struct{})
	d.goalSummaryRunner = func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(finished)
		return "", ctx.Err()
	}
	managed := addGoalSummarySession(t, d, "s-summary-stop", t.TempDir(), false,
		"summarize shutdown behavior", "raw user shutdown", "raw launch shutdown")
	candidate, ok := d.goalSummaryCandidate(managed.Snapshot())
	if !ok || !d.startOverallGoalSummary(runCtx, candidate) {
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

func addGoalSummarySession(t *testing.T, d *Daemon, id, cwd string, private bool, goal, lastUser, launch string) *session.Session {
	t.Helper()
	managed := addGoalSummarySessionWithoutHistory(t, d, id, cwd, private, goal, lastUser, launch)
	now := time.Now().UTC().Add(-2 * time.Second)
	if err := os.MkdirAll(d.cfg.Hooks.HistoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	basename := id + ".md"
	conversationID := "conversation-" + id
	history := "---\nconversation_id: " + conversationID + "\nagent: codex\n---\n" + goal
	if err := os.WriteFile(filepath.Join(d.cfg.Hooks.HistoryRoot, basename), []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hooks.ApplyVerifiedCanonicalHistoryBinding(
		d.cfg.Hooks.SessionStateDir, id, "codex", d.cfg.Hooks.HistoryRoot, basename, conversationID, now,
	); err != nil {
		t.Fatal(err)
	}
	return managed
}

func addGoalSummarySessionWithoutHistory(t *testing.T, d *Daemon, id, cwd string, private bool, goal, lastUser, launch string) *session.Session {
	t.Helper()
	managed := session.NewSession(id, id, "codex", cwd)
	if private {
		managed.MarkPrivate()
	}
	d.mu.Lock()
	d.sessions[id] = managed
	d.mu.Unlock()
	now := time.Now().UTC().Add(-5 * time.Second)
	if err := hooks.InitSessionState(d.cfg.Hooks.SessionStateDir, id, "codex", launch, now); err != nil {
		t.Fatal(err)
	}
	if err := hooks.ApplyEventWithContract(
		d.cfg.Hooks.SessionStateDir, id, "codex", hooks.EventPromptSubmit, "",
		hooks.TurnContractUpdate{Goal: goal, LastUserMessage: lastUser}, now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := hooks.ApplyEvent(d.cfg.Hooks.SessionStateDir, id, "codex", hooks.EventTurnEnd, "", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return managed
}

func assertGoalSummary(t *testing.T, d *Daemon, id, want string, trusted bool) {
	t.Helper()
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, id)
	if err != nil || state == nil || state.TurnContract == nil {
		t.Fatalf("state %s=%+v err=%v", id, state, err)
	}
	if state.TurnContract.OverallGoal != want {
		t.Fatalf("overall goal %s=%q want %q", id, state.TurnContract.OverallGoal, want)
	}
	gotTrusted := state.TurnContract.OverallGoalProvenance == hooks.OverallGoalSummarizerProvenance
	if gotTrusted != trusted {
		t.Fatalf("trusted %s=%v want %v", id, gotTrusted, trusted)
	}
}
