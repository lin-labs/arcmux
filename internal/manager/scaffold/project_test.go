package scaffold

import (
	"os"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/paths"
)

func TestScaffoldCreatesEphemeralDirs(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	p := paths.ForProject(dataRoot, vault, "demo")
	if err := Project(p); err != nil {
		t.Fatalf("Project scaffold: %v", err)
	}

	for _, d := range []string{
		p.EphemeralRoot, p.Scratchpads, p.ConsultInbox, p.Heartbeats,
	} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("expected dir %q: %v", d, err)
		}
	}
}

func TestScaffoldDoesNotTouchVault(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	p := paths.ForProject(dataRoot, vault, "demo")
	if err := Project(p); err != nil {
		t.Fatalf("Project scaffold: %v", err)
	}

	// arcmux must not write anywhere under the vault — that's elonco's job.
	entries, err := os.ReadDir(vault)
	if err != nil {
		t.Fatalf("read vault: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("scaffold should not write into vault; got entries: %v", names)
	}
}

func TestScaffoldIdempotent(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	p := paths.ForProject(dataRoot, vault, "demo")

	for i := 0; i < 3; i++ {
		if err := Project(p); err != nil {
			t.Fatalf("Project iter %d: %v", i, err)
		}
	}
}

func TestScaffoldRejectsEmptySlug(t *testing.T) {
	if err := Project(paths.Project{}); err == nil {
		t.Error("expected error for empty Project slug")
	}
}
