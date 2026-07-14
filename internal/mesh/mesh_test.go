package mesh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lin-labs/arcmux/internal/config"
)

func testConfig(addr string) config.ParsedMeshConfig {
	return config.ParsedMeshConfig{
		ListenAddr: addr, HeartbeatInterval: 20 * time.Millisecond,
		StaleAfter: 45 * time.Millisecond, DeadAfter: 90 * time.Millisecond,
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 40 * time.Millisecond,
		HandshakeTimeout: 300 * time.Millisecond, MaxMessageBytes: 1024, WriterQueue: 2,
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startManager(t *testing.T, manager *Manager, ctx context.Context) {
	t.Helper()
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start mesh manager: %v", err)
	}
}

func waitState(t *testing.T, manager *Manager, peer, state string) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range manager.Status() {
			if s.PeerID == peer && s.State == state {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("peer %s never reached %s; status=%+v", peer, state, manager.Status())
	return Status{}
}

func TestRegistryAtomicPermissionsAndHashedAccept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "mesh.json")
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	r := &Registry{Version: 1, DeviceID: "ref", Serve: true, Accept: map[string]string{"devbox": TokenHash(token)}}
	if err := SaveRegistry(path, r); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode=%o want 600", got)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), token) {
		t.Fatal("registry exposed raw token")
	}
	got, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accept["devbox"] != TokenHash(token) {
		t.Fatal("hashed accept credential changed")
	}
}

func TestRegistryRejectsBidirectionalPeerInV1(t *testing.T) {
	r := &Registry{Version: 1, DeviceID: "ref", Accept: map[string]string{"labs": "hash"}, Peers: []Peer{{ID: "labs", URL: "ws://labs/v1/mesh", Token: "raw"}}}
	if err := r.Validate(); err == nil {
		t.Fatal("v1 accepted a peer in both directions")
	}
}

func TestRegistryRejectsDuplicateAcceptedCredentialWithoutExposingIt(t *testing.T) {
	credential := TokenHash("same-secret-credential")
	r := &Registry{
		Version: 1, DeviceID: "ref",
		Accept: map[string]string{"devbox": credential, "labs": credential},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("registry accepted one inbound credential for two peer identities")
	}
	if strings.Contains(err.Error(), credential) || strings.Contains(err.Error(), "same-secret-credential") {
		t.Fatalf("validation error exposed credential material: %v", err)
	}
}

func TestRegistryRejectsInsecurePermissionsAndSymlink(t *testing.T) {
	dir := t.TempDir()
	insecure := filepath.Join(dir, "insecure.json")
	if err := os.WriteFile(insecure, []byte(`{"version":1,"device_id":"ref","serve":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(insecure); err == nil {
		t.Fatal("world-readable mesh registry accepted")
	}
	secure := filepath.Join(dir, "secure.json")
	if err := os.WriteFile(secure, []byte(`{"version":1,"device_id":"ref","serve":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "mesh-link.json")
	if err := os.Symlink(secure, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(link); err == nil {
		t.Fatal("symlinked mesh registry accepted")
	}
}

func TestFullJitterBoundsPreventBusyLoop(t *testing.T) {
	min, max := 500*time.Millisecond, 30*time.Second
	for attempt := 1; attempt <= 20; attempt++ {
		cap := min
		for i := 1; i < attempt && cap < max; i++ {
			cap *= 2
		}
		if cap > max {
			cap = max
		}
		for i := 0; i < 100; i++ {
			d := fullJitter(attempt, min, max)
			if d < min/4 || d > cap {
				t.Fatalf("attempt %d delay %v outside [%v,%v]", attempt, d, min/4, cap)
			}
		}
	}
}

func TestConnectPingAndGracefulShutdown(t *testing.T) {
	token, _ := NewToken()
	server := New(testConfig("127.0.0.1:0"), &Registry{Version: 1, DeviceID: "server", Serve: true, Accept: map[string]string{"client": TokenHash(token)}}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startManager(t, server, ctx)
	client := New(testConfig("127.0.0.1:0"), &Registry{Version: 1, DeviceID: "client", Accept: map[string]string{}, Peers: []Peer{{ID: "server", URL: "ws://" + server.Addr() + meshPath, Token: token}}}, testLogger())
	startManager(t, client, ctx)
	waitState(t, client, "server", "connected")
	pingCtx, pingCancel := context.WithTimeout(ctx, time.Second)
	defer pingCancel()
	if _, err := client.Ping(pingCtx, "server"); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	client.Stop(stopCtx)
	server.Stop(stopCtx)
}

func TestAcceptedInboundPeersVisibleOfflineAndSorted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(testConfig("127.0.0.1:0"), &Registry{Version: 1, DeviceID: "server", Accept: map[string]string{"z-peer": "hash-z", "a-peer": "hash-a"}}, testLogger())
	startManager(t, m, ctx)
	status := m.Status()
	if len(status) != 2 || status[0].PeerID != "a-peer" || status[1].PeerID != "z-peer" {
		t.Fatalf("status not deterministic: %+v", status)
	}
	for _, s := range status {
		if s.Direction != "inbound" || s.State != "disconnected" {
			t.Fatalf("configured inbound peer not visible offline: %+v", s)
		}
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	m.Stop(stopCtx)
}

func TestReconnectAfterDelayedStartDropAndRestart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	token, _ := NewToken()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(testConfig("127.0.0.1:0"), &Registry{Version: 1, DeviceID: "client", Accept: map[string]string{}, Peers: []Peer{{ID: "server", URL: "ws://" + addr + meshPath, Token: token}}}, testLogger())
	startManager(t, client, ctx)
	waitState(t, client, "server", "disconnected")
	newServer := func() *Manager {
		s := New(testConfig(addr), &Registry{Version: 1, DeviceID: "server", Serve: true, Accept: map[string]string{"client": TokenHash(token)}}, testLogger())
		startManager(t, s, ctx)
		return s
	}
	server := newServer()
	waitState(t, client, "server", "connected")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	server.Stop(stopCtx)
	stopCancel()
	waitState(t, client, "server", "disconnected")
	server = newServer()
	waitState(t, client, "server", "connected")
	stopCtx, stopCancel = context.WithTimeout(context.Background(), time.Second)
	client.Stop(stopCtx)
	server.Stop(stopCtx)
	stopCancel()
}

func dialRaw(t *testing.T, addr, token, device string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+meshPath, &websocket.DialOptions{HTTPHeader: headers, Subprotocols: []string{subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := writeEnvelope(ctx, conn, Envelope{Version: 1, Type: "hello", DeviceID: device}); err != nil {
		t.Fatal(err)
	}
	if env, err := readEnvelope(ctx, conn); err != nil || env.Type != "welcome" {
		t.Fatalf("welcome=%+v err=%v", env, err)
	}
	return conn
}

func dialUnhandshaken(t *testing.T, addr, token string, protocols []string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+meshPath, &websocket.DialOptions{HTTPHeader: headers, Subprotocols: protocols})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestAuthSubprotocolMalformedOversizeAndDuplicateIsolation(t *testing.T) {
	token, _ := NewToken()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := New(testConfig("127.0.0.1:0"), &Registry{Version: 1, DeviceID: "server", Serve: true, Accept: map[string]string{"client": TokenHash(token)}}, testLogger())
	startManager(t, server, ctx)
	defer func() { c, cc := context.WithTimeout(context.Background(), time.Second); defer cc(); server.Stop(c) }()

	dialCtx, dialCancel := context.WithTimeout(ctx, time.Second)
	_, resp, err := websocket.Dial(dialCtx, "ws://"+server.Addr()+meshPath, &websocket.DialOptions{Subprotocols: []string{subprotocol}})
	dialCancel()
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dial err=%v response=%v", err, resp)
	}

	noProtocol := dialUnhandshaken(t, server.Addr(), token, nil)
	readCtx, readCancel := context.WithTimeout(ctx, time.Second)
	if _, _, err := noProtocol.Read(readCtx); err == nil {
		t.Fatal("server accepted missing subprotocol")
	}
	readCancel()

	for _, hello := range []Envelope{{Version: 2, Type: "hello", DeviceID: "client"}, {Version: 1, Type: "hello", DeviceID: "impostor"}} {
		conn := dialUnhandshaken(t, server.Addr(), token, []string{subprotocol})
		hctx, hcancel := context.WithTimeout(ctx, time.Second)
		if err := writeEnvelope(hctx, conn, hello); err != nil {
			t.Fatal(err)
		}
		if _, err := readEnvelope(hctx, conn); err == nil {
			t.Fatalf("server accepted bad hello %+v", hello)
		}
		hcancel()
	}

	first := dialRaw(t, server.Addr(), token, "client")
	second := dialRaw(t, server.Addr(), token, "client")
	waitState(t, server, "client", "connected")
	_ = first.Close(websocket.StatusNormalClosure, "old duplicate")
	time.Sleep(30 * time.Millisecond)
	waitState(t, server, "client", "connected")

	badCtx, badCancel := context.WithTimeout(ctx, time.Second)
	if err := second.Write(badCtx, websocket.MessageText, []byte("{bad")); err != nil {
		t.Fatal(err)
	}
	badCancel()
	waitState(t, server, "client", "disconnected")

	binary := dialRaw(t, server.Addr(), token, "client")
	binCtx, binCancel := context.WithTimeout(ctx, time.Second)
	if err := binary.Write(binCtx, websocket.MessageBinary, []byte(`{"version":1,"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	binCancel()
	waitState(t, server, "client", "disconnected")

	oversize := dialRaw(t, server.Addr(), token, "client")
	overCtx, overCancel := context.WithTimeout(ctx, time.Second)
	if err := oversize.Write(overCtx, websocket.MessageText, make([]byte, 2048)); err != nil {
		t.Fatal(err)
	}
	overCancel()
	waitState(t, server, "client", "disconnected")
}

func TestReplacementClearsOldPeerEventLossState(t *testing.T) {
	token, _ := NewToken()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := New(testConfig("127.0.0.1:0"), &Registry{
		Version: 1, DeviceID: "server", Serve: true,
		Accept: map[string]string{"client": TokenHash(token)},
	}, testLogger())
	startManager(t, server, ctx)
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		server.Stop(stopCtx)
	}()

	first := dialRaw(t, server.Addr(), token, "client")
	defer first.CloseNow()
	server.mu.RLock()
	old := server.peers["client"]
	server.mu.RUnlock()
	old.eventMu.Lock()
	old.eventDirty = true
	old.eventGapSent = true
	old.eventMu.Unlock()

	second := dialRaw(t, server.Addr(), token, "client")
	defer second.CloseNow()
	select {
	case <-old.done:
	case <-time.After(time.Second):
		t.Fatal("replacement did not close old peer runtime")
	}
	old.eventMu.Lock()
	dirty, gapSent := old.eventDirty, old.eventGapSent
	old.eventMu.Unlock()
	if dirty || gapSent {
		t.Fatalf("replacement retained old event loss state: dirty=%v gap_sent=%v", dirty, gapSent)
	}
}

func TestInboundPeerTransitionsStaleThenDead(t *testing.T) {
	token, _ := NewToken()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := New(testConfig("127.0.0.1:0"), &Registry{Version: 1, DeviceID: "server", Serve: true, Accept: map[string]string{"client": TokenHash(token)}}, testLogger())
	startManager(t, server, ctx)
	conn := dialRaw(t, server.Addr(), token, "client")
	defer conn.CloseNow()
	waitState(t, server, "client", "stale")
	waitState(t, server, "client", "dead")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	server.Stop(stopCtx)
}

func TestBackpressureAndPingTimeoutCleanup(t *testing.T) {
	p := &peerRuntime{send: make(chan Envelope, 1), done: make(chan struct{}), pending: map[string]chan time.Duration{}}
	m := &Manager{}
	if err := m.enqueue(p, Envelope{}); err != nil {
		t.Fatal(err)
	}
	if err := m.enqueue(p, Envelope{}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("err=%v want ErrBackpressure", err)
	}

	p.pending["x"] = make(chan time.Duration)
	p.removePending("x")
	if len(p.pending) != 0 {
		t.Fatal("timed-out ping remained pending")
	}
}

func TestListenerBindFailureIsReturned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	m := New(testConfig(ln.Addr().String()), &Registry{Version: 1, DeviceID: "server", Serve: true, Accept: map[string]string{}}, testLogger())
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("occupied mesh listener reported successful start")
	}
}

func TestRapidAcceptDropCarriesBackoffAcrossHandshakes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}, Subprotocols: []string{subprotocol}})
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if _, err := readEnvelope(ctx, conn); err != nil {
			return
		}
		if err := writeEnvelope(ctx, conn, Envelope{Version: 1, Type: "welcome", DeviceID: "server"}); err != nil {
			return
		}
		_ = conn.CloseNow()
	}))
	defer server.Close()

	cfg := testConfig("127.0.0.1:0")
	cfg.ReconnectMin = 20 * time.Millisecond
	cfg.ReconnectMax = 200 * time.Millisecond
	cfg.DeadAfter = time.Second
	client := New(cfg, &Registry{Version: 1, DeviceID: "client", Peers: []Peer{{
		ID: "server", URL: "ws" + strings.TrimPrefix(server.URL, "http") + meshPath, Token: "unused",
	}}}, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startManager(t, client, ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, status := range client.Status() {
			if status.PeerID == "server" && status.Attempts >= 3 {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
				defer stopCancel()
				client.Stop(stopCtx)
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("rapid handshake drops kept resetting backoff: %+v", client.Status())
}
