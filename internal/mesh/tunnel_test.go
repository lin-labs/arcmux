package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTunnelProcess struct {
	done     chan error
	stopOnce sync.Once
	stopped  chan struct{}
}

type delayedStopTunnelProcess struct {
	done  chan error
	delay time.Duration
	once  sync.Once
}

func (p *delayedStopTunnelProcess) Done() <-chan error { return p.done }

func (p *delayedStopTunnelProcess) Stop() {
	p.once.Do(func() {
		go func() {
			time.Sleep(p.delay)
			p.done <- context.Canceled
			close(p.done)
		}()
	})
}

func newFakeTunnelProcess() *fakeTunnelProcess {
	return &fakeTunnelProcess{done: make(chan error, 1), stopped: make(chan struct{})}
}

func (p *fakeTunnelProcess) Done() <-chan error { return p.done }

func (p *fakeTunnelProcess) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopped)
		p.done <- context.Canceled
		close(p.done)
	})
}

func (p *fakeTunnelProcess) exit(err error) {
	p.stopOnce.Do(func() {
		p.done <- err
		close(p.done)
	})
}

func managedTunnelPeer(id, token, local string) Peer {
	return Peer{
		ID: id, URL: "ws://" + id + ".example:7788/v1/mesh", Token: token,
		SSHTunnel: &SSHTunnel{
			Target: id, LocalAddr: local, RemoteAddr: "127.0.0.1:7788",
		},
	}
}

func waitTransportState(t *testing.T, manager *Manager, peer, state string) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, status := range manager.Status() {
			if status.PeerID == peer && status.TransportState == state {
				return status
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("peer %s transport never reached %s; status=%+v", peer, state, manager.Status())
	return Status{}
}

func TestManagedSSHTunnelRestartsAfterProcessDeathAndRedactsSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const token = "mesh-secret-that-must-never-escape"
	peer := managedTunnelPeer("devbox", token, "127.0.0.1:18443")
	var logs bytes.Buffer
	manager := New(testConfig("127.0.0.1:0"), &Registry{
		Version: RegistryVersion, DeviceID: "ref", Peers: []Peer{peer},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	manager.probe = func(context.Context, Peer) error { return errors.New("offline") }
	manager.tunnelRetryDelay = func(int, time.Duration, time.Duration) time.Duration { return time.Millisecond }
	launched := make(chan *fakeTunnelProcess, 3)
	manager.tunnelLauncher = func(context.Context, Peer) (managedTunnelProcess, error) {
		process := newFakeTunnelProcess()
		launched <- process
		return process, nil
	}
	startManager(t, manager, ctx)

	first := <-launched
	waitTransportState(t, manager, "devbox", "running")
	first.exit(errors.New("transport failed with " + token))
	second := <-launched
	status := waitTransportState(t, manager, "devbox", "running")
	if status.TransportAttempts < 2 {
		t.Fatalf("transport attempts=%d, want restart attempt", status.TransportAttempts)
	}

	statusJSON, err := json.Marshal(manager.Status())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statusJSON), token) {
		t.Fatalf("mesh token leaked through status: %s", statusJSON)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	manager.Stop(stopCtx)
	stopCancel()
	if strings.Contains(logs.String(), token) {
		t.Fatalf("mesh token leaked through logs: %s", logs.String())
	}
	select {
	case <-second.stopped:
	case <-time.After(time.Second):
		t.Fatal("daemon stop did not terminate its managed tunnel")
	}
}

func TestManagedSSHTunnelUnreachableHostUsesBoundedBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := managedTunnelPeer("labs", "token", "127.0.0.1:18444")
	manager := New(testConfig("127.0.0.1:0"), &Registry{
		Version: RegistryVersion, DeviceID: "ref", Peers: []Peer{peer},
	}, testLogger())
	manager.probe = func(context.Context, Peer) error { return errors.New("offline") }
	var launches atomic.Int32
	manager.tunnelLauncher = func(context.Context, Peer) (managedTunnelProcess, error) {
		launches.Add(1)
		return nil, errors.New("ssh host unreachable")
	}
	type bounds struct {
		attempt  int
		min, max time.Duration
	}
	observed := make(chan bounds, 4)
	manager.tunnelRetryDelay = func(attempt int, min, max time.Duration) time.Duration {
		observed <- bounds{attempt: attempt, min: min, max: max}
		return 10 * time.Millisecond
	}
	startManager(t, manager, ctx)

	for want := 1; want <= 3; want++ {
		got := <-observed
		if got.attempt != want || got.min != manager.cfg.ReconnectMin || got.max > maxReachabilityProbeRetry {
			t.Fatalf("retry %d bounds=%+v", want, got)
		}
	}
	status := waitTransportState(t, manager, "labs", "backoff")
	if status.TransportNextRetryAt == nil || status.TransportLastError != "ssh host unreachable" {
		t.Fatalf("unreachable transport status=%+v", status)
	}
	if got := launches.Load(); got < 3 || got > 5 {
		t.Fatalf("launch count=%d, want bounded retry progress", got)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	manager.Stop(stopCtx)
}

func TestManagedSSHTunnelsOperateIndependentlyPerPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	devbox := managedTunnelPeer("devbox", "devbox-token", "127.0.0.1:18443")
	labs := managedTunnelPeer("labs", "labs-token", "127.0.0.1:18444")
	manager := New(testConfig("127.0.0.1:0"), &Registry{
		Version: RegistryVersion, DeviceID: "ref", Peers: []Peer{devbox, labs},
	}, testLogger())
	manager.probe = func(context.Context, Peer) error { return errors.New("offline") }
	manager.tunnelRetryDelay = func(int, time.Duration, time.Duration) time.Duration { return time.Millisecond }
	devboxProcess := newFakeTunnelProcess()
	var labsAttempts atomic.Int32
	manager.tunnelLauncher = func(_ context.Context, peer Peer) (managedTunnelProcess, error) {
		if peer.ID == "devbox" {
			return devboxProcess, nil
		}
		labsAttempts.Add(1)
		return nil, errors.New("labs unavailable")
	}
	startManager(t, manager, ctx)

	waitTransportState(t, manager, "devbox", "running")
	deadline := time.Now().Add(time.Second)
	for labsAttempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if labsAttempts.Load() < 3 {
		t.Fatalf("labs did not retry independently: attempts=%d", labsAttempts.Load())
	}
	if status := waitTransportState(t, manager, "devbox", "running"); status.TransportAttempts != 1 {
		t.Fatalf("labs churn disturbed devbox transport: %+v", status)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	manager.Stop(stopCtx)
}

func TestManagedSSHTunnelRecreatedAcrossDaemonRestart(t *testing.T) {
	peer := managedTunnelPeer("devbox", "token", "127.0.0.1:18443")
	registry := &Registry{Version: RegistryVersion, DeviceID: "ref", Peers: []Peer{peer}}
	var launches atomic.Int32
	var processesMu sync.Mutex
	var processes []*fakeTunnelProcess
	newManager := func() (*Manager, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		manager := New(testConfig("127.0.0.1:0"), registry, testLogger())
		manager.probe = func(context.Context, Peer) error { return errors.New("offline") }
		manager.tunnelLauncher = func(context.Context, Peer) (managedTunnelProcess, error) {
			process := newFakeTunnelProcess()
			processesMu.Lock()
			processes = append(processes, process)
			processesMu.Unlock()
			launches.Add(1)
			return process, nil
		}
		startManager(t, manager, ctx)
		return manager, cancel
	}

	first, cancelFirst := newManager()
	waitTransportState(t, first, "devbox", "running")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	first.Stop(stopCtx)
	stopCancel()
	cancelFirst()

	second, cancelSecond := newManager()
	defer cancelSecond()
	waitTransportState(t, second, "devbox", "running")
	if got := launches.Load(); got != 2 {
		t.Fatalf("tunnel launches across daemon restart=%d, want 2", got)
	}
	processesMu.Lock()
	firstProcess := processes[0]
	processesMu.Unlock()
	select {
	case <-firstProcess.stopped:
	default:
		t.Fatal("first daemon left its tunnel process running")
	}

	stopCtx, stopCancel = context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	second.Stop(stopCtx)
}

func TestSSHTunnelUsesStructuredFixedArgumentsAndLoopbackDialURL(t *testing.T) {
	peer := managedTunnelPeer("devbox", "token", "127.0.0.1:18443")
	args, err := sshTunnelArgs(peer)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-N", "-T", "BatchMode=yes", "ExitOnForwardFailure=yes",
		"ClearAllForwardings=no", "127.0.0.1:18443:127.0.0.1:7788", "devbox",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ssh args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, peer.Token) || strings.Contains(joined, "sh -c") {
		t.Fatalf("ssh args expose a token or shell surface: %q", joined)
	}
	if got := peer.DialURL(); got != "ws://127.0.0.1:18443/v1/mesh" {
		t.Fatalf("managed tunnel dial URL=%q", got)
	}
}

func TestSSHTunnelOpenSSHEffectiveConfigRetainsLocalForward(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client is not installed")
	}
	peer := managedTunnelPeer("devbox", "token", "127.0.0.1:18443")
	args, err := sshTunnelArgs(peer)
	if err != nil {
		t.Fatal(err)
	}
	// Ignore user SSH config so Match/Include rules cannot influence this
	// contract check. `ssh -G` prints effective configuration without dialing.
	effectiveArgs := append([]string{"-G", "-F", "/dev/null"}, args...)
	output, err := exec.Command("ssh", effectiveArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v: %s", err, output)
	}
	effective := strings.ToLower(string(output))
	if !strings.Contains(effective, "clearallforwardings no") ||
		!strings.Contains(effective, "localforward [127.0.0.1]:18443 [127.0.0.1]:7788") {
		t.Fatalf("OpenSSH effective config lost managed local forward:\n%s", output)
	}
}

func TestSSHTunnelRejectsInheritedForwardsBeforeStartingTransport(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	transportMarker := filepath.Join(dir, "transport-started")
	script := `#!/bin/sh
if [ "$1" = "-G" ]; then
  printf '%s\n' \
    'hostname devbox.internal' \
    'localforward 0.0.0.0:9000 127.0.0.1:9000' \
    'remoteforward 0.0.0.0:9001 127.0.0.1:9001' \
    'dynamicforward 0.0.0.0:9002'
  exit 0
fi
printf 'started\n' > "$ARCMUX_TEST_TRANSPORT_MARKER"
exit 0
`
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARCMUX_TEST_TRANSPORT_MARKER", transportMarker)
	peer := managedTunnelPeer("devbox", "secret-token", "127.0.0.1:18443")

	process, err := launchSSHTunnel(context.Background(), peer)
	if process != nil || err == nil {
		t.Fatalf("inherited forwarding config launched transport: process=%v err=%v", process, err)
	}
	if !strings.Contains(err.Error(), "inherits forwarding directives") ||
		strings.Contains(err.Error(), "0.0.0.0") || strings.Contains(err.Error(), peer.Token) {
		t.Fatalf("forward rejection was unsafe or unhelpful: %v", err)
	}
	if _, statErr := os.Stat(transportMarker); !os.IsNotExist(statErr) {
		t.Fatalf("forwarding SSH child started despite inherited forwards: %v", statErr)
	}
}

func TestManagerStopWaitsForManagedTunnelReaper(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := managedTunnelPeer("devbox", "token", "127.0.0.1:18443")
	manager := New(testConfig("127.0.0.1:0"), &Registry{
		Version: RegistryVersion, DeviceID: "ref", Peers: []Peer{peer},
	}, testLogger())
	manager.probe = func(context.Context, Peer) error { return errors.New("offline") }
	process := &delayedStopTunnelProcess{done: make(chan error, 1), delay: 75 * time.Millisecond}
	manager.tunnelLauncher = func(context.Context, Peer) (managedTunnelProcess, error) {
		return process, nil
	}
	startManager(t, manager, ctx)
	waitTransportState(t, manager, "devbox", "running")

	started := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	err := manager.Stop(stopCtx)
	stopCancel()
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < process.delay {
		t.Fatalf("Manager.Stop returned after %s before delayed reaper %s", elapsed, process.delay)
	}
	select {
	case _, ok := <-process.Done():
		if ok {
			t.Fatal("reaper result remained unread")
		}
	default:
		t.Fatal("managed tunnel Done channel was not joined")
	}
}

func TestUpsertPeerPreservesManagedTransportAcrossCredentialRotation(t *testing.T) {
	peer := managedTunnelPeer("devbox", "old-token", "127.0.0.1:18443")
	registry := &Registry{Version: RegistryVersion, DeviceID: "ref", Peers: []Peer{peer}}
	registry.UpsertPeer(Peer{ID: "devbox", URL: "ws://new.example:7788/v1/mesh", Token: "new-token"})
	if got := registry.Peers[0]; got.SSHTunnel == nil || got.Token != "new-token" || got.URL != "ws://new.example:7788/v1/mesh" {
		t.Fatalf("credential rotation dropped managed transport or failed to update pairing: %+v", got)
	}
}

func TestSanitizePeerErrorRedactsBeforeLengthBound(t *testing.T) {
	const token = "very-sensitive-peer-token"
	peer := Peer{Token: token}
	message := strings.Repeat("x", 230) + token + strings.Repeat("z", 100)
	got := sanitizePeerError(peer, errors.New(message))
	if strings.Contains(got, token) || strings.Contains(got, token[:10]) || len(got) > 240 {
		t.Fatalf("bounded peer error retained secret material: %q", got)
	}
}
