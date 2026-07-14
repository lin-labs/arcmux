// Package mesh provides arcmux's best-effort, authenticated device transport.
// It intentionally carries only transport control messages in protocol v1;
// remote session and artifact semantics are layered on it separately.
package mesh

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/lin-labs/arcmux/internal/config"
)

const (
	ProtocolVersion = 1
	meshPath        = "/v1/mesh"
	subprotocol     = "arcmux.mesh.v1"
)

var ErrBackpressure = errors.New("mesh peer writer queue is full")
var errHeartbeatTimeout = errors.New("peer heartbeat timed out")

type Envelope struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	SentAt   string `json:"sent_at,omitempty"`
}

type Status struct {
	PeerID      string     `json:"peer_id"`
	Direction   string     `json:"direction,omitempty"`
	State       string     `json:"state"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	Attempts    int        `json:"attempts,omitempty"`
	Protocol    int        `json:"protocol,omitempty"`
	RoundTripMS int64      `json:"round_trip_ms,omitempty"`
}

type Manager struct {
	cfg      config.ParsedMeshConfig
	registry *Registry
	logger   *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	server *http.Server
	ln     net.Listener
	wg     sync.WaitGroup

	mu      sync.RWMutex
	peers   map[string]*peerRuntime
	status  map[string]Status
	gen     atomic.Uint64
	started bool
}

type peerRuntime struct {
	generation uint64
	peerID     string
	direction  string
	conn       *websocket.Conn
	send       chan Envelope
	done       chan struct{}
	closeOnce  sync.Once
	pendingMu  sync.Mutex
	pending    map[string]chan time.Duration
}

func New(cfg config.ParsedMeshConfig, registry *Registry, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{cfg: cfg, registry: registry, logger: logger,
		peers: map[string]*peerRuntime{}, status: map[string]Status{}}
}

func (m *Manager) Start(parent context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.ctx, m.cancel = context.WithCancel(parent)
	for _, peer := range m.registry.Peers {
		m.status[peer.ID] = Status{PeerID: peer.ID, Direction: "outbound", State: "disconnected"}
	}
	for peerID := range m.registry.Accept {
		m.status[peerID] = Status{PeerID: peerID, Direction: "inbound", State: "disconnected"}
	}
	m.mu.Unlock()

	if m.registry.Serve {
		if err := m.startListener(); err != nil {
			m.cancel()
			m.mu.Lock()
			m.started = false
			m.mu.Unlock()
			return err
		}
	}
	for _, p := range m.registry.Peers {
		peer := p
		m.wg.Add(1)
		go func() { defer m.wg.Done(); m.connectLoop(peer) }()
	}
	return nil
}

func (m *Manager) startListener() error {
	ln, err := net.Listen("tcp", m.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on mesh address %s: %w", m.cfg.ListenAddr, err)
	}
	m.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc(meshPath, m.handleUpgrade)
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: m.cfg.HandshakeTimeout}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.logger.Error("mesh listener stopped; local sessions unaffected", "error", err)
		}
	}()
	m.logger.Info("mesh listener started", "addr", ln.Addr().String())
	return nil
}

func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	if m.cancel != nil {
		m.cancel()
	}
	peers := make([]*peerRuntime, 0, len(m.peers))
	for _, p := range m.peers {
		peers = append(peers, p)
	}
	m.mu.Unlock()
	for _, p := range peers {
		p.close(websocket.StatusGoingAway, "daemon stopping")
	}
	if m.server != nil {
		_ = m.server.Shutdown(ctx)
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (m *Manager) Addr() string {
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

func (m *Manager) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	peerID, ok := m.authorize(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}, Subprotocols: []string{subprotocol}})
	if err != nil {
		return
	}
	if conn.Subprotocol() != subprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "mesh subprotocol required")
		return
	}
	conn.SetReadLimit(m.cfg.MaxMessageBytes)
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.HandshakeTimeout)
	defer cancel()
	hello, err := readEnvelope(ctx, conn)
	if err != nil || hello.Type != "hello" || hello.Version != ProtocolVersion || hello.DeviceID != peerID {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid mesh handshake")
		m.setError(peerID, "mesh handshake rejected")
		return
	}
	welcome := Envelope{Version: ProtocolVersion, Type: "welcome", DeviceID: m.registry.DeviceID, SentAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeEnvelope(ctx, conn, welcome); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "handshake write failed")
		return
	}
	m.activate(peerID, "inbound", conn)
}

func (m *Manager) authorize(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	got := header[len(prefix):]
	for id, token := range m.registry.Accept {
		if subtle.ConstantTimeCompare([]byte(TokenHash(got)), []byte(token)) == 1 {
			return id, true
		}
	}
	return "", false
}

func (m *Manager) connectLoop(peer Peer) {
	attempt := 0
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.updateStatus(peer.ID, func(s *Status) { s.State = "connecting"; s.Attempts = attempt + 1; s.NextRetryAt = nil })
		headers := http.Header{"Authorization": []string{"Bearer " + peer.Token}}
		ctx, cancel := context.WithTimeout(m.ctx, m.cfg.HandshakeTimeout)
		conn, _, err := websocket.Dial(ctx, peer.URL, &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{subprotocol}})
		if err == nil {
			if conn.Subprotocol() != subprotocol {
				err = errors.New("mesh subprotocol mismatch")
			}
			conn.SetReadLimit(m.cfg.MaxMessageBytes)
			if err == nil {
				err = writeEnvelope(ctx, conn, Envelope{Version: ProtocolVersion, Type: "hello", DeviceID: m.registry.DeviceID, SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
			}
			if err == nil {
				var welcome Envelope
				welcome, err = readEnvelope(ctx, conn)
				if err == nil && (welcome.Type != "welcome" || welcome.Version != ProtocolVersion || welcome.DeviceID != peer.ID) {
					err = fmt.Errorf("peer identity or protocol mismatch")
				}
			}
		}
		cancel()
		if err == nil {
			connectedAt := time.Now()
			p, activated := m.activate(peer.ID, "outbound", conn)
			if !activated {
				return
			}
			select {
			case <-p.done:
			case <-m.ctx.Done():
				return
			}
			if m.ctx.Err() != nil {
				return
			}
			// A handshake alone is not proof of a healthy connection. Preserve
			// exponential backoff across rapid accept/drop flapping and reset only
			// after the link survived a full dead-peer window.
			if time.Since(connectedAt) >= m.cfg.DeadAfter {
				attempt = 0
			}
		} else {
			if conn != nil {
				_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
			}
			m.setError(peer.ID, sanitizeError(err))
		}
		attempt++
		delay := fullJitter(attempt, m.cfg.ReconnectMin, m.cfg.ReconnectMax)
		next := time.Now().Add(delay)
		m.updateStatus(peer.ID, func(s *Status) { s.State = "disconnected"; s.Attempts = attempt; s.NextRetryAt = &next })
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-m.ctx.Done():
			t.Stop()
			return
		}
	}
}

func fullJitter(attempt int, min, max time.Duration) time.Duration {
	cap := min
	for i := 1; i < attempt && cap < max; i++ {
		cap *= 2
	}
	if cap > max {
		cap = max
	}
	if cap <= 0 {
		return 0
	}
	// Keep a small floor so repeated failures cannot busy-loop while retaining
	// full jitter across the remainder of the current exponential window.
	floor := min / 4
	return floor + time.Duration(rand.Int64N(int64(cap-floor)+1))
}

func (m *Manager) activate(peerID, direction string, conn *websocket.Conn) (*peerRuntime, bool) {
	p := &peerRuntime{generation: m.gen.Add(1), peerID: peerID, direction: direction, conn: conn,
		send: make(chan Envelope, m.cfg.WriterQueue), done: make(chan struct{}), pending: map[string]chan time.Duration{}}
	now := time.Now()
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		_ = conn.CloseNow()
		return nil, false
	}
	old := m.peers[peerID]
	m.peers[peerID] = p
	m.status[peerID] = Status{PeerID: peerID, Direction: direction, State: "connected", ConnectedAt: &now, LastSeen: &now, LastSuccess: &now, Protocol: ProtocolVersion}
	// Register connection goroutines before releasing the lifecycle lock so
	// Stop cannot begin Wait while an accepted handshake is still activating.
	m.wg.Add(3)
	m.mu.Unlock()
	if old != nil {
		old.close(websocket.StatusNormalClosure, "replaced by newer connection")
	}
	go func() { defer m.wg.Done(); m.writer(p) }()
	go func() { defer m.wg.Done(); m.reader(p) }()
	go func() { defer m.wg.Done(); m.heartbeat(p) }()
	return p, true
}

func (m *Manager) writer(p *peerRuntime) {
	for {
		select {
		case env := <-p.send:
			ctx, cancel := context.WithTimeout(m.ctx, m.cfg.HandshakeTimeout)
			err := writeEnvelope(ctx, p.conn, env)
			cancel()
			if err != nil {
				m.disconnect(p, err)
				return
			}
		case <-p.done:
			return
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Manager) reader(p *peerRuntime) {
	for {
		env, err := readEnvelope(m.ctx, p.conn)
		if err != nil {
			m.disconnect(p, err)
			return
		}
		if env.Version != ProtocolVersion {
			m.disconnect(p, errors.New("protocol version mismatch"))
			return
		}
		now := time.Now()
		m.updateCurrent(p, func(s *Status) { s.LastSeen = &now; s.LastSuccess = &now; s.State = "connected" })
		switch env.Type {
		case "ping":
			_ = m.enqueue(p, Envelope{Version: ProtocolVersion, Type: "pong", ID: env.ID, SentAt: time.Now().UTC().Format(time.RFC3339Nano)})
		case "pong":
			p.pendingMu.Lock()
			ch := p.pending[env.ID]
			delete(p.pending, env.ID)
			p.pendingMu.Unlock()
			if ch != nil {
				sent, _ := time.Parse(time.RFC3339Nano, env.ID)
				rtt := time.Since(sent)
				m.updateCurrent(p, func(s *Status) { s.RoundTripMS = rtt.Milliseconds() })
				select {
				case ch <- rtt:
				default:
				}
				close(ch)
			}
		default:
			m.disconnect(p, fmt.Errorf("unsupported frame type %q", env.Type))
			return
		}
	}
}

func (m *Manager) heartbeat(p *peerRuntime) {
	ticker := time.NewTicker(m.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.RLock()
			s := m.status[p.peerID]
			current := m.peers[p.peerID] == p
			m.mu.RUnlock()
			if !current {
				return
			}
			if s.LastSeen == nil {
				m.disconnect(p, errHeartbeatTimeout)
				return
			}
			age := time.Since(*s.LastSeen)
			if age >= m.cfg.DeadAfter {
				m.disconnect(p, errHeartbeatTimeout)
				return
			}
			if age >= m.cfg.StaleAfter {
				m.updateCurrent(p, func(s *Status) { s.State = "stale" })
			}
			_, _, _ = m.sendPing(p, false)
		case <-p.done:
			return
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Manager) sendPing(p *peerRuntime, tracked bool) (string, <-chan time.Duration, error) {
	id := time.Now().UTC().Format(time.RFC3339Nano)
	var ch chan time.Duration
	if tracked {
		ch = make(chan time.Duration, 1)
		p.pendingMu.Lock()
		p.pending[id] = ch
		p.pendingMu.Unlock()
	}
	if err := m.enqueue(p, Envelope{Version: ProtocolVersion, Type: "ping", ID: id, SentAt: id}); err != nil {
		if tracked {
			p.pendingMu.Lock()
			delete(p.pending, id)
			p.pendingMu.Unlock()
			close(ch)
		}
		return "", nil, err
	}
	return id, ch, nil
}

func (m *Manager) enqueue(p *peerRuntime, env Envelope) error {
	select {
	case <-p.done:
		return errors.New("mesh peer disconnected")
	default:
	}
	select {
	case p.send <- env:
		return nil
	default:
		return ErrBackpressure
	}
}

func (m *Manager) Ping(ctx context.Context, peerID string) (time.Duration, error) {
	m.mu.RLock()
	p := m.peers[peerID]
	m.mu.RUnlock()
	if p == nil {
		return 0, fmt.Errorf("peer %q is not connected", peerID)
	}
	id, ch, err := m.sendPing(p, true)
	if err != nil {
		return 0, err
	}
	select {
	case rtt, ok := <-ch:
		if !ok {
			return 0, errors.New("peer disconnected")
		}
		return rtt, nil
	case <-ctx.Done():
		p.removePending(id)
		return 0, ctx.Err()
	case <-p.done:
		p.removePending(id)
		return 0, errors.New("peer disconnected")
	}
}

func (p *peerRuntime) removePending(id string) {
	p.pendingMu.Lock()
	ch := p.pending[id]
	delete(p.pending, id)
	p.pendingMu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (m *Manager) disconnect(p *peerRuntime, err error) {
	p.close(websocket.StatusNormalClosure, "connection ended")
	m.mu.Lock()
	if m.peers[p.peerID] == p {
		delete(m.peers, p.peerID)
		s := m.status[p.peerID]
		if errors.Is(err, errHeartbeatTimeout) {
			s.State = "dead"
		} else {
			s.State = "disconnected"
		}
		s.LastError = sanitizeError(err)
		m.status[p.peerID] = s
	}
	m.mu.Unlock()
}

func (p *peerRuntime) close(status websocket.StatusCode, reason string) {
	p.closeOnce.Do(func() {
		close(p.done)
		// CloseNow unblocks the single reader/writer immediately. A blocking
		// close handshake here would delay status transitions and daemon reloads
		// for several seconds when Wi-Fi/VPN vanished.
		_ = p.conn.CloseNow()
		p.pendingMu.Lock()
		for id, ch := range p.pending {
			delete(p.pending, id)
			close(ch)
		}
		p.pendingMu.Unlock()
	})
}

func (m *Manager) Status() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.status))
	for _, s := range m.status {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
}

func (m *Manager) updateStatus(id string, fn func(*Status)) {
	m.mu.Lock()
	s := m.status[id]
	s.PeerID = id
	fn(&s)
	m.status[id] = s
	m.mu.Unlock()
}
func (m *Manager) updateCurrent(p *peerRuntime, fn func(*Status)) {
	m.mu.Lock()
	if m.peers[p.peerID] == p {
		s := m.status[p.peerID]
		fn(&s)
		m.status[p.peerID] = s
	}
	m.mu.Unlock()
}
func (m *Manager) setError(id, msg string) { m.updateStatus(id, func(s *Status) { s.LastError = msg }) }

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 240 {
		s = s[:240]
	}
	return s
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (Envelope, error) {
	typ, b, err := conn.Read(ctx)
	if err != nil {
		return Envelope{}, err
	}
	if typ != websocket.MessageText {
		return Envelope{}, errors.New("mesh frame must be text JSON")
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return env, fmt.Errorf("malformed mesh frame: %w", err)
	}
	return env, nil
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
