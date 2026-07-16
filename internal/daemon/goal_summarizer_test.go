package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestGoalSummarySingleFlightAndGlobalConcurrencyBound(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	profile := newMeshApplicationTestDaemon(t, "ref")
	profile.goalSummarySlots = d.goalSummarySlots
	var active atomic.Int32
	var maximum atomic.Int32
	var calls atomic.Int32
	started := make(chan string, 4)
	release := make(chan struct{})
	d.goalSummaryRunner = func(_ context.Context, _, goal string) (string, error) {
		calls.Add(1)
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- goal
		<-release
		active.Add(-1)
		return "Consolidate bounded inference with isolated execution", nil
	}
	profile.goalSummaryRunner = d.goalSummaryRunner

	candidates := make([]goalSummaryCandidate, 0, 3)
	owners := []*Daemon{d, d, profile}
	for i, goal := range []string{"first independent objective", "second independent objective", "third independent objective"} {
		owner := owners[i]
		managed := addGoalSummarySession(t, owner, "s-bound-"+string(rune('a'+i)), t.TempDir(), false,
			goal, "raw user "+goal, "raw launch "+goal)
		candidate, ok := owner.goalSummaryCandidate(managed.Snapshot())
		if !ok {
			t.Fatalf("candidate %d missing", i)
		}
		candidates = append(candidates, candidate)
	}
	if !d.startOverallGoalSummary(context.Background(), candidates[0]) ||
		!d.startOverallGoalSummary(context.Background(), candidates[0]) {
		t.Fatal("single-flight start failed")
	}
	if !d.startOverallGoalSummary(context.Background(), candidates[1]) ||
		!profile.startOverallGoalSummary(context.Background(), candidates[2]) {
		t.Fatal("bounded start failed")
	}
	for i := 0; i < goalSummaryGlobalLimit; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bounded producer did not start")
		}
	}
	select {
	case extra := <-started:
		t.Fatalf("third/global duplicate producer overlapped: %q", extra)
	case <-time.After(100 * time.Millisecond):
	}
	if maximum.Load() != goalSummaryGlobalLimit || calls.Load() != goalSummaryGlobalLimit {
		t.Fatalf("maximum=%d calls=%d", maximum.Load(), calls.Load())
	}
	close(release)
	d.goalSummaryWG.Wait()
	profile.goalSummaryWG.Wait()

	// The candidate skipped only because the pool was full was not retry-gated.
	if !profile.startOverallGoalSummary(context.Background(), candidates[2]) {
		t.Fatal("third candidate did not retry")
	}
	profile.goalSummaryWG.Wait()
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want 3", calls.Load())
	}
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

func TestExplicitLegacyProducerKindPreservesCompatibilityWithoutBasenameInference(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	// The binary is intentionally named codex: provider behavior comes only
	// from ARCMUX_GOAL_PROVIDER, never the executable basename.
	fakeProducer := filepath.Join(dir, "codex")
	script := `#!/bin/sh
printf '%s' "$*" > "$FAKE_ARGS_FILE"
printf '%s' 'Legacy producer remains compatible'
`
	if err := os.WriteFile(fakeProducer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "legacy-cli")
	t.Setenv("ARCMUX_GOAL_BIN", fakeProducer)
	t.Setenv("ARCMUX_GOAL_MODEL", "legacy-model")
	t.Setenv("FAKE_ARGS_FILE", argsPath)

	got, err := runOverallGoalModel(context.Background(), "", "semantic goal")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Legacy producer remains compatible" {
		t.Fatalf("summary=%q", got)
	}
	args, _ := os.ReadFile(argsPath)
	invocation := string(args)
	if !strings.Contains(invocation, "--no-alt-screen") || !strings.Contains(invocation, "-m legacy-model") ||
		strings.Contains(invocation, "exec --ephemeral") {
		t.Fatalf("explicit legacy invocation changed: %s", invocation)
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
	producer, err := resolveGoalSummaryProducer(config.CurrentWorkConfig{})
	if err != nil || producer.kind != goalSummaryProducerOpenAI {
		t.Fatalf("automatic producer=%+v err=%v", producer, err)
	}
	t.Setenv("ARCMUX_GOAL_PROVIDER", "xai")
	producer, err = resolveGoalSummaryProducer(config.CurrentWorkConfig{})
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

func TestBoundedGoalReadersRejectOversizedStreamingOutput(t *testing.T) {
	if _, err := readBoundedGoalSummary(strings.NewReader(strings.Repeat("x", goalSummaryOutputBytes+1))); err == nil {
		t.Fatal("oversized API output was accepted")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "producer")
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 2000 ]; do printf '0123456789'; i=$((i+1)); done\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGoalSummaryCommand(execCommandContext(context.Background(), fake)); err == nil {
		t.Fatal("oversized legacy stdout was accepted")
	}
}

func execCommandContext(ctx context.Context, path string) *exec.Cmd {
	return exec.CommandContext(ctx, path)
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
	if !d.startOverallGoalSummary(runCtx, goalSummaryCandidate{
		sessionID: "s-summary-stop",
		input: hooks.OverallGoalInputSnapshot{
			Revision: 1, SessionID: "s-summary-stop", Agent: "codex", TurnCount: 1, LastTurnEndAt: time.Now(),
		},
		goal: "summarize shutdown behavior",
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

func addGoalSummarySession(t *testing.T, d *Daemon, id, cwd string, private bool, goal, lastUser, launch string) *session.Session {
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
