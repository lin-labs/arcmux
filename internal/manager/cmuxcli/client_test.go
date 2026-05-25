package cmuxcli

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls [][]string
	out   string
	outs  map[string]string // substring → output
	err   error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.outs != nil {
		joined := strings.Join(args, " ")
		for k, v := range f.outs {
			if strings.Contains(joined, k) {
				return v, f.err
			}
		}
	}
	return f.out, f.err
}

func TestNewWorkspace(t *testing.T) {
	f := &fakeRunner{out: "OK workspace:32\n"}
	c := newWithRunner(f)
	ws, err := c.NewWorkspace(context.Background(), NewWorkspaceOptions{
		Name:    "team-foo",
		CWD:     "/tmp/work",
		Command: "claude",
		Focus:   true,
	})
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if ws.Ref != "workspace:32" {
		t.Errorf("ref = %q, want workspace:32", ws.Ref)
	}
	got := strings.Join(f.calls[0], " ")
	for _, want := range []string{"new-workspace", "--name", "team-foo", "--cwd", "/tmp/work", "--command", "claude", "--focus", "true"} {
		if !strings.Contains(got, want) {
			t.Errorf("call missing %q in %q", want, got)
		}
	}
}

func TestNewWorkspaceRejectsBadOutput(t *testing.T) {
	f := &fakeRunner{out: "definitely not a ref\n"}
	c := newWithRunner(f)
	if _, err := c.NewWorkspace(context.Background(), NewWorkspaceOptions{Name: "x"}); err == nil {
		t.Error("expected error on unparsable output")
	}
}

func TestNewPane(t *testing.T) {
	f := &fakeRunner{out: "OK pane:42\n"}
	c := newWithRunner(f)
	p, err := c.NewPane(context.Background(), NewPaneOptions{Workspace: "workspace:2", Direction: "right"})
	if err != nil {
		t.Fatalf("NewPane: %v", err)
	}
	if p.Ref != "pane:42" {
		t.Errorf("ref = %q, want pane:42", p.Ref)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "new-pane") || !strings.Contains(got, "workspace:2") || !strings.Contains(got, "right") {
		t.Errorf("call = %q", got)
	}
}

func TestListPanes(t *testing.T) {
	f := &fakeRunner{out: `{
		"workspace_ref": "workspace:32",
		"panes": [
			{"ref": "pane:39", "index": 0, "focused": true, "surface_refs": ["surface:123"], "selected_surface_ref": "surface:123", "surface_count": 1}
		]
	}`}
	c := newWithRunner(f)
	panes, err := c.ListPanes(context.Background(), "workspace:32")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("panes = %d, want 1", len(panes))
	}
	if panes[0].Ref != "pane:39" {
		t.Errorf("pane[0].Ref = %q, want pane:39", panes[0].Ref)
	}
	if !panes[0].Focused {
		t.Error("pane[0].Focused should be true")
	}
}

func TestSend(t *testing.T) {
	f := &fakeRunner{out: ""}
	c := newWithRunner(f)
	if err := c.Send(context.Background(), "surface:5", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	args := f.calls[0]
	got := strings.Join(args, " ")
	// Must use --surface (NOT --target — that was the bug; --target is not a
	// real cmux flag, which turned the wake into literal text in the input box).
	if !strings.Contains(got, "send") || !strings.Contains(got, "--surface") || !strings.Contains(got, "surface:5") {
		t.Errorf("call = %q (must contain send, --surface, surface:5)", got)
	}
	if strings.Contains(got, "--target") {
		t.Errorf("call leaked --target: %q (must use --surface)", got)
	}
	// Last positional arg must end with `\n` so cmux fires Enter and submits.
	last := args[len(args)-1]
	if !strings.HasSuffix(last, `\n`) {
		t.Errorf("payload missing trailing literal \\n (got %q)", last)
	}
}

func TestSendDoesNotDoubleNewline(t *testing.T) {
	f := &fakeRunner{out: ""}
	c := newWithRunner(f)
	if err := c.Send(context.Background(), "surface:5", `already\n`); err != nil {
		t.Fatalf("Send: %v", err)
	}
	last := f.calls[0][len(f.calls[0])-1]
	if last != `already\n` {
		t.Errorf("payload = %q, want %q (existing \\n must not be doubled)", last, `already\n`)
	}
}

func TestSendRawNoNewline(t *testing.T) {
	f := &fakeRunner{out: ""}
	c := newWithRunner(f)
	if err := c.SendRaw(context.Background(), "surface:5", "edit me"); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	last := f.calls[0][len(f.calls[0])-1]
	if last != "edit me" {
		t.Errorf("payload = %q, want %q (SendRaw must not append)", last, "edit me")
	}
}

func TestCloseWorkspace(t *testing.T) {
	f := &fakeRunner{out: "OK workspace:32\n"}
	c := newWithRunner(f)
	if err := c.CloseWorkspace(context.Background(), "workspace:32"); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "close-workspace") || !strings.Contains(got, "workspace:32") {
		t.Errorf("call = %q", got)
	}
}

func TestNotifyAndStatus(t *testing.T) {
	f := &fakeRunner{}
	c := newWithRunner(f)
	if err := c.Notify(context.Background(), "workspace:1", "title", "body"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if err := c.SetStatus(context.Background(), "pane:1", "working"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := c.SetProgress(context.Background(), "pane:1", 0.42); err != nil {
		t.Fatalf("SetProgress: %v", err)
	}
	if err := c.Log(context.Background(), "pane:1", "hello"); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if err := c.TriggerFlash(context.Background(), "surface:1"); err != nil {
		t.Fatalf("TriggerFlash: %v", err)
	}
	if len(f.calls) != 5 {
		t.Errorf("calls = %d, want 5", len(f.calls))
	}
}

func TestParseOKRef(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"OK workspace:32\n", "workspace:32"},
		{"OK pane:5", "pane:5"},
		{"  OK surface:9\n", "surface:9"},
		// Multi-token output — first ref-shaped token wins.
		{"OK surface:149 pane:63 workspace:56\n", "surface:149"},
		{"Error: something", ""},
		{"OK foo bar", ""},
	} {
		got := parseOKRef(tc.in)
		if got != tc.want {
			t.Errorf("parseOKRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseOKRefKind(t *testing.T) {
	out := "OK surface:149 pane:63 workspace:56\n"
	cases := map[string]string{
		"pane":      "pane:63",
		"workspace": "workspace:56",
		"surface":   "surface:149",
		"missing":   "",
	}
	for kind, want := range cases {
		if got := parseOKRefKind(out, kind); got != want {
			t.Errorf("parseOKRefKind(%q, %q) = %q, want %q", out, kind, got, want)
		}
	}
}

func TestNewPaneMultiToken(t *testing.T) {
	f := &fakeRunner{out: "OK surface:149 pane:63 workspace:56\n"}
	c := newWithRunner(f)
	p, err := c.NewPane(context.Background(), NewPaneOptions{Workspace: "workspace:56", Direction: "right"})
	if err != nil {
		t.Fatalf("NewPane: %v", err)
	}
	if p.Ref != "pane:63" {
		t.Errorf("pane ref = %q, want pane:63", p.Ref)
	}
	if p.SelectedSurf != "surface:149" {
		t.Errorf("surface ref = %q, want surface:149", p.SelectedSurf)
	}
}
