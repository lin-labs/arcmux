package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lin-labs/arcmux/internal/mesh"
)

func TestInfoJSONIncludesAuthoritativeMeshDeviceIDBeforeAnyBinding(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "mesh.json")
	if err := mesh.SaveRegistry(registryPath, &mesh.Registry{
		Version: mesh.RegistryVersion, DeviceID: "ref", Accept: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	configText := "[daemon]\n" +
		"socket = \"" + filepath.Join(dir, "offline.sock") + "\"\n" +
		"[mesh]\nregistry_path = \"" + registryPath + "\"\n"
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := cmdInfo([]string{"--json", "--config", configPath}, &output); err != nil {
		t.Fatal(err)
	}
	var info daemonInfo
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.DeviceID != "ref" {
		t.Fatalf("info device_id=%q, want registry authority ref; output=%s", info.DeviceID, output.String())
	}
}

func TestInfoJSONUsesEmptyDeviceIDWhenMeshIdentityIsUnconfigured(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	configText := "[daemon]\n" +
		"socket = \"" + filepath.Join(dir, "offline.sock") + "\"\n" +
		"[mesh]\nregistry_path = \"" + filepath.Join(dir, "missing-mesh.json") + "\"\n"
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := cmdInfo([]string{"--json", "--config", configPath}, &output); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(output.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	deviceID, present := raw["device_id"]
	if !present || deviceID != "" {
		t.Fatalf("unconfigured device_id must be present and empty: %s", output.String())
	}
}
