package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type managedTunnelProcess interface {
	Done() <-chan error
	Stop()
}

type tunnelLauncher func(context.Context, Peer) (managedTunnelProcess, error)

type execTunnelProcess struct {
	cmd      *exec.Cmd
	done     chan error
	stopOnce sync.Once
}

const sshConfigInspectionTimeout = 5 * time.Second
const maxSSHEffectiveConfigBytes = 1 << 20

func (p *execTunnelProcess) Done() <-chan error { return p.done }

func (p *execTunnelProcess) Stop() {
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
}

func launchSSHTunnel(ctx context.Context, peer Peer) (managedTunnelProcess, error) {
	args, err := sshTunnelArgs(peer)
	if err != nil {
		return nil, err
	}
	if err := rejectInheritedSSHForwards(ctx, peer); err != nil {
		return nil, err
	}
	// No shell, inherited stdin, or captured child output: the registry can
	// describe only one fixed local forward, and neither remote banners nor
	// credential-agent diagnostics can enter arcmux status or logs.
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh transport: %w", err)
	}
	process := &execTunnelProcess{cmd: cmd, done: make(chan error, 1)}
	go func() {
		process.done <- cmd.Wait()
		close(process.done)
	}()
	return process, nil
}

func rejectInheritedSSHForwards(ctx context.Context, peer Peer) error {
	if peer.SSHTunnel == nil {
		return errors.New("ssh tunnel is not configured")
	}
	if err := peer.SSHTunnel.Validate(); err != nil {
		return err
	}
	inspectCtx, cancel := context.WithTimeout(ctx, sshConfigInspectionTimeout)
	defer cancel()
	// Inspect the user's real alias because it may carry required HostName,
	// ProxyJump, or IdentityFile semantics. Never surface the effective config:
	// it can contain private paths and network topology.
	cmd := exec.CommandContext(inspectCtx, "ssh", sshTunnelInspectionArgs(peer)...)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("inspect ssh target configuration: open output")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("inspect ssh target configuration: %w", err)
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, maxSSHEffectiveConfigBytes+1))
	if len(output) > maxSSHEffectiveConfigBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("inspect ssh target configuration: output exceeds safety limit")
	}
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("inspect ssh target configuration: read output")
	}
	if err := cmd.Wait(); err != nil {
		if inspectCtx.Err() != nil {
			return fmt.Errorf("inspect ssh target configuration: %w", inspectCtx.Err())
		}
		return fmt.Errorf("inspect ssh target configuration: %w", err)
	}
	if effectiveConfigHasForward(output) {
		return errors.New("ssh target inherits forwarding directives; refusing managed tunnel")
	}
	return nil
}

func effectiveConfigHasForward(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "localforward", "remoteforward", "dynamicforward":
			return true
		}
	}
	return false
}

func sshTunnelArgs(peer Peer) ([]string, error) {
	if peer.SSHTunnel == nil {
		return nil, errors.New("ssh tunnel is not configured")
	}
	if err := peer.SSHTunnel.Validate(); err != nil {
		return nil, err
	}
	tunnel := peer.SSHTunnel
	args := sshTunnelBaseArgs()
	args = append(args,
		"-L", tunnel.LocalAddr+":"+tunnel.RemoteAddr,
		"--", tunnel.Target,
	)
	return args, nil
}

// sshTunnelBaseArgs is the single source of truth for every production SSH
// option. Effective-config inspection uses this exact set and omits only the
// arcmux-owned -L, so config precedence cannot differ between inspection and
// the forwarding child.
func sshTunnelBaseArgs() []string {
	return []string{
		"-N", "-T",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=5",
		"-o", "ClearAllForwardings=no",
		"-o", "ControlMaster=no",
		"-o", "PermitLocalCommand=no",
		"-o", "StrictHostKeyChecking=yes",
	}
}

func sshTunnelInspectionArgs(peer Peer) []string {
	args := []string{"-G"}
	args = append(args, sshTunnelBaseArgs()...)
	return append(args, "--", peer.SSHTunnel.Target)
}

// DialURL selects the arcmux-owned local forward when configured while keeping
// the paired peer URL intact for future direct-tailnet routing.
func (p Peer) DialURL() string {
	if p.SSHTunnel == nil {
		return p.URL
	}
	return (&url.URL{Scheme: "ws", Host: p.SSHTunnel.LocalAddr, Path: meshPath}).String()
}

func (m *Manager) superviseTunnel(peer Peer) {
	attempt := 0
	for {
		if m.ctx.Err() != nil {
			return
		}
		m.updateStatus(peer.ID, func(status *Status) {
			status.TransportKind = "ssh"
			status.TransportState = "starting"
			status.TransportAttempts = attempt + 1
			status.TransportNextRetryAt = nil
		})
		startedAt := m.tunnelNow()
		process, err := m.tunnelLauncher(m.ctx, peer)
		if err == nil {
			m.updateStatus(peer.ID, func(status *Status) {
				status.TransportState = "running"
				status.TransportLastError = ""
			})
			m.logger.Info("mesh peer transport started", "peer", peer.ID, "transport", "ssh")
			select {
			case err = <-process.Done():
			case <-m.ctx.Done():
				process.Stop()
				// Reap the exact child before this supervisor exits. Manager.Stop's
				// caller-owned deadline bounds this wait and ReloadMesh refuses to
				// start a replacement manager if the old transport did not drain.
				<-process.Done()
				return
			}
			if m.ctx.Err() != nil {
				return
			}
			if err == nil {
				err = errors.New("ssh transport exited")
			}
			if m.tunnelNow().Sub(startedAt) >= m.cfg.DeadAfter {
				attempt = 0
			}
		}

		attempt++
		minRetry, maxRetry := m.retryBounds(retryAfterTransportFailure)
		delay := m.tunnelRetryDelay(attempt, minRetry, maxRetry)
		next := m.tunnelNow().Add(delay)
		safeError := sanitizePeerError(peer, err)
		m.updateStatus(peer.ID, func(status *Status) {
			status.TransportState = "backoff"
			status.TransportAttempts = attempt
			status.TransportLastError = safeError
			status.TransportNextRetryAt = &next
		})
		m.logger.Warn("mesh peer transport stopped; retry scheduled",
			"peer", peer.ID, "transport", "ssh", "error", safeError)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func sanitizePeerError(peer Peer, err error) string {
	return sanitizeSecretsError(err, peer.Token, TokenHash(peer.Token))
}

func sanitizeSecretsError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	// Redact before applying the public status length bound. Truncating first
	// could retain only a prefix of a token and thereby evade exact replacement.
	safe := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			safe = strings.ReplaceAll(safe, secret, "[REDACTED]")
		}
	}
	if len(safe) > 240 {
		safe = safe[:240]
	}
	return safe
}
