package session

import (
	"sync"
	"time"
)

// State represents the lifecycle state of an agent session.
type State string

const (
	StateStarting    State = "starting"
	StateHandshaking State = "handshaking"
	StateIdle        State = "idle"
	StateWorking     State = "working"
	StateStuck       State = "stuck"
	StateEscalated   State = "escalated"
	StateExited      State = "exited"
	StateFailed      State = "failed"
)

// Session holds the runtime state of a managed agent session.
type Session struct {
	mu sync.RWMutex

	ID        string
	Name      string
	Agent     string
	CWD       string
	Transport string
	// TmuxSessionName is the tmux session that owns this agent pane.
	// New tmux-backed sessions use one tmux session per agent so session-
	// scoped environment is isolated from sibling agents.
	TmuxSessionName string
	// TmuxTarget is the stable pane target used for routing. For tmux-backed
	// sessions this is a "%pane_id" so window/session names can change without
	// breaking SendKeys / capture / pipe-pane routing.
	TmuxTarget string
	// CurrentCommand is the most recent prompt sent (truncated to ~200
	// runes), set by SendPrompt/sendExecPrompt. The Capture RPC may
	// override this with the live `tmux display-message #{pane_current_command}`
	// for tmux sessions when the pane is alive.
	CurrentCommand string
	// BackendSessionID is the underlying agent-native session/thread id
	// (e.g. claude `--session-id` UUID, codex `thread_id`). Only the exec
	// transport's structured-output parsers populate this; for the tmux
	// transport it stays empty because there's no machine-readable handle
	// to scrape from a pane. Persisted so exec-transport sessions can
	// resume their backend session across daemon restarts.
	BackendSessionID string
	PID              int
	State            State
	Health           string // "healthy", "stuck", "escalated"
	StartedAt        time.Time
	LastActivityAt   time.Time
	IdleSince        *time.Time
	// WorkingSince marks when the session most recently entered the working
	// state (a prompt was delivered and confirmed ingested). The health
	// monitor uses it as the per-turn reference for hook-driven idle
	// detection: a turn_end hook is trusted as "this turn finished" only
	// when it landed after WorkingSince, so a stale turn_end from a prior
	// turn — or a dropped hook — can never mark the current turn idle
	// prematurely. The mirror of IdleSince.
	WorkingSince *time.Time
	NudgeCount   int
	Env          map[string]string
	AutoClose    bool   // for exec transport: transition to StateExited on subprocess exit
	OwnerID      string // C1: free-form caller attribution tag (e.g. "elonco:my-project"); empty for legacy callers
	// private is trusted daemon provenance for sessions whose local context
	// must not cross ordinary inventory, audit, or model-evidence boundaries.
	// It is deliberately not inferred from caller-controlled OwnerID or Env.
	private bool
	// ProjectSlug is the project this session's cwd resolved to at spawn
	// (.agents/project.yaml > registry > folder name); Platform marks the slug
	// as a platform monorepo, where many concurrent agents share one folder.
	ProjectSlug string
	Platform    bool
	// Feature is the session's evolving feature slug, mirrored from the hook
	// state doc (turn_contract.feature) by the daemon sweep. When it appears
	// or changes, the daemon renames the tmux session to <project>-<feature>
	// so listings and `tmux ls` say what each agent is actually working on.
	Feature string
	// TmuxNameRequested marks that the caller supplied an explicit tmux
	// session name at create time; the daemon never renames those.
	TmuxNameRequested bool
}

// NewSession creates a session in starting state.
func NewSession(id, name, agent, cwd string) *Session {
	now := time.Now()
	return &Session{
		ID:             id,
		Name:           name,
		Agent:          agent,
		CWD:            cwd,
		State:          StateStarting,
		Health:         "healthy",
		StartedAt:      now,
		LastActivityAt: now,
	}
}

// SetState transitions the session state and records activity.
func (s *Session) SetState(state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.State = state
	now := time.Now()
	s.LastActivityAt = now
	if state == StateIdle {
		s.IdleSince = &now
	} else {
		s.IdleSince = nil
	}
	if state == StateWorking {
		s.WorkingSince = &now
	} else {
		s.WorkingSince = nil
	}
}

// RecordActivity updates last activity without changing state.
func (s *Session) RecordActivity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastActivityAt = time.Now()
}

// IncrementNudge bumps the nudge counter and returns the new count.
func (s *Session) IncrementNudge() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NudgeCount++
	return s.NudgeCount
}

// ResetNudge clears the nudge counter.
func (s *Session) ResetNudge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NudgeCount = 0
}

// SetTransport records which runtime transport backs the session.
func (s *Session) SetTransport(transport string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Transport = transport
}

// SetPID updates the active OS process id, or 0 when no process is active.
func (s *Session) SetPID(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PID = pid
}

// SetCurrentCommand updates the current runtime command string.
func (s *Session) SetCurrentCommand(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CurrentCommand = command
}

// SetBackendSessionID records the underlying agent-native session/thread id.
func (s *Session) SetBackendSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BackendSessionID = id
}

// SetHealth updates the health string.
func (s *Session) SetHealth(health string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Health = health
}

// SetAutoClose records whether the session should transition to StateExited
// when its subprocess (exec transport) finishes, rather than parking at idle.
func (s *Session) SetAutoClose(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AutoClose = v
}

// SetOwnerID records the caller-attribution tag. Empty is allowed.
func (s *Session) SetOwnerID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.OwnerID = id
}

// MarkPrivate records trusted daemon provenance. It is intentionally
// one-directional: a private session cannot be made public later by mutating
// caller-controlled metadata.
func (s *Session) MarkPrivate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.private = true
}

// SetProject records the spawn-time project resolution.
func (s *Session) SetProject(slug string, platform bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ProjectSlug = slug
	s.Platform = platform
}

// SetFeature records the current feature slug (mirrored from the hook state
// doc by the daemon sweep).
func (s *Session) SetFeature(feature string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Feature = feature
}

// SetTmuxNameRequested marks the tmux session name as caller-chosen.
func (s *Session) SetTmuxNameRequested(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TmuxNameRequested = v
}

// SetTmuxSessionName updates the owning tmux session name (after a rename).
func (s *Session) SetTmuxSessionName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TmuxSessionName = name
}

// SetEnv replaces the session environment snapshot.
func (s *Session) SetEnv(env map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(env) == 0 {
		s.Env = nil
		return
	}
	copied := make(map[string]string, len(env))
	for k, v := range env {
		copied[k] = v
	}
	s.Env = copied
}

// Snapshot returns a read-consistent copy of session fields (without the mutex).
type Snapshot struct {
	ID                string
	Name              string
	Agent             string
	CWD               string
	Transport         string
	Env               map[string]string
	TmuxSessionName   string
	TmuxTarget        string
	CurrentCommand    string
	BackendSessionID  string
	PID               int
	State             State
	Health            string
	StartedAt         time.Time
	LastActivityAt    time.Time
	IdleSince         *time.Time
	WorkingSince      *time.Time
	NudgeCount        int
	AutoClose         bool
	OwnerID           string
	Private           bool
	ProjectSlug       string
	Platform          bool
	Feature           string
	TmuxNameRequested bool
}

func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		ID:                s.ID,
		Name:              s.Name,
		Agent:             s.Agent,
		CWD:               s.CWD,
		Transport:         s.Transport,
		Env:               copyEnvMap(s.Env),
		TmuxSessionName:   s.TmuxSessionName,
		TmuxTarget:        s.TmuxTarget,
		CurrentCommand:    s.CurrentCommand,
		BackendSessionID:  s.BackendSessionID,
		PID:               s.PID,
		State:             s.State,
		Health:            s.Health,
		StartedAt:         s.StartedAt,
		LastActivityAt:    s.LastActivityAt,
		IdleSince:         s.IdleSince,
		WorkingSince:      s.WorkingSince,
		NudgeCount:        s.NudgeCount,
		AutoClose:         s.AutoClose,
		OwnerID:           s.OwnerID,
		Private:           s.private,
		ProjectSlug:       s.ProjectSlug,
		Platform:          s.Platform,
		Feature:           s.Feature,
		TmuxNameRequested: s.TmuxNameRequested,
	}
}

func copyEnvMap(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	copied := make(map[string]string, len(env))
	for k, v := range env {
		copied[k] = v
	}
	return copied
}
