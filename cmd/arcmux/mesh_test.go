package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/mesh"
)

func meshTestConfig(t *testing.T, dir, device string) string {
	t.Helper()
	p := filepath.Join(dir, device+".toml")
	registry := filepath.Join(dir, device+"-mesh.json")
	content := "[daemon]\nhttp_addr = \"127.0.0.1:1\"\n[mesh]\nregistry_path = \"" + registry + "\"\nlisten_addr = \"127.0.0.1:0\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMeshServeDocumentedOrderIsIdempotentAndJoinReadsStdin(t *testing.T) {
	dir := t.TempDir()
	serverCfg := meshTestConfig(t, dir, "server")
	args := []string{"serve", "client", "--url", "ws://server.test:7788/v1/mesh", "--device", "server", "--config", serverCfg}
	var inviteOut bytes.Buffer
	if err := cmdMesh(args, strings.NewReader(""), &inviteOut); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var invite meshInvite
	if err := json.Unmarshal(inviteOut.Bytes(), &invite); err != nil {
		t.Fatal(err)
	}
	if invite.Token == "" || invite.PeerID != "server" {
		t.Fatalf("bad invite: %+v", invite)
	}
	registry, err := mesh.LoadRegistry(filepath.Join(dir, "server-mesh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if registry.Accept["client"] != mesh.TokenHash(invite.Token) {
		t.Fatal("server did not store only invite token hash")
	}
	invitePath := filepath.Join(dir, "devbox.invite.json")
	if err := cmdMesh([]string{"serve", "devbox", "--url", "ws://server.test:7788/v1/mesh", "--output", invitePath, "--config", serverCfg}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("serve output file: %v", err)
	}
	inviteInfo, err := os.Stat(invitePath)
	if err != nil {
		t.Fatal(err)
	}
	if inviteInfo.Mode().Perm() != 0o600 {
		t.Fatalf("invite mode=%o want 600", inviteInfo.Mode().Perm())
	}

	var repeat bytes.Buffer
	if err := cmdMesh(args, strings.NewReader(""), &repeat); err != nil {
		t.Fatalf("repeat serve: %v", err)
	}
	var repeated meshInvite
	if err := json.Unmarshal(repeat.Bytes(), &repeated); err != nil {
		t.Fatal(err)
	}
	if !repeated.AlreadyConfigured || repeated.Token != "" {
		t.Fatalf("repeat should succeed without re-emitting secret: %+v", repeated)
	}

	clientCfg := meshTestConfig(t, dir, "client")
	var joined bytes.Buffer
	if err := cmdMesh([]string{"join", "-", "--device", "client", "--config", clientCfg}, bytes.NewReader(inviteOut.Bytes()), &joined); err != nil {
		t.Fatalf("join stdin: %v", err)
	}
	clientRegistry, err := mesh.LoadRegistry(filepath.Join(dir, "client-mesh.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(clientRegistry.Peers) != 1 || clientRegistry.Peers[0].Token != invite.Token {
		t.Fatalf("client registry missing credential: %+v", clientRegistry.Peers)
	}
	info, _ := os.Stat(filepath.Join(dir, "client-mesh.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("client credential mode=%o", info.Mode().Perm())
	}
}

func TestMeshGrantIsExplicitReadOnlyAndRevokeRestoresTransportOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := meshTestConfig(t, dir, "server")
	parsedRegistry := filepath.Join(dir, "server-mesh.json")
	registry := &mesh.Registry{
		Version:  mesh.RegistryVersion,
		DeviceID: "server",
		Accept:   map[string]string{"client": mesh.TokenHash("token")},
		Grants:   map[string][]string{},
	}
	if err := mesh.SaveRegistry(parsedRegistry, registry); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cmdMesh([]string{"grant", "client", "--config", cfgPath}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("grant: %v", err)
	}
	updated, err := mesh.LoadRegistry(parsedRegistry)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		mesh.ScopeArtifactsRead: true,
		mesh.ScopeEventsRead:    true,
		mesh.ScopeSessionsRead:  true,
	}
	if len(updated.Grants["client"]) != len(want) {
		t.Fatalf("grants=%v", updated.Grants["client"])
	}
	for _, scope := range updated.Grants["client"] {
		if !want[scope] {
			t.Fatalf("unexpected grant %q", scope)
		}
	}
	if !strings.Contains(out.String(), "saved for next daemon start") {
		t.Fatalf("offline grant output=%q", out.String())
	}

	if err := cmdMesh([]string{"grant", "client", "shell.execute", "--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("unsafe scope accepted")
	}
	if err := cmdMesh([]string{"grant", "stranger", "--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("unpaired peer accepted")
	}
	if err := cmdMesh([]string{"revoke", "client", "--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	updated, err = mesh.LoadRegistry(parsedRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updated.Grants["client"]; ok {
		t.Fatalf("revoke left grants: %v", updated.Grants)
	}
}

func TestMeshGrantAcceptsPrepareWithoutChangingReadDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := meshTestConfig(t, dir, "server")
	registryPath := filepath.Join(dir, "server-mesh.json")
	registry := &mesh.Registry{
		Version: mesh.RegistryVersion, DeviceID: "server",
		Accept: map[string]string{"client": mesh.TokenHash("token")}, Grants: map[string][]string{},
	}
	if err := mesh.SaveRegistry(registryPath, registry); err != nil {
		t.Fatal(err)
	}

	if err := cmdMesh([]string{"grant", "client", mesh.ScopeHandoffsPrepare, "--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("prepare grant: %v", err)
	}
	updated, err := mesh.LoadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Grants["client"]; len(got) != 1 || got[0] != mesh.ScopeHandoffsPrepare {
		t.Fatalf("prepare grant = %v", got)
	}

	if err := cmdMesh([]string{"grant", "client", mesh.ScopeHandoffsLaunch, "--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("dormant launch scope accepted before launch RPC exists")
	}
	updated, err = mesh.LoadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Grants["client"]; len(got) != 1 || got[0] != mesh.ScopeHandoffsPrepare {
		t.Fatalf("rejected launch grant mutated grants to %v", got)
	}

	if err := cmdMesh([]string{"grant", "client", "--config", cfgPath}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("default grant: %v", err)
	}
	updated, err = mesh.LoadRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		mesh.ScopeArtifactsRead: true,
		mesh.ScopeEventsRead:    true,
		mesh.ScopeSessionsRead:  true,
	}
	if got := updated.Grants["client"]; len(got) != len(want) {
		t.Fatalf("default grants = %v", got)
	} else {
		for _, scope := range got {
			if !want[scope] {
				t.Fatalf("default grant unexpectedly contains %q", scope)
			}
		}
	}
}

func TestMeshServeValidatesBeforeWritingRegistry(t *testing.T) {
	dir := t.TempDir()
	cfg := meshTestConfig(t, dir, "server")
	err := cmdMesh([]string{"serve", "client", "--url", "https://wrong/v1/mesh", "--device", "server", "--config", cfg}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil {
		t.Fatal("invalid URL accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "server-mesh.json")); !os.IsNotExist(err) {
		t.Fatalf("registry mutated before validation: %v", err)
	}
}

func TestMeshURLValidationRejectsTLSAndCredentialSurfaces(t *testing.T) {
	for _, raw := range []string{
		"wss://server.test/v1/mesh",
		"ws:///v1/mesh",
		"ws://user:pass@server.test/v1/mesh",
		"ws://server.test/v1/mesh?token=secret",
		"ws://server.test/v1/mesh#fragment",
	} {
		if err := validateMeshURL(raw); err == nil {
			t.Fatalf("accepted unsafe or unsupported URL %q", raw)
		}
	}
}

func TestConfigureTailscalePreservesOccupiedPortAndReusesOwnMapping(t *testing.T) {
	original := runTailscale
	defer func() { runTailscale = original }()
	var configureCalls int
	runTailscale = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "serve" && args[1] == "status" {
			return []byte(`{"TCP":{"7788":{"TCPForward":"127.0.0.1:9999"}}}`), nil
		}
		configureCalls++
		return nil, nil
	}
	if err := configureTailscale("127.0.0.1:7788", 7788); err == nil {
		t.Fatal("occupied Tailscale port was overwritten")
	}
	if configureCalls != 0 {
		t.Fatal("configure command ran after occupied-port refusal")
	}

	runTailscale = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "serve" && args[1] == "status" {
			return []byte(`{"TCP":{"7788":{"TCPForward":"127.0.0.1:7788"}}}`), nil
		}
		configureCalls++
		return nil, nil
	}
	if err := configureTailscale("127.0.0.1:7788", 7788); err != nil {
		t.Fatal(err)
	}
	if configureCalls != 0 {
		t.Fatal("idempotent existing mapping was reconfigured")
	}
}

func TestMeshStatusTextDecodesSnakeCaseFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"peers":[{"peer_id":"devbox","state":"disconnected","direction":"outbound","last_error":"offline"}]}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "status.toml")
	httpAddr := strings.TrimPrefix(server.URL, "http://")
	if err := os.WriteFile(cfgPath, []byte("[daemon]\nhttp_addr = \""+httpAddr+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdMesh([]string{"status", "--config", cfgPath}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "devbox") || !strings.Contains(got, "error=offline") {
		t.Fatalf("text status lost snake_case fields: %q", got)
	}
}
