package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/handoff"
)

const maxHandoffGoalFile = 16 << 10

type handoffPrepareInput struct {
	ProfileScope    string                  `json:"profile_scope"`
	SessionID       string                  `json:"session_id"`
	TargetPeer      string                  `json:"target_peer"`
	TargetAgent     string                  `json:"target_agent"`
	Project         string                  `json:"project"`
	Goal            string                  `json:"goal"`
	History         string                  `json:"history,omitempty"`
	ConversationID  string                  `json:"conversation_id,omitempty"`
	ParentHandoffID string                  `json:"parent_handoff_id,omitempty"`
	Validation      handoff.ValidationState `json:"validation,omitempty"`
}

type handoffSourceStatus struct {
	HandoffID string              `json:"handoff_id"`
	State     handoff.SourceState `json:"state"`
}

func cmdHandoff(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: arcmux handoff prepare|launch|receive|list|show|retry")
	}
	switch args[0] {
	case "prepare":
		return cmdHandoffPrepare(args[1:], stdin, stdout)
	case "list":
		return cmdHandoffList(args[1:], stdout)
	case "show":
		return cmdHandoffShow(args[1:], stdout)
	case "retry":
		return cmdHandoffRetry(args[1:], stdout)
	case "launch":
		return cmdHandoffLaunch(args[1:], stdout)
	case "receive":
		return cmdHandoffReceive(args[1:], stdout)
	default:
		return fmt.Errorf("unknown handoff subcommand %q", args[0])
	}
}

func cmdHandoffReceive(args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		return errors.New("usage: arcmux handoff receive <marker>")
	}
	instructions, err := handoff.ReceiveLaunchInstructions(handoff.DefaultLaunchRendezvousRoot(), args[0])
	if err != nil {
		return errors.New("handoff instructions are unavailable")
	}
	_, err = stdout.Write(instructions)
	return err
}

func cmdHandoffLaunch(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: arcmux handoff launch <handoff-id> [--wait duration] [--config path]")
	}
	id := rest[0]
	fs := flag.NewFlagSet("handoff launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wait := fs.Duration("wait", 0, "wait for target acceptance")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || id == "" {
		return errors.New("usage: arcmux handoff launch <handoff-id> [--wait duration] [--config path]")
	}
	if *wait < 0 {
		return errors.New("--wait must not be negative")
	}
	response, err := meshAPI(cfg, http.MethodPost, "/mesh/handoffs/launch?id="+url.QueryEscape(id))
	if err != nil {
		return err
	}
	if *wait == 0 {
		_, err = stdout.Write(response)
		return err
	}
	return waitForHandoffLaunch(cfg, response, *wait, stdout)
}

func cmdHandoffPrepare(args []string, stdin io.Reader, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 3 {
		return handoffPrepareUsage()
	}
	peer, scope, sessionID := rest[0], rest[1], rest[2]
	fs := flag.NewFlagSet("handoff prepare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "registered project slug")
	agent := fs.String("agent", "", "target agent profile")
	goalFile := fs.String("goal-file", "", "goal file path or - for stdin")
	history := fs.String("history", "", "synced history basename")
	conversation := fs.String("conversation", "", "conversation id")
	parent := fs.String("parent", "", "parent handoff id")
	validation := fs.String("validation", string(handoff.ValidationNotRun), "not_run, passed, or failed")
	wait := fs.Duration("wait", 0, "wait for remote preparation")
	if err := fs.Parse(rest[3:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *project == "" || *agent == "" || *goalFile == "" {
		return handoffPrepareUsage()
	}
	if *wait < 0 {
		return errors.New("--wait must not be negative")
	}
	state := handoff.ValidationState(*validation)
	if state != handoff.ValidationNotRun && state != handoff.ValidationPassed && state != handoff.ValidationFailed {
		return errors.New("--validation must be not_run, passed, or failed")
	}
	goal, err := readHandoffGoal(*goalFile, stdin)
	if err != nil {
		return err
	}
	request := handoffPrepareInput{
		ProfileScope: scope, SessionID: sessionID, TargetPeer: peer, TargetAgent: *agent,
		Project: *project, Goal: goal, History: *history, ConversationID: *conversation,
		ParentHandoffID: *parent, Validation: state,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	response, err := meshAPIBody(cfg, http.MethodPost, "/mesh/handoffs", body)
	if err != nil {
		return err
	}
	if *wait == 0 {
		_, err = stdout.Write(response)
		return err
	}
	return waitForHandoff(cfg, response, *wait, stdout)
}

func cmdHandoffList(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return errors.New("usage: arcmux handoff list [--config path]")
	}
	return writeMeshJSON(cfg, http.MethodGet, "/mesh/handoffs", nil, stdout)
}

func cmdHandoffShow(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: arcmux handoff show <handoff-id> [--config path]")
	}
	return writeMeshJSON(cfg, http.MethodGet, "/mesh/handoffs?id="+url.QueryEscape(rest[0]), nil, stdout)
}

func cmdHandoffRetry(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: arcmux handoff retry <handoff-id> [--config path]")
	}
	return writeMeshJSON(cfg, http.MethodPost, "/mesh/handoffs/retry?id="+url.QueryEscape(rest[0]), nil, stdout)
}

func handoffPrepareUsage() error {
	return errors.New("usage: arcmux handoff prepare <peer> <root|profile:name> <session-id> --project P --agent A --goal-file <path|-> [--history basename] [--conversation ID] [--parent ID] [--validation not_run|passed|failed] [--wait duration]")
}

func readHandoffGoal(path string, stdin io.Reader) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, maxHandoffGoalFile+1))
	} else {
		data, err = readHandoffGoalFile(path, nil)
	}
	if err != nil {
		return "", fmt.Errorf("read --goal-file: %w", err)
	}
	if len(data) > maxHandoffGoalFile {
		return "", fmt.Errorf("--goal-file exceeds %d bytes", maxHandoffGoalFile)
	}
	goal := strings.TrimSpace(string(data))
	if goal == "" {
		return "", errors.New("--goal-file is empty")
	}
	return goal, nil
}

// readHandoffGoalFile opens exactly one no-follow descriptor, validates that
// descriptor with fstat, and reads only from it. A path replacement after the
// open therefore cannot substitute credentials or other unintended content.
func readHandoffGoalFile(path string, afterOpen func()) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("--goal-file must be an accessible regular non-symlink file")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open --goal-file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("--goal-file must be a regular non-symlink file")
	}
	if info.Size() > maxHandoffGoalFile {
		return nil, fmt.Errorf("--goal-file exceeds %d bytes", maxHandoffGoalFile)
	}
	if afterOpen != nil {
		afterOpen()
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHandoffGoalFile+1))
	if err != nil {
		return nil, errors.New("read --goal-file")
	}
	after, statErr := file.Stat()
	if statErr != nil || int64(len(data)) != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("--goal-file changed while reading")
	}
	return data, nil
}

func waitForHandoff(cfg *config.Config, initial []byte, wait time.Duration, stdout io.Writer) error {
	var status handoffSourceStatus
	if err := json.Unmarshal(initial, &status); err != nil {
		return fmt.Errorf("decode handoff status: %w", err)
	}
	if status.HandoffID == "" {
		return errors.New("daemon returned an empty handoff id")
	}
	if handoffPrepareTerminal(status.State) {
		return writeHandoffTerminal(status, initial, stdout)
	}
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_, _ = stdout.Write(initial)
			return fmt.Errorf("timed out waiting for handoff %s to prepare", status.HandoffID)
		}
		delay := 200 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		time.Sleep(delay)
		response, err := meshAPI(cfg, http.MethodGet, "/mesh/handoffs?id="+url.QueryEscape(status.HandoffID))
		if err != nil {
			return err
		}
		initial = response
		if err := json.Unmarshal(response, &status); err != nil {
			return fmt.Errorf("decode handoff status: %w", err)
		}
		if handoffPrepareTerminal(status.State) {
			return writeHandoffTerminal(status, response, stdout)
		}
	}
}

func writeHandoffTerminal(status handoffSourceStatus, response []byte, stdout io.Writer) error {
	if _, err := stdout.Write(response); err != nil {
		return err
	}
	if status.State == handoff.SourceFailed {
		return fmt.Errorf("handoff %s preparation failed", status.HandoffID)
	}
	return nil
}

func handoffPrepareTerminal(state handoff.SourceState) bool {
	return state == handoff.SourceRemotePrepared || state == handoff.SourceFailed
}

func waitForHandoffLaunch(cfg *config.Config, initial []byte, wait time.Duration, stdout io.Writer) error {
	var status handoffSourceStatus
	if err := json.Unmarshal(initial, &status); err != nil {
		return fmt.Errorf("decode handoff status: %w", err)
	}
	if status.HandoffID == "" {
		return errors.New("daemon returned an empty handoff id")
	}
	if handoffLaunchTerminal(status.State) {
		return writeHandoffLaunchTerminal(status, initial, stdout)
	}
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_, _ = stdout.Write(initial)
			return fmt.Errorf("timed out waiting for handoff %s to launch", status.HandoffID)
		}
		delay := 200 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		time.Sleep(delay)
		response, err := meshAPI(cfg, http.MethodGet, "/mesh/handoffs?id="+url.QueryEscape(status.HandoffID))
		if err != nil {
			return err
		}
		initial = response
		if err := json.Unmarshal(response, &status); err != nil {
			return fmt.Errorf("decode handoff status: %w", err)
		}
		if handoffLaunchTerminal(status.State) {
			return writeHandoffLaunchTerminal(status, response, stdout)
		}
	}
}

func handoffLaunchTerminal(state handoff.SourceState) bool {
	return state == handoff.SourceAccepted || state == handoff.SourceFailed
}

func writeHandoffLaunchTerminal(status handoffSourceStatus, response []byte, stdout io.Writer) error {
	if _, err := stdout.Write(response); err != nil {
		return err
	}
	if status.State == handoff.SourceFailed {
		return fmt.Errorf("handoff %s launch failed", status.HandoffID)
	}
	return nil
}
