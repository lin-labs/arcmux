// Package project holds the arcmux project registry: a small, declarative map
// from a stable project slug to where its code and plan docs live. arcmux is a
// pure substrate and does not otherwise associate sessions with a "project";
// the babysit subsystem uses this registry to scope a voice call to a
// project's panes (by cwd / owner_id) and to locate its plan/PRD docs.
package project

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Project is one registered project.
type Project struct {
	Slug      string   `toml:"slug"`
	RepoCWD   string   `toml:"repo_cwd"`
	PlanGlobs []string `toml:"plan_globs"`
}

// Registry maps project slugs to their Project. Loaded from
// ~/.config/arcmux/projects.toml; a missing file yields an empty registry so
// the daemon runs fine without one.
type Registry struct {
	projects map[string]Project
}

type fileShape struct {
	Project []Project `toml:"project"`
}

// DefaultPath returns ~/.config/arcmux/projects.toml.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "arcmux", "projects.toml")
}

// Load reads the registry from path (DefaultPath when empty). A nonexistent
// file is not an error — it returns an empty registry.
func Load(path string) (*Registry, error) {
	if path == "" {
		path = DefaultPath()
	}
	reg := &Registry{projects: map[string]Project{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return nil, err
	}
	var fs fileShape
	if err := toml.Unmarshal(data, &fs); err != nil {
		return nil, err
	}
	for _, p := range fs.Project {
		if p.Slug == "" {
			continue
		}
		p.RepoCWD = expandHome(p.RepoCWD)
		reg.projects[p.Slug] = p
	}
	return reg, nil
}

// Resolve returns the registered project for a slug.
func (r *Registry) Resolve(slug string) (Project, bool) {
	if r == nil {
		return Project{}, false
	}
	p, ok := r.projects[slug]
	return p, ok
}

// Matches reports whether a session belongs to this project. Membership: the
// session's cwd is within the project's repo_cwd, OR its owner_id tag contains
// the slug as a colon-delimited component (e.g. "elonco:voxtop",
// "project:voxtop").
func (p Project) Matches(sessionCWD, ownerID string) bool {
	if p.RepoCWD != "" && sessionCWD != "" && withinDir(p.RepoCWD, sessionCWD) {
		return true
	}
	if p.Slug == "" {
		return false
	}
	for _, part := range strings.Split(ownerID, ":") {
		if part == p.Slug {
			return true
		}
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func withinDir(dir, target string) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if dir == target {
		return true
	}
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
