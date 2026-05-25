package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/paths"
)

func TestScaffoldCreatesDirs(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	p := paths.ForProject(dataRoot, vault, "demo")
	if err := Project(p, vault, "Build the demo by Friday"); err != nil {
		t.Fatalf("Project scaffold: %v", err)
	}

	for _, d := range []string{
		p.EphemeralRoot, p.Scratchpads, p.ConsultInbox, p.Heartbeats,
		p.VaultRoot, p.ArcmuxDir, p.PrinciplesDir, p.DeliverDir, p.ElonDir, p.TeamsDir, p.RetrosDir,
		paths.GlobalRolesDir(vault),
	} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("expected dir %q: %v", d, err)
		}
	}

	if _, err := os.Stat(filepath.Join(p.ArcmuxDir, "README.md")); err != nil {
		t.Errorf("README missing: %v", err)
	}
	missionPath := filepath.Join(p.ArcmuxDir, "mission.md")
	body, err := os.ReadFile(missionPath)
	if err != nil {
		t.Fatalf("mission.md missing: %v", err)
	}
	if !strings.Contains(string(body), "Build the demo by Friday") {
		t.Errorf("mission.md missing seed content; got: %s", body)
	}

	for _, role := range []string{"elon.md", "manager.md", "ic-base.md"} {
		if _, err := os.Stat(filepath.Join(paths.GlobalRolesDir(vault), role)); err != nil {
			t.Errorf("role seed %q missing: %v", role, err)
		}
	}
}

func TestScaffoldIdempotent(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	p := paths.ForProject(dataRoot, vault, "demo")

	for i := 0; i < 3; i++ {
		if err := Project(p, vault, "mission"); err != nil {
			t.Fatalf("Project iter %d: %v", i, err)
		}
	}
}

func TestScaffoldDoesNotOverwriteExistingRoles(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	p := paths.ForProject(dataRoot, vault, "demo")

	rolesDir := paths.GlobalRolesDir(vault)
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(rolesDir, "elon.md")
	if err := os.WriteFile(custom, []byte("USER_EDITED"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Project(p, vault, "mission"); err != nil {
		t.Fatalf("Project: %v", err)
	}

	body, _ := os.ReadFile(custom)
	if string(body) != "USER_EDITED" {
		t.Errorf("elon.md was overwritten; got %q", body)
	}
}
