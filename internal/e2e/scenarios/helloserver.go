// Package scenarios holds concrete agent-behavioral e2e scenario
// implementations.
package scenarios

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lin-labs/arcmux/internal/e2e"
)

// HelloServer is the floor scenario: a direct-dispatch agent task that
// asks claude to build a tiny Go HTTP server in a fresh workrepo. The
// validate.sh script asserts static layout, runs `make test`, then spins
// the server up out-of-band and curls it.
//
// Mode: direct (one claude -p invocation, no arcmux team chain).
// Agent wall-cap: 5 min. Total scenario cap inherits the Runner default.
type HelloServer struct {
	// AgentWallCap caps how long the claude -p invocation may run. Zero
	// uses the default (5 minutes).
	AgentWallCap time.Duration
}

func (HelloServer) Name() string { return "hello-server" }

func (h HelloServer) Run(ctx context.Context, env *e2e.Env, log io.Writer) (*e2e.Outcome, error) {
	missionBytes, err := env.ReadScenarioFile("prompt.md")
	if err != nil {
		return nil, fmt.Errorf("read prompt.md: %w", err)
	}
	mission := string(missionBytes)
	env.Tracef("loaded mission: %d bytes", len(missionBytes))

	cap := h.AgentWallCap
	if cap <= 0 {
		cap = 5 * time.Minute
	}

	mode := env.Mode
	if mode == "" {
		mode = "elonco"
	}

	var (
		agentWall time.Duration
		err2      error
	)
	switch mode {
	case "direct":
		agentWall, err2 = e2e.DispatchDirect(ctx, env, mission, cap)
	case "elonco":
		// In elonco mode the agent runs inside a cmux pane and we poll
		// arcmux for "stable idle" as the completion signal. Stable
		// window 6s — long enough to not catch a pre-handshake transient,
		// short enough to keep the scenario fast once claude exits.
		agentWall, err2 = e2e.DispatchElonko(ctx, env, mission, cap, 6*time.Second)
	default:
		return nil, fmt.Errorf("unknown e2e mode %q (expected 'direct' or 'elonco')", mode)
	}
	if err2 != nil {
		// Agent failed (timeout, non-zero exit, spawn error). Still try
		// to validate — some scenarios may produce partially-correct
		// artifacts even on agent error, and the validate output is the
		// honest verdict.
		env.Tracef("agent dispatch returned error: %v (validating anyway)", err2)
	}

	validateOut, vErr := e2e.RunValidateScript(ctx, env, "validate.sh")
	if vErr != nil {
		return &e2e.Outcome{
			Status:         "fail",
			Mode:           mode,
			AgentWallTime:  agentWall,
			Detail:         fmt.Sprintf("validate.sh failed: %v", vErr),
			ValidateOutput: validateOut,
		}, nil
	}

	return &e2e.Outcome{
		Status:         "pass",
		Mode:           mode,
		AgentWallTime:  agentWall,
		ValidateOutput: validateOut,
	}, nil
}
