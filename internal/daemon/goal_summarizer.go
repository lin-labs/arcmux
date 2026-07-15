package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"golang.org/x/sys/unix"
)

const (
	goalSummaryPollInterval = 2 * time.Second
	goalSummaryRetryAfter   = time.Minute
	goalConversationBytes   = 12_000
)

type goalSummaryAttempt struct {
	key string
	at  time.Time
}

type goalSummaryCandidate struct {
	sessionID string
	agent     string
	turnCount int
	turnEnd   time.Time
	current   string
	history   string
}

// runOverallGoalSummarizer is daemon-owned: pane hooks only record turn state.
// No pane-callable CLI accepts summary text or can stamp trusted provenance.
func (d *Daemon) runOverallGoalSummarizer(ctx context.Context) {
	ticker := time.NewTicker(goalSummaryPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.queueOverallGoalSummaries(ctx)
		}
	}
}

func (d *Daemon) queueOverallGoalSummaries(ctx context.Context) {
	now := time.Now()
	for _, managed := range d.ListSessions() {
		snap := managed.Snapshot()
		if snap.Private {
			continue
		}
		candidate, ok := d.goalSummaryCandidate(snap.ID, snap.Agent, snap.CWD)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%d/%s", candidate.turnCount, candidate.turnEnd.UTC().Format(time.RFC3339Nano))
		d.goalSummaryMu.Lock()
		attempt := d.goalSummaryAttempts[snap.ID]
		if attempt.key == key && now.Sub(attempt.at) < goalSummaryRetryAfter {
			d.goalSummaryMu.Unlock()
			continue
		}
		d.goalSummaryAttempts[snap.ID] = goalSummaryAttempt{key: key, at: now}
		d.goalSummaryMu.Unlock()

		go func(candidate goalSummaryCandidate) {
			if err := d.refreshOverallGoal(ctx, candidate); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, hooks.ErrStaleOverallGoal) {
				d.logger.Debug("overall-goal refresh skipped", "session_id", candidate.sessionID, "error", err)
			}
		}(candidate)
	}
}

func (d *Daemon) goalSummaryCandidate(sessionID, agent, cwd string) (goalSummaryCandidate, bool) {
	state, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, sessionID)
	if err != nil || state == nil || state.TurnCount < 1 || state.LastTurnEndAt.IsZero() {
		return goalSummaryCandidate{}, false
	}
	if tc := state.TurnContract; tc != nil &&
		tc.OverallGoalProvenance == hooks.OverallGoalSummarizerProvenance &&
		!tc.OverallGoalUpdatedAt.Before(state.LastTurnEndAt) {
		return goalSummaryCandidate{}, false
	}
	history, err := findSessionHistory(d.overallGoalHistoryRoot(), cwd)
	if err != nil || history == "" {
		return goalSummaryCandidate{}, false
	}
	current := ""
	if state.TurnContract != nil {
		current = state.TurnContract.OverallGoal
	}
	return goalSummaryCandidate{
		sessionID: sessionID, agent: agent, turnCount: state.TurnCount,
		turnEnd: state.LastTurnEndAt, current: current, history: history,
	}, true
}

// refreshOverallGoalOnce is the synchronous owner path used by tests and by
// future owner-local triggers. It never accepts caller-provided summary text.
func (d *Daemon) refreshOverallGoalOnce(ctx context.Context, sessionID string) error {
	managed, ok := d.GetSession(sessionID)
	if !ok {
		return errors.New("session not found")
	}
	snap := managed.Snapshot()
	if snap.Private {
		return errors.New("private session summary is disabled")
	}
	candidate, ok := d.goalSummaryCandidate(snap.ID, snap.Agent, snap.CWD)
	if !ok {
		return errors.New("session has no pending summarizable turn")
	}
	return d.refreshOverallGoal(ctx, candidate)
}

func (d *Daemon) refreshOverallGoal(ctx context.Context, candidate goalSummaryCandidate) error {
	conversation, err := readFileTail(candidate.history, goalConversationBytes)
	if err != nil || strings.TrimSpace(conversation) == "" {
		return errors.New("conversation history is unavailable")
	}
	runner := d.goalSummaryRunner
	if runner == nil {
		runner = runOverallGoalModel
	}
	updated, err := runner(ctx, candidate.current, conversation)
	if err != nil {
		return err
	}
	updated = strings.TrimSpace(updated)
	if updated == "" {
		return errors.New("overall-goal summarizer returned empty output")
	}
	return hooks.ApplySummarizedOverallGoal(
		d.cfg.Hooks.SessionStateDir, candidate.sessionID, candidate.agent, updated,
		candidate.turnCount, candidate.turnEnd, time.Now(),
	)
}

func (d *Daemon) overallGoalHistoryRoot() string {
	if d.goalHistoryRoot != "" {
		return d.goalHistoryRoot
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "agents", "histories")
}

func findSessionHistory(root, cwd string) (string, error) {
	if root == "" || cwd == "" {
		return "", nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	if strings.TrimSpace(host) == "" {
		return "", errors.New("local hostname is unavailable")
	}
	var best string
	var bestTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		head, _ := io.ReadAll(io.LimitReader(file, 1500))
		_ = file.Close()
		fmCWD, fmHost := historyFrontmatter(head)
		if fmCWD != cwd || fmHost != host {
			continue
		}
		info, err := entry.Info()
		if err == nil && (best == "" || info.ModTime().After(bestTime)) {
			best, bestTime = path, info.ModTime()
		}
	}
	return best, nil
}

func historyFrontmatter(data []byte) (cwd, host string) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "cwd":
			cwd = value
		case "host":
			host = value
		}
	}
	return cwd, host
}

func readFileTail(path string, limit int64) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	rootFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat(rootFD, filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	if file == nil {
		_ = unix.Close(fd)
		return "", errors.New("open conversation history")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return "", errors.New("conversation history is not a regular file")
	}
	defer file.Close()
	if offset := info.Size() - limit; offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	return string(data), err
}

func runOverallGoalModel(ctx context.Context, current, conversation string) (string, error) {
	bin := strings.TrimSpace(os.Getenv("ARCMUX_GOAL_BIN"))
	if bin == "" {
		bin = "grok"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "", errors.New("overall-goal summarizer is unavailable")
	}
	timeout := 90 * time.Second
	if raw := strings.TrimSpace(os.Getenv("ARCMUX_GOAL_TIMEOUT")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			timeout = parsed
		} else if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}
	model := strings.TrimSpace(os.Getenv("ARCMUX_GOAL_MODEL"))
	if model == "" {
		model = "grok-4.3"
	}
	if current == "" {
		current = "(none yet)"
	}
	prompt := "You maintain a running OVERALL GOAL for an agent work session: what the user is ultimately trying to achieve across the whole conversation.\n" +
		"It evolves with the conversation.\n\nCurrent overall goal:\n" + current +
		"\n\nConversation so far (newest last):\n" + conversation +
		"\n\nReturn the UPDATED overall goal. Normally ONE succinct line. If the conversation has clearly shifted into multiple separate themes, return a short markdown checklist instead: completed or abandoned earlier goals as '- [x] ...' and the active one(s) as '- [ ] ...'. Output ONLY the goal text — no preamble, no quotes, no explanation.\n"
	file, err := os.CreateTemp("", "arcmux-overall-goal-*.md")
	if err != nil {
		return "", errors.New("create summarizer prompt")
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", errors.New("protect summarizer prompt")
	}
	if _, err := file.WriteString(prompt); err != nil {
		_ = file.Close()
		return "", errors.New("write summarizer prompt")
	}
	if err := file.Close(); err != nil {
		return "", errors.New("close summarizer prompt")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "--no-alt-screen", "--disable-web-search", "-m", model, "--prompt-file", path)
	cmd.Stdin = strings.NewReader("")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("overall-goal summarizer failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}
