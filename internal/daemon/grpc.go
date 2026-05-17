package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	atrsv1 "github.com/lin-labs/arcmux/gen/atrs/v1"
	"github.com/lin-labs/arcmux/internal/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCServer implements the atrs.v1.AgentRuntime gRPC service.
type GRPCServer struct {
	atrsv1.UnimplementedAgentRuntimeServer
	daemon *Daemon
}

// NewGRPCServer creates the gRPC service implementation.
func NewGRPCServer(d *Daemon) *GRPCServer {
	return &GRPCServer{daemon: d}
}

func (s *GRPCServer) CreateSession(ctx context.Context, req *atrsv1.CreateSessionRequest) (*atrsv1.CreateSessionResponse, error) {
	sess, err := s.daemon.CreateSession(ctx, CreateSessionRequest{
		Agent:       req.Agent,
		CWD:         req.Cwd,
		Prompt:      req.Prompt,
		Name:        req.SessionName,
		TmuxSession: req.TmuxSession,
		TmuxWindow:  req.TmuxWindow,
		Env:         req.Env,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create session: %v", err)
	}

	snap := sess.Snapshot()
	return &atrsv1.CreateSessionResponse{
		SessionId:  snap.ID,
		TmuxTarget: snap.TmuxTarget,
		Pid:        int64(snap.PID),
		State:      string(snap.State),
	}, nil
}

func (s *GRPCServer) SendPrompt(ctx context.Context, req *atrsv1.SendPromptRequest) (*atrsv1.SendPromptResponse, error) {
	if err := s.daemon.SendPrompt(ctx, req.SessionId, req.Text, req.ConfirmDelivery, req.WaitIdle); err != nil {
		return nil, status.Errorf(codes.Internal, "send prompt: %v", err)
	}

	sess, ok := s.daemon.GetSession(req.SessionId)
	if !ok {
		return &atrsv1.SendPromptResponse{Delivered: true}, nil
	}
	return &atrsv1.SendPromptResponse{
		Delivered: true,
		State:     string(sess.Snapshot().State),
	}, nil
}

func (s *GRPCServer) Capture(ctx context.Context, req *atrsv1.CaptureRequest) (*atrsv1.CaptureResponse, error) {
	output, err := s.daemon.Capture(ctx, req.SessionId, req.IncludeHistory)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "capture: %v", err)
	}

	sess, ok := s.daemon.GetSession(req.SessionId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "session not found")
	}
	snap := sess.Snapshot()

	resp := &atrsv1.CaptureResponse{
		Output: output,
		State:  string(snap.State),
		Cwd:    snap.CWD,
	}

	// Get pane info for current command
	info, err := s.daemon.tmux.GetPaneInfo(ctx, snap.TmuxTarget)
	if err == nil {
		resp.CurrentCommand = info.CurrentCommand
		if info.CWD != "" {
			resp.Cwd = info.CWD
		}
	}

	if snap.IdleSince != nil {
		resp.IdleSince = snap.IdleSince.Format(time.RFC3339)
	}

	return resp, nil
}

func (s *GRPCServer) Status(ctx context.Context, req *atrsv1.StatusRequest) (*atrsv1.StatusResponse, error) {
	sess, err := s.daemon.Status(req.SessionId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	snap := sess.Snapshot()

	resp := &atrsv1.StatusResponse{
		SessionId:      snap.ID,
		State:          string(snap.State),
		Agent:          snap.Agent,
		TmuxTarget:     snap.TmuxTarget,
		Pid:            int64(snap.PID),
		StartedAt:      snap.StartedAt.Format(time.RFC3339),
		LastActivityAt: snap.LastActivityAt.Format(time.RFC3339),
		Health:         snap.Health,
		NudgeCount:     int32(snap.NudgeCount),
	}

	// Populate hook state from watcher
	hookEvents := s.daemon.watcher.LatestEvents(snap.ID)
	if len(hookEvents) > 0 {
		last := hookEvents[len(hookEvents)-1]
		resp.HookState = &atrsv1.HookState{
			Source:       s.daemon.profiles[snap.Agent].HookType,
			LastToolUse:  last.Tool,
			LastSignalAt: last.Timestamp.Format(time.RFC3339),
		}
	}

	return resp, nil
}

func (s *GRPCServer) Kill(ctx context.Context, req *atrsv1.KillRequest) (*atrsv1.KillResponse, error) {
	timeout := 30 * time.Second
	if req.Timeout != "" {
		if parsed, err := time.ParseDuration(req.Timeout); err == nil {
			timeout = parsed
		}
	}

	if err := s.daemon.Kill(ctx, req.SessionId, req.Graceful, timeout); err != nil {
		return nil, status.Errorf(codes.Internal, "kill: %v", err)
	}

	return &atrsv1.KillResponse{
		Killed:     true,
		FinalState: string(session.StateExited),
	}, nil
}

func (s *GRPCServer) ListSessions(ctx context.Context, req *atrsv1.ListSessionsRequest) (*atrsv1.ListSessionsResponse, error) {
	sessions := s.daemon.ListSessions()
	resp := &atrsv1.ListSessionsResponse{
		Sessions: make([]*atrsv1.SessionSummary, 0, len(sessions)),
	}

	for _, sess := range sessions {
		snap := sess.Snapshot()
		resp.Sessions = append(resp.Sessions, &atrsv1.SessionSummary{
			SessionId:   snap.ID,
			Agent:       snap.Agent,
			Cwd:         snap.CWD,
			State:       string(snap.State),
			TmuxTarget:  snap.TmuxTarget,
			StartedAt:   snap.StartedAt.Format(time.RFC3339),
			SessionName: snap.Name,
		})
	}

	return resp, nil
}

func (s *GRPCServer) StreamOutput(req *atrsv1.StreamOutputRequest, stream atrsv1.AgentRuntime_StreamOutputServer) error {
	sess, ok := s.daemon.GetSession(req.SessionId)
	if !ok {
		return status.Errorf(codes.NotFound, "session not found: %s", req.SessionId)
	}
	snap := sess.Snapshot()

	// Open the output log file and tail it
	outputPath := s.daemon.outputFilePath(snap.ID)
	f, err := os.Open(outputPath)
	if err != nil {
		return status.Errorf(codes.Internal, "open output file: %v", err)
	}
	defer f.Close()

	// Seek to end for live tailing
	f.Seek(0, io.SeekEnd)

	buf := make([]byte, 4096)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		default:
		}

		n, err := f.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&atrsv1.OutputChunk{
				Text:      string(buf[:n]),
				Timestamp: time.Now().Format(time.RFC3339),
			}); sendErr != nil {
				return sendErr
			}
		}

		if err == io.EOF {
			// Check if session is still alive
			currentSnap := sess.Snapshot()
			if currentSnap.State == session.StateExited || currentSnap.State == session.StateFailed {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err != nil {
			return status.Errorf(codes.Internal, "read output: %v", err)
		}
	}
}

func (s *GRPCServer) Subscribe(req *atrsv1.SubscribeRequest, stream atrsv1.AgentRuntime_SubscribeServer) error {
	ch, subID := s.daemon.Subscribe(req.SessionId)
	defer s.daemon.Unsubscribe(subID)

	filterTypes := make(map[string]bool, len(req.EventTypes))
	for _, t := range req.EventTypes {
		filterTypes[t] = true
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case event, ok := <-ch:
			if !ok {
				return nil
			}

			// Apply type filter
			if len(filterTypes) > 0 && !filterTypes[event.Type] {
				continue
			}

			protoEvent := &atrsv1.Event{
				SessionId: event.SessionID,
				Type:      event.Type,
				Timestamp: event.Timestamp.Format(time.RFC3339),
				State:     event.State,
				Message:   event.Message,
				Data:      event.Data,
			}

			if err := stream.Send(protoEvent); err != nil {
				return err
			}
		}
	}
}

// Ensure compile-time interface compliance.
var _ atrsv1.AgentRuntimeServer = (*GRPCServer)(nil)

// Silence unused import for fmt
var _ = fmt.Sprintf
