package project

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Resolution is what a session cwd maps to: a stable project slug and whether
// that slug names a PLATFORM (a monorepo serving many features/projects at
// once). Platforms expect MANY concurrent agents in the same folder, so the
// folder name alone cannot discriminate sessions — the per-session feature
// slug (turn_contract.feature) is the discriminator, and the daemon renames
// tmux sessions to <slug>-<feature> once it is known.
type Resolution struct {
	Slug     string
	Platform bool
	// Source records where the slug came from: "agents-yaml", "registry",
	// or "dirname". Diagnostic only.
	Source string
}

// ResolveCWD maps a session cwd to its project slug. Resolution order follows
// the cross-agent convention: an in-repo .agents/project.yaml (or legacy
// .agents/project.json) wins, walking from cwd upward; else a registered
// project whose repo_cwd contains cwd; else the base name of the cwd.
// Platform-ness is looked up in the platforms list (see LoadPlatforms).
func (r *Registry) ResolveCWD(cwd string, platforms map[string]bool) Resolution {
	res := Resolution{}
	if slug := slugFromAgentsConfig(cwd); slug != "" {
		res = Resolution{Slug: slug, Source: "agents-yaml"}
	} else if slug := r.slugByCWD(cwd); slug != "" {
		res = Resolution{Slug: slug, Source: "registry"}
	} else if cwd != "" {
		res = Resolution{Slug: filepath.Base(filepath.Clean(cwd)), Source: "dirname"}
	}
	res.Platform = platforms[res.Slug]
	return res
}

// slugByCWD finds a registered project whose repo_cwd contains cwd.
func (r *Registry) slugByCWD(cwd string) string {
	if r == nil || cwd == "" {
		return ""
	}
	for slug, p := range r.projects {
		if p.RepoCWD != "" && withinDir(p.RepoCWD, cwd) {
			return slug
		}
	}
	return ""
}

// slugFromAgentsConfig walks from dir upward looking for .agents/project.yaml
// (or legacy .agents/project.json) and returns its project slug. The yaml is
// deliberately parsed line-wise — the file is a flat, conventional few-line
// document (`project: <slug>`) and arcmux takes no YAML dependency. A
// `project: null` (registered, no slug yet) yields "".
func slugFromAgentsConfig(dir string) string {
	dir = filepath.Clean(dir)
	home, _ := os.UserHomeDir()
	for i := 0; i < 16 && dir != "/" && dir != "." && dir != home; i++ {
		if slug := readProjectKey(filepath.Join(dir, ".agents", "project.yaml")); slug != "" {
			return slug
		}
		if slug := readProjectKey(filepath.Join(dir, ".agents", "project.json")); slug != "" {
			return slug
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// readProjectKey extracts the `project:` value from a flat yaml (or the
// "project" string field from a flat json) without a parser dependency.
func readProjectKey(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		var v string
		switch {
		case strings.HasPrefix(line, "project:"):
			v = strings.TrimSpace(strings.TrimPrefix(line, "project:"))
		case strings.HasPrefix(line, "\"project\""):
			// json: "project": "slug",
			_, after, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(after), ","))
		default:
			continue
		}
		v = strings.Trim(v, `"'`)
		if v == "" || v == "null" || v == "~" {
			return ""
		}
		return v
	}
	return ""
}

// DefaultPlatformsPath returns the hand-maintained cross-agent registry that
// carries the platforms: list (~/agents/projects.yaml).
func DefaultPlatformsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "agents", "projects.yaml")
}

// LoadPlatforms reads the `platforms:` list from the cross-agent registry
// yaml: entries of the form `- name: <slug>` under a top-level `platforms:`
// key. Same line-wise parsing rationale as readProjectKey. A missing file
// yields an empty (never nil) map.
func LoadPlatforms(path string) map[string]bool {
	if path == "" {
		path = DefaultPlatformsPath()
	}
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	in := false
	for sc.Scan() {
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "platforms:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		// A new top-level key (no indentation, not a list item) ends the block.
		if !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "-") && trimmed != "" {
			break
		}
		if name, ok := strings.CutPrefix(trimmed, "- name:"); ok {
			if v := strings.Trim(strings.TrimSpace(name), `"'`); v != "" {
				out[v] = true
			}
		}
	}
	return out
}
