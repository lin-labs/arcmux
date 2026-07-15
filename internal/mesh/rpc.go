package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	CapabilityRPCV1           = "rpc.v1"
	CapabilitySessionsReadV1  = "sessions.read.v1"
	CapabilityArtifactsReadV1 = "artifacts.read.v1"
	CapabilityEventsV1        = "events.v1"
	CapabilityHandoffsV1      = "handoffs.v1"

	ScopeSessionsRead    = "sessions.read"
	ScopeArtifactsRead   = "artifacts.read"
	ScopeEventsRead      = "events.read"
	ScopeHandoffsPrepare = "handoffs.prepare"
	ScopeHandoffsLaunch  = "handoffs.launch"

	MaxApplicationPayload = 48 << 10
	MaxPendingCalls       = 128

	// EventGapName is reserved by the transport as the typed signal that one or
	// more at-most-once events were dropped. It remains ordinary authenticated,
	// events-capability traffic; it grants no additional access or control.
	EventGapName = "events.gap"
)

var (
	defaultCapabilities = []string{
		CapabilityArtifactsReadV1,
		CapabilityEventsV1,
		CapabilityHandoffsV1,
		CapabilityRPCV1,
		CapabilitySessionsReadV1,
	}

	ErrBackpressure          = errors.New("mesh peer writer queue is full")
	ErrPeerDisconnected      = errors.New("mesh peer disconnected")
	ErrCapabilityUnavailable = errors.New("mesh capability unavailable")
	ErrMethodNotRegistered   = errors.New("mesh RPC method is not registered locally")
	ErrTooManyPendingCalls   = errors.New("mesh peer has too many pending RPC calls")
	ErrPayloadTooLarge       = errors.New("mesh application payload exceeds 48 KiB")

	safeMethod = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	safeScope  = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
)

const (
	ErrorUnsupportedMethod  = "unsupported_method"
	ErrorPermissionDenied   = "permission_denied"
	ErrorCapabilityRequired = "capability_required"
	ErrorInvalidRequest     = "invalid_request"
	ErrorBackpressure       = "backpressure"
	ErrorPayloadTooLarge    = "payload_too_large"
	ErrorInternal           = "internal_error"
)

// MethodSpec is registered by both callers and receivers so Call can enforce
// negotiated capabilities and receivers can enforce their local grant policy.
// RequiredScope is mandatory for handlers: application RPC is default-deny.
type MethodSpec struct {
	Name          string
	Capability    string
	RequiredScope string
}

// Principal is derived exclusively from the authenticated connection and the
// receiver's local registry. Peer-controlled request payloads cannot alter it.
type Principal struct {
	PeerID string   `json:"peer_id"`
	Scopes []string `json:"scopes,omitempty"`
}

func (p Principal) HasScope(scope string) bool {
	for _, granted := range p.Scopes {
		if granted == scope {
			return true
		}
	}
	return false
}

// RequestHandler handles one capability- and grant-authorized request.
type RequestHandler func(context.Context, Principal, json.RawMessage) (any, error)

// RPCError is a typed error returned by a remote RPC endpoint.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("mesh RPC %s: %s", e.Code, e.Message)
}

// Event is a generic, capability-gated application update. The application
// layer owns event names and schemas.
type Event struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data,omitempty"`
}

type PeerEvent struct {
	PeerID string
	Event  Event
}

type eventSubscriber struct {
	ch    chan PeerEvent
	peers map[string]eventSubscriberPeerState
}

// eventSubscriberPeerState keeps loss recovery scoped to the authenticated
// source peer. gapPending means a gap has been successfully delivered to the
// subscriber channel and covers subsequent dropped normal events until that
// peer's next normal event is delivered. dirty means a gap itself (or an event
// not already covered by a delivered gap) was dropped and must be retried before
// that peer's next normal delivery.
type eventSubscriberPeerState struct {
	dirty      bool
	gapPending bool
}

type registeredMethod struct {
	spec    MethodSpec
	handler RequestHandler
}

type rpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type callResult struct {
	response rpcResponse
	err      error
}

func validateMethodSpec(spec MethodSpec, requireScope bool) error {
	if !safeMethod.MatchString(spec.Name) {
		return fmt.Errorf("invalid mesh RPC method %q", spec.Name)
	}
	if !safeScope.MatchString(spec.Capability) {
		return fmt.Errorf("invalid mesh RPC capability %q", spec.Capability)
	}
	if requireScope && !safeScope.MatchString(spec.RequiredScope) {
		return fmt.Errorf("mesh RPC handler %q requires an explicit authorization scope", spec.Name)
	}
	if spec.RequiredScope != "" && !safeScope.MatchString(spec.RequiredScope) {
		return fmt.Errorf("invalid mesh RPC scope %q", spec.RequiredScope)
	}
	return nil
}

// RegisterMethod records the capability contract needed by Call. Repeating an
// identical registration is safe; conflicting registrations are rejected.
func (m *Manager) RegisterMethod(spec MethodSpec) error {
	if err := validateMethodSpec(spec, false); err != nil {
		return err
	}
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()
	if existing, ok := m.handlers[spec.Name]; ok {
		if existing.spec != spec {
			return fmt.Errorf("mesh RPC method %q already has a different specification", spec.Name)
		}
		return nil
	}
	m.handlers[spec.Name] = registeredMethod{spec: spec}
	return nil
}

// RegisterHandler registers a receiver and its mandatory authorization scope.
// The handler is never invoked unless the authenticated peer has that scope.
func (m *Manager) RegisterHandler(spec MethodSpec, handler RequestHandler) error {
	if handler == nil {
		return errors.New("mesh RPC handler is nil")
	}
	if err := validateMethodSpec(spec, true); err != nil {
		return err
	}
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()
	if existing, ok := m.handlers[spec.Name]; ok {
		if existing.spec != spec {
			return fmt.Errorf("mesh RPC method %q already has a different specification", spec.Name)
		}
		if existing.handler != nil {
			return fmt.Errorf("mesh RPC method %q already has a handler", spec.Name)
		}
	}
	m.handlers[spec.Name] = registeredMethod{spec: spec, handler: handler}
	return nil
}

// SetCapabilities replaces capabilities advertised in future handshakes. It
// must be called before Start. Protocol v1 and its websocket path/subprotocol
// remain unchanged, so peers without this field continue to connect and ping.
func (m *Manager) SetCapabilities(capabilities ...string) error {
	normalized, err := normalizeCapabilities(capabilities)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("mesh capabilities cannot change after Start")
	}
	m.capabilities = normalized
	return nil
}

func (m *Manager) capabilitiesSnapshot() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.capabilities...)
}

// NegotiatedCapabilities returns a snapshot of capabilities agreed with peer.
func (m *Manager) NegotiatedCapabilities(peerID string) []string {
	m.mu.RLock()
	p := m.peers[peerID]
	m.mu.RUnlock()
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.capabilities))
	for capability := range p.capabilities {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

func normalizeCapabilities(capabilities []string) ([]string, error) {
	seen := make(map[string]bool, len(capabilities))
	out := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if !safeScope.MatchString(capability) {
			return nil, fmt.Errorf("invalid mesh capability %q", capability)
		}
		if !seen[capability] {
			seen[capability] = true
			out = append(out, capability)
		}
	}
	sort.Strings(out)
	return out, nil
}

func intersectCapabilities(local, remote []string) []string {
	remoteSet := make(map[string]bool, len(remote))
	for _, capability := range remote {
		if safeScope.MatchString(capability) {
			remoteSet[capability] = true
		}
	}
	out := make([]string, 0, len(local))
	for _, capability := range local {
		if remoteSet[capability] {
			out = append(out, capability)
		}
	}
	sort.Strings(out)
	return out
}

func (p *peerRuntime) supports(capability string) bool {
	_, ok := p.capabilities[capability]
	return ok
}

func (m *Manager) method(name string) (registeredMethod, bool) {
	m.handlersMu.RLock()
	defer m.handlersMu.RUnlock()
	method, ok := m.handlers[name]
	return method, ok
}

// Call invokes a registered method on an authenticated peer. Calls are
// bounded per connection and cleaned up on timeout, cancellation, replacement,
// or disconnect. A timed-out request emits a best-effort cancel frame.
func (m *Manager) Call(ctx context.Context, peerID, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	registered, ok := m.method(method)
	if !ok {
		return fmt.Errorf("%w: %s", ErrMethodNotRegistered, method)
	}
	m.mu.RLock()
	p := m.peers[peerID]
	m.mu.RUnlock()
	if p == nil {
		return fmt.Errorf("%w: %s", ErrPeerDisconnected, peerID)
	}
	for _, capability := range []string{CapabilityRPCV1, registered.spec.Capability} {
		if !p.supports(capability) {
			return fmt.Errorf("%w: peer %q did not negotiate %s", ErrCapabilityUnavailable, peerID, capability)
		}
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal mesh RPC params: %w", err)
	}
	payload, err := json.Marshal(rpcRequest{Method: method, Params: paramsJSON})
	if err != nil {
		return err
	}
	if len(payload) > MaxApplicationPayload {
		return ErrPayloadTooLarge
	}
	id := fmt.Sprintf("rpc-%d-%d", p.generation, m.gen.Add(1))
	response := make(chan callResult, 1)
	p.rpcMu.Lock()
	select {
	case <-p.done:
		p.rpcMu.Unlock()
		return ErrPeerDisconnected
	default:
	}
	if len(p.rpcPending) >= MaxPendingCalls {
		p.rpcMu.Unlock()
		return ErrTooManyPendingCalls
	}
	p.rpcPending[id] = response
	p.rpcMu.Unlock()
	if err := m.enqueueApp(p, Envelope{Version: ProtocolVersion, Type: "request", ID: id, SentAt: nowString(), Payload: payload}); err != nil {
		p.removeRPCCall(id)
		return err
	}
	select {
	case received := <-response:
		if received.err != nil {
			return received.err
		}
		if received.response.Error != nil {
			return received.response.Error
		}
		if result != nil && len(received.response.Result) > 0 {
			if err := json.Unmarshal(received.response.Result, result); err != nil {
				return fmt.Errorf("decode mesh RPC result: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		if p.removeRPCCall(id) {
			_ = m.enqueueApp(p, Envelope{Version: ProtocolVersion, Type: "cancel", ID: id, SentAt: nowString()})
		}
		return ctx.Err()
	case <-p.done:
		p.removeRPCCall(id)
		return ErrPeerDisconnected
	}
}

func (p *peerRuntime) removeRPCCall(id string) bool {
	p.rpcMu.Lock()
	_, ok := p.rpcPending[id]
	delete(p.rpcPending, id)
	p.rpcMu.Unlock()
	return ok
}

func (m *Manager) enqueueApp(p *peerRuntime, env Envelope) error {
	select {
	case <-p.done:
		return ErrPeerDisconnected
	default:
	}
	select {
	case p.app <- env:
		return nil
	default:
		return ErrBackpressure
	}
}

func (m *Manager) enqueueEvent(p *peerRuntime, env Envelope) error {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	select {
	case <-p.done:
		return ErrPeerDisconnected
	default:
	}

	// A caller-provided events.gap is already the reserved loss signal. Fold it
	// together with any pending transport loss so a saturated peer sees one gap,
	// not an unbounded stream of equivalent markers.
	if env.eventGap {
		if p.eventGapSent {
			return nil
		}
		if tryEnqueueEvent(p, env) {
			p.eventDirty = false
			p.eventGapSent = true
			return nil
		}
		p.eventDirty = true
		return ErrBackpressure
	}

	// Once any normal event is lost, a gap must be queued before a later normal
	// event. Keep the dirty bit until queue capacity actually exists.
	if p.eventDirty && !p.eventGapSent {
		if !tryEnqueueEvent(p, meshGapEnvelope()) {
			return ErrBackpressure
		}
		p.eventDirty = false
		p.eventGapSent = true
	}
	if tryEnqueueEvent(p, env) {
		return nil
	}
	// A queued gap covers normal events dropped behind it. Once that gap is
	// written, a later overflow starts a new dirty epoch.
	if !p.eventGapSent {
		p.eventDirty = true
	}
	return ErrBackpressure
}

func tryEnqueueEvent(p *peerRuntime, env Envelope) bool {
	select {
	case p.events <- env:
		return true
	default:
		return false
	}
}

func meshGapEnvelope() Envelope {
	payload, _ := json.Marshal(Event{Name: EventGapName})
	return Envelope{
		Version: ProtocolVersion, Type: "event", SentAt: nowString(), Payload: payload,
		eventGap: true,
	}
}

// eventDelivered advances loss bookkeeping after the writer has successfully
// put an event on the wire. It also flushes a pending gap when the writer itself
// is what frees queue capacity, so recovery does not require another producer.
func (m *Manager) eventDelivered(p *peerRuntime, env Envelope) {
	p.eventMu.Lock()
	defer p.eventMu.Unlock()
	select {
	case <-p.done:
		p.eventDirty = false
		p.eventGapSent = false
		return
	default:
	}
	if env.eventGap {
		p.eventGapSent = false
	}
	if p.eventDirty && !p.eventGapSent && tryEnqueueEvent(p, meshGapEnvelope()) {
		p.eventDirty = false
		p.eventGapSent = true
	}
}

func (m *Manager) handleApplicationEnvelope(p *peerRuntime, env Envelope) {
	if len(env.Payload) > MaxApplicationPayload {
		m.disconnect(p, ErrPayloadTooLarge)
		return
	}
	switch env.Type {
	case "request":
		m.handleRequest(p, env)
	case "response":
		m.handleResponse(p, env)
	case "cancel":
		m.handleCancel(p, env)
	case "event":
		m.handleEvent(p, env)
	}
}

func (m *Manager) handleRequest(p *peerRuntime, env Envelope) {
	if env.ID == "" || !p.supports(CapabilityRPCV1) {
		m.disconnect(p, errors.New("request without id or negotiated RPC capability"))
		return
	}
	var request rpcRequest
	if err := json.Unmarshal(env.Payload, &request); err != nil || !safeMethod.MatchString(request.Method) {
		m.disconnect(p, errors.New("malformed mesh RPC request"))
		return
	}
	method, ok := m.method(request.Method)
	if !ok || method.handler == nil {
		m.sendRPCError(p, env.ID, ErrorUnsupportedMethod, "method is not supported")
		return
	}
	if !p.supports(method.spec.Capability) {
		m.sendRPCError(p, env.ID, ErrorCapabilityRequired, "required capability was not negotiated")
		return
	}
	principal := m.principalFor(p.peerID)
	if !principal.HasScope(method.spec.RequiredScope) {
		m.sendRPCError(p, env.ID, ErrorPermissionDenied, "peer is not granted the required scope")
		return
	}
	handlerCtx, cancel := context.WithCancel(m.ctx)
	p.rpcMu.Lock()
	if len(p.inflight) >= MaxPendingCalls {
		p.rpcMu.Unlock()
		cancel()
		m.sendRPCError(p, env.ID, ErrorBackpressure, "too many in-flight requests")
		return
	}
	if _, exists := p.inflight[env.ID]; exists {
		p.rpcMu.Unlock()
		cancel()
		m.sendRPCError(p, env.ID, ErrorInvalidRequest, "duplicate request id")
		return
	}
	p.inflight[env.ID] = cancel
	p.rpcMu.Unlock()
	go m.runHandler(handlerCtx, p, env.ID, method.handler, principal, request.Params)
}

func (m *Manager) runHandler(ctx context.Context, p *peerRuntime, id string, handler RequestHandler, principal Principal, params json.RawMessage) {
	var result any
	var handlerErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				handlerErr = fmt.Errorf("handler panic: %v", recovered)
			}
		}()
		result, handlerErr = handler(ctx, principal, params)
	}()
	p.rpcMu.Lock()
	cancel, active := p.inflight[id]
	delete(p.inflight, id)
	p.rpcMu.Unlock()
	canceled := ctx.Err() != nil
	if cancel != nil {
		cancel()
	}
	if !active || canceled {
		return
	}
	if handlerErr != nil {
		if typed, ok := handlerErr.(*RPCError); ok {
			m.sendRPCError(p, id, typed.Code, typed.Message)
		} else {
			m.sendRPCError(p, id, ErrorInternal, "request failed")
		}
		return
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		m.sendRPCError(p, id, ErrorInternal, "result encoding failed")
		return
	}
	payload, err := json.Marshal(rpcResponse{Result: resultJSON})
	if err != nil || len(payload) > MaxApplicationPayload {
		m.sendRPCError(p, id, ErrorPayloadTooLarge, "response exceeds application payload limit")
		return
	}
	_ = m.enqueueApp(p, Envelope{Version: ProtocolVersion, Type: "response", ID: id, SentAt: nowString(), Payload: payload})
}

func (m *Manager) sendRPCError(p *peerRuntime, id, code, message string) {
	if !safeScope.MatchString(code) {
		code = ErrorInternal
	}
	if len(message) > 240 {
		message = message[:240]
	}
	payload, _ := json.Marshal(rpcResponse{Error: &RPCError{Code: code, Message: message}})
	_ = m.enqueueApp(p, Envelope{Version: ProtocolVersion, Type: "response", ID: id, SentAt: nowString(), Payload: payload})
}

func (m *Manager) handleResponse(p *peerRuntime, env Envelope) {
	if env.ID == "" || !p.supports(CapabilityRPCV1) {
		m.disconnect(p, errors.New("response without id or negotiated RPC capability"))
		return
	}
	var response rpcResponse
	if err := json.Unmarshal(env.Payload, &response); err != nil || (len(response.Result) == 0 && response.Error == nil) || (len(response.Result) > 0 && response.Error != nil) {
		m.disconnect(p, errors.New("malformed mesh RPC response"))
		return
	}
	if response.Error != nil && (!safeScope.MatchString(response.Error.Code) || len(response.Error.Message) > 240) {
		m.disconnect(p, errors.New("malformed mesh RPC error"))
		return
	}
	p.rpcMu.Lock()
	waiter := p.rpcPending[env.ID]
	delete(p.rpcPending, env.ID)
	p.rpcMu.Unlock()
	if waiter != nil {
		waiter <- callResult{response: response}
	}
}

func (m *Manager) handleCancel(p *peerRuntime, env Envelope) {
	if env.ID == "" || len(env.Payload) != 0 || !p.supports(CapabilityRPCV1) {
		m.disconnect(p, errors.New("malformed mesh RPC cancel"))
		return
	}
	p.rpcMu.Lock()
	cancel := p.inflight[env.ID]
	delete(p.inflight, env.ID)
	p.rpcMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) principalFor(peerID string) Principal {
	grants := append([]string(nil), m.registry.Grants[peerID]...)
	sort.Strings(grants)
	return Principal{PeerID: peerID, Scopes: grants}
}

// SendEvent sends one authorized, bounded event. A full event queue returns
// ErrBackpressure without affecting heartbeat/control traffic or local sessions.
func (m *Manager) SendEvent(peerID string, event Event) error {
	if !safeMethod.MatchString(event.Name) {
		return fmt.Errorf("invalid mesh event name %q", event.Name)
	}
	m.mu.RLock()
	p := m.peers[peerID]
	m.mu.RUnlock()
	if p == nil {
		return ErrPeerDisconnected
	}
	if !p.supports(CapabilityEventsV1) {
		return fmt.Errorf("%w: peer %q did not negotiate %s", ErrCapabilityUnavailable, peerID, CapabilityEventsV1)
	}
	if !m.principalFor(peerID).HasScope(ScopeEventsRead) {
		return &RPCError{Code: ErrorPermissionDenied, Message: "peer is not granted events.read"}
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(payload) > MaxApplicationPayload {
		return ErrPayloadTooLarge
	}
	return m.enqueueEvent(p, Envelope{
		Version: ProtocolVersion, Type: "event", SentAt: nowString(), Payload: payload,
		eventGap: event.Name == EventGapName,
	})
}

// SubscribeEvents returns a bounded local subscription. Slow subscribers drop
// events independently and never block the mesh reader or heartbeat.
func (m *Manager) SubscribeEvents(buffer int) (<-chan PeerEvent, func()) {
	if buffer < 1 {
		buffer = 1
	}
	id := m.eventSubID.Add(1)
	subscriber := &eventSubscriber{
		ch:    make(chan PeerEvent, buffer),
		peers: make(map[string]eventSubscriberPeerState),
	}
	m.eventsMu.Lock()
	m.eventSubs[id] = subscriber
	m.eventsMu.Unlock()
	var once sync.Once
	return subscriber.ch, func() {
		once.Do(func() {
			m.eventsMu.Lock()
			if existing, ok := m.eventSubs[id]; ok {
				delete(m.eventSubs, id)
				existing.peers = nil
				close(existing.ch)
			}
			m.eventsMu.Unlock()
		})
	}
}

func (m *Manager) handleEvent(p *peerRuntime, env Envelope) {
	if env.ID != "" || !p.supports(CapabilityEventsV1) {
		m.disconnect(p, errors.New("malformed event or events capability not negotiated"))
		return
	}
	var event Event
	if err := json.Unmarshal(env.Payload, &event); err != nil || !safeMethod.MatchString(event.Name) {
		m.disconnect(p, errors.New("malformed mesh event"))
		return
	}
	received := PeerEvent{PeerID: p.peerID, Event: event}
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	for _, subscriber := range m.eventSubs {
		state := subscriber.peers[p.peerID]
		if event.Name == EventGapName {
			// A received gap is an independent authenticated loss signal. Never
			// suppress it merely because an earlier gap from this peer was
			// delivered; that old marker may already have been consumed. If this
			// delivery fails, remember the loss per peer and synthesize one gap at
			// the next opportunity for the same peer.
			if tryPublishEvent(subscriber.ch, received) {
				state.dirty = false
				state.gapPending = true
			} else {
				state.dirty = true
				state.gapPending = false
			}
			subscriber.peers[p.peerID] = state
			continue
		}
		if state.dirty {
			gap := PeerEvent{PeerID: p.peerID, Event: Event{Name: EventGapName}}
			if !tryPublishEvent(subscriber.ch, gap) {
				subscriber.peers[p.peerID] = state
				continue
			}
			state.dirty = false
			state.gapPending = true
		}
		if tryPublishEvent(subscriber.ch, received) {
			// FIFO channel order now guarantees the gap precedes this normal event.
			delete(subscriber.peers, p.peerID)
		} else if !state.gapPending {
			state.dirty = true
			subscriber.peers[p.peerID] = state
		} else {
			// The successfully delivered gap covers normal events dropped before
			// this peer's next normal delivery. Keep only this peer pending;
			// unrelated traffic cannot consume or clear its recovery epoch.
			subscriber.peers[p.peerID] = state
		}
	}
}

func tryPublishEvent(ch chan PeerEvent, event PeerEvent) bool {
	select {
	case ch <- event:
		return true
	default:
		return false
	}
}

func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }
