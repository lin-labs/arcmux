package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/profile"
	"github.com/lin-labs/arcmux/internal/session"
)

type execRunConfig struct {
	cmd             *exec.Cmd
	parser          execOutputParser
	finalOutputPath string
}

type execOutputParser interface {
	HandleStdoutLine(sess *session.Session, line string)
	FinalOutput() string
}

func (d *Daemon) sendExecPrompt(ctx context.Context, sess *session.Session, prof profile.Profile, text string, confirmDelivery, waitIdle bool) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("prompt text is empty")
	}
	if waitIdle {
		if err := d.waitForSessionIdle(ctx, sess.Snapshot().ID); err != nil {
			return err
		}
	}

	snap := sess.Snapshot()
	if snap.State == session.StateExited {
		return fmt.Errorf("session exited: %s", snap.ID)
	}

	d.mu.RLock()
	_, running := d.processes[snap.ID]
	d.mu.RUnlock()
	if running {
		return fmt.Errorf("session busy: %s", snap.ID)
	}

	runCfg, err := d.buildExecRunConfig(sess, prof, text)
	if err != nil {
		return err
	}

	stdout, err := runCfg.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := runCfg.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	outputPath := d.outputFilePath(snap.ID)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("prepare output dir: %w", err)
	}
	commandText := strings.Join(runCfg.cmd.Args, " ")
	if err := os.WriteFile(outputPath, []byte(fmt.Sprintf("[arcmux exec] running: %s\n", commandText)), 0o644); err != nil {
		return fmt.Errorf("prepare output file: %w", err)
	}

	runCtx, cancel := context.WithCancel(d.ctx)
	if err := runCfg.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start exec process: %w", err)
	}

	sess.SetPID(runCfg.cmd.Process.Pid)
	sess.SetCurrentCommand(commandText)
	sess.SetState(session.StateWorking)
	sess.SetHealth("healthy")
	sess.ResetNudge()
	d.persistSessions()

	d.mu.Lock()
	d.processes[snap.ID] = runCfg.cmd.Process
	d.monitors[snap.ID] = cancel
	d.mu.Unlock()

	d.emitStateChanged(snap.ID, session.StateWorking, "prompt started")
	d.emitEvent(Event{
		SessionID: snap.ID,
		Type:      "prompt_started",
		Message:   strings.TrimSpace(text),
		Timestamp: time.Now(),
	})

	go d.waitExecPrompt(runCtx, sess, runCfg, stdout, stderr, outputPath)

	_ = confirmDelivery
	if waitIdle {
		if err := d.waitForSessionIdle(ctx, snap.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) waitExecPrompt(runCtx context.Context, sess *session.Session, runCfg *execRunConfig, stdout, stderr io.ReadCloser, outputPath string) {
	sessionID := sess.Snapshot().ID
	var stdoutWG doneWaitGroup
	var stderrWG doneWaitGroup
	var stderrBuf bytes.Buffer

	stdoutWG.Go(func() {
		scanLines(stdout, func(line string) {
			runCfg.parser.HandleStdoutLine(sess, line)
		})
	})
	stderrWG.Go(func() {
		scanLines(stderr, func(line string) {
			if stderrBuf.Len() > 0 {
				stderrBuf.WriteByte('\n')
			}
			stderrBuf.WriteString(line)
		})
	})

	waitErr := runCfg.cmd.Wait()
	stdoutWG.Wait()
	stderrWG.Wait()

	if runCfg.finalOutputPath != "" {
		defer os.Remove(runCfg.finalOutputPath)
	}

	output := finalizeExecOutput(runCfg.parser, runCfg.finalOutputPath, stderrBuf.String(), waitErr)
	if err := os.WriteFile(outputPath, []byte(output), 0o644); err != nil {
		d.logger.Warn("write exec output failed", "session_id", sessionID, "error", err)
	}

	d.mu.Lock()
	delete(d.processes, sessionID)
	if cancel, ok := d.monitors[sessionID]; ok {
		cancel()
		delete(d.monitors, sessionID)
	}
	d.mu.Unlock()

	snap := sess.Snapshot()
	sess.SetPID(0)
	sess.SetCurrentCommand("")
	if snap.State == session.StateExited {
		return
	}

	if waitErr != nil {
		sess.SetState(session.StateIdle)
		d.persistSessions()
		d.emitStateChanged(sessionID, session.StateIdle, "prompt failed")
		d.emitEvent(Event{
			SessionID: sessionID,
			Type:      "prompt_failed",
			Message:   waitErr.Error(),
			Timestamp: time.Now(),
		})
		return
	}

	sess.SetState(session.StateIdle)
	d.persistSessions()
	d.emitStateChanged(sessionID, session.StateIdle, "prompt completed")
	d.emitEvent(Event{
		SessionID: sessionID,
		Type:      "prompt_completed",
		Timestamp: time.Now(),
	})

	select {
	case <-runCtx.Done():
	default:
	}
}

func (d *Daemon) buildExecRunConfig(sess *session.Session, prof profile.Profile, prompt string) (*execRunConfig, error) {
	switch prof.ExecDriver {
	case profile.ExecDriverCodexExecJSON:
		return d.buildCodexExecRun(sess, prompt)
	case profile.ExecDriverClaudePrintStreamJSON:
		return d.buildClaudeExecRun(sess, prompt)
	default:
		return nil, fmt.Errorf("unsupported exec driver: %s", prof.ExecDriver)
	}
}

func (d *Daemon) buildCodexExecRun(sess *session.Session, prompt string) (*execRunConfig, error) {
	snap := sess.Snapshot()
	lastOutputPath := filepath.Join(os.TempDir(), fmt.Sprintf("arcmux-codex-last-%s-%d.txt", snap.ID, time.Now().UnixNano()))
	args := []string{"exec"}
	if snap.BackendSessionID != "" {
		args = append(args, "resume", "--dangerously-bypass-approvals-and-sandbox", "--json", "-o", lastOutputPath, snap.BackendSessionID, "-")
	} else {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox", "--json", "-o", lastOutputPath, "-")
	}

	cmd, err := commandWithSnapshotEnv("codex", args, nil, snap)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(prompt)

	return &execRunConfig{
		cmd:             cmd,
		parser:          &codexExecParser{},
		finalOutputPath: lastOutputPath,
	}, nil
}

func (d *Daemon) buildClaudeExecRun(sess *session.Session, prompt string) (*execRunConfig, error) {
	snap := sess.Snapshot()
	backendSessionID := snap.BackendSessionID
	if backendSessionID == "" {
		backendSessionID = generateUUID()
		sess.SetBackendSessionID(backendSessionID)
	}

	args := []string{
		"-p",
		"--dangerously-skip-permissions",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
	}
	if snap.BackendSessionID == "" {
		args = append(args, "--session-id", backendSessionID)
	} else {
		args = append(args, "--resume", backendSessionID)
	}
	args = append(args, prompt)

	cmd, err := commandWithSnapshotEnv("claude", args, []string{"ANTHROPIC_API_KEY", "CLAUDECODE"}, snap)
	if err != nil {
		return nil, err
	}

	return &execRunConfig{
		cmd:    cmd,
		parser: &claudePrintParser{},
	}, nil
}

func (d *Daemon) waitForSessionIdle(ctx context.Context, sessionID string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		sess, ok := d.GetSession(sessionID)
		if !ok {
			return fmt.Errorf("session not found: %s", sessionID)
		}
		snap := sess.Snapshot()
		if snap.State != session.StateWorking && snap.State != session.StateHandshaking && snap.State != session.StateStarting {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *Daemon) killExecSession(ctx context.Context, sess *session.Session, graceful bool, timeout time.Duration) error {
	snap := sess.Snapshot()

	d.mu.RLock()
	proc := d.processes[snap.ID]
	cancel := d.monitors[snap.ID]
	d.mu.RUnlock()

	if proc != nil {
		if graceful {
			_ = proc.Signal(os.Interrupt)
			waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
			defer waitCancel()
			if err := d.waitForExecProcessExit(waitCtx, snap.ID); err != nil {
				_ = proc.Kill()
			}
		} else {
			_ = proc.Kill()
		}
	}

	if cancel != nil {
		cancel()
	}

	d.mu.Lock()
	delete(d.processes, snap.ID)
	delete(d.monitors, snap.ID)
	d.mu.Unlock()

	sess.SetPID(0)
	sess.SetCurrentCommand("")
	sess.SetState(session.StateExited)
	d.persistSessions()
	d.emitStateChanged(snap.ID, session.StateExited, "killed")
	return nil
}

func (d *Daemon) waitForExecProcessExit(ctx context.Context, sessionID string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		d.mu.RLock()
		_, running := d.processes[sessionID]
		d.mu.RUnlock()
		if !running {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func mergedEnv(unset []string, snap session.Snapshot) []string {
	base := os.Environ()
	if len(unset) > 0 {
		filtered := base[:0]
		for _, env := range base {
			keep := true
			for _, key := range unset {
				if strings.HasPrefix(env, key+"=") {
					keep = false
					break
				}
			}
			if keep {
				filtered = append(filtered, env)
			}
		}
		base = filtered
	}

	if len(snap.Env) == 0 {
		return base
	}
	envMap := make(map[string]string, len(base)+len(snap.Env))
	for _, env := range base {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	for k, v := range snap.Env {
		envMap[k] = v
	}

	merged := make([]string, 0, len(envMap))
	for k, v := range envMap {
		merged = append(merged, k+"="+v)
	}
	return merged
}

func commandWithSnapshotEnv(name string, args, unset []string, snap session.Snapshot) (*exec.Cmd, error) {
	env := mergedEnv(unset, snap)
	path, err := resolveExecutable(name, env)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = snap.CWD
	cmd.Env = env
	return cmd, nil
}

func resolveExecutable(name string, env []string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		return name, nil
	}

	pathValue := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			pathValue = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	if pathValue == "" {
		return exec.LookPath(name)
	}

	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func finalizeExecOutput(parser execOutputParser, finalOutputPath, stderrText string, waitErr error) string {
	if finalOutputPath != "" {
		if data, err := os.ReadFile(finalOutputPath); err == nil && len(bytes.TrimSpace(data)) > 0 {
			return strings.TrimSpace(string(data))
		}
	}

	output := strings.TrimSpace(parser.FinalOutput())
	if output != "" {
		return output
	}

	stderrText = strings.TrimSpace(stderrText)
	if stderrText != "" {
		if waitErr != nil {
			return strings.TrimSpace(stderrText + "\n" + waitErr.Error())
		}
		return stderrText
	}
	if waitErr != nil {
		return waitErr.Error()
	}
	return ""
}

func scanLines(r io.Reader, handle func(line string)) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		handle(scanner.Text())
	}
}

type doneWaitGroup struct {
	done chan struct{}
}

func (g *doneWaitGroup) Go(fn func()) {
	g.done = make(chan struct{})
	go func() {
		defer close(g.done)
		fn()
	}()
}

func (g *doneWaitGroup) Wait() {
	if g.done == nil {
		return
	}
	<-g.done
}

type codexExecParser struct {
	lastOutput string
	raw        []string
}

func (p *codexExecParser) HandleStdoutLine(sess *session.Session, line string) {
	p.raw = append(p.raw, line)
	var event struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return
	}
	if event.Type == "thread.started" && event.ThreadID != "" {
		sess.SetBackendSessionID(event.ThreadID)
	}
	if event.Type == "item.completed" && event.Item.Type == "agent_message" && strings.TrimSpace(event.Item.Text) != "" {
		p.lastOutput = strings.TrimSpace(event.Item.Text)
	}
	if event.Type == "result" && strings.TrimSpace(p.lastOutput) == "" && strings.TrimSpace(event.Result) != "" {
		p.lastOutput = strings.TrimSpace(event.Result)
	}
}

func (p *codexExecParser) FinalOutput() string {
	if strings.TrimSpace(p.lastOutput) != "" {
		return p.lastOutput
	}
	return strings.Join(p.raw, "\n")
}

type claudePrintParser struct {
	lastOutput string
	resultText string
	raw        []string
}

func (p *claudePrintParser) HandleStdoutLine(sess *session.Session, line string) {
	p.raw = append(p.raw, line)
	var event struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		Result    string `json:"result"`
		Message   struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return
	}
	if event.SessionID != "" {
		sess.SetBackendSessionID(event.SessionID)
	}

	if event.Type == "assistant" {
		parts := make([]string, 0, len(event.Message.Content))
		for _, item := range event.Message.Content {
			if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
				parts = append(parts, item.Text)
			}
		}
		if len(parts) > 0 {
			p.lastOutput = strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	if event.Type == "result" && strings.TrimSpace(event.Result) != "" {
		p.resultText = strings.TrimSpace(event.Result)
		if strings.TrimSpace(p.lastOutput) == "" {
			p.lastOutput = p.resultText
		}
	}
}

func (p *claudePrintParser) FinalOutput() string {
	if strings.TrimSpace(p.lastOutput) != "" {
		return p.lastOutput
	}
	if strings.TrimSpace(p.resultText) != "" {
		return p.resultText
	}
	return strings.Join(p.raw, "\n")
}

func generateUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1_000_000_000_000)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexed[0:8],
		hexed[8:12],
		hexed[12:16],
		hexed[16:20],
		hexed[20:32],
	)
}
