package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
)

type fakeRunner struct {
	calls [][]string
	outs  map[string]string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for k, v := range f.outs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

func TestStartCreatesWorkspaceAndPane(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	f := &fakeRunner{outs: map[string]string{
		"new-workspace": "OK workspace:1\n",
		"list-panes":    `{"workspace_ref":"workspace:1","panes":[{"ref":"pane:7","index":0,"focused":true,"surface_refs":["surface:1"]}]}`,
	}}
	cli := cmuxcli.NewWithRunnerForTest(f)

	p, err := Start(context.Background(), Options{
		Agent:     "claude",
		Project:   "demo",
		Mission:   "do the demo",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Cmux:      cli,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if p.ElonPane.Ref != "pane:7" {
		t.Errorf("ElonPane.Ref = %q, want pane:7", p.ElonPane.Ref)
	}
	if p.Workspace.Ref != "workspace:1" {
		t.Errorf("Workspace.Ref = %q, want workspace:1", p.Workspace.Ref)
	}

	var sawWS, sawList bool
	var wsCall string
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "new-workspace") {
			sawWS = true
			wsCall = j
		}
		if strings.Contains(j, "list-panes") {
			sawList = true
		}
	}
	if !sawWS {
		t.Error("expected new-workspace call")
	}
	if !sawList {
		t.Error("expected list-panes call")
	}
	// Workspace creation should pass the agent command and vault cwd.
	if !strings.Contains(wsCall, "claude") {
		t.Errorf("new-workspace missing --command claude: %q", wsCall)
	}
	if !strings.Contains(wsCall, vault) {
		t.Errorf("new-workspace missing vault cwd %q: %q", vault, wsCall)
	}

	entries, err := p.DB.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(entries) == 0 || entries[0].Action != "manager-mode-started" {
		t.Errorf("expected manager-mode-started audit, got %+v", entries)
	}
}

func TestStartRejectsBadAgent(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Agent:     "bash",
		Project:   "demo",
		DataRoot:  t.TempDir(),
		VaultRoot: t.TempDir(),
		Cmux:      cmuxcli.NewWithRunnerForTest(&fakeRunner{}),
	})
	if err == nil {
		t.Error("expected error for unsupported agent")
	}
}

func TestStartRejectsBadProject(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Agent:     "claude",
		Project:   "../evil",
		DataRoot:  t.TempDir(),
		VaultRoot: t.TempDir(),
		Cmux:      cmuxcli.NewWithRunnerForTest(&fakeRunner{}),
	})
	if err == nil {
		t.Error("expected error for invalid project slug")
	}
}

func TestStartRequiresVault(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Agent:    "claude",
		Project:  "demo",
		DataRoot: t.TempDir(),
		Cmux:     cmuxcli.NewWithRunnerForTest(&fakeRunner{}),
	})
	if err == nil {
		t.Error("expected error when VaultRoot is empty")
	}
}
