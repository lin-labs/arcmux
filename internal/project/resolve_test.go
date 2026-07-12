package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCWD_AgentsYamlWinsAndWalksUp(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "mono")
	sub := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(filepath.Join(repo, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "project: elonco\nprimary_stack: go\n"
	if err := os.WriteFile(filepath.Join(repo, ".agents", "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := &Registry{projects: map[string]Project{}}
	platforms := map[string]bool{"elonco": true}
	res := reg.ResolveCWD(sub, platforms)
	if res.Slug != "elonco" || !res.Platform || res.Source != "agents-yaml" {
		t.Fatalf("resolve = %+v, want elonco/platform/agents-yaml", res)
	}
}

func TestResolveCWD_RegistryThenDirname(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "somerepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := &Registry{projects: map[string]Project{
		"voxtop": {Slug: "voxtop", RepoCWD: repo},
	}}
	res := reg.ResolveCWD(filepath.Join(repo, "sub"), nil)
	if res.Slug != "voxtop" || res.Source != "registry" {
		t.Fatalf("registry resolve = %+v", res)
	}

	other := filepath.Join(root, "loose-folder")
	res = reg.ResolveCWD(other, nil)
	if res.Slug != "loose-folder" || res.Source != "dirname" || res.Platform {
		t.Fatalf("dirname resolve = %+v", res)
	}
}

func TestReadProjectKey_NullAndJSON(t *testing.T) {
	dir := t.TempDir()
	y := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(y, []byte("project: null\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readProjectKey(y); got != "" {
		t.Errorf("null project = %q, want empty", got)
	}
	j := filepath.Join(dir, "project.json")
	if err := os.WriteFile(j, []byte("{\n  \"project\": \"arcmux\",\n  \"primary_stack\": \"go\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readProjectKey(j); got != "arcmux" {
		t.Errorf("json project = %q, want arcmux", got)
	}
}

func TestLoadPlatforms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.yaml")
	content := `projects:
  - repo: arcmux
    path: /home/x/Projects/arcmux
platforms:
  - name: elonco
    vault: ~/agents/obsAgents/elonco
  - name: arcmux
projects_extra: ignored
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p := LoadPlatforms(path)
	if !p["elonco"] || !p["arcmux"] {
		t.Fatalf("platforms = %+v, want elonco+arcmux", p)
	}
	if len(p) != 2 {
		t.Fatalf("platforms = %+v, want exactly 2", p)
	}
	if got := LoadPlatforms(filepath.Join(dir, "missing.yaml")); got == nil || len(got) != 0 {
		t.Fatalf("missing file should yield empty map, got %+v", got)
	}
}
