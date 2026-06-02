package daemon

import (
	"strings"
	"testing"
)

func TestAgentStartCommand(t *testing.T) {
	tests := []struct {
		agent        string
		wantOK       bool
		wantContains string
	}{
		{"claude", true, "cld --remote-control"},
		{"codex", true, "codex --dangerously-bypass-approvals-and-sandbox"},
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
