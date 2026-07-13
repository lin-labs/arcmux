package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
)

// On-demand session summarization: the same evolving overall_goal + FEATURE
// pass the unified hook fires at turn_end, but triggered by a subscriber
// (voxtop babysit) when it finds a session with no recorded briefing — e.g. a
// session whose vault log never matched, or one that predates the hook. The
// conversation source here is the pane scrollback (capture-pane history), so
// it works even when no vault history file exists. The result lands in the
// conventional place: the session state doc, via the single writer
// (ApplyContractOnly), exactly as if the hook summarizer had run.

const summarizePromptHeader = "You maintain a running OVERALL GOAL for an agent work session: what the user is ultimately trying to achieve across the whole conversation.\nIt evolves with the conversation.\n\n"

const summarizePromptFooter = "Return the UPDATED overall goal. Normally ONE succinct line. If the conversation has clearly shifted into multiple separate themes, return a short markdown checklist instead: completed or abandoned earlier goals as '- [x] ...' and the active one(s) as '- [ ] ...'. Output ONLY the goal text — no preamble, no quotes, no explanation.\nThen, on the very LAST line, output exactly: FEATURE: <a short kebab-case slug (2-4 words) naming the feature or workstream this session is about>."

// SummarizeSessionByID captures the session's pane scrollback, asks the goal
// model (ARCMUX_GOAL_BIN, default grok) for the updated overall goal + feature
// slug, and records them on the state doc. Debounced: one in-flight pass per
// session; a concurrent call returns ErrSummarizeInFlight.
func (d *Daemon) SummarizeSessionByID(ctx context.Context, sessionID string) (string, error) {
	if _, loaded := d.summarizing.LoadOrStore(sessionID, true); loaded {
		return "", ErrSummarizeInFlight
	}
	defer d.summarizing.Delete(sessionID)

	d.mu.RLock()
	sess := d.sessions[sessionID]
	d.mu.RUnlock()
	if sess == nil {
		return "", fmt.Errorf("unknown session %q", sessionID)
	}
	snap := sess.Snapshot()
	if snap.TmuxTarget == "" {
		return "", fmt.Errorf("session %s has no tmux pane to summarize", sessionID)
	}

	convo, err := d.tmux.CapturePaneHistory(ctx, snap.TmuxTarget)
	if err != nil {
		return "", fmt.Errorf("capture pane history: %w", err)
	}
	convo = strings.TrimSpace(convo)
	if convo == "" {
		return "", fmt.Errorf("pane %s has no content to summarize", snap.TmuxTarget)
	}
	if len(convo) > 12000 {
		convo = convo[len(convo)-12000:]
	}

	current := ""
	if st, err := hooks.ReadSessionState(d.cfg.Hooks.SessionStateDir, sessionID); err == nil && st != nil && st.TurnContract != nil {
		current = st.TurnContract.OverallGoal
	}
	if current == "" {
		current = "(none yet)"
	}

	prompt := summarizePromptHeader +
		"Current overall goal:\n" + current + "\n\n" +
		"Conversation so far (newest last):\n" + convo + "\n\n" +
		summarizePromptFooter

	out, err := d.runGoalModel(ctx, prompt)
	if err != nil {
		return "", err
	}

	goal, feature := splitGoalAndFeature(out)
	if goal == "" && feature == "" {
		return "", fmt.Errorf("goal model returned no usable output")
	}
	update := hooks.TurnContractUpdate{OverallGoal: goal, Feature: feature, Source: "summarize"}
	if err := hooks.ApplyContractOnly(d.cfg.Hooks.SessionStateDir, sessionID, snap.Agent, update, time.Now()); err != nil {
		return "", err
	}
	d.logger.Info("session.summarized", "session_id", sessionID, "feature", feature)
	return goal, nil
}

// ErrSummarizeInFlight signals a debounced duplicate summarize request.
var ErrSummarizeInFlight = fmt.Errorf("summarize already in flight for this session")

// runGoalModel invokes the goal summarizer CLI (same contract as the hook
// script): stdin detached, bounded by ctx, prompt via temp file.
func (d *Daemon) runGoalModel(ctx context.Context, prompt string) (string, error) {
	bin := os.Getenv("ARCMUX_GOAL_BIN")
	if bin == "" {
		bin = "grok"
	}
	model := os.Getenv("ARCMUX_GOAL_MODEL")
	if model == "" {
		model = "grok-4.3"
	}
	pf, err := os.CreateTemp("", "arcmux-goal-*.txt")
	if err != nil {
		return "", err
	}
	defer os.Remove(pf.Name())
	if _, err := pf.WriteString(prompt); err != nil {
		pf.Close()
		return "", err
	}
	pf.Close()

	cmd := exec.CommandContext(ctx, bin,
		"--no-alt-screen", "--disable-web-search", "-m", model, "--prompt-file", pf.Name())
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("goal model (%s): %w", bin, err)
	}
	return string(out), nil
}

// splitGoalAndFeature separates the trailing "FEATURE: <slug>" line from the
// goal text, mirroring the hook script's parsing.
func splitGoalAndFeature(raw string) (goal, feature string) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "FEATURE:"); ok {
			feature = strings.TrimSpace(after)
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), feature
}
