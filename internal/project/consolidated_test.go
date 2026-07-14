package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConsolidatedRegistry(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projects.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConsolidatedSimpleAndIgnoresUnrelatedKeys(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := writeConsolidatedRegistry(t, `
projects:
  - repo: arcmux
    project: arcmux
    path: ~/Tools/arcmux
    vault: ~/agents/obsProjects/arcmux
platforms:
  - name: olympus
    tracker: linear
`)
	registry, err := LoadConsolidated(path)
	if err != nil {
		t.Fatalf("LoadConsolidated: %v", err)
	}
	got, ok := registry.ResolveProject("arcmux")
	if !ok {
		t.Fatal("arcmux not resolved")
	}
	want := ResolvedProject{Slug: "arcmux", RepoPaths: []string{filepath.Join(home, "Tools", "arcmux")}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveProject = %#v, want %#v", got, want)
	}
	if _, ok := registry.ResolveProject("missing"); ok {
		t.Fatal("missing project resolved")
	}
}

func TestLoadConsolidatedMultiRepoAndWorktrees(t *testing.T) {
	path := writeConsolidatedRegistry(t, `
projects:
  - project: multi
    repos:
      - repo: api
        path: /srv/multi/api
        roots: [cmd]
      - repo: web
        path: /srv/multi/web
    worktrees: /srv/multi/worktrees
`)
	registry, err := LoadConsolidated(path)
	if err != nil {
		t.Fatalf("LoadConsolidated: %v", err)
	}
	got, ok := registry.ResolveProject("multi")
	if !ok {
		t.Fatal("multi not resolved")
	}
	want := ResolvedProject{
		Slug:          "multi",
		RepoPaths:     []string{"/srv/multi/api", "/srv/multi/web"},
		WorktreesRoot: "/srv/multi/worktrees",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveProject = %#v, want %#v", got, want)
	}
}

func TestLoadConsolidatedAcceptsScalarRepoBinding(t *testing.T) {
	path := writeConsolidatedRegistry(t, `
projects:
  - project: multi
    repos:
      - /srv/multi/api
      - path: /srv/multi/web
`)
	registry, err := LoadConsolidated(path)
	if err != nil {
		t.Fatalf("LoadConsolidated: %v", err)
	}
	got, ok := registry.ResolveProject("multi")
	if !ok {
		t.Fatal("multi not resolved")
	}
	want := []string{"/srv/multi/api", "/srv/multi/web"}
	if !reflect.DeepEqual(got.RepoPaths, want) {
		t.Fatalf("RepoPaths = %#v, want %#v", got.RepoPaths, want)
	}
}

func TestLoadConsolidatedRejectsDuplicateSlug(t *testing.T) {
	path := writeConsolidatedRegistry(t, `
projects:
  - project: duplicate
    path: /srv/one
  - project: duplicate
    path: /srv/two
`)
	_, err := LoadConsolidated(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate project slug") {
		t.Fatalf("LoadConsolidated error = %v, want duplicate", err)
	}
}

func TestLoadConsolidatedSkipsNullProject(t *testing.T) {
	path := writeConsolidatedRegistry(t, `
projects:
  - repo: discovered
    project: null
    path: /srv/discovered
  - project: assigned
    path: /srv/assigned
`)
	registry, err := LoadConsolidated(path)
	if err != nil {
		t.Fatalf("LoadConsolidated: %v", err)
	}
	if _, ok := registry.ResolveProject("discovered"); ok {
		t.Fatal("null project should not resolve")
	}
	if _, ok := registry.ResolveProject("assigned"); !ok {
		t.Fatal("assigned project not resolved")
	}
}

func TestLoadConsolidatedRejectsInvalidPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "relative", path: "relative/repo"},
		{name: "parent traversal", path: "/srv/projects/../secret"},
		{name: "home escape", path: "~/../secret"},
		{name: "other user home", path: "~other/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConsolidatedRegistry(t, "projects:\n  - project: invalid\n    path: "+tc.path+"\n")
			if _, err := LoadConsolidated(path); err == nil {
				t.Fatalf("LoadConsolidated accepted %q", tc.path)
			}
		})
	}
}

func TestResolveProjectReturnsIndependentPaths(t *testing.T) {
	path := writeConsolidatedRegistry(t, "projects:\n  - project: one\n    path: /srv/one\n")
	registry, err := LoadConsolidated(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := registry.ResolveProject("one")
	first.RepoPaths[0] = "/mutated"
	second, _ := registry.ResolveProject("one")
	if second.RepoPaths[0] != "/srv/one" {
		t.Fatalf("registry mutated through result: %#v", second)
	}
}
