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
	fs.Bool("update-roles", false, "")
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
			[]string{"--update-roles", "--focus=false", "--mission", "do X", "claude", "arcmux"},
			[]string{"--update-roles", "--focus=false", "--mission", "do X"},
			[]string{"claude", "arcmux"},
		},
		{
			"all positionals before flags",
			[]string{"claude", "arcmux", "--update-roles", "--focus=false", "--mission", "do X"},
			[]string{"--update-roles", "--focus=false", "--mission", "do X"},
			[]string{"claude", "arcmux"},
		},
		{
			"mixed",
			[]string{"--mission", "do X", "claude", "--update-roles", "arcmux"},
			[]string{"--mission", "do X", "--update-roles"},
			[]string{"claude", "arcmux"},
		},
		{
			"bool flag without value",
			[]string{"--update-roles", "claude", "arcmux"},
			[]string{"--update-roles"},
			[]string{"claude", "arcmux"},
		},
		{
			"-- separator passes through to positionals",
			[]string{"--update-roles", "--", "claude", "--this-is-positional"},
			[]string{"--update-roles"},
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
