// Package scenarios holds concrete eval scenario implementations.
package scenarios

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lin-labs/arcmux/internal/eval"
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

func (h HelloServer) Run(ctx context.Context, env *eval.Env, log io.Writer) (*eval.Outcome, error) {
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

	agentWall, err := eval.DispatchDirect(ctx, env, mission, cap)
	if err != nil {
		// Agent failed (timeout, non-zero exit). Still try to validate —
		// some scenarios may produce partially-correct artifacts even on
		// agent error, and the validate output is the honest verdict.
		env.Tracef("agent dispatch returned error: %v (validating anyway)", err)
	}

	validateOut, vErr := eval.RunValidateScript(ctx, env, "validate.sh")
	if vErr != nil {
		return &eval.Outcome{
			Status:         "fail",
			Mode:           "direct",
			AgentWallTime:  agentWall,
			Detail:         fmt.Sprintf("validate.sh failed: %v", vErr),
			ValidateOutput: validateOut,
		}, nil
	}

	return &eval.Outcome{
		Status:         "pass",
		Mode:           "direct",
		AgentWallTime:  agentWall,
		ValidateOutput: validateOut,
	}, nil
}
