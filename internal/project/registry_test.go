package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileIsEmpty(t *testing.T) {
	reg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if _, ok := reg.Resolve("anything"); ok {
		t.Errorf("empty registry should resolve nothing")
	}
}

func TestLoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.toml")
	content := `
[[project]]
slug = "voxtop"
repo_cwd = "/home/blin/Projects/voxtop"
plan_globs = ["docs/prd-*.md", "docs/plans/*.md"]

[[project]]
slug = "arcmux"
repo_cwd = "/home/blin/Projects/arcmux"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Resolve("voxtop")
	if !ok {
		t.Fatal("voxtop not resolved")
	}
	if p.RepoCWD != "/home/blin/Projects/voxtop" {
		t.Errorf("repo_cwd = %q", p.RepoCWD)
	}
	if len(p.PlanGlobs) != 2 {
		t.Errorf("plan_globs = %v", p.PlanGlobs)
	}
	if _, ok := reg.Resolve("ghost"); ok {
		t.Errorf("ghost should not resolve")
	}
}

func TestMatchesByCWD(t *testing.T) {
	p := Project{Slug: "voxtop", RepoCWD: "/home/blin/Projects/voxtop"}
	cases := []struct {
		cwd  string
		want bool
	}{
		{"/home/blin/Projects/voxtop", true},
		{"/home/blin/Projects/voxtop/VoxtopServer", true},
		{"/home/blin/Projects/arcmux", false},
		{"/home/blin/Projects/voxtop-other", false}, // prefix-but-not-subdir
		{"", false},
	}
	for _, c := range cases {
		if got := p.Matches(c.cwd, ""); got != c.want {
			t.Errorf("Matches(cwd=%q) = %v, want %v", c.cwd, got, c.want)
		}
	}
}

func TestMatchesByOwnerID(t *testing.T) {
	p := Project{Slug: "voxtop"}
	cases := []struct {
		owner string
		want  bool
	}{
		{"elonco:voxtop", true},
		{"project:voxtop", true},
		{"voxtop", true},
		{"elonco:arcmux", false},
		{"", false},
	}
	for _, c := range cases {
		if got := p.Matches("", c.owner); got != c.want {
			t.Errorf("Matches(owner=%q) = %v, want %v", c.owner, got, c.want)
		}
	}
}
