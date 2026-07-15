package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testManifest() Manifest {
	now := time.Date(2026, 7, 14, 23, 0, 0, 0, time.UTC)
	historyDigest := strings.Repeat("a", 64)
	return Manifest{
		SchemaVersion:   ManifestVersion,
		HandoffID:       "handoff-1",
		TraceID:         "trace-1",
		ParentHandoffID: "handoff-parent",
		Source: SourceSession{
			DeviceID: "ref", ProfileScope: "profile:codex", SessionID: "session-1",
		},
		SourceAgent: "codex",
		Target:      TargetAgent{DeviceID: "devbox", Profile: "codex"},
		Goal: GoalSummary{
			Text: "Continue the arcmux handoff implementation.", Provenance: "explicit_operator",
			UpdatedAt: now,
		},
		History: HistoryRef{
			ArtifactID: "history-1", Basename: "2026-07-14-arcmux.md", SHA256: historyDigest,
			SizeBytes: 4096, ConversationID: "conversation-1",
		},
		Repository: RepositorySnapshot{
			ProjectSlug: "arcmux", RepoSlug: "lin-labs/arcmux", Branch: "boyan/handoff",
			SourceHead: strings.Repeat("b", 40), BaseCommit: strings.Repeat("c", 40),
			TreeDigest: strings.Repeat("d", 40), Cleanliness: RepositoryClean,
			Transfer: TransferRemoteBranch,
		},
		Artifacts: []ArtifactRef{
			{Kind: ArtifactPullRequest, ID: "pr-4", Repo: &ArtifactRepoRef{Repo: "lin-labs/arcmux", Commit: strings.Repeat("b", 40)}},
			{Kind: ArtifactSessionHistory, ID: "session-history", Session: &ArtifactSessionRef{ProfileScope: "profile:codex", SessionID: "session-1"}},
		},
		Validation: ValidationEvidence{State: ValidationNotRun},
		CreatedAt:  now,
	}
}

func TestManifestDigestIsCanonicalJSONSHA256(t *testing.T) {
	manifest := testManifest()
	got, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}

	changed := manifest
	changed.Goal.Text = "A different bounded goal."
	other, err := changed.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if other == got {
		t.Fatal("manifest content change did not change digest")
	}
}

func TestManifestTypeGraphContainsNoMaps(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var inspect func(reflect.Type)
	inspect = func(value reflect.Type) {
		if seen[value] {
			return
		}
		seen[value] = true
		switch value.Kind() {
		case reflect.Map:
			t.Fatalf("manifest type graph contains map %s", value)
		case reflect.Pointer, reflect.Slice, reflect.Array:
			inspect(value.Elem())
		case reflect.Struct:
			if value.PkgPath() == "time" {
				return
			}
			for i := 0; i < value.NumField(); i++ {
				inspect(value.Field(i).Type)
			}
		}
	}
	inspect(reflect.TypeOf(Manifest{}))
}

func TestManifestRejectsPathCredentialAndUnsafeRepository(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"history path", func(m *Manifest) { m.History.Basename = "../history.md" }},
		{"credential in goal", func(m *Manifest) { m.Goal.Text = "OPENAI_API_KEY=sk-abcdefghijklmnop" }},
		{"control in goal", func(m *Manifest) { m.Goal.Text = "launch\nnow" }},
		{"repository traversal", func(m *Manifest) { m.Repository.RepoSlug = "lin-labs/../secret" }},
		{"dirty remote branch", func(m *Manifest) { m.Repository.Cleanliness = RepositoryDirty }},
		{"patch on remote branch", func(m *Manifest) { m.Repository.Patch = testPatch() }},
		{"same source and target", func(m *Manifest) { m.Target.DeviceID = m.Source.DeviceID }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
}

func TestStoredPatchTransferRequiresBoundedPatch(t *testing.T) {
	manifest := testManifest()
	manifest.Repository.Transfer = TransferStoredPatch
	manifest.Repository.Cleanliness = RepositoryDirty
	if err := manifest.Validate(); err == nil {
		t.Fatal("stored_patch without patch accepted")
	}
	manifest.Repository.Patch = testPatch()
	manifest.Repository.TreeDigest = manifest.Repository.Patch.ResultTree
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid stored patch rejected: %v", err)
	}
	manifest.Repository.Patch.SizeBytes = maxPatchBytes + 1
	if err := manifest.Validate(); err == nil {
		t.Fatal("oversize stored patch accepted")
	}
}

func TestValidationEvidenceStateShape(t *testing.T) {
	manifest := testManifest()
	completed := manifest.CreatedAt.Add(-time.Minute)
	manifest.Validation = ValidationEvidence{State: ValidationPassed, RepositoryRevision: manifest.Repository.SourceHead, CompletedAt: &completed}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid completed evidence rejected: %v", err)
	}
	manifest.Validation.RepositoryRevision = strings.Repeat("e", 40)
	if err := manifest.Validate(); err == nil {
		t.Fatal("validation evidence for a different repository revision accepted")
	}
	manifest.Validation.RepositoryRevision = manifest.Repository.SourceHead
	manifest.Validation.CompletedAt = nil
	if err := manifest.Validate(); err == nil {
		t.Fatal("completed evidence without timestamp accepted")
	}
}

func TestStoredPatchValidationBindsResultTree(t *testing.T) {
	manifest := testManifest()
	manifest.Repository.Transfer = TransferStoredPatch
	manifest.Repository.Cleanliness = RepositoryDirty
	manifest.Repository.Patch = testPatch()
	manifest.Repository.TreeDigest = manifest.Repository.Patch.ResultTree
	completed := manifest.CreatedAt.Add(-time.Minute)
	manifest.Validation = ValidationEvidence{
		State: ValidationPassed, RepositoryRevision: manifest.Repository.Patch.ResultTree, CompletedAt: &completed,
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("matching patch validation rejected: %v", err)
	}
	manifest.Repository.TreeDigest = strings.Repeat("d", 40)
	if err := manifest.Validate(); err == nil {
		t.Fatal("patch result tree different from repository tree accepted")
	}
}

func TestGoalRejectsKnownCredentialForms(t *testing.T) {
	credential := func(parts ...string) string { return strings.Join(parts, "") }
	credentials := []string{
		credential("ghp_", "abcdefghijklmnopqrstuvwxyz123456"),
		credential("github_pat_", "abcdefghijklmnopqrstuvwxyz123456"),
		credential("xoxb-", "1234567890-", "abcdefghijklmnop"),
		credential("xai-", "abcdefghijklmnopqrstuvwxyz123456"),
		credential("sk-", "abcdefghijklmnopqrstuvwxyz123456"),
		credential("sk_", "abcdefghijklmnopqrstuvwxyz123456"),
		"Bearer abcdefghijklmnopqrstuvwxyz",
		credential("-----BEGIN OPENSSH ", "PRIVATE KEY-----"),
		"OPENAI_API_KEY = abcdefghijklmnop",
	}
	for _, credential := range credentials {
		t.Run(credential[:min(12, len(credential))], func(t *testing.T) {
			manifest := testManifest()
			manifest.Goal.Text = "Continue with " + credential
			if err := manifest.Validate(); err == nil {
				t.Fatalf("credential form accepted: %s", credential)
			}
		})
	}
}

func TestGitRefRejectsOptionAndDotComponents(t *testing.T) {
	for _, branch := range []string{"--upload-pack=evil", "-c", "foo/.bar", "foo/./bar", "foo/../bar"} {
		t.Run(branch, func(t *testing.T) {
			manifest := testManifest()
			manifest.Repository.Branch = branch
			if err := manifest.Validate(); err == nil {
				t.Fatalf("unsafe git ref accepted: %s", branch)
			}
		})
	}
}

func testPatch() *StoredPatchRef {
	return &StoredPatchRef{
		ArtifactID: "patch-1", SHA256: strings.Repeat("e", 64), SizeBytes: 1024,
		ResultTree: strings.Repeat("f", 40),
	}
}
