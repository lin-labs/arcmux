package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConsolidatedRegistry is the read-only subset of ~/agents/projects.yaml that
// handoff preparation needs. It deliberately ignores platform, tracker, vault,
// and state metadata.
type ConsolidatedRegistry struct {
	projects map[string]ResolvedProject
}

// ResolvedProject contains the local checkout locations associated with one
// logical project and its optional managed-worktree root.
type ResolvedProject struct {
	Slug          string
	RepoPaths     []string
	WorktreesRoot string
}

type consolidatedFile struct {
	Projects []consolidatedProject `yaml:"projects"`
}

type consolidatedProject struct {
	Project   *string            `yaml:"project"`
	Repo      string             `yaml:"repo"`
	Path      string             `yaml:"path"`
	Repos     []consolidatedRepo `yaml:"repos"`
	Worktrees string             `yaml:"worktrees"`
}

type consolidatedRepo struct {
	Repo string `yaml:"repo"`
	Path string `yaml:"path"`
}

func (r *consolidatedRepo) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("repo binding must be a path string or mapping")
		}
		r.Path = node.Value
		return nil
	case yaml.MappingNode:
		type plainRepo consolidatedRepo
		var decoded plainRepo
		if err := node.Decode(&decoded); err != nil {
			return err
		}
		*r = consolidatedRepo(decoded)
		return nil
	default:
		return fmt.Errorf("repo binding must be a path string or mapping")
	}
}

// DefaultConsolidatedPath returns ~/agents/projects.yaml.
func DefaultConsolidatedPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "agents", "projects.yaml")
}

// LoadConsolidated reads the project subset of the consolidated agents
// registry. A missing registry is returned as an error: unlike the optional
// arcmux-specific registry, callers explicitly rely on this file to prepare a
// handoff safely.
func LoadConsolidated(path string) (*ConsolidatedRegistry, error) {
	if path == "" {
		path = DefaultConsolidatedPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read consolidated project registry: %w", err)
	}

	var file consolidatedFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse consolidated project registry: %w", err)
	}

	registry := &ConsolidatedRegistry{projects: make(map[string]ResolvedProject)}
	for i, entry := range file.Projects {
		// project: null is how the registry represents a discovered checkout
		// that has not been assigned a logical project yet.
		if entry.Project == nil {
			continue
		}
		slug := strings.TrimSpace(*entry.Project)
		if slug == "" {
			return nil, fmt.Errorf("projects[%d]: empty project slug", i)
		}
		if _, exists := registry.projects[slug]; exists {
			return nil, fmt.Errorf("projects[%d]: duplicate project slug %q", i, slug)
		}

		paths := make([]string, 0, 1+len(entry.Repos))
		if entry.Path != "" {
			resolved, err := validatedRegistryPath(entry.Path)
			if err != nil {
				return nil, fmt.Errorf("projects[%d] %q path: %w", i, slug, err)
			}
			paths = append(paths, resolved)
		}
		for repoIndex, repo := range entry.Repos {
			if strings.TrimSpace(repo.Path) == "" {
				return nil, fmt.Errorf("projects[%d] %q repos[%d]: path required", i, slug, repoIndex)
			}
			resolved, err := validatedRegistryPath(repo.Path)
			if err != nil {
				return nil, fmt.Errorf("projects[%d] %q repos[%d] path: %w", i, slug, repoIndex, err)
			}
			paths = append(paths, resolved)
		}
		if len(paths) == 0 {
			return nil, fmt.Errorf("projects[%d] %q: path or repos required", i, slug)
		}

		worktrees := ""
		if entry.Worktrees != "" {
			worktrees, err = validatedRegistryPath(entry.Worktrees)
			if err != nil {
				return nil, fmt.Errorf("projects[%d] %q worktrees: %w", i, slug, err)
			}
		}
		registry.projects[slug] = ResolvedProject{
			Slug:          slug,
			RepoPaths:     paths,
			WorktreesRoot: worktrees,
		}
	}
	return registry, nil
}

// ResolveProject returns a copy of the configured checkout locations for slug.
// An unknown slug is the ordinary false result, not an error.
func (r *ConsolidatedRegistry) ResolveProject(slug string) (ResolvedProject, bool) {
	if r == nil {
		return ResolvedProject{}, false
	}
	project, ok := r.projects[slug]
	if !ok {
		return ResolvedProject{}, false
	}
	project.RepoPaths = append([]string(nil), project.RepoPaths...)
	return project, true
}

func validatedRegistryPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path required")
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", fmt.Errorf("path contains NUL")
	}
	if hasDotDotComponent(raw) {
		return "", fmt.Errorf("parent traversal is not allowed: %q", raw)
	}
	if raw == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		raw = home
	} else if strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		raw = filepath.Join(home, raw[2:])
	} else if strings.HasPrefix(raw, "~") {
		return "", fmt.Errorf("unsupported home expansion: %q", raw)
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("path must be absolute: %q", raw)
	}
	return filepath.Clean(raw), nil
}

func hasDotDotComponent(path string) bool {
	for _, component := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if component == ".." {
			return true
		}
	}
	return false
}
