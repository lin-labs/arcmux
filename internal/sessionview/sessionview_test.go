package sessionview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/session"
)

func TestBuildRedactsUnsafeRuntimeFieldsAndRawPrompt(t *testing.T) {
	now := time.Date(2026, 7, 14, 22, 0, 0, 0, time.UTC)
	idleSince := now.Add(-time.Minute)
	snap := session.Snapshot{
		ID:               "s-safe-123",
		Name:             "safe",
		Agent:            "codex",
		CWD:              "/Users/test/project",
		Transport:        "tmux",
		Env:              map[string]string{"OPENAI_API_KEY": "env-super-secret"},
		TmuxSessionName:  "private-tmux-session",
		TmuxTarget:       "%private-pane",
		CurrentCommand:   "raw-current-command",
		BackendSessionID: "backend-private-id",
		PID:              4242,
		State:            session.StateIdle,
		Health:           "healthy",
		StartedAt:        now.Add(-time.Hour),
		LastActivityAt:   now.Add(-time.Minute),
		IdleSince:        &idleSince,
		NudgeCount:       2,
		OwnerID:          "owner",
	}
	hookState := &hooks.SessionState{
		UpdatedAt:  now,
		TurnCount:  3,
		EventsSeen: 8,
		TurnContract: &hooks.TurnContract{
			Goal:            "ship it api_key=sk-live123456789",
			OverallGoal:     "Keep bearer abcdefghijklmnop private",
			LastUserMessage: "raw-user-prompt-must-not-cross",
			VaultLink:       "/Users/test/agents/histories/2026-07-14-private.md",
			Source:          "session-hook",
			UpdatedAt:       now,
		},
	}

	_, detail, err := Build(RootProfileScope, snap, hookState, now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	encoded := string(data)
	for _, forbidden := range []string{
		"env-super-secret", "OPENAI_API_KEY", "private-tmux-session",
		"%private-pane", "raw-current-command", "backend-private-id",
		"4242", "raw-user-prompt-must-not-cross", "/Users/test/agents/histories",
		"sk-live123456789", "abcdefghijklmnop",
		`"env"`, `"pid"`, `"tmux_target"`, `"tmux_session_name"`,
		`"current_command"`, `"backend_session_id"`, `"last_user_message"`,
		`"latest_ask"`,
	} {
		if strings.Contains(encoded, forbidden) {
			t.Errorf("serialized detail leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, `"basename":"2026-07-14-private.md"`) {
		t.Errorf("history basename missing: %s", encoded)
	}
	if detail.Summary.Work == nil {
		t.Fatal("safe summarized work is missing")
	}
	if detail.Summary.Work.LatestAsk != "" {
		t.Fatalf("latest ask must not be synthesized from goal or raw prompt: %#v", detail.Summary.Work)
	}
}

func TestProfileScopeAndDeterministicSort(t *testing.T) {
	alpha, err := NamedProfileScope("alpha")
	if err != nil {
		t.Fatalf("NamedProfileScope: %v", err)
	}
	if got := string(alpha); got != "profile:alpha" {
		t.Fatalf("scope = %q", got)
	}
	for _, invalid := range []ProfileScope{"", "profile:", "profile:Alpha", "alpha"} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("scope %q unexpectedly valid", invalid)
		}
	}
	if _, err := NewLocator(RootProfileScope, "../escape"); err == nil {
		t.Fatal("path-like session id unexpectedly valid")
	}

	summaries := []Summary{
		{Locator: Locator{ProfileScope: alpha, SessionID: "same"}},
		{Locator: Locator{ProfileScope: RootProfileScope, SessionID: "z"}},
		{Locator: Locator{ProfileScope: RootProfileScope, SessionID: "same"}},
	}
	Sort(summaries)
	got := []string{
		string(summaries[0].Locator.ProfileScope) + "/" + summaries[0].Locator.SessionID,
		string(summaries[1].Locator.ProfileScope) + "/" + summaries[1].Locator.SessionID,
		string(summaries[2].Locator.ProfileScope) + "/" + summaries[2].Locator.SessionID,
	}
	want := []string{"profile:alpha/same", "root/same", "root/z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sort = %v, want %v", got, want)
		}
	}
}

func TestBuildCurrentWorkRequiresSummarizerProvenance(t *testing.T) {
	now := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	snap := session.Snapshot{
		ID: "s-work", Agent: "codex", State: session.StateWorking,
		StartedAt: now.Add(-time.Hour), LastActivityAt: now,
	}
	raw := &hooks.SessionState{TurnContract: &hooks.TurnContract{
		OverallGoal: "raw launch prompt", OverallGoalUpdatedAt: now,
	}}
	summary, _, err := Build(RootProfileScope, snap, raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CurrentWork != nil {
		t.Fatalf("unproven overall goal crossed into current work: %#v", summary.CurrentWork)
	}

	proven := &hooks.SessionState{TurnContract: &hooks.TurnContract{
		OverallGoal:           "  Ship\nremote surfaces api_key=sk-live123456789  ",
		OverallGoalProvenance: hooks.OverallGoalSummarizerProvenance,
		OverallGoalUpdatedAt:  now,
	}}
	summary, _, err = Build(RootProfileScope, snap, proven, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CurrentWork != nil {
		t.Fatalf("credential-like current work must be omitted, got %#v", summary.CurrentWork)
	}
}

func TestBuildCurrentWorkAdvancesSourceFreshness(t *testing.T) {
	now := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	summaryUpdated := now.Add(2 * time.Minute)
	snap := session.Snapshot{
		ID: "s-fresh", Agent: "codex", State: session.StateWorking,
		StartedAt: now.Add(-time.Hour), LastActivityAt: now,
	}
	hookState := &hooks.SessionState{UpdatedAt: summaryUpdated, TurnContract: &hooks.TurnContract{
		OverallGoal: "Ship exact native remote identity", OverallGoalProvenance: hooks.OverallGoalSummarizerProvenance,
		OverallGoalUpdatedAt: summaryUpdated,
	}}
	summary, _, err := Build(RootProfileScope, snap, hookState, summaryUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CurrentWork == nil || !summary.Freshness.SourceUpdatedAt.Equal(summaryUpdated) {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestNormalizeCurrentWorkRejectsCredentialLikeSummary(t *testing.T) {
	now := time.Now().UTC()
	for _, value := range []string{
		"postgres://user:password@host/db",
		"https://user:pass@example.com/path",
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnop",
		"AWS_SECRET_ACCESS_KEY=abcdefghijklmnop",
		"GCP_CREDENTIALS=/tmp/gcp.json",
		"xai_api_key=xai_abcdefghijklmnop",
		"api key = sk-proj-abcdefghijklmnop",
		"access token: abcdefghijklmnop",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==",
		"JWT eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk",
		"-----BEGIN PRIVATE KEY-----",
	} {
		if _, err := NormalizeCurrentWork(&CurrentWorkSummary{
			Summary: value, Provenance: hooks.OverallGoalSummarizerProvenance, UpdatedAt: now,
		}); err == nil {
			t.Fatalf("credential-like current work accepted: %q", value)
		}
	}
}

func TestNormalizeCurrentWorkBoundsAndRejectsSpoofedSource(t *testing.T) {
	now := time.Now().UTC()
	if _, err := NormalizeCurrentWork(&CurrentWorkSummary{
		Summary: "pretend safe", Provenance: "hook.raw_prompt", UpdatedAt: now,
	}); err == nil {
		t.Fatal("spoofed provenance accepted")
	}
	value, err := NormalizeCurrentWork(&CurrentWorkSummary{
		Summary:    strings.Repeat("x", 300),
		Provenance: hooks.OverallGoalSummarizerProvenance,
		UpdatedAt:  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(value.Summary)) != maxCurrentWorkRunes {
		t.Fatalf("summary runes=%d", len([]rune(value.Summary)))
	}
}
