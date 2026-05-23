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

	ID               string
	Name             string
	Agent            string
	CWD              string
	Transport        string
	TmuxTarget       string // e.g. "agents:myapp.%42"
	CurrentCommand   string
	BackendSessionID string
	PID              int
	State            State
	Health           string // "healthy", "stuck", "escalated"
	StartedAt        time.Time
	LastActivityAt   time.Time
	IdleSince        *time.Time
	NudgeCount       int
	Env              map[string]string
	AutoClose        bool // for exec transport: transition to StateExited on subprocess exit
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
	s.LastActivityAt = time.Now()
	if state == StateIdle {
		now := time.Now()
		s.IdleSince = &now
	} else {
		s.IdleSince = nil
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
	ID               string
	Name             string
	Agent            string
	CWD              string
	Transport        string
	Env              map[string]string
	TmuxTarget       string
	CurrentCommand   string
	BackendSessionID string
	PID              int
	State            State
	Health           string
	StartedAt        time.Time
	LastActivityAt   time.Time
	IdleSince        *time.Time
	NudgeCount       int
	AutoClose        bool
}

func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		ID:               s.ID,
		Name:             s.Name,
		Agent:            s.Agent,
		CWD:              s.CWD,
		Transport:        s.Transport,
		Env:              copyEnvMap(s.Env),
		TmuxTarget:       s.TmuxTarget,
		CurrentCommand:   s.CurrentCommand,
		BackendSessionID: s.BackendSessionID,
		PID:              s.PID,
		State:            s.State,
		Health:           s.Health,
		StartedAt:        s.StartedAt,
		LastActivityAt:   s.LastActivityAt,
		IdleSince:        s.IdleSince,
		NudgeCount:       s.NudgeCount,
		AutoClose:        s.AutoClose,
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
