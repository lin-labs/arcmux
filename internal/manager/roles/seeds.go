// Package roles bundles the seed role files arcmux scaffolds into the
// global role library on first run.
package roles

import (
	"embed"
	"strings"
)

//go:embed files/*.md
var fs embed.FS

// Get returns the raw markdown body of the seed role with the given name
// (e.g. "elon", "manager", "ic-base"). Returns false if not seeded.
func Get(name string) (string, bool) {
	b, err := fs.ReadFile("files/" + name + ".md")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// List returns the names of all seeded roles.
func List() []string {
	entries, err := fs.ReadDir("files")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".md") {
			out = append(out, strings.TrimSuffix(n, ".md"))
		}
	}
	return out
}
