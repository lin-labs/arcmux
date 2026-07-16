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

const (
	maxHandoffGoalFile        = 16 << 10
	handoffPostRequestTimeout = 90 * time.Second
)

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
	HandoffID       string              `json:"handoff_id"`
	State           handoff.SourceState `json:"state"`
	ContextLoaded   bool                `json:"context_loaded"`
	RetirementState string              `json:"retirement_state"`
}

func cmdHandoff(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: arcmux handoff prepare|launch|receive|acknowledge|verify|retire|list|show|retry")
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
	case "acknowledge":
		return cmdHandoffAcknowledge(args[1:], stdout)
	case "verify":
		return cmdHandoffVerify(args[1:], stdout)
	case "retire":
		return cmdHandoffRetire(args[1:], stdout)
	default:
		return fmt.Errorf("unknown handoff subcommand %q", args[0])
	}
}

func cmdHandoffVerify(args []string, stdout io.Writer) error {
	return cmdHandoffVerifyWithAPITimeout(args, stdout, handoffPostRequestTimeout)
}

func cmdHandoffVerifyWithAPITimeout(args []string, stdout io.Writer, apiTimeout time.Duration) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: arcmux handoff verify <handoff-id> [--wait duration] [--config path]")
	}
	id := rest[0]
	fs := flag.NewFlagSet("handoff verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	wait := fs.Duration("wait", 0, "wait for context-loaded acknowledgement")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if id == "" || fs.NArg() != 0 {
		return errors.New("usage: arcmux handoff verify <handoff-id> [--wait duration] [--config path]")
	}
	if *wait < 0 || *wait > apiTimeout {
		return fmt.Errorf("--wait must be between 0 and %s", apiTimeout)
	}
	deadline := time.Now().Add(*wait)
	for {
		response, err := meshAPIBodyWithTimeout(cfg, http.MethodPost, "/mesh/handoffs/verify?id="+url.QueryEscape(id), nil, apiTimeout)
		if err != nil {
			return err
		}
		var status handoffSourceStatus
		if err := json.Unmarshal(response, &status); err != nil {
			return fmt.Errorf("decode handoff verification: %w", err)
		}
		var envelope struct {
			VerificationState string `json:"verification_state"`
		}
		_ = json.Unmarshal(response, &envelope)
		if status.ContextLoaded && envelope.VerificationState == "context_loaded" {
			_, err = stdout.Write(response)
			return err
		}
		if *wait == 0 || envelope.VerificationState == "mismatch" || time.Now().After(deadline) {
			_, _ = stdout.Write(response)
			if envelope.VerificationState == "mismatch" {
				return fmt.Errorf("handoff %s verification identity mismatch", id)
			}
			if *wait > 0 {
				return fmt.Errorf("timed out waiting for handoff %s context-loaded acknowledgement", id)
			}
			return fmt.Errorf("handoff %s context is not loaded", id)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func cmdHandoffRetire(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: arcmux handoff retire <handoff-id> [--after-turn-end] [--timeout duration] [--config path]")
	}
	id := rest[0]
	fs := flag.NewFlagSet("handoff retire", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	afterTurnEnd := fs.Bool("after-turn-end", false, "retire after a newer durable source turn end")
	timeout := fs.Duration("timeout", 10*time.Second, "graceful source close timeout")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if id == "" || fs.NArg() != 0 {
		return errors.New("usage: arcmux handoff retire <handoff-id> [--after-turn-end] [--timeout duration] [--config path]")
	}
	if *timeout < time.Second || *timeout > 5*time.Minute || *timeout%time.Second != 0 {
		return errors.New("--timeout must be whole seconds between 1s and 5m")
	}
	body, err := json.Marshal(struct {
		HandoffID      string `json:"handoff_id"`
		AfterTurnEnd   bool   `json:"after_turn_end"`
		TimeoutSeconds int64  `json:"timeout_seconds"`
	}{HandoffID: id, AfterTurnEnd: *afterTurnEnd, TimeoutSeconds: int64(*timeout / time.Second)})
	if err != nil {
		return err
	}
	response, err := meshAPIBodyWithTimeout(cfg, http.MethodPost, "/mesh/handoffs/retire", body, handoffPostRequestTimeout)
	if err != nil {
		return err
	}
	var status handoffSourceStatus
	if err := json.Unmarshal(response, &status); err != nil {
		return fmt.Errorf("decode handoff retirement: %w", err)
	}
	if _, err := stdout.Write(response); err != nil {
		return err
	}
	if !*afterTurnEnd && status.RetirementState != "retired" {
		return fmt.Errorf("handoff %s source retirement remains pending", id)
	}
	return nil
}

func cmdHandoffAcknowledge(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return errors.New("usage: arcmux handoff acknowledge <marker> --phase context-loaded")
	}
	marker := args[0]
	fs := flag.NewFlagSet("handoff acknowledge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	phase := fs.String("phase", "", "acknowledgement phase")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if marker == "" || fs.NArg() != 0 || *phase != "context-loaded" {
		return errors.New("usage: arcmux handoff acknowledge <marker> --phase context-loaded")
	}
	_, replay, err := handoff.AcknowledgeLaunchContext(handoff.DefaultLaunchRendezvousRoot(), marker, handoff.ContextLoadedPhase)
	if err != nil {
		if errors.Is(err, handoff.ErrTargetNotAccepted) {
			return errors.New("handoff target is not accepted yet; validate context and retry acknowledgement")
		}
		return errors.New("handoff acknowledgement is unavailable")
	}
	return json.NewEncoder(stdout).Encode(struct {
		State         string `json:"state"`
		Phase         string `json:"phase"`
		ContextLoaded bool   `json:"context_loaded"`
		Replay        bool   `json:"replay"`
	}{State: "acknowledged", Phase: "context_loaded", ContextLoaded: true, Replay: replay})
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
	return cmdHandoffLaunchWithAPITimeout(args, stdout, handoffPostRequestTimeout)
}

func cmdHandoffLaunchWithAPITimeout(args []string, stdout io.Writer, apiTimeout time.Duration) error {
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
	response, err := meshAPIBodyWithTimeout(cfg, http.MethodPost, "/mesh/handoffs/launch?id="+url.QueryEscape(id), nil, apiTimeout)
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
	return cmdHandoffPrepareWithAPITimeout(args, stdin, stdout, handoffPostRequestTimeout)
}

func cmdHandoffPrepareWithAPITimeout(args []string, stdin io.Reader, stdout io.Writer, apiTimeout time.Duration) error {
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
	response, err := meshAPIBodyWithTimeout(cfg, http.MethodPost, "/mesh/handoffs", body, apiTimeout)
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
	return cmdHandoffRetryWithAPITimeout(args, stdout, handoffPostRequestTimeout)
}

func cmdHandoffRetryWithAPITimeout(args []string, stdout io.Writer, apiTimeout time.Duration) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: arcmux handoff retry <handoff-id> [--config path]")
	}
	response, err := meshAPIBodyWithTimeout(cfg, http.MethodPost, "/mesh/handoffs/retry?id="+url.QueryEscape(rest[0]), nil, apiTimeout)
	if err != nil {
		return err
	}
	_, err = stdout.Write(response)
	return err
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
	if strings.ContainsAny(goal, "\r\n") {
		return "", errors.New("--goal-file action goal must be a single line")
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
