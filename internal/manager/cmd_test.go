package manager

import (
	"flag"
	"reflect"
	"testing"
)

func makeTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("mission", "", "")
	fs.String("data-root", "", "")
	fs.String("vault-root", "", "")
	fs.String("command", "", "")
	fs.Bool("focus", true, "")
	return fs
}

func TestSplitFlagsAndPositionals(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantFlags   []string
		wantPositns []string
	}{
		{
			"all flags before positionals",
			[]string{"--focus=false", "--mission", "do X", "claude", "arcmux"},
			[]string{"--focus=false", "--mission", "do X"},
			[]string{"claude", "arcmux"},
		},
		{
			"all positionals before flags",
			[]string{"claude", "arcmux", "--focus=false", "--mission", "do X"},
			[]string{"--focus=false", "--mission", "do X"},
			[]string{"claude", "arcmux"},
		},
		{
			"mixed",
			[]string{"--mission", "do X", "claude", "--focus=false", "arcmux"},
			[]string{"--mission", "do X", "--focus=false"},
			[]string{"claude", "arcmux"},
		},
		{
			"--command takes a value",
			[]string{"--command", "claude --append-system-prompt-file /tmp/r.md", "claude", "arcmux"},
			[]string{"--command", "claude --append-system-prompt-file /tmp/r.md"},
			[]string{"claude", "arcmux"},
		},
		{
			"-- separator passes through to positionals",
			[]string{"--focus=false", "--", "claude", "--this-is-positional"},
			[]string{"--focus=false"},
			[]string{"claude", "--this-is-positional"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := makeTestFlagSet()
			gotFlags, gotPos := splitFlagsAndPositionals(tc.args, fs)
			if !reflect.DeepEqual(gotFlags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", gotFlags, tc.wantFlags)
			}
			if !reflect.DeepEqual(gotPos, tc.wantPositns) {
				t.Errorf("positionals = %q, want %q", gotPos, tc.wantPositns)
			}
		})
	}
}
