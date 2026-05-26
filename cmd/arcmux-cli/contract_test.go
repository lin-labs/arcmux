package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/store"
)

// commonArgs returns the (--project, p, --data-root, dr) prefix every
// contract subcommand expects.
func commonArgs(project, dataRoot string) []string {
	return []string{"--project", project, "--data-root", dataRoot}
}

func TestCmdContractCreateHappyFlags(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"

	var out bytes.Buffer
	args := append(commonArgs(project, dataRoot),
		"--id", "c-1",
		"--team", "alpha",
		"--ic-role", "linus",
		"--priority", "5",
		"--objective", "ship the auth module",
		"--output-format", "PR",
		"--tools", "bash,edit,read",
		"--boundaries", "no migrations,no secrets",
		"--acceptance", "tests pass,docs updated",
		"--depends-on", "c-0",
	)
	if err := cmdContractCreate(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	var ack struct {
		OK       bool   `json:"ok"`
		ID       string `json:"id"`
		State    string `json:"state"`
		Team     string `json:"team"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v\nraw: %s", err, out.String())
	}
	if !ack.OK || ack.ID != "c-1" || ack.State != store.ContractPending || ack.Team != "alpha" || ack.Priority != 5 {
		t.Errorf("ack = %+v", ack)
	}

	// Read it back via get.
	out.Reset()
	if err := cmdContractGet(append(commonArgs(project, dataRoot), "--id", "c-1"), &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	var got struct {
		Contract store.Contract `json:"contract"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	c := got.Contract
	if c.Objective != "ship the auth module" || c.OutputFormat != "PR" || c.ICRole != "linus" {
		t.Errorf("get core fields wrong: %+v", c)
	}
	wantTools := []string{"bash", "edit", "read"}
	if !equalSlices(c.Tools, wantTools) {
		t.Errorf("tools = %v, want %v", c.Tools, wantTools)
	}
	wantBoundaries := []string{"no migrations", "no secrets"}
	if !equalSlices(c.Boundaries, wantBoundaries) {
		t.Errorf("boundaries = %v, want %v", c.Boundaries, wantBoundaries)
	}
	wantAcceptance := []string{"tests pass", "docs updated"}
	if !equalSlices(c.AcceptanceCriteria, wantAcceptance) {
		t.Errorf("acceptance = %v, want %v", c.AcceptanceCriteria, wantAcceptance)
	}
	if !equalSlices(c.DependsOn, []string{"c-0"}) {
		t.Errorf("depends_on = %v, want [c-0]", c.DependsOn)
	}
}

func TestCmdContractCreateObjectiveFromStdin(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"
	var out bytes.Buffer
	args := append(commonArgs(project, dataRoot),
		"--id", "c-stdin", "--team", "alpha",
	)
	long := "this is a long\nobjective\nspanning multiple lines\n"
	if err := cmdContractCreate(args, strings.NewReader(long), &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	out.Reset()
	if err := cmdContractGet(append(commonArgs(project, dataRoot), "--id", "c-stdin"), &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	var got struct {
		Contract store.Contract `json:"contract"`
	}
	_ = json.Unmarshal(out.Bytes(), &got)
	// Trailing newline trimmed; internal newlines preserved.
	wantObj := "this is a long\nobjective\nspanning multiple lines"
	if got.Contract.Objective != wantObj {
		t.Errorf("objective = %q, want %q", got.Contract.Objective, wantObj)
	}
}

func TestCmdContractCreateRejectsDuplicate(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"
	var out bytes.Buffer
	args := append(commonArgs(project, dataRoot),
		"--id", "dup", "--team", "alpha", "--objective", "first",
	)
	if err := cmdContractCreate(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("first create: %v", err)
	}
	out.Reset()
	err := cmdContractCreate(args, strings.NewReader(""), &out)
	if err == nil {
		t.Fatalf("want duplicate error, got nil; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %q, want substring 'already exists'", err)
	}
}

func TestCmdContractCreateRejectsBadInput(t *testing.T) {
	t.Setenv("ARCMUX_PROJECT", "")
	t.Setenv("ARCMUX_DATA", "")

	cases := []struct {
		name string
		args []string
		body string
	}{
		{"missing id", []string{"--project", "p", "--data-root", t.TempDir(),
			"--team", "alpha", "--objective", "x"}, ""},
		{"missing team", []string{"--project", "p", "--data-root", t.TempDir(),
			"--id", "c", "--objective", "x"}, ""},
		{"missing objective", []string{"--project", "p", "--data-root", t.TempDir(),
			"--id", "c", "--team", "alpha"}, ""},
		{"both flag and stdin", []string{"--project", "p", "--data-root", t.TempDir(),
			"--id", "c", "--team", "alpha", "--objective", "x"}, "y"},
		{"bad id", []string{"--project", "p", "--data-root", t.TempDir(),
			"--id", "../evil", "--team", "alpha", "--objective", "x"}, ""},
		{"bad team", []string{"--project", "p", "--data-root", t.TempDir(),
			"--id", "c", "--team", "../evil", "--objective", "x"}, ""},
		{"bad depends-on", []string{"--project", "p", "--data-root", t.TempDir(),
			"--id", "c", "--team", "alpha", "--objective", "x", "--depends-on", "c-1,../evil"}, ""},
		{"missing project", []string{"--data-root", t.TempDir(),
			"--id", "c", "--team", "alpha", "--objective", "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := cmdContractCreate(tc.args, strings.NewReader(tc.body), &out); err == nil {
				t.Fatalf("expected error, got nil; stdout=%q", out.String())
			}
		})
	}
}

func TestCmdContractGetMissing(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"
	var out bytes.Buffer
	err := cmdContractGet(append(commonArgs(project, dataRoot), "--id", "ghost"), &out)
	if err == nil {
		t.Fatalf("want not-found error, got nil; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %q, want substring 'not found'", err)
	}
}

func TestCmdContractListFilters(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"

	seedContract(t, dataRoot, project, "a-1", "alpha", "do A", 1)
	seedContract(t, dataRoot, project, "a-2", "alpha", "do A2", 5)
	seedContract(t, dataRoot, project, "b-1", "beta", "do B", 3)

	// List all (sorted: a-2(5), b-1(3), a-1(1)).
	var out bytes.Buffer
	if err := cmdContractList(commonArgs(project, dataRoot), &out); err != nil {
		t.Fatalf("list all: %v", err)
	}
	var all struct {
		Contracts []store.Contract `json:"contracts"`
	}
	if err := json.Unmarshal(out.Bytes(), &all); err != nil {
		t.Fatalf("decode list all: %v", err)
	}
	if len(all.Contracts) != 3 {
		t.Fatalf("list all = %d contracts, want 3", len(all.Contracts))
	}
	wantOrder := []string{"a-2", "b-1", "a-1"}
	for i, want := range wantOrder {
		if all.Contracts[i].ID != want {
			t.Errorf("list[%d].ID = %q, want %q", i, all.Contracts[i].ID, want)
		}
	}

	// Filter by team.
	out.Reset()
	if err := cmdContractList(append(commonArgs(project, dataRoot), "--team", "alpha"), &out); err != nil {
		t.Fatalf("list team: %v", err)
	}
	var alpha struct {
		Contracts []store.Contract `json:"contracts"`
	}
	_ = json.Unmarshal(out.Bytes(), &alpha)
	if len(alpha.Contracts) != 2 {
		t.Fatalf("team-alpha = %d, want 2", len(alpha.Contracts))
	}

	// Filter by both.
	out.Reset()
	args := append(commonArgs(project, dataRoot), "--team", "alpha", "--state", store.ContractPending)
	if err := cmdContractList(args, &out); err != nil {
		t.Fatalf("list team+state: %v", err)
	}
}

func TestCmdContractTransitionFlow(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"
	seedContract(t, dataRoot, project, "c-1", "alpha", "do work", 1)

	// pending → ready.
	var out bytes.Buffer
	args := append(commonArgs(project, dataRoot),
		"--id", "c-1", "--to", store.ContractReady,
		"--reason", "deps met", "--by", "elon",
	)
	if err := cmdContractTransition(args, &out); err != nil {
		t.Fatalf("transition pending→ready: %v", err)
	}
	var ack struct {
		OK   bool   `json:"ok"`
		ID   string `json:"id"`
		From string `json:"from"`
		To   string `json:"to"`
		By   string `json:"by"`
	}
	if err := json.Unmarshal(out.Bytes(), &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if !ack.OK || ack.From != store.ContractPending || ack.To != store.ContractReady || ack.By != "elon" {
		t.Errorf("ack = %+v", ack)
	}

	// ready → working.
	out.Reset()
	args = append(commonArgs(project, dataRoot),
		"--id", "c-1", "--to", store.ContractWorking, "--by", "manager",
	)
	if err := cmdContractTransition(args, &out); err != nil {
		t.Fatalf("transition ready→working: %v", err)
	}

	// Invalid: working → ready is not a valid transition.
	out.Reset()
	args = append(commonArgs(project, dataRoot),
		"--id", "c-1", "--to", store.ContractReady, "--by", "manager",
	)
	if err := cmdContractTransition(args, &out); err == nil {
		t.Fatalf("want invalid-transition error, got nil; out=%q", out.String())
	}
}

func TestCmdContractTransitionDepsBlocks(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"
	seedContract(t, dataRoot, project, "parent", "alpha", "first", 1)
	seedContractDep(t, dataRoot, project, "child", "alpha", "second", 1, []string{"parent"})

	// Try child → ready while parent still pending. Must error.
	var out bytes.Buffer
	args := append(commonArgs(project, dataRoot),
		"--id", "child", "--to", store.ContractReady, "--by", "test",
	)
	err := cmdContractTransition(args, &out)
	if err == nil {
		t.Fatalf("want dep-not-completed error, got nil; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Errorf("err = %q, want substring 'parent'", err)
	}
}

func TestCmdContractDeps(t *testing.T) {
	dataRoot := t.TempDir()
	project := "ctest"
	seedContract(t, dataRoot, project, "p1", "alpha", "x", 1)
	seedContract(t, dataRoot, project, "p2", "alpha", "y", 1)
	seedContractDep(t, dataRoot, project, "child", "alpha", "z", 1, []string{"p1", "p2"})

	// Child's parents.
	var out bytes.Buffer
	if err := cmdContractDeps(append(commonArgs(project, dataRoot), "--id", "child"), &out); err != nil {
		t.Fatalf("deps child: %v", err)
	}
	var deps struct {
		ID       string   `json:"id"`
		Parents  []string `json:"parents"`
		Children []string `json:"children"`
	}
	if err := json.Unmarshal(out.Bytes(), &deps); err != nil {
		t.Fatalf("decode deps: %v", err)
	}
	if deps.ID != "child" {
		t.Errorf("id = %q, want child", deps.ID)
	}
	if len(deps.Parents) != 2 {
		t.Errorf("parents = %v, want 2 entries", deps.Parents)
	}
	if len(deps.Children) != 0 {
		t.Errorf("children = %v, want empty", deps.Children)
	}

	// Parent's children.
	out.Reset()
	if err := cmdContractDeps(append(commonArgs(project, dataRoot), "--id", "p1"), &out); err != nil {
		t.Fatalf("deps p1: %v", err)
	}
	deps = struct {
		ID       string   `json:"id"`
		Parents  []string `json:"parents"`
		Children []string `json:"children"`
	}{}
	_ = json.Unmarshal(out.Bytes(), &deps)
	if len(deps.Children) != 1 || deps.Children[0] != "child" {
		t.Errorf("p1 children = %v, want [child]", deps.Children)
	}
}

func TestCmdContractDispatcher(t *testing.T) {
	var out bytes.Buffer
	if err := cmdContract([]string{}, strings.NewReader(""), &out); err == nil {
		t.Error("want usage error on empty args")
	}
	if err := cmdContract([]string{"nope"}, strings.NewReader(""), &out); err == nil {
		t.Error("want unknown-subcommand error")
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{",,,", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitCSV(tc.in)
		if !equalSlices(got, tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDefaultActor(t *testing.T) {
	t.Setenv("ARCMUX_ROLE", "")
	if got := defaultActor(); got != "arcmux-cli" {
		t.Errorf("unset: got %q, want arcmux-cli", got)
	}
	t.Setenv("ARCMUX_ROLE", "elon")
	if got := defaultActor(); got != "elon" {
		t.Errorf("set: got %q, want elon", got)
	}
}

// TestContractE2EBinaryReadback builds bin/arcmux-cli and exercises the
// full contract lifecycle (create → list → get → transition → deps) as a
// subprocess. Validates that the JSON envelope shape every spawned
// role-holder sees is the same one in-process callers get.
func TestContractE2EBinaryReadback(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e — requires building arcmux-cli binary")
	}
	bin := filepath.Join(t.TempDir(), "arcmux-cli")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build arcmux-cli: %v", err)
	}

	dataRoot := t.TempDir()
	project := "e2econ"
	common := []string{"--project", project, "--data-root", dataRoot}

	// Create parent contract.
	create := exec.Command(bin, append([]string{"contract", "create"}, append(common,
		"--id", "parent", "--team", "alpha", "--objective", "lay foundation",
		"--priority", "5")...)...)
	create.Stderr = os.Stderr
	if out, err := create.Output(); err != nil {
		t.Fatalf("create parent: %v\nraw: %s", err, out)
	}

	// Create child contract depending on parent.
	createChild := exec.Command(bin, append([]string{"contract", "create"}, append(common,
		"--id", "child", "--team", "alpha", "--objective", "build atop",
		"--priority", "9", "--depends-on", "parent")...)...)
	createChild.Stderr = os.Stderr
	if out, err := createChild.Output(); err != nil {
		t.Fatalf("create child: %v\nraw: %s", err, out)
	}

	// list — child first (priority 9), then parent (priority 5).
	list := exec.Command(bin, append([]string{"contract", "list"}, common...)...)
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("list: %v\nraw: %s", err, out)
	}
	var listOut struct {
		Contracts []store.Contract `json:"contracts"`
	}
	if err := json.Unmarshal(out, &listOut); err != nil {
		t.Fatalf("decode list: %v\nraw: %s", err, out)
	}
	if len(listOut.Contracts) != 2 {
		t.Fatalf("list returned %d contracts, want 2; raw=%s", len(listOut.Contracts), out)
	}
	if listOut.Contracts[0].ID != "child" || listOut.Contracts[1].ID != "parent" {
		t.Errorf("list order = [%s, %s], want [child, parent]",
			listOut.Contracts[0].ID, listOut.Contracts[1].ID)
	}

	// deps — child has parent as parent.
	deps := exec.Command(bin, append([]string{"contract", "deps"}, append(common, "--id", "child")...)...)
	deps.Stderr = os.Stderr
	out, err = deps.Output()
	if err != nil {
		t.Fatalf("deps: %v\nraw: %s", err, out)
	}
	var depOut struct {
		Parents  []string `json:"parents"`
		Children []string `json:"children"`
	}
	_ = json.Unmarshal(out, &depOut)
	if len(depOut.Parents) != 1 || depOut.Parents[0] != "parent" {
		t.Errorf("deps.parents = %v, want [parent]", depOut.Parents)
	}

	// Transition parent through full happy path.
	for _, target := range []string{store.ContractReady, store.ContractWorking, store.ContractValidating, store.ContractCompleted} {
		cmd := exec.Command(bin, append([]string{"contract", "transition"},
			append(common, "--id", "parent", "--to", target, "--by", "e2e")...)...)
		cmd.Stderr = os.Stderr
		if out, err := cmd.Output(); err != nil {
			t.Fatalf("transition parent→%s: %v\nraw: %s", target, err, out)
		}
	}

	// Now child should be allowed to go ready.
	transChild := exec.Command(bin, append([]string{"contract", "transition"},
		append(common, "--id", "child", "--to", store.ContractReady, "--by", "e2e")...)...)
	transChild.Stderr = os.Stderr
	if out, err := transChild.Output(); err != nil {
		t.Fatalf("transition child→ready: %v\nraw: %s", err, out)
	}

	// get child — verify state.
	get := exec.Command(bin, append([]string{"contract", "get"}, append(common, "--id", "child")...)...)
	get.Stderr = os.Stderr
	out, err = get.Output()
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	var getOut struct {
		Contract store.Contract `json:"contract"`
	}
	_ = json.Unmarshal(out, &getOut)
	if getOut.Contract.State != store.ContractReady {
		t.Errorf("child state = %q, want ready", getOut.Contract.State)
	}
}

// seedContract creates a basic contract via the CLI handler for tests that
// need a pre-existing row. Keeps the per-test setup small.
func seedContract(t *testing.T, dataRoot, project, id, team, objective string, priority int) {
	t.Helper()
	var out bytes.Buffer
	args := []string{
		"--project", project, "--data-root", dataRoot,
		"--id", id, "--team", team, "--objective", objective,
	}
	if priority != 0 {
		args = append(args, "--priority", itoa(priority))
	}
	if err := cmdContractCreate(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func seedContractDep(t *testing.T, dataRoot, project, id, team, objective string, priority int, deps []string) {
	t.Helper()
	var out bytes.Buffer
	args := []string{
		"--project", project, "--data-root", dataRoot,
		"--id", id, "--team", team, "--objective", objective,
		"--depends-on", strings.Join(deps, ","),
	}
	if priority != 0 {
		args = append(args, "--priority", itoa(priority))
	}
	if err := cmdContractCreate(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func itoa(n int) string {
	// strconv would work; this keeps a single-purpose helper local.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
