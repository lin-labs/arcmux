package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lin-labs/arcmux/internal/config"
)

func rpcTestConfig(addr string) config.ParsedMeshConfig {
	cfg := testConfig(addr)
	cfg.MaxMessageBytes = 64 << 10
	cfg.WriterQueue = 64
	cfg.HeartbeatInterval = 50 * time.Millisecond
	cfg.StaleAfter = 300 * time.Millisecond
	cfg.DeadAfter = 600 * time.Millisecond
	return cfg
}

func startRPCPair(t *testing.T, serverGrants []string, serverCaps, clientCaps []string) (*Manager, *Manager) {
	t.Helper()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverRegistry := &Registry{
		Version: RegistryVersion, DeviceID: "server", Serve: true,
		Accept: map[string]string{"client": TokenHash(token)},
		Grants: map[string][]string{"client": append([]string(nil), serverGrants...)},
	}
	server := New(rpcTestConfig("127.0.0.1:0"), serverRegistry, testLogger())
	if serverCaps != nil {
		if err := server.SetCapabilities(serverCaps...); err != nil {
			t.Fatal(err)
		}
	}
	startManager(t, server, ctx)
	clientRegistry := &Registry{
		Version: RegistryVersion, DeviceID: "client", Accept: map[string]string{}, Grants: map[string][]string{},
		Peers: []Peer{{ID: "server", URL: "ws://" + server.Addr() + meshPath, Token: token}},
	}
	client := New(rpcTestConfig("127.0.0.1:0"), clientRegistry, testLogger())
	if clientCaps != nil {
		if err := client.SetCapabilities(clientCaps...); err != nil {
			t.Fatal(err)
		}
	}
	startManager(t, client, ctx)
	waitState(t, client, "server", "connected")
	waitState(t, server, "client", "connected")
	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		client.Stop(stopCtx)
		server.Stop(stopCtx)
	})
	return server, client
}

func TestCapabilityIntersectionAndOldPeerCompatibility(t *testing.T) {
	t.Run("exact intersection", func(t *testing.T) {
		serverCaps := []string{CapabilityRPCV1, CapabilitySessionsReadV1}
		clientCaps := []string{CapabilityArtifactsReadV1, CapabilityRPCV1, CapabilitySessionsReadV1}
		server, client := startRPCPair(t, nil, serverCaps, clientCaps)
		want := []string{CapabilityRPCV1, CapabilitySessionsReadV1}
		if got := client.NegotiatedCapabilities("server"); !reflect.DeepEqual(got, want) {
			t.Fatalf("client capabilities=%v want %v", got, want)
		}
		if got := server.NegotiatedCapabilities("client"); !reflect.DeepEqual(got, want) {
			t.Fatalf("server capabilities=%v want %v", got, want)
		}
	})

	t.Run("old client keeps pinging new server", func(t *testing.T) {
		token, _ := NewToken()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server := New(rpcTestConfig("127.0.0.1:0"), &Registry{
			Version: 1, DeviceID: "server", Serve: true,
			Accept: map[string]string{"old-client": TokenHash(token)},
		}, testLogger())
		startManager(t, server, ctx)
		defer func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
			defer stopCancel()
			server.Stop(stopCtx)
		}()
		conn := dialRaw(t, server.Addr(), token, "old-client")
		defer conn.CloseNow()
		if got := server.NegotiatedCapabilities("old-client"); len(got) != 0 {
			t.Fatalf("old client unexpectedly negotiated %v", got)
		}
		ping := Envelope{Version: 1, Type: "ping", ID: "old-ping"}
		ioCtx, ioCancel := context.WithTimeout(ctx, time.Second)
		defer ioCancel()
		if err := writeEnvelope(ioCtx, conn, ping); err != nil {
			t.Fatal(err)
		}
		response, err := readEnvelope(ioCtx, conn)
		if err != nil || response.Type != "pong" || response.ID != ping.ID {
			t.Fatalf("pong=%+v err=%v", response, err)
		}
	})

	t.Run("new client keeps pinging old server", func(t *testing.T) {
		oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}, Subprotocols: []string{subprotocol}})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_, err = readEnvelope(r.Context(), conn)
			if err != nil {
				return
			}
			if err := writeEnvelope(r.Context(), conn, Envelope{Version: 1, Type: "welcome", DeviceID: "old-server"}); err != nil {
				return
			}
			for {
				env, err := readEnvelope(r.Context(), conn)
				if err != nil {
					return
				}
				if env.Type == "ping" {
					_ = writeEnvelope(r.Context(), conn, Envelope{Version: 1, Type: "pong", ID: env.ID})
				}
			}
		}))
		defer oldServer.Close()
		client := New(rpcTestConfig("127.0.0.1:0"), &Registry{
			Version: 1, DeviceID: "new-client", Accept: map[string]string{},
			Peers: []Peer{{ID: "old-server", URL: "ws" + strings.TrimPrefix(oldServer.URL, "http") + meshPath, Token: "unused"}},
		}, testLogger())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startManager(t, client, ctx)
		waitState(t, client, "old-server", "connected")
		if got := client.NegotiatedCapabilities("old-server"); len(got) != 0 {
			t.Fatalf("old server unexpectedly negotiated %v", got)
		}
		pingCtx, pingCancel := context.WithTimeout(ctx, time.Second)
		defer pingCancel()
		if _, err := client.Ping(pingCtx, "old-server"); err != nil {
			t.Fatalf("ping old server: %v", err)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		client.Stop(stopCtx)
	})
}

func TestRPCOutOfOrderCorrelationAndUnsupportedMethod(t *testing.T) {
	server, client := startRPCPair(t, []string{ScopeSessionsRead}, nil, nil)
	spec := MethodSpec{Name: "test.echo", Capability: CapabilitySessionsReadV1, RequiredScope: ScopeSessionsRead}
	completed := make(chan string, 2)
	if err := server.RegisterHandler(spec, func(ctx context.Context, principal Principal, raw json.RawMessage) (any, error) {
		var request struct {
			Value string        `json:"value"`
			Delay time.Duration `json:"delay"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		select {
		case <-time.After(request.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		completed <- request.Value
		return map[string]string{"value": request.Value}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterMethod(spec); err != nil {
		t.Fatal(err)
	}
	type response struct {
		Value string `json:"value"`
	}
	results := make(chan response, 2)
	errs := make(chan error, 2)
	for _, request := range []struct {
		value string
		delay time.Duration
	}{{"slow", 80 * time.Millisecond}, {"fast", 5 * time.Millisecond}} {
		request := request
		go func() {
			var got response
			err := client.Call(context.Background(), "server", spec.Name, map[string]any{"value": request.value, "delay": request.delay}, &got)
			errs <- err
			results <- got
		}()
	}
	if first := <-completed; first != "fast" {
		t.Fatalf("responses were not completed out of order: first=%q", first)
	}
	values := map[string]bool{}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		values[(<-results).Value] = true
	}
	if !values["slow"] || !values["fast"] {
		t.Fatalf("correlated results=%v", values)
	}

	unsupported := MethodSpec{Name: "test.missing", Capability: CapabilityRPCV1, RequiredScope: ScopeSessionsRead}
	if err := client.RegisterMethod(unsupported); err != nil {
		t.Fatal(err)
	}
	err := client.Call(context.Background(), "server", unsupported.Name, nil, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != ErrorUnsupportedMethod {
		t.Fatalf("unsupported error=%v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Ping(pingCtx, "server"); err != nil {
		t.Fatalf("unsupported method disconnected peer: %v", err)
	}
}

func TestRPCGrantAuthorizationIsDefaultDenyAndPrincipalIsAuthenticated(t *testing.T) {
	spec := MethodSpec{Name: "sessions.list", Capability: CapabilitySessionsReadV1, RequiredScope: ScopeSessionsRead}

	t.Run("missing grant blocks before handler", func(t *testing.T) {
		server, client := startRPCPair(t, nil, nil, nil)
		var called atomic.Bool
		if err := server.RegisterHandler(spec, func(context.Context, Principal, json.RawMessage) (any, error) {
			called.Store(true)
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := client.RegisterMethod(spec); err != nil {
			t.Fatal(err)
		}
		err := client.Call(context.Background(), "server", spec.Name, nil, nil)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != ErrorPermissionDenied {
			t.Fatalf("error=%v want permission_denied", err)
		}
		if called.Load() {
			t.Fatal("unauthorized handler was invoked")
		}
	})

	t.Run("wrong scope blocks before handler", func(t *testing.T) {
		server, client := startRPCPair(t, []string{ScopeArtifactsRead}, nil, nil)
		var called atomic.Bool
		_ = server.RegisterHandler(spec, func(context.Context, Principal, json.RawMessage) (any, error) {
			called.Store(true)
			return nil, nil
		})
		_ = client.RegisterMethod(spec)
		err := client.Call(context.Background(), "server", spec.Name, nil, nil)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != ErrorPermissionDenied || called.Load() {
			t.Fatalf("error=%v called=%v", err, called.Load())
		}
	})

	t.Run("granted handler receives authenticated principal", func(t *testing.T) {
		server, client := startRPCPair(t, []string{ScopeSessionsRead}, nil, nil)
		principalSeen := make(chan Principal, 1)
		_ = server.RegisterHandler(spec, func(_ context.Context, principal Principal, _ json.RawMessage) (any, error) {
			principalSeen <- principal
			return map[string]bool{"ok": true}, nil
		})
		_ = client.RegisterMethod(spec)
		var result map[string]bool
		if err := client.Call(context.Background(), "server", spec.Name, nil, &result); err != nil {
			t.Fatal(err)
		}
		principal := <-principalSeen
		if principal.PeerID != "client" || !reflect.DeepEqual(principal.Scopes, []string{ScopeSessionsRead}) {
			t.Fatalf("principal=%+v", principal)
		}
	})
}

func TestCallRequiresRegisteredAndNegotiatedCapability(t *testing.T) {
	_, client := startRPCPair(t, nil, []string{CapabilityRPCV1}, []string{CapabilityRPCV1, CapabilitySessionsReadV1})
	spec := MethodSpec{Name: "sessions.list", Capability: CapabilitySessionsReadV1, RequiredScope: ScopeSessionsRead}
	if err := client.RegisterMethod(spec); err != nil {
		t.Fatal(err)
	}
	err := client.Call(context.Background(), "server", spec.Name, nil, nil)
	if !errors.Is(err, ErrCapabilityUnavailable) || !strings.Contains(err.Error(), CapabilitySessionsReadV1) {
		t.Fatalf("capability error=%v", err)
	}
	if err := client.Call(context.Background(), "server", "unregistered.method", nil, nil); !errors.Is(err, ErrMethodNotRegistered) {
		t.Fatalf("unregistered error=%v", err)
	}
}

func TestRPCTimeoutCancellationAndDisconnectCleanup(t *testing.T) {
	server, client := startRPCPair(t, []string{ScopeSessionsRead}, nil, nil)
	spec := MethodSpec{Name: "test.block", Capability: CapabilityRPCV1, RequiredScope: ScopeSessionsRead}
	started := make(chan struct{}, 2)
	canceled := make(chan struct{}, 2)
	if err := server.RegisterHandler(spec, func(ctx context.Context, _ Principal, _ json.RawMessage) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return nil, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	_ = client.RegisterMethod(spec)

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer timeoutCancel()
	err := client.Call(timeoutCtx, "server", spec.Name, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
	<-started
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("remote handler did not receive cancel")
	}
	assertRPCMapsEmpty(t, client, "server")
	assertRPCMapsEmpty(t, server, "client")

	callDone := make(chan error, 1)
	go func() { callDone <- client.Call(context.Background(), "server", spec.Name, nil, nil) }()
	<-started
	client.mu.RLock()
	oldPeer := client.peers["server"]
	client.mu.RUnlock()
	client.disconnect(oldPeer, errors.New("test disconnect"))
	select {
	case err := <-callDone:
		if !errors.Is(err, ErrPeerDisconnected) {
			t.Fatalf("disconnect error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call was not released on disconnect")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel remote handler")
	}
	oldPeer.rpcMu.Lock()
	remaining := len(oldPeer.rpcPending)
	oldPeer.rpcMu.Unlock()
	if remaining != 0 {
		t.Fatalf("disconnect left %d pending calls", remaining)
	}
}

func assertRPCMapsEmpty(t *testing.T, manager *Manager, peerID string) {
	t.Helper()
	manager.mu.RLock()
	p := manager.peers[peerID]
	manager.mu.RUnlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.rpcMu.Lock()
		pending, inflight := len(p.rpcPending), len(p.inflight)
		p.rpcMu.Unlock()
		if pending == 0 && inflight == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("RPC state was not cleaned up")
}

func TestEventDeliveryBackpressureAndControlPriority(t *testing.T) {
	server, client := startRPCPair(t, []string{ScopeEventsRead}, nil, nil)
	events, unsubscribe := client.SubscribeEvents(1)
	defer unsubscribe()
	if err := server.SendEvent("client", Event{Name: "sessions.updated", Data: json.RawMessage(`{"id":"s1"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-events:
		if received.PeerID != "server" || received.Event.Name != "sessions.updated" {
			t.Fatalf("event=%+v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not delivered")
	}

	// A saturated event queue is independently bounded; heartbeat/control can
	// still enqueue and a live peer remains pingable after an event burst.
	p := &peerRuntime{
		done: make(chan struct{}), send: make(chan Envelope, 1), events: make(chan Envelope, 1),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}
	m := &Manager{
		registry: &Registry{Grants: map[string][]string{"peer": {ScopeEventsRead}}},
		peers:    map[string]*peerRuntime{"peer": p},
	}
	if err := m.SendEvent("peer", Event{Name: "test.event"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SendEvent("peer", Event{Name: "test.event"}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("event overflow=%v want ErrBackpressure", err)
	}
	first := <-p.events
	m.eventDelivered(p, first)
	gap := <-p.events
	var gapEvent Event
	if err := json.Unmarshal(gap.Payload, &gapEvent); err != nil || gapEvent.Name != EventGapName || !gap.eventGap {
		t.Fatalf("overflow gap=%+v decoded=%+v err=%v", gap, gapEvent, err)
	}
	m.eventDelivered(p, gap)
	if len(p.events) != 0 {
		t.Fatalf("overflow emitted more than one gap: queued=%d", len(p.events))
	}
	if err := m.SendEvent("peer", Event{Name: "test.recovered"}); err != nil {
		t.Fatalf("normal event did not recover after gap: %v", err)
	}
	recovered := <-p.events
	var recoveredEvent Event
	if err := json.Unmarshal(recovered.Payload, &recoveredEvent); err != nil || recoveredEvent.Name != "test.recovered" {
		t.Fatalf("recovered event=%+v err=%v", recoveredEvent, err)
	}
	if err := m.enqueue(p, Envelope{Version: 1, Type: "ping"}); err != nil {
		t.Fatalf("event overflow starved control queue: %v", err)
	}
	for range 100 {
		_ = server.SendEvent("client", Event{Name: "test.burst"})
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
	defer pingCancel()
	if _, err := client.Ping(pingCtx, "server"); err != nil {
		t.Fatalf("event burst starved heartbeat: %v", err)
	}
}

func TestLocalSubscriberOverflowDeliversOneGapBeforeNormal(t *testing.T) {
	m := &Manager{eventSubs: make(map[uint64]*eventSubscriber)}
	events, cancel := m.SubscribeEvents(1)
	defer cancel()
	p := &peerRuntime{
		peerID: "server", done: make(chan struct{}),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}

	m.handleApplicationEnvelope(p, testEventEnvelope(t, "test.one"))
	m.handleApplicationEnvelope(p, testEventEnvelope(t, "test.lost"))
	if first := <-events; first.Event.Name != "test.one" {
		t.Fatalf("first event=%+v", first)
	}

	// The first event after capacity returns is covered by the gap if the
	// one-slot channel cannot hold both. Further events while the gap is pending
	// must not create duplicate gap markers.
	m.handleApplicationEnvelope(p, testEventEnvelope(t, "test.covered"))
	m.handleApplicationEnvelope(p, testEventEnvelope(t, "test.also-covered"))
	if gap := <-events; gap.PeerID != "server" || gap.Event.Name != EventGapName {
		t.Fatalf("subscriber gap=%+v", gap)
	}
	m.handleApplicationEnvelope(p, testEventEnvelope(t, "test.recovered"))
	if recovered := <-events; recovered.Event.Name != "test.recovered" {
		t.Fatalf("subscriber recovery=%+v", recovered)
	}
	select {
	case extra := <-events:
		t.Fatalf("subscriber received duplicate gap/event: %+v", extra)
	default:
	}
}

func TestLocalSubscriberOverflowRecoveryIsScopedPerPeer(t *testing.T) {
	m := &Manager{eventSubs: make(map[uint64]*eventSubscriber)}
	events, cancel := m.SubscribeEvents(1)
	defer cancel()
	peerA := &peerRuntime{
		peerID: "peer-a", done: make(chan struct{}),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}
	peerB := &peerRuntime{
		peerID: "peer-b", done: make(chan struct{}),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}

	// Fill the subscriber with A, then lose another A event.
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "test.a-one"))
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "test.a-lost"))
	if first := <-events; first.PeerID != "peer-a" || first.Event.Name != "test.a-one" {
		t.Fatalf("first event=%+v", first)
	}

	// B may continue independently. It must not inherit or clear A's loss.
	m.handleApplicationEnvelope(peerB, testEventEnvelope(t, "test.b-one"))
	if fromB := <-events; fromB.PeerID != "peer-b" || fromB.Event.Name != "test.b-one" {
		t.Fatalf("unrelated event=%+v", fromB)
	}
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "test.a-covered"))
	if gap := <-events; gap.PeerID != "peer-a" || gap.Event.Name != EventGapName {
		t.Fatalf("peer-scoped gap=%+v", gap)
	}
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "test.a-recovered"))
	if recovered := <-events; recovered.PeerID != "peer-a" || recovered.Event.Name != "test.a-recovered" {
		t.Fatalf("peer-a recovery=%+v", recovered)
	}
}

func TestLocalSubscriberRepeatedRealGapsAreNotSuppressedForever(t *testing.T) {
	m := &Manager{eventSubs: make(map[uint64]*eventSubscriber)}
	events, cancel := m.SubscribeEvents(1)
	defer cancel()
	peerA := &peerRuntime{
		peerID: "peer-a", done: make(chan struct{}),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}

	// Once the first real gap has actually entered the subscriber channel, a
	// later independent real gap from the same peer must still be deliverable.
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, EventGapName))
	if first := <-events; first.PeerID != "peer-a" || first.Event.Name != EventGapName {
		t.Fatalf("first real gap=%+v", first)
	}
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, EventGapName))
	if second := <-events; second.PeerID != "peer-a" || second.Event.Name != EventGapName {
		t.Fatalf("second real gap=%+v", second)
	}

	// If a real gap itself is dropped, the next A event retries one A gap before
	// any A normal event. B traffic cannot satisfy that retry.
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, EventGapName))
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, EventGapName))
	if queued := <-events; queued.PeerID != "peer-a" || queued.Event.Name != EventGapName {
		t.Fatalf("queued real gap=%+v", queued)
	}
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "test.a-after-gap-loss"))
	if retried := <-events; retried.PeerID != "peer-a" || retried.Event.Name != EventGapName {
		t.Fatalf("retried real gap=%+v", retried)
	}
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "test.a-recovered"))
	if recovered := <-events; recovered.PeerID != "peer-a" || recovered.Event.Name != "test.a-recovered" {
		t.Fatalf("recovered event=%+v", recovered)
	}
}

func TestLocalSubscriberGapMapsDaemonResyncToLostPeer(t *testing.T) {
	m := &Manager{eventSubs: make(map[uint64]*eventSubscriber)}
	events, cancel := m.SubscribeEvents(1)
	defer cancel()
	peerA := &peerRuntime{
		peerID: "peer-a", done: make(chan struct{}),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}
	peerB := &peerRuntime{
		peerID: "peer-b", done: make(chan struct{}),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}

	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "sessions.changed"))
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "sessions.changed"))
	<-events
	m.handleApplicationEnvelope(peerB, testEventEnvelope(t, "sessions.changed"))
	<-events
	m.handleApplicationEnvelope(peerA, testEventEnvelope(t, "sessions.changed"))

	// This mirrors the daemon consumer: the PeerID on events.gap selects which
	// remote inventory is reconciled. A's overflow must schedule A, never B.
	resyncs := make([]string, 0, 1)
	if event := <-events; event.Event.Name == EventGapName {
		resyncs = append(resyncs, event.PeerID)
	}
	if !reflect.DeepEqual(resyncs, []string{"peer-a"}) {
		t.Fatalf("daemon-style resync peers=%v, want [peer-a]", resyncs)
	}
}

func TestGapEventKeepsOrdinaryAuthorizationAndPayloadLimits(t *testing.T) {
	p := &peerRuntime{
		peerID: "peer", done: make(chan struct{}), events: make(chan Envelope, 1),
		capabilities: map[string]struct{}{CapabilityEventsV1: {}},
	}
	m := &Manager{
		registry: &Registry{Grants: map[string][]string{}},
		peers:    map[string]*peerRuntime{"peer": p},
	}
	if err := m.SendEvent("peer", Event{Name: EventGapName}); !isRPCCode(err, ErrorPermissionDenied) {
		t.Fatalf("reserved gap bypassed events.read: %v", err)
	}
	m.registry.Grants["peer"] = []string{ScopeEventsRead}
	if err := m.SendEvent("peer", Event{Name: EventGapName, Data: json.RawMessage(`{"reason":"source-gap"}`)}); err != nil {
		t.Fatalf("authorized gap rejected: %v", err)
	}
	if err := m.SendEvent("peer", Event{Name: EventGapName, Data: json.RawMessage(`"` + strings.Repeat("x", MaxApplicationPayload) + `"`)}); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("gap bypassed payload bound: %v", err)
	}

	// Receiving a gap without events.v1 is the same protocol violation as any
	// other event; the reserved name is not a control-plane escape hatch.
	noCapability := &peerRuntime{peerID: "remote", done: make(chan struct{})}
	receiver := &Manager{
		peers:  map[string]*peerRuntime{"remote": noCapability},
		status: map[string]Status{"remote": {PeerID: "remote"}},
	}
	receiver.handleApplicationEnvelope(noCapability, testEventEnvelope(t, EventGapName))
	select {
	case <-noCapability.done:
	default:
		t.Fatal("unauthorized received gap did not disconnect peer")
	}
}

func isRPCCode(err error, code string) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == code
}

func TestConcurrentSubscriberCancelAndDelivery(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		m := &Manager{eventSubs: make(map[uint64]*eventSubscriber)}
		events, cancel := m.SubscribeEvents(4)
		p := &peerRuntime{
			peerID: "server", done: make(chan struct{}),
			capabilities: map[string]struct{}{CapabilityEventsV1: {}},
		}
		env := testEventEnvelope(t, "test.concurrent")
		var wg sync.WaitGroup
		for worker := 0; worker < 4; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 100 {
					m.handleApplicationEnvelope(p, env)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cancel()
		}()
		wg.Wait()
		for range events {
		}
	}
}

func testEventEnvelope(t *testing.T, name string) Envelope {
	t.Helper()
	payload, err := json.Marshal(Event{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return Envelope{Version: ProtocolVersion, Type: "event", Payload: payload}
}

func TestMaxPendingCallsAndPayloadLimit(t *testing.T) {
	spec := MethodSpec{Name: "test.limit", Capability: CapabilityRPCV1, RequiredScope: ScopeSessionsRead}
	p := &peerRuntime{
		generation: 1, done: make(chan struct{}), app: make(chan Envelope, MaxPendingCalls+1),
		rpcPending: make(map[string]chan callResult), capabilities: map[string]struct{}{CapabilityRPCV1: {}},
	}
	for i := range MaxPendingCalls {
		p.rpcPending[fmt.Sprintf("existing-%d", i)] = make(chan callResult, 1)
	}
	m := &Manager{
		peers: map[string]*peerRuntime{"peer": p}, handlers: map[string]registeredMethod{},
	}
	if err := m.RegisterMethod(spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Call(context.Background(), "peer", spec.Name, nil, nil); !errors.Is(err, ErrTooManyPendingCalls) {
		t.Fatalf("pending limit error=%v", err)
	}
	big := strings.Repeat("x", MaxApplicationPayload)
	if err := m.Call(context.Background(), "peer", spec.Name, big, nil); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("payload error=%v", err)
	}
}

func TestHandlerRegistrationRequiresExplicitScope(t *testing.T) {
	m := New(rpcTestConfig("127.0.0.1:0"), &Registry{}, testLogger())
	err := m.RegisterHandler(MethodSpec{Name: "test.open", Capability: CapabilityRPCV1}, func(context.Context, Principal, json.RawMessage) (any, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("handler without authorization scope was accepted")
	}
}
