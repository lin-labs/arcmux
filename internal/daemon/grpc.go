package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/sessionview"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCServer implements the arcmux.v1.AgentRuntime gRPC service.
type GRPCServer struct {
	arcmuxv1.UnimplementedAgentRuntimeServer
	daemon *Daemon
}

// NewGRPCServer creates the gRPC service implementation.
func NewGRPCServer(d *Daemon) *GRPCServer {
	return &GRPCServer{daemon: d}
}

func (s *GRPCServer) CreateSession(ctx context.Context, req *arcmuxv1.CreateSessionRequest) (*arcmuxv1.CreateSessionResponse, error) {
	sess, created, err := s.daemon.createSessionWithIdempotency(ctx, CreateSessionRequest{
		Agent:       req.Agent,
		CWD:         req.Cwd,
		Prompt:      req.Prompt,
		Name:        req.SessionName,
		TmuxSession: req.TmuxSession,
		TmuxWindow:  req.TmuxWindow,
		Env:         req.Env,
		AutoClose:   req.AutoClose,
		OwnerID:     req.OwnerId,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create session: %v", err)
	}

	snap := sess.Snapshot()
	return &arcmuxv1.CreateSessionResponse{
		SessionId:  snap.ID,
		TmuxTarget: snap.TmuxTarget,
		Pid:        int64(snap.PID),
		State:      string(snap.State),
		OwnerId:    snap.OwnerID,
		Created:    created,
	}, nil
}

func (s *GRPCServer) SendPrompt(ctx context.Context, req *arcmuxv1.SendPromptRequest) (*arcmuxv1.SendPromptResponse, error) {
	if err := s.daemon.SendPrompt(ctx, req.SessionId, req.Text, req.ConfirmDelivery, req.WaitIdle); err != nil {
		return nil, status.Errorf(codes.Internal, "send prompt: %v", err)
	}

	sess, ok := s.daemon.GetSession(req.SessionId)
	if !ok {
		return &arcmuxv1.SendPromptResponse{Delivered: true}, nil
	}
	return &arcmuxv1.SendPromptResponse{
		Delivered: true,
		State:     string(sess.Snapshot().State),
	}, nil
}

func (s *GRPCServer) Capture(ctx context.Context, req *arcmuxv1.CaptureRequest) (*arcmuxv1.CaptureResponse, error) {
	output, err := s.daemon.Capture(ctx, req.SessionId, req.IncludeHistory)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "capture: %v", err)
	}

	sess, ok := s.daemon.GetSession(req.SessionId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "session not found")
	}
	snap := sess.Snapshot()

	resp := &arcmuxv1.CaptureResponse{
		Output:         output,
		CurrentCommand: snap.CurrentCommand,
		State:          string(snap.State),
		Cwd:            snap.CWD,
	}

	// Get pane info for current command
	if snap.TmuxTarget != "" {
		info, err := s.daemon.tmux.GetPaneInfo(ctx, snap.TmuxTarget)
		if err == nil {
			resp.CurrentCommand = info.CurrentCommand
			if info.CWD != "" {
				resp.Cwd = info.CWD
			}
		}
	}

	if snap.IdleSince != nil {
		resp.IdleSince = snap.IdleSince.Format(time.RFC3339)
	}

	return resp, nil
}

func (s *GRPCServer) Status(ctx context.Context, req *arcmuxv1.StatusRequest) (*arcmuxv1.StatusResponse, error) {
	sess, err := s.daemon.Status(req.SessionId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	snap := sess.Snapshot()

	resp := &arcmuxv1.StatusResponse{
		SessionId:      snap.ID,
		State:          string(snap.State),
		Agent:          snap.Agent,
		TmuxTarget:     snap.TmuxTarget,
		Pid:            int64(snap.PID),
		StartedAt:      snap.StartedAt.Format(time.RFC3339),
		LastActivityAt: snap.LastActivityAt.Format(time.RFC3339),
		Health:         snap.Health,
		NudgeCount:     int32(snap.NudgeCount),
		OwnerId:        snap.OwnerID,
	}

	// Populate hook state from watcher
	hookEvents := s.daemon.watcher.LatestEvents(snap.ID)
	if len(hookEvents) > 0 {
		last := hookEvents[len(hookEvents)-1]
		resp.HookState = &arcmuxv1.HookState{
			Source:       s.daemon.profiles[snap.Agent].HookType,
			LastToolUse:  last.Tool,
			LastSignalAt: last.Timestamp.Format(time.RFC3339),
		}
	}

	return resp, nil
}

func (s *GRPCServer) Kill(ctx context.Context, req *arcmuxv1.KillRequest) (*arcmuxv1.KillResponse, error) {
	timeout := 30 * time.Second
	if req.Timeout != "" {
		if parsed, err := time.ParseDuration(req.Timeout); err == nil {
			timeout = parsed
		}
	}

	if err := s.daemon.Kill(ctx, req.SessionId, req.Graceful, timeout); err != nil {
		return nil, status.Errorf(codes.Internal, "kill: %v", err)
	}

	return &arcmuxv1.KillResponse{
		Killed:     true,
		FinalState: string(session.StateExited),
	}, nil
}

func (s *GRPCServer) ListSessions(ctx context.Context, req *arcmuxv1.ListSessionsRequest) (*arcmuxv1.ListSessionsResponse, error) {
	sessions := s.daemon.ListSessions()
	scope := sessionview.RootProfileScope
	if s.daemon.cfg.Daemon.ProfileName != "" {
		var err error
		scope, err = sessionview.NamedProfileScope(s.daemon.cfg.Daemon.ProfileName)
		if err != nil {
			return nil, status.Error(codes.Internal, "invalid daemon profile scope")
		}
	}
	catalog := s.daemon.SessionCatalog()
	resp := &arcmuxv1.ListSessionsResponse{
		Sessions: make([]*arcmuxv1.SessionSummary, 0, len(sessions)),
	}

	for _, sess := range sessions {
		snap := sess.Snapshot()
		historyBasename := ""
		if locator, err := sessionview.NewLocator(scope, snap.ID); err == nil {
			if detail, ok := catalog.Get(locator); ok && detail.Summary.History != nil {
				historyBasename = detail.Summary.History.Basename
			}
		}
		resp.Sessions = append(resp.Sessions, &arcmuxv1.SessionSummary{
			SessionId:       snap.ID,
			Agent:           snap.Agent,
			Cwd:             snap.CWD,
			State:           string(snap.State),
			TmuxTarget:      snap.TmuxTarget,
			StartedAt:       snap.StartedAt.Format(time.RFC3339),
			SessionName:     snap.Name,
			OwnerId:         snap.OwnerID,
			ProfileScope:    string(scope),
			HistoryBasename: historyBasename,
		})
	}

	return resp, nil
}

func (s *GRPCServer) ListAgents(ctx context.Context, req *arcmuxv1.ListAgentsRequest) (*arcmuxv1.ListAgentsResponse, error) {
	profiles := s.daemon.ListAgentProfiles()
	resp := &arcmuxv1.ListAgentsResponse{
		Agents: make([]*arcmuxv1.AgentInfo, 0, len(profiles)),
	}
	for name, prof := range profiles {
		resp.Agents = append(resp.Agents, &arcmuxv1.AgentInfo{
			Name:         name,
			Class:        prof.Class,
			Transport:    prof.Transport,
			ExecDriver:   prof.ExecDriver,
			HookType:     prof.HookType,
			StartCommand: prof.StartCommand,
			HookBacked:   prof.HookBacked(),
		})
	}
	sort.Slice(resp.Agents, func(i, j int) bool { return resp.Agents[i].Name < resp.Agents[j].Name })
	return resp, nil
}

func (s *GRPCServer) StreamOutput(req *arcmuxv1.StreamOutputRequest, stream arcmuxv1.AgentRuntime_StreamOutputServer) error {
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
			if sendErr := stream.Send(&arcmuxv1.OutputChunk{
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

func (s *GRPCServer) Subscribe(req *arcmuxv1.SubscribeRequest, stream arcmuxv1.AgentRuntime_SubscribeServer) error {
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

			protoEvent := &arcmuxv1.Event{
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
var _ arcmuxv1.AgentRuntimeServer = (*GRPCServer)(nil)

// Silence unused import for fmt
var _ = fmt.Sprintf
