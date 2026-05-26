package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestCodexExecParserTracksThreadAndOutput(t *testing.T) {
	sess := session.NewSession("s-1", "test", "codex_exec", "/tmp")
	parser := &codexExecParser{}

	parser.HandleStdoutLine(sess, `{"type":"thread.started","thread_id":"019e37da-a6a4-7ef0-ac59-eadf9b3919f7"}`)
	parser.HandleStdoutLine(sess, `{"type":"item.completed","item":{"type":"agent_message","text":"OK"}}`)

	if got := sess.Snapshot().BackendSessionID; got != "019e37da-a6a4-7ef0-ac59-eadf9b3919f7" {
		t.Fatalf("BackendSessionID = %q", got)
	}
	if got := parser.FinalOutput(); got != "OK" {
		t.Fatalf("FinalOutput = %q, want OK", got)
	}
}

func TestClaudePrintParserTracksSessionAndOutput(t *testing.T) {
	sess := session.NewSession("s-1", "test", "claude_exec", "/tmp")
	parser := &claudePrintParser{}

	parser.HandleStdoutLine(sess, `{"type":"assistant","session_id":"d989d949-8265-4e9f-847c-3437ed8d49dc","message":{"content":[{"type":"text","text":"Hello"},{"type":"text","text":"World"}]}}`)

	if got := sess.Snapshot().BackendSessionID; got != "d989d949-8265-4e9f-847c-3437ed8d49dc" {
		t.Fatalf("BackendSessionID = %q", got)
	}
	if got := parser.FinalOutput(); got != "Hello\nWorld" {
		t.Fatalf("FinalOutput = %q", got)
	}
}

func TestFinalizeExecOutputPrefersStructuredFile(t *testing.T) {
	tmp := t.TempDir() + "/last.txt"
	if err := os.WriteFile(tmp, []byte("file output"), 0o644); err != nil {
		t.Fatalf("write temp output: %v", err)
	}

	parser := &codexExecParser{lastOutput: "parser output"}
	got := finalizeExecOutput(parser, tmp, "stderr text", nil)
	if got != "file output" {
		t.Fatalf("finalizeExecOutput = %q, want file output", got)
	}
}

func TestGenerateUUIDShape(t *testing.T) {
	u := generateUUID()
	parts := strings.Split(u, "-")
	if len(parts) != 5 {
		t.Fatalf("uuid parts = %d, want 5 (%q)", len(parts), u)
	}
	if len(parts[2]) != 4 || parts[2][0] != '4' {
		t.Fatalf("uuid version field malformed: %q", u)
	}
	if len(parts[3]) != 4 || !strings.ContainsRune("89ab", rune(parts[3][0])) {
		t.Fatalf("uuid variant field malformed: %q", u)
	}
}

func TestSendExecPromptWaitIdleBlocksUntilOutputReady(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "hooks")
	fakeClaude := filepath.Join(tmpDir, "claude")
	script := `#!/bin/sh
sleep 0.3
printf '%s\n' '{"type":"assistant","session_id":"test-session","message":{"content":[{"type":"text","text":"OK"}]}}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: filepath.Join(tmpDir, "arcmux.sock"),
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Hooks: config.HooksConfig{
			HookOutputDir: hookDir,
		},
		Agents: config.DefaultAgentProfiles(),
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	d.ctx = context.Background()

	sess := session.NewSession("s-1", "wait-idle", "claude_exec", tmpDir)
	sess.SetTransport(profile.TransportExec)
	sess.SetState(session.StateIdle)
	d.sessions[sess.ID] = sess

	start := time.Now()
	if err := d.SendPrompt(context.Background(), sess.ID, "say ok", true, true); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("SendPrompt returned too early: %v", elapsed)
	}

	if got := sess.Snapshot().State; got != session.StateIdle {
		t.Fatalf("state after SendPrompt = %q, want %q", got, session.StateIdle)
	}
	out, err := d.Capture(context.Background(), sess.ID, true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := strings.TrimSpace(out); got != "OK" {
		t.Fatalf("Capture output = %q, want OK", got)
	}
}

// TestCreateSession_ExecPropagatesEnvToProcess regresses the env-drop
// bug: arcmuxv1.CreateSessionRequest.Env flows through the gRPC handler
// and Daemon.CreateSession, gets stored on session.Session, and MUST
// land on the spawned process's environment. For the exec transport,
// that means cmd.Env in buildExecRunConfig contains the entries.
//
// We don't actually run the agent here — that requires a full claude/codex
// fake. Instead we drive sendExecPrompt indirectly via a script masked
// onto $PATH that just `env | grep ARCMUX_FOO` and writes to a sentinel
// file. If the env var made it through, the sentinel has the value.
func TestCreateSession_ExecPropagatesEnvToProcess(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "hooks")
	sentinel := filepath.Join(tmpDir, "env.out")
	fakeClaude := filepath.Join(tmpDir, "claude")
	// The exec driver invokes `claude -p ... --output-format stream-json ...`.
	// Our stub ignores the args and just dumps the env vars we care about,
	// then emits a structured assistant message so the parser is happy.
	script := `#!/bin/sh
env | grep -E '^(ARCMUX_|OBS_AGENTS=)' > "` + sentinel + `" || true
printf '%s\n' '{"type":"assistant","session_id":"test-session","message":{"content":[{"type":"text","text":"OK"}]}}'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: filepath.Join(tmpDir, "arcmux.sock"),
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Hooks: config.HooksConfig{
			HookOutputDir: hookDir,
		},
		Agents: config.DefaultAgentProfiles(),
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	d.ctx = context.Background()

	sess, err := d.CreateSession(context.Background(), CreateSessionRequest{
		Agent: "claude_exec",
		CWD:   tmpDir,
		Name:  "env-test",
		Env: map[string]string{
			"ARCMUX_FOO":       "bar",
			"ARCMUX_ROLE_FILE": "/elon/regions/region-a.md",
			"OBS_AGENTS":       "/tmp/obs-agents",
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Snapshot env survived all the way to the Session object.
	gotEnv := sess.Snapshot().Env
	if gotEnv["ARCMUX_FOO"] != "bar" {
		t.Errorf("session env ARCMUX_FOO=%q, want bar (full env=%v)", gotEnv["ARCMUX_FOO"], gotEnv)
	}

	// Now actually drive sendExecPrompt — that's the codepath the
	// subprocess inherits cmd.Env from.
	if err := d.SendPrompt(context.Background(), sess.Snapshot().ID, "hello", true, true); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	// The fake claude wrote the captured env to sentinel.
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "ARCMUX_FOO=bar") {
		t.Errorf("sentinel missing ARCMUX_FOO=bar; got:\n%s", got)
	}
	if !strings.Contains(got, "ARCMUX_ROLE_FILE=/elon/regions/region-a.md") {
		t.Errorf("sentinel missing ARCMUX_ROLE_FILE; got:\n%s", got)
	}
	if !strings.Contains(got, "OBS_AGENTS=/tmp/obs-agents") {
		t.Errorf("sentinel missing OBS_AGENTS; got:\n%s", got)
	}
}

func TestCreateSessionPersistsExecSessionImmediately(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket: filepath.Join(tmpDir, "arcmux.sock"),
			LogDir: filepath.Join(tmpDir, "logs"),
		},
		Hooks: config.HooksConfig{
			HookOutputDir: filepath.Join(tmpDir, "hooks"),
		},
		Agents: config.DefaultAgentProfiles(),
	}
	d := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	sess, err := d.CreateSession(context.Background(), CreateSessionRequest{
		Agent: "claude_exec",
		CWD:   tmpDir,
		Name:  "persist-me",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "sessions.json"))
	if err != nil {
		t.Fatalf("read sessions.json: %v", err)
	}
	if !strings.Contains(string(data), sess.Snapshot().ID) {
		t.Fatalf("sessions.json did not include session %s: %s", sess.Snapshot().ID, data)
	}
}
