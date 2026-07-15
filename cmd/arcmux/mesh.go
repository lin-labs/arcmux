package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/mesh"
)

type meshInvite struct {
	Version           int    `json:"version"`
	PeerID            string `json:"peer_id"`
	URL               string `json:"url"`
	Token             string `json:"token,omitempty"`
	DaemonReloaded    bool   `json:"daemon_reloaded,omitempty"`
	AlreadyConfigured bool   `json:"already_configured,omitempty"`
}

func cmdMesh(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: arcmux mesh status|ping|serve|join|grant|revoke")
	}
	switch args[0] {
	case "status":
		return cmdMeshStatus(args[1:], stdout)
	case "ping":
		return cmdMeshPing(args[1:], stdout)
	case "serve":
		return cmdMeshServe(args[1:], stdout)
	case "join":
		return cmdMeshJoin(args[1:], stdin, stdout)
	case "grant":
		return cmdMeshGrant(args[1:], stdout)
	case "revoke":
		return cmdMeshRevoke(args[1:], stdout)
	case "sessions":
		return cmdMeshSessions(args[1:], stdout)
	case "session":
		return cmdMeshSession(args[1:], stdout)
	case "artifacts":
		return cmdMeshRemoteArtifacts(args[1:], stdout)
	case "artifact":
		return cmdMeshRemoteArtifact(args[1:], stdout)
	case "subscribe":
		return cmdMeshSubscribe(args[1:], stdout)
	default:
		return fmt.Errorf("unknown mesh subcommand %q", args[0])
	}
}

var meshReadScopes = []string{
	mesh.ScopeArtifactsRead,
	mesh.ScopeEventsRead,
	mesh.ScopeSessionsRead,
}

// cmdMeshGrant enables explicit application access for one paired peer.
// Pairing alone remains transport-only; omitting scopes grants only the full
// safe read set so the common two-device setup stays a one-command operation.
func cmdMeshGrant(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: arcmux mesh grant <peer> [sessions.read artifacts.read events.read handoffs.prepare]")
	}
	peerID := rest[0]
	scopes := append([]string(nil), rest[1:]...)
	if len(scopes) == 0 {
		scopes = append(scopes, meshReadScopes...)
	}
	allowed := make(map[string]bool, len(meshReadScopes)+1)
	for _, scope := range meshReadScopes {
		allowed[scope] = true
	}
	allowed[mesh.ScopeHandoffsPrepare] = true
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		if !allowed[scope] {
			return fmt.Errorf("unsupported mesh grant scope %q", scope)
		}
		if seen[scope] {
			return fmt.Errorf("duplicate mesh grant scope %q", scope)
		}
		seen[scope] = true
	}
	parsed, err := cfg.Mesh.Parse()
	if err != nil {
		return err
	}
	registry, err := mesh.LoadRegistry(parsed.RegistryPath)
	if err != nil {
		return err
	}
	if !meshPeerConfigured(registry, peerID) {
		return fmt.Errorf("mesh peer %q is not paired", peerID)
	}
	registry.Grants[peerID] = scopes
	if err := mesh.SaveRegistry(parsed.RegistryPath, registry); err != nil {
		return err
	}
	reloaded, err := reloadMesh(cfg)
	if err != nil {
		return err
	}
	state := "saved for next daemon start"
	if reloaded {
		state = "running daemon reloaded"
	}
	_, err = fmt.Fprintf(stdout, "granted %s to %s; %s\n", strings.Join(scopes, ","), peerID, state)
	return err
}

func cmdMeshRevoke(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: arcmux mesh revoke <peer>")
	}
	parsed, err := cfg.Mesh.Parse()
	if err != nil {
		return err
	}
	registry, err := mesh.LoadRegistry(parsed.RegistryPath)
	if err != nil {
		return err
	}
	delete(registry.Grants, rest[0])
	if err := mesh.SaveRegistry(parsed.RegistryPath, registry); err != nil {
		return err
	}
	reloaded, err := reloadMesh(cfg)
	if err != nil {
		return err
	}
	state := "saved for next daemon start"
	if reloaded {
		state = "running daemon reloaded"
	}
	_, err = fmt.Fprintf(stdout, "revoked application access from %s; %s\n", rest[0], state)
	return err
}

func meshPeerConfigured(registry *mesh.Registry, peerID string) bool {
	if registry == nil {
		return false
	}
	if _, ok := registry.Accept[peerID]; ok {
		return true
	}
	for _, peer := range registry.Peers {
		if peer.ID == peerID {
			return true
		}
	}
	return false
}

func meshConfig(args []string) (*config.Config, []string, error) {
	path := ""
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" || args[i] == "-c" {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs a path", args[i])
			}
			path = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	cfg, err := config.Load(path)
	return cfg, out, err
}

func cmdMeshStatus(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	jsonOut := false
	for _, a := range rest {
		if a == "--json" {
			jsonOut = true
		} else {
			return fmt.Errorf("unknown flag %s", a)
		}
	}
	b, err := meshAPI(cfg, http.MethodGet, "/mesh/status")
	if err != nil {
		return err
	}
	if jsonOut {
		_, err = stdout.Write(b)
		return err
	}
	var response struct {
		Enabled bool `json:"enabled"`
		Peers   []struct {
			PeerID      string `json:"peer_id"`
			State       string `json:"state"`
			Direction   string `json:"direction"`
			LastError   string `json:"last_error"`
			RoundTripMS int64  `json:"round_trip_ms"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(b, &response); err != nil {
		return err
	}
	if !response.Enabled {
		_, err = fmt.Fprintln(stdout, "mesh disabled")
		return err
	}
	if len(response.Peers) == 0 {
		_, err = fmt.Fprintln(stdout, "no peers configured")
		return err
	}
	for _, p := range response.Peers {
		fmt.Fprintf(stdout, "%s\t%s\t%s\trtt=%dms", p.PeerID, p.State, p.Direction, p.RoundTripMS)
		if p.LastError != "" {
			fmt.Fprintf(stdout, "\terror=%s", p.LastError)
		}
		fmt.Fprintln(stdout)
	}
	return nil
}

func cmdMeshPing(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: arcmux mesh ping <peer>")
	}
	b, err := meshAPI(cfg, http.MethodPost, "/mesh/ping?peer="+url.QueryEscape(rest[0]))
	if err != nil {
		return err
	}
	_, err = stdout.Write(b)
	return err
}

func meshAPI(cfg *config.Config, method, path string) ([]byte, error) {
	return meshAPIBody(cfg, method, path, nil)
}

func meshAPIBody(cfg *config.Config, method, path string, body []byte) ([]byte, error) {
	if cfg.Daemon.HTTPAddr == "" {
		return nil, errors.New("daemon http_addr is disabled")
	}
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, _ := http.NewRequest(method, "http://"+cfg.Daemon.HTTPAddr+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cfg.Daemon.HTTPAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Daemon.HTTPAuthToken)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemon mesh API unavailable at %s: %w", cfg.Daemon.HTTPAddr, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mesh API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func cmdMeshServe(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		return fmt.Errorf("usage: arcmux mesh serve <peer> --url ws://host:port/v1/mesh [--output file]")
	}
	peerID := rest[0]
	fs := flag.NewFlagSet("mesh serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	device := fs.String("device", "", "local device id")
	publicURL := fs.String("url", "", "URL joiners use")
	output := fs.String("output", "", "0600 invite path")
	tailscalePort := fs.Int("tailscale-port", 0, "configure raw TCP Serve on this tailnet port")
	rotate := fs.Bool("rotate", false, "rotate an existing peer credential")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *publicURL == "" {
		return fmt.Errorf("usage: arcmux mesh serve <peer> --url ws://host:port/v1/mesh [--output file]")
	}
	if err := validateMeshURL(*publicURL); err != nil {
		return fmt.Errorf("--url: %w", err)
	}
	if *tailscalePort < 0 || *tailscalePort > 65535 {
		return errors.New("--tailscale-port must be between 1 and 65535")
	}
	parsed, err := cfg.Mesh.Parse()
	if err != nil {
		return err
	}
	if !parsed.Enabled {
		return errors.New("mesh is disabled in config")
	}
	r, err := mesh.LoadRegistry(parsed.RegistryPath)
	if err != nil {
		return err
	}
	if r.DeviceID == "" {
		if *device != "" {
			r.DeviceID = *device
		} else {
			host, _ := os.Hostname()
			r.DeviceID = strings.Split(host, ".")[0]
		}
	} else if *device != "" && *device != r.DeviceID {
		return fmt.Errorf("registry device is already %q", r.DeviceID)
	}
	if _, exists := r.Accept[peerID]; exists && !*rotate {
		r.Serve = true
		if err := r.Validate(); err != nil {
			return err
		}
		if err := configureTailscale(parsed.ListenAddr, *tailscalePort); err != nil {
			return err
		}
		if err := mesh.SaveRegistry(parsed.RegistryPath, r); err != nil {
			return err
		}
		reloaded, err := reloadMesh(cfg)
		if err != nil {
			return err
		}
		result := meshInvite{Version: mesh.ProtocolVersion, PeerID: r.DeviceID, URL: *publicURL, DaemonReloaded: reloaded, AlreadyConfigured: true}
		b, _ := json.MarshalIndent(result, "", "  ")
		b = append(b, '\n')
		_, err = stdout.Write(b)
		return err
	}
	token, err := mesh.NewToken()
	if err != nil {
		return err
	}
	r.Serve = true
	r.Accept[peerID] = mesh.TokenHash(token)
	if err := r.Validate(); err != nil {
		return err
	}
	if err := configureTailscale(parsed.ListenAddr, *tailscalePort); err != nil {
		return err
	}
	if err := mesh.SaveRegistry(parsed.RegistryPath, r); err != nil {
		return err
	}
	reloaded, err := reloadMesh(cfg)
	if err != nil {
		return err
	}
	invite := meshInvite{Version: mesh.ProtocolVersion, PeerID: r.DeviceID, URL: *publicURL, Token: token, DaemonReloaded: reloaded}
	b, _ := json.MarshalIndent(invite, "", "  ")
	b = append(b, '\n')
	if *output != "" {
		if err := writeSecretFile(*output, b); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "invite written to %s\n", *output)
		return err
	}
	_, err = stdout.Write(b)
	return err
}

func cmdMeshJoin(args []string, stdin io.Reader, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || strings.HasPrefix(rest[0], "--") {
		return fmt.Errorf("usage: arcmux mesh join <invite-file|-> [--device id]")
	}
	inviteSource := rest[0]
	fs := flag.NewFlagSet("mesh join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	device := fs.String("device", "", "local device id")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: arcmux mesh join <invite-file|->")
	}
	var b []byte
	if inviteSource == "-" {
		b, err = io.ReadAll(io.LimitReader(stdin, 64<<10))
	} else {
		info, statErr := os.Stat(inviteSource)
		if statErr != nil {
			return statErr
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("invite file must be mode 0600 or stricter")
		}
		b, err = os.ReadFile(inviteSource)
	}
	if err != nil {
		return err
	}
	var invite meshInvite
	if err := json.Unmarshal(b, &invite); err != nil {
		return fmt.Errorf("parse invite: %w", err)
	}
	if invite.Version != mesh.ProtocolVersion || invite.PeerID == "" || invite.URL == "" || invite.Token == "" {
		return errors.New("invite is incomplete or unsupported")
	}
	if err := validateMeshURL(invite.URL); err != nil {
		return fmt.Errorf("invite URL: %w", err)
	}
	parsed, err := cfg.Mesh.Parse()
	if err != nil {
		return err
	}
	if !parsed.Enabled {
		return errors.New("mesh is disabled in config")
	}
	r, err := mesh.LoadRegistry(parsed.RegistryPath)
	if err != nil {
		return err
	}
	if r.DeviceID == "" {
		if *device != "" {
			r.DeviceID = *device
		} else {
			host, _ := os.Hostname()
			r.DeviceID = strings.Split(host, ".")[0]
		}
	} else if *device != "" && *device != r.DeviceID {
		return fmt.Errorf("registry device is already %q", r.DeviceID)
	}
	r.UpsertPeer(mesh.Peer{ID: invite.PeerID, URL: invite.URL, Token: invite.Token})
	if err := mesh.SaveRegistry(parsed.RegistryPath, r); err != nil {
		return err
	}
	reloaded, err := reloadMesh(cfg)
	if err != nil {
		return err
	}
	if reloaded {
		_, err = fmt.Fprintf(stdout, "joined %s; running daemon reloaded mesh without touching sessions\n", invite.PeerID)
	} else {
		_, err = fmt.Fprintf(stdout, "joined %s; daemon offline, configured for next start\n", invite.PeerID)
	}
	return err
}

func reloadMesh(cfg *config.Config) (bool, error) {
	if cfg.Daemon.HTTPAddr == "" {
		return false, nil
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.Daemon.HTTPAddr+"/mesh/reload", nil)
	if cfg.Daemon.HTTPAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Daemon.HTTPAuthToken)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("mesh saved but running daemon rejected reload: %s", strings.TrimSpace(string(b)))
	}
	return true, nil
}

func configureTailscale(listenAddr string, port int) error {
	if port == 0 {
		return nil
	}
	_, localPort, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("mesh listen address: %w", err)
	}
	target := "127.0.0.1:" + localPort
	statusOut, err := runTailscale("serve", "status", "--json")
	if err != nil {
		return fmt.Errorf("tailscale serve status: %w: %s", err, strings.TrimSpace(string(statusOut)))
	}
	var status struct {
		TCP map[string]struct {
			TCPForward string `json:"TCPForward"`
		} `json:"TCP"`
	}
	if len(statusOut) > 0 {
		if err := json.Unmarshal(statusOut, &status); err != nil {
			return fmt.Errorf("parse tailscale serve status: %w", err)
		}
	}
	portKey := fmt.Sprint(port)
	if existing, ok := status.TCP[portKey]; ok {
		if existing.TCPForward != target {
			return fmt.Errorf("tailscale TCP port %d already forwards to %s; refusing to replace it", port, existing.TCPForward)
		}
		return nil
	}
	out, err := runTailscale("serve", "--bg", "--tcp", portKey, "tcp://"+target)
	if err != nil {
		return fmt.Errorf("tailscale serve: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var runTailscale = func(args ...string) ([]byte, error) {
	return exec.Command("tailscale", args...).CombinedOutput()
}

func validateMeshURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "ws" {
		return errors.New("protocol v1 raw-TCP Serve requires ws://")
	}
	if u.Host == "" || u.Path != "/v1/mesh" {
		return errors.New("URL must include a host and end in /v1/mesh")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("userinfo, query strings, and fragments are not allowed")
	}
	return nil
}

func writeSecretFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".invite-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
