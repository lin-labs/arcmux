package daemon

import (
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/session"
)

func TestAgentStartCommand(t *testing.T) {
	tests := []struct {
		agent        string
		wantOK       bool
		wantContains string
	}{
		{"claude", true, "cld --remote-control"},
		{"codex", true, "cdx remote-control"},
		{"gemini", false, ""},
		{"", false, ""},
	}

	for _, tc := range tests {
		cmd, ok := agentStartCommand(tc.agent)
		if ok != tc.wantOK {
			t.Errorf("agentStartCommand(%q) ok = %v, want %v", tc.agent, ok, tc.wantOK)
		}
		if tc.wantOK && !strings.Contains(cmd, tc.wantContains) {
			t.Errorf("agentStartCommand(%q) = %q, want it to contain %q", tc.agent, cmd, tc.wantContains)
		}
		if !tc.wantOK && cmd != "" {
			t.Errorf("agentStartCommand(%q) = %q, want empty", tc.agent, cmd)
		}
	}
}

func TestIsCodexRemoteServerSnapshot(t *testing.T) {
	tests := []struct {
		name string
		snap session.Snapshot
		want bool
	}{
		{
			name: "codex remote server",
			snap: session.Snapshot{Agent: "codex", CurrentCommand: "cdx remote-control"},
			want: true,
		},
		{
			name: "codex tui",
			snap: session.Snapshot{
				Agent:          "codex",
				CurrentCommand: "codex --dangerously-bypass-approvals-and-sandbox --no-alt-screen",
			},
			want: false,
		},
		{
			name: "claude remote control",
			snap: session.Snapshot{Agent: "claude", CurrentCommand: "cld --remote-control"},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCodexRemoteServerSnapshot(tc.snap); got != tc.want {
				t.Fatalf("isCodexRemoteServerSnapshot() = %v, want %v", got, tc.want)
			}
		})
	}
}
