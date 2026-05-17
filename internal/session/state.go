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

	ID             string
	Name           string
	Agent          string
	CWD            string
	TmuxTarget     string // e.g. "agents:myapp.%42"
	PID            int
	State          State
	Health         string // "healthy", "stuck", "escalated"
	StartedAt      time.Time
	LastActivityAt time.Time
	IdleSince      *time.Time
	NudgeCount     int
	Env            map[string]string
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

// Snapshot returns a read-consistent copy of session fields (without the mutex).
type Snapshot struct {
	ID             string
	Name           string
	Agent          string
	CWD            string
	TmuxTarget     string
	PID            int
	State          State
	Health         string
	StartedAt      time.Time
	LastActivityAt time.Time
	IdleSince      *time.Time
	NudgeCount     int
}

func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		ID:             s.ID,
		Name:           s.Name,
		Agent:          s.Agent,
		CWD:            s.CWD,
		TmuxTarget:     s.TmuxTarget,
		PID:            s.PID,
		State:          s.State,
		Health:         s.Health,
		StartedAt:      s.StartedAt,
		LastActivityAt: s.LastActivityAt,
		IdleSince:      s.IdleSince,
		NudgeCount:     s.NudgeCount,
	}
}
