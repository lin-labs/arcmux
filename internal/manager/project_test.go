package manager

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	cmuxbackend "github.com/lin-labs/arcmux/internal/mux/cmux"
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

func okCmux() *fakeRunner {
	return &fakeRunner{outs: map[string]string{
		"new-workspace": "OK workspace:1\n",
		"list-panes":    `{"workspace_ref":"workspace:1","panes":[{"ref":"pane:7","index":0,"focused":true,"surface_refs":["surface:1"]}]}`,
	}}
}

// TestRegisterSession_CreatesWorkspaceAndPane pins the substrate-only
// shape of the post-C4 registrar: workspace + pane + bootstrap script +
// project meta land on disk, and the audit log records the registration.
// Nothing role-shaped (mission inbox seed, scratchpad seed) — those are
// the caller's job now.
func TestRegisterSession_CreatesWorkspaceAndPane(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	f := okCmux()
	backend := cmuxbackend.New(cmuxcli.NewWithRunnerForTest(f))

	r, err := RegisterSession(context.Background(), Options{
		Agent:     "claude",
		Project:   "demo",
		Command:   "claude --some-flag",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Mux:       backend,
	})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	defer r.Close()

	if r.Pane.Ref != "pane:7" {
		t.Errorf("Pane.Ref = %q, want pane:7", r.Pane.Ref)
	}
	if r.Group.Ref != "workspace:1" {
		t.Errorf("Group.Ref = %q, want workspace:1", r.Group.Ref)
	}
	if r.BootstrapPath == "" {
		t.Error("BootstrapPath empty")
	}
	if _, err := os.Stat(r.BootstrapPath); err != nil {
		t.Errorf("bootstrap script missing: %v", err)
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
	if !strings.Contains(wsCall, "bootstrap.sh") {
		t.Errorf("new-workspace missing bootstrap script: %q", wsCall)
	}

	// The bootstrap script body exec's the caller-supplied command verbatim.
	body, err := os.ReadFile(r.BootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if !strings.Contains(string(body), "exec claude --some-flag") {
		t.Errorf("bootstrap missing caller command:\n%s", string(body))
	}

	entries, err := r.DB.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(entries) == 0 || entries[0].Action != "session-registered" {
		t.Errorf("expected session-registered audit, got %+v", entries)
	}
}

// TestRegisterSession_WritesProjectMeta verifies the singleton header
// the rest of the substrate reads from lands on disk with the spawned
// pane's ref so pulse / heartbeats can find it.
func TestRegisterSession_WritesProjectMeta(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	r, err := RegisterSession(context.Background(), Options{
		Agent:     "claude",
		Project:   "meta",
		Command:   "claude",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(okCmux())),
	})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	defer r.Close()

	meta, err := r.DB.GetProjectMeta()
	if err != nil {
		t.Fatalf("GetProjectMeta: %v", err)
	}
	if meta.PaneRef != r.Pane.Ref {
		t.Errorf("meta.PaneRef = %q, want %q", meta.PaneRef, r.Pane.Ref)
	}
	if meta.WorkspaceRef != r.Group.Ref {
		t.Errorf("meta.WorkspaceRef = %q, want %q", meta.WorkspaceRef, r.Group.Ref)
	}
}

// TestRegisterSession_NoInboxOrScratchpadSeeded pins the post-C4
// invariant: arcmux is a pure substrate librarian. It does NOT push a
// mission message into any inbox, and it does NOT write a scratchpad
// file. That seeding moved to elonco.
func TestRegisterSession_NoInboxOrScratchpadSeeded(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	r, err := RegisterSession(context.Background(), Options{
		Agent:     "claude",
		Project:   "puresub",
		Command:   "claude",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(okCmux())),
	})
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	defer r.Close()

	// No per-session inbox sub-buckets exist — the registrar never
	// creates one.
	if r.DB.HasSessionInbox("puresub") {
		t.Error("RegisterSession created a session-inbox sub-bucket; want pure-substrate (none)")
	}
	if r.DB.HasSessionInbox(r.Pane.Ref) {
		t.Error("RegisterSession created a pane-keyed inbox; want pure-substrate (none)")
	}

	// No scratchpad dir written on disk under the project's ephemeral
	// root (scaffold creates the empty directory, but no file inside).
	entries, err := os.ReadDir(r.Paths.Scratchpads)
	if err != nil {
		// Dir absent is also acceptable.
		return
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("scratchpads dir is non-empty; arcmux should not seed any files (got %v)", names)
	}
}

func TestRegisterSession_RejectsBadProject(t *testing.T) {
	_, err := RegisterSession(context.Background(), Options{
		Agent:     "claude",
		Project:   "../evil",
		DataRoot:  t.TempDir(),
		VaultRoot: t.TempDir(),
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(&fakeRunner{})),
	})
	if err == nil {
		t.Error("expected error for invalid project slug")
	}
}

func TestRegisterSession_RequiresVault(t *testing.T) {
	_, err := RegisterSession(context.Background(), Options{
		Agent:    "claude",
		Project:  "demo",
		DataRoot: t.TempDir(),
		Mux:      cmuxbackend.New(cmuxcli.NewWithRunnerForTest(&fakeRunner{})),
	})
	if err == nil {
		t.Error("expected error when VaultRoot is empty")
	}
}

func TestRegisterSession_RequiresAgent(t *testing.T) {
	_, err := RegisterSession(context.Background(), Options{
		Project:   "demo",
		DataRoot:  t.TempDir(),
		VaultRoot: t.TempDir(),
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(&fakeRunner{})),
	})
	if err == nil {
		t.Error("expected error when Agent is empty")
	}
}
