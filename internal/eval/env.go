// Package eval implements arcmux's agent-behavioral eval harness. Where
// internal/e2e proves the substrate behaves correctly when poked via
// arcmux-call, internal/eval proves that an agent (claude headless) — given
// a mission, a fresh workdir, and a budget — can produce an artifact that
// passes a per-scenario assertion script.
//
// The harness is deliberately scoped to direct-dispatch in v0: one
// claude -p invocation per scenario, against a fresh sandbox workrepo, then
// a validate.sh assertion. Chain-mode (Elon→Manager→IC routed via the
// arcmux substrate) is a separate Scenario implementation tracked in §F15
// forward-plan — keeping it out of v0 means the framework can prove itself
// on the smallest possible failure surface.
//
// Per-scenario isolation: each scenario gets a unique temp dir under
// $TMPDIR/arcmux-eval-<unix-nano>-<scenario>/ containing the workrepo,
// trace log, and any spawned-agent artifacts. Nothing touches the user's
// live arcmux state, vault, or running claude session.
package eval

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Env is one scenario's isolated sandbox. It owns:
//   - a per-run temp dir at $TMPDIR/arcmux-eval-<ts>-<scenario>/
//   - a workrepo subdir the agent writes code into (also the agent's cwd)
//   - an artifacts dir for harness-owned artifacts (server logs, etc.)
//   - a trace log capturing every spawned-process invocation
//
// Teardown is best-effort and idempotent; the harness keeps the temp dir
// on failure for debugging and on --keep, and removes it on pass otherwise.
type Env struct {
	Scenario     string // canonical name (e.g. "hello-server")
	TempRoot     string // $TMPDIR/arcmux-eval-<ts>-<scenario>/
	WorkRepo     string // <TempRoot>/workrepo — agent's cwd
	ArtifactsDir string // <TempRoot>/artifacts — harness-owned
	LogsDir      string // <TempRoot>/logs
	TracePath    string // <TempRoot>/trace.log
	ScenarioDir  string // testdata/eval-scenarios/<scenario>/ (read-only)
	ClaudeBin    string // resolved path to the claude CLI
	BaseEnv      []string

	trace *os.File
}

// NewEnv creates a fresh isolated Env. scenarioDir must point at the
// scenario's testdata directory (containing prompt.md, validate.sh, etc.).
// Caller must call Teardown.
func NewEnv(scenario, scenarioDir, claudeBin string, baseEnv []string) (*Env, error) {
	if scenario == "" {
		return nil, fmt.Errorf("NewEnv: scenario name required")
	}
	if scenarioDir == "" {
		return nil, fmt.Errorf("NewEnv: scenarioDir required")
	}
	if _, err := os.Stat(scenarioDir); err != nil {
		return nil, fmt.Errorf("scenario dir %q: %w", scenarioDir, err)
	}
	if claudeBin == "" {
		return nil, fmt.Errorf("NewEnv: claude binary path required")
	}
	if _, err := os.Stat(claudeBin); err != nil {
		return nil, fmt.Errorf("claude bin %q: %w", claudeBin, err)
	}

	ts := time.Now().UnixNano()
	tag := fmt.Sprintf("%d-%s", ts, sanitize(scenario))
	tempRoot := filepath.Join(os.TempDir(), "arcmux-eval-"+tag)
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir temp root: %w", err)
	}

	workRepo := filepath.Join(tempRoot, "workrepo")
	artifactsDir := filepath.Join(tempRoot, "artifacts")
	logsDir := filepath.Join(tempRoot, "logs")
	for _, d := range []string{workRepo, artifactsDir, logsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	tracePath := filepath.Join(tempRoot, "trace.log")
	traceFile, err := os.Create(tracePath)
	if err != nil {
		return nil, fmt.Errorf("create trace: %w", err)
	}

	env := &Env{
		Scenario:     scenario,
		TempRoot:     tempRoot,
		WorkRepo:     workRepo,
		ArtifactsDir: artifactsDir,
		LogsDir:      logsDir,
		TracePath:    tracePath,
		ScenarioDir:  scenarioDir,
		ClaudeBin:    claudeBin,
		BaseEnv:      baseEnv,
		trace:        traceFile,
	}
	env.tracef("=== eval scenario=%s temp=%s ===", scenario, tempRoot)
	env.tracef("workrepo=%s scenario_dir=%s claude_bin=%s",
		workRepo, scenarioDir, claudeBin)
	return env, nil
}

// TraceWriter returns the trace log writer.
func (e *Env) TraceWriter() io.Writer { return e.trace }

// Tracef writes a timestamped line into the trace log.
func (e *Env) Tracef(f string, args ...any) { e.tracef(f, args...) }

func (e *Env) tracef(f string, args ...any) {
	if e.trace == nil {
		return
	}
	fmt.Fprintf(e.trace, "["+time.Now().Format("15:04:05.000")+"] "+f+"\n", args...)
}

// ReadScenarioFile returns the contents of testdata/eval-scenarios/<scenario>/<name>.
func (e *Env) ReadScenarioFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(e.ScenarioDir, name))
}

// ScenarioPath returns the absolute path to a file inside the scenario dir.
func (e *Env) ScenarioPath(name string) string {
	return filepath.Join(e.ScenarioDir, name)
}

// SpawnedEnv returns the env list to pass to spawned claude (or other)
// subprocesses. It:
//   - inherits BaseEnv (or os.Environ() if BaseEnv is nil)
//   - strips ARCMUX_* and CLAUDECODE/ANTHROPIC_API_KEY so the child claude
//     uses its own OAuth (mirrors shared AGENTS.md tmux-dispatch advice)
//   - leaves PATH alone so claude (and go, make, curl, python3) resolve
//
// Callers may append extra "KEY=VALUE" entries as overrides.
func (e *Env) SpawnedEnv(extra ...string) []string {
	base := e.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		switch {
		case strings.HasPrefix(kv, "ARCMUX_"),
			strings.HasPrefix(kv, "CLAUDECODE="),
			strings.HasPrefix(kv, "CLAUDE_CODE_"),
			strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN="):
			continue
		}
		out = append(out, kv)
	}
	out = append(out, extra...)
	return out
}

// RunCommand runs an arbitrary command with the spawned env. Returns
// combined stdout (logs both streams into the trace).
func (e *Env) RunCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = e.SpawnedEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	e.tracef("$ (cwd=%s) %s %s", dir, name, strings.Join(args, " "))
	err := cmd.Run()
	combined := append(append([]byte{}, stdout.Bytes()...), stderr.Bytes()...)
	if err != nil {
		e.tracef("  err: %v (stderr tail=%q)", err, tail(stderr.String(), 200))
		return combined, fmt.Errorf("%s %v: %w (stderr=%s)", name, args, err, tail(stderr.String(), 400))
	}
	e.tracef("  ok: %d bytes stdout, %d bytes stderr", stdout.Len(), stderr.Len())
	return combined, nil
}

// Teardown removes the temp root unless keepArtifacts is true. Closes the
// trace file. Idempotent.
func (e *Env) Teardown(stdout io.Writer, keepArtifacts bool) {
	if e.trace != nil {
		e.tracef("=== teardown ===")
		_ = e.trace.Close()
		e.trace = nil
	}
	if keepArtifacts {
		fmt.Fprintf(stdout, "    artifacts kept: %s\n", e.TempRoot)
		return
	}
	if err := os.RemoveAll(e.TempRoot); err != nil {
		fmt.Fprintf(stdout, "    teardown: rm %s: %v\n", e.TempRoot, err)
	}
}

// sanitize keeps alnum + dashes/underscores; replaces other chars with '-'.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "scenario"
	}
	return out
}

// tail returns the last n runes of s, suffix-only for trace brevity.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
