package manager

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

func TestStartCreatesWorkspaceAndPane(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	f := okCmux()
	backend := cmuxbackend.New(cmuxcli.NewWithRunnerForTest(f))

	p, err := Start(context.Background(), Options{
		Agent:     "claude",
		Project:   "demo",
		Mission:   "do the demo",
		Command:   "claude --some-flag",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Mux:       backend,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if p.ElonPane.Ref != "pane:7" {
		t.Errorf("ElonPane.Ref = %q, want pane:7", p.ElonPane.Ref)
	}
	if p.Group.Ref != "workspace:1" {
		t.Errorf("Group.Ref = %q, want workspace:1", p.Group.Ref)
	}
	if p.BootstrapPath == "" {
		t.Error("BootstrapPath empty")
	}
	if _, err := os.Stat(p.BootstrapPath); err != nil {
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
	body, err := os.ReadFile(p.BootstrapPath)
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if !strings.Contains(string(body), "exec claude --some-flag") {
		t.Errorf("bootstrap missing caller command:\n%s", string(body))
	}

	entries, err := p.DB.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(entries) == 0 || entries[0].Action != "manager-mode-started" {
		t.Errorf("expected manager-mode-started audit, got %+v", entries)
	}
}

// TestStartSeedsMissionInboxAndScratchpad verifies the substrate-seeding
// the launcher performs on every successful Start: the mission becomes the
// first inbox message (verb=add, from=user), the audit entry records the
// id and scratchpad path, and the scratchpad lands on disk with the
// expected shape.
func TestStartSeedsMissionInboxAndScratchpad(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	mission := "ship the kernel by friday"

	p, err := Start(context.Background(), Options{
		Agent:     "claude",
		Project:   "seed",
		Mission:   mission,
		Command:   "claude",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(okCmux())),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	// Inbox: exactly one message, with the mission body verbatim.
	msgs, err := p.DB.PeekElonInbox(10)
	if err != nil {
		t.Fatalf("PeekElonInbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("inbox len = %d, want 1; msgs=%+v", len(msgs), msgs)
	}
	m := msgs[0]
	if m.Verb != "add" || m.From != "user" || m.Body != mission {
		t.Errorf("inbox msg = %+v, want verb=add from=user body=%q", m, mission)
	}
	if m.ID == "" {
		t.Error("inbox msg ID empty")
	}
	if p.MissionInboxID != m.ID {
		t.Errorf("Project.MissionInboxID = %q, want %q", p.MissionInboxID, m.ID)
	}

	// Scratchpad file exists at the path Project reports, with 0600 perms,
	// and parses as JSON with the bootstrap struct populated.
	info, err := os.Stat(p.ScratchpadPath)
	if err != nil {
		t.Fatalf("stat scratchpad: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("scratchpad perm = %o, want 0600", got)
	}
	if !strings.HasSuffix(p.ScratchpadPath, filepath.Join("arcmux", "seed", "scratchpads", "elon.json")) {
		t.Errorf("scratchpad path = %q, missing expected suffix", p.ScratchpadPath)
	}
	body, err := os.ReadFile(p.ScratchpadPath)
	if err != nil {
		t.Fatalf("read scratchpad: %v", err)
	}
	var pad struct {
		Turn      int `json:"turn"`
		Bootstrap struct {
			Project        string `json:"project"`
			Agent          string `json:"agent"`
			MissionSeeded  bool   `json:"mission_seeded"`
			MissionInboxID string `json:"mission_inbox_id"`
			MissionBytes   int    `json:"mission_bytes"`
		} `json:"bootstrap"`
	}
	if err := json.Unmarshal(body, &pad); err != nil {
		t.Fatalf("unmarshal scratchpad: %v\nraw:\n%s", err, body)
	}
	if pad.Turn != 0 {
		t.Errorf("scratchpad.turn = %d, want 0", pad.Turn)
	}
	if pad.Bootstrap.Project != "seed" || pad.Bootstrap.Agent != "claude" {
		t.Errorf("scratchpad bootstrap header off: %+v", pad.Bootstrap)
	}
	if !pad.Bootstrap.MissionSeeded {
		t.Errorf("mission_seeded = false, want true")
	}
	if pad.Bootstrap.MissionInboxID != m.ID {
		t.Errorf("scratchpad mission_inbox_id = %q, want %q", pad.Bootstrap.MissionInboxID, m.ID)
	}
	if pad.Bootstrap.MissionBytes != len(mission) {
		t.Errorf("scratchpad mission_bytes = %d, want %d", pad.Bootstrap.MissionBytes, len(mission))
	}

	// Audit entry carries the seeded metadata.
	entries, err := p.DB.RecentAudit(5)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entries")
	}
	a := entries[0]
	if a.Action != "manager-mode-started" {
		t.Fatalf("audit action = %q, want manager-mode-started", a.Action)
	}
	if got, _ := a.Detail["mission_seeded"].(bool); !got {
		t.Errorf("audit detail.mission_seeded = %v, want true", a.Detail["mission_seeded"])
	}
	if got, _ := a.Detail["mission_inbox_id"].(string); got != m.ID {
		t.Errorf("audit detail.mission_inbox_id = %q, want %q", got, m.ID)
	}
	if got, _ := a.Detail["scratchpad_path"].(string); got != p.ScratchpadPath {
		t.Errorf("audit detail.scratchpad_path = %q, want %q", got, p.ScratchpadPath)
	}
}

// TestStartEmptyMissionSkipsInboxPush asserts the "no mission" branch:
// inbox stays empty, scratchpad is still written with a (no mission
// supplied) focus, and the audit entry records mission_seeded=false.
func TestStartEmptyMissionSkipsInboxPush(t *testing.T) {
	for _, mission := range []string{"", "   \n\t  "} {
		t.Run("mission="+mission, func(t *testing.T) {
			dataRoot := t.TempDir()
			vault := t.TempDir()
			p, err := Start(context.Background(), Options{
				Agent:     "claude",
				Project:   "empty",
				Mission:   mission,
				Command:   "claude",
				DataRoot:  dataRoot,
				VaultRoot: vault,
				Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(okCmux())),
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer p.Close()

			msgs, err := p.DB.PeekElonInbox(10)
			if err != nil {
				t.Fatalf("PeekElonInbox: %v", err)
			}
			if len(msgs) != 0 {
				t.Errorf("inbox len = %d, want 0 for empty mission; msgs=%+v", len(msgs), msgs)
			}
			if p.MissionInboxID != "" {
				t.Errorf("MissionInboxID = %q, want empty", p.MissionInboxID)
			}
			if _, err := os.Stat(p.ScratchpadPath); err != nil {
				t.Fatalf("scratchpad missing: %v", err)
			}
			body, err := os.ReadFile(p.ScratchpadPath)
			if err != nil {
				t.Fatalf("read scratchpad: %v", err)
			}
			if !strings.Contains(string(body), "no mission supplied") {
				t.Errorf("scratchpad focus missing '(no mission supplied)' marker:\n%s", body)
			}
			entries, _ := p.DB.RecentAudit(5)
			if got, _ := entries[0].Detail["mission_seeded"].(bool); got {
				t.Errorf("audit mission_seeded = true, want false")
			}
		})
	}
}

// TestStartE2EArcmuxCallReadback builds bin/arcmux-cli and uses it to peek
// the seeded inbox and read the seeded scratchpad. This is the honest
// dogfood path — the same binary every spawned pane will run.
func TestStartE2EArcmuxCallReadback(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e dogfood — requires building arcmux-cli binary")
	}

	bin := filepath.Join(t.TempDir(), "arcmux-cli")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/arcmux-cli")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build arcmux-cli: %v", err)
	}

	dataRoot := t.TempDir()
	vault := t.TempDir()
	mission := "e2e: validate the dogfood roundtrip"

	p, err := Start(context.Background(), Options{
		Agent:     "claude",
		Project:   "e2e",
		Mission:   mission,
		Command:   "claude",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(okCmux())),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Close the launcher's DB before subprocess access — bbolt holds an
	// exclusive write lock per file.
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// arcmux-cli inbox peek — should see the mission message.
	peek := exec.Command(bin, "inbox", "peek",
		"--project", "e2e", "--data-root", dataRoot, "--n", "10")
	peek.Stderr = os.Stderr
	out, err := peek.Output()
	if err != nil {
		t.Fatalf("inbox peek: %v", err)
	}
	var peekOut struct {
		Messages []struct {
			ID   string `json:"id"`
			Verb string `json:"verb"`
			From string `json:"from"`
			Body string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &peekOut); err != nil {
		t.Fatalf("decode peek: %v\nraw: %s", err, out)
	}
	if len(peekOut.Messages) != 1 {
		t.Fatalf("peek messages = %d, want 1; raw=%s", len(peekOut.Messages), out)
	}
	m := peekOut.Messages[0]
	if m.Verb != "add" || m.From != "user" || m.Body != mission {
		t.Errorf("peek msg = %+v, want verb=add from=user body=%q", m, mission)
	}
	if m.ID != p.MissionInboxID {
		t.Errorf("peek msg id = %q, want %q", m.ID, p.MissionInboxID)
	}

	// arcmux-cli scratchpad read — should see the seeded JSON.
	read := exec.Command(bin, "scratchpad", "read",
		"--project", "e2e", "--data-root", dataRoot, "--role", "elon")
	read.Stderr = os.Stderr
	out, err = read.Output()
	if err != nil {
		t.Fatalf("scratchpad read: %v", err)
	}
	var readOut struct {
		Exists  bool   `json:"exists"`
		Content string `json:"content"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(out, &readOut); err != nil {
		t.Fatalf("decode read: %v\nraw: %s", err, out)
	}
	if !readOut.Exists {
		t.Fatal("scratchpad reports exists=false after launcher seed")
	}
	if readOut.Path != p.ScratchpadPath {
		t.Errorf("scratchpad path = %q, want %q", readOut.Path, p.ScratchpadPath)
	}
	if !strings.Contains(readOut.Content, `"mission_seeded": true`) {
		t.Errorf("scratchpad content missing mission_seeded:true marker:\n%s", readOut.Content)
	}
	if !strings.Contains(readOut.Content, p.MissionInboxID) {
		t.Errorf("scratchpad content missing mission inbox id %q:\n%s", p.MissionInboxID, readOut.Content)
	}
}

func TestStartRejectsBadProject(t *testing.T) {
	_, err := Start(context.Background(), Options{
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

func TestStartRequiresVault(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Agent:    "claude",
		Project:  "demo",
		DataRoot: t.TempDir(),
		Mux:      cmuxbackend.New(cmuxcli.NewWithRunnerForTest(&fakeRunner{})),
	})
	if err == nil {
		t.Error("expected error when VaultRoot is empty")
	}
}

func TestStartRequiresAgent(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Project:   "demo",
		DataRoot:  t.TempDir(),
		VaultRoot: t.TempDir(),
		Mux:       cmuxbackend.New(cmuxcli.NewWithRunnerForTest(&fakeRunner{})),
	})
	if err == nil {
		t.Error("expected error when Agent is empty")
	}
}
