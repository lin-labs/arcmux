package roles

import (
	"strings"
	"testing"
)

func TestEmbeddedRoles(t *testing.T) {
	for _, name := range []string{"elon", "manager", "ic-base"} {
		body, ok := Get(name)
		if !ok {
			t.Errorf("Get(%q) not found", name)
			continue
		}
		if !strings.Contains(body, "---") {
			t.Errorf("role %q missing frontmatter", name)
		}
	}
}

func TestList(t *testing.T) {
	got := List()
	want := map[string]bool{"elon": true, "manager": true, "ic-base": true}
	for name := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("List() missing %q", name)
		}
	}
}
