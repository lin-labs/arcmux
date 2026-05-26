package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/scenariotest"
)

// GRPCRoundtrip proves that the post-F11 arcmux-cli subcommands route
// through the daemon's gRPC instead of opening bbolt directly. Before
// F11, arcmux-cli `audit`/`inbox` opened state.bolt itself; with a
// daemon running, the daemon holds the bbolt write lock for its
// uptime, so a sibling reader would block on flock. Routing through
// gRPC eliminates that race.
//
// SETUP: start an isolated daemon (its own socket, data_root, tmux).
//
// ACT (all against the live daemon, while it owns the bbolt lock):
//
//  1. `arcmux-cli audit recent --socket <sock> --n 5`
//     Fresh daemon, nothing queued yet -> {"entries":[]}, no error.
//     The pre-F11 CLI would have blocked here on flock.
//
//  2. `arcmux-cli inbox push --socket <sock> --session <name> --body hi --from me`
//     Body routed through Send. The named session was never created via
//     CreateSession, so the daemon's Send returns NotFound — we accept
//     that as proof the RPC was reached (the wire shape we care about).
//     "expect either delivered or queued (don't pin)" relaxes further to
//     "either delivered, queued, or NotFound" in a substrate-only run,
//     because creating a real tmux-backed session is out of scope for
//     this scenario (the pulse-wake scenario doesn't either).
//
//  3. `arcmux-cli inbox peek --socket <sock> --session <name> --n 5`
//     PeekInbox is tolerant of missing inbox buckets -> {"messages":[]}.
//
//  4. `arcmux-cli ready --socket <sock> --session <name>`
//     Ready handles unknown sessions explicitly:
//     {"ready": false, "reason": "no-such-session", ...}.
//
// ASSERT: each call returned the expected wire shape. The fact that any
// of these completed AT ALL while the daemon was up is the substrate
// guarantee F11 buys: bbolt is no longer a contended resource between
// the CLI and the daemon.
type GRPCRoundtrip struct{}

func (GRPCRoundtrip) Name() string { return "grpc-rt" }

func (GRPCRoundtrip) Run(ctx context.Context, env *scenariotest.Env, log io.Writer) error {
	if err := env.StartDaemon(ctx, 10*time.Second); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	defer env.StopDaemon(5 * time.Second)

	sessionName := "test-session"
	sock := env.SocketPath

	// ACT 1: audit recent against an empty store.
	{
		var resp struct {
			Entries []map[string]any `json:"entries"`
		}
		if err := env.RunCallJSON(ctx, &resp,
			"audit", "recent", "--socket", sock, "--n", "5",
		); err != nil {
			return fmt.Errorf("audit recent: %w", err)
		}
		if len(resp.Entries) != 0 {
			// Not fatal — a noisy daemon could plausibly have written
			// some audit rows during startup. Log but don't fail.
			fmt.Fprintf(log, "grpc-roundtrip: audit recent returned %d entries (expected 0, tolerated)\n", len(resp.Entries))
		}
	}

	// ACT 2: inbox push for a session that doesn't exist. Either we get
	// a delivered/queued ack (if the daemon grew tolerance later) or a
	// NotFound — both prove the gRPC path was reached.
	{
		out, err := env.RunCall(ctx,
			"inbox", "push",
			"--socket", sock,
			"--session", sessionName,
			"--from", "me",
			"--body", "hi",
		)
		switch {
		case err == nil:
			var ack struct {
				OK        bool   `json:"ok"`
				ID        string `json:"id"`
				Delivered bool   `json:"delivered"`
				Queued    bool   `json:"queued"`
			}
			if jerr := json.Unmarshal(out, &ack); jerr != nil {
				return fmt.Errorf("inbox push: decode %q: %w", string(out), jerr)
			}
			if !ack.OK || ack.ID == "" {
				return fmt.Errorf("inbox push: bad ack %+v (raw=%s)", ack, string(out))
			}
			if !ack.Delivered && !ack.Queued {
				return fmt.Errorf("inbox push: neither delivered nor queued: %+v", ack)
			}
			fmt.Fprintf(log, "grpc-roundtrip: inbox push delivered=%v queued=%v id=%s\n",
				ack.Delivered, ack.Queued, ack.ID)
		default:
			// We expect "no session named" from the daemon's Send when the
			// pane wasn't created. Anything else is an unexpected error.
			if !strings.Contains(err.Error(), "no session named") &&
				!strings.Contains(err.Error(), "NotFound") {
				return fmt.Errorf("inbox push: unexpected err: %w", err)
			}
			fmt.Fprintf(log, "grpc-roundtrip: inbox push hit expected NotFound (no real pane): %v\n", err)
		}
	}

	// ACT 3: inbox peek — tolerant of missing inbox, returns empty list.
	{
		var resp struct {
			Messages []map[string]any `json:"messages"`
			Session  string           `json:"session"`
		}
		if err := env.RunCallJSON(ctx, &resp,
			"inbox", "peek",
			"--socket", sock,
			"--session", sessionName,
			"--n", "5",
		); err != nil {
			return fmt.Errorf("inbox peek: %w", err)
		}
		if resp.Session != sessionName {
			return fmt.Errorf("inbox peek: session=%q want %q", resp.Session, sessionName)
		}
		// messages may be empty or contain a queued msg from ACT 2 — both fine.
		fmt.Fprintf(log, "grpc-roundtrip: inbox peek messages=%d\n", len(resp.Messages))
	}

	// ACT 4: ready against an unknown session.
	{
		var resp struct {
			Ready        bool   `json:"ready"`
			Reason       string `json:"reason"`
			State        string `json:"state"`
			LastSignalAt string `json:"last_signal_at"`
			Session      string `json:"session"`
		}
		if err := env.RunCallJSON(ctx, &resp,
			"ready",
			"--socket", sock,
			"--session", sessionName,
		); err != nil {
			return fmt.Errorf("ready: %w", err)
		}
		if resp.Session != sessionName {
			return fmt.Errorf("ready: session=%q want %q", resp.Session, sessionName)
		}
		// For an unknown session, the daemon returns ready=false,
		// reason="no-such-session". If the daemon evolved to lazily-
		// create sessions, ready=true with a real state is also fine.
		if !resp.Ready && resp.Reason != "no-such-session" {
			return fmt.Errorf("ready: ready=false but reason=%q (want no-such-session)", resp.Reason)
		}
		fmt.Fprintf(log, "grpc-roundtrip: ready ready=%v reason=%q state=%q\n",
			resp.Ready, resp.Reason, resp.State)
	}

	fmt.Fprintf(log, "grpc-roundtrip PASS: daemon=%s\n", sock)
	return nil
}
