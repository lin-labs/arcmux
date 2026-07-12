package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGenericHook_CapturesArtifacts verifies the claude path end-to-end: a
// PostToolUse Write payload produces an `--artifact create:<path>` call to
// `arcmux hook` and a `paths` field in the JSONL audit line.
func TestGenericHook_CapturesArtifacts(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	outDir := filepath.Join(tmpDir, "out")
	installer := NewInstaller(outDir)
	if _, err := installer.Install("s-art", "claude_hooks", hookDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	script := GenericHookPath(hookDir)
	argvFile := filepath.Join(tmpDir, "argv.txt")
	fakeArcmux := filepath.Join(tmpDir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-art",
		"ARCMUX_HOOK_OUTPUT_DIR="+outDir,
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"/tmp/plans/design doc.md","content":"..."}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v (%s)", err, out)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	call := string(argv)
	if !strings.Contains(call, "--artifact create:/tmp/plans/design doc.md") {
		t.Errorf("argv missing artifact flag: %q", call)
	}
	data, err := os.ReadFile(installer.OutputPath("s-art"))
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	if !strings.Contains(string(data), `"paths":"create:/tmp/plans/design doc.md"`) {
		t.Errorf("jsonl missing paths field: %q", data)
	}
}

// TestGenericHook_CapturesCodexPatchArtifacts covers the codex apply_patch
// dialect: file ops parsed from the patch text headers, with relative paths
// resolved against the hook process's working directory.
func TestGenericHook_CapturesCodexPatchArtifacts(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	outDir := filepath.Join(tmpDir, "out")
	installer := NewInstaller(outDir)
	if _, err := installer.Install("s-cdx", "claude_hooks", hookDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	script := GenericHookPath(hookDir)
	argvFile := filepath.Join(tmpDir, "argv.txt")
	fakeArcmux := filepath.Join(tmpDir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := `{"hook_event_name":"PostToolUse","tool_name":"apply_patch","tool_input":{"input":"*** Begin Patch\n*** Add File: docs/new.md\n+hello\n*** Update File: internal/x.go\n*** End Patch"}}`
	cmd := exec.Command("/bin/sh", script)
	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-cdx",
		"ARCMUX_HOOK_OUTPUT_DIR="+outDir,
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v (%s)", err, out)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("fake arcmux never invoked: %v", err)
	}
	call := string(argv)
	if !strings.Contains(call, "--artifact create:"+filepath.Join(repoDir, "docs/new.md")) {
		t.Errorf("argv missing Add File artifact: %q", call)
	}
	if !strings.Contains(call, "--artifact edit:"+filepath.Join(repoDir, "internal/x.go")) {
		t.Errorf("argv missing Update File artifact: %q", call)
	}
}

// TestGenericHook_ReadToolIsNotAnArtifact guards the false-positive path: a
// PostToolUse for Read carries a file_path but must NOT be recorded.
func TestGenericHook_ReadToolIsNotAnArtifact(t *testing.T) {
	tmpDir := t.TempDir()
	hookDir := filepath.Join(tmpDir, "claude")
	outDir := filepath.Join(tmpDir, "out")
	installer := NewInstaller(outDir)
	if _, err := installer.Install("s-read", "claude_hooks", hookDir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	script := GenericHookPath(hookDir)
	argvFile := filepath.Join(tmpDir, "argv.txt")
	fakeArcmux := filepath.Join(tmpDir, "arcmux")
	if err := os.WriteFile(fakeArcmux, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+argvFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(),
		"ARCMUX_SESSION_ID=s-read",
		"ARCMUX_HOOK_OUTPUT_DIR="+outDir,
		"ARCMUX_BIN="+fakeArcmux,
	)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"/etc/passwd"}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run hook: %v (%s)", err, out)
	}
	argv, _ := os.ReadFile(argvFile)
	if strings.Contains(string(argv), "--artifact") {
		t.Errorf("Read must not produce an artifact, argv: %q", argv)
	}
}

func TestApplyArtifacts_MergeUpgradeCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// First touch: an Edit records the file as "edit".
	upd := TurnContractUpdate{Artifacts: []ArtifactOp{{Op: "edit", Path: "/a.md"}}}
	if err := ApplyEventWithContract(dir, "s-1", "claude", EventToolEnd, "Edit", upd, now); err != nil {
		t.Fatal(err)
	}
	// Second touch by Write upgrades op to "create" and bumps Edits.
	upd = TurnContractUpdate{Artifacts: []ArtifactOp{{Op: "create", Path: "/a.md"}, {Op: "edit", Path: "/b.md"}}}
	if err := ApplyEventWithContract(dir, "s-1", "claude", EventToolEnd, "Write", upd, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	st, err := ReadSessionState(dir, "s-1")
	if err != nil || st == nil {
		t.Fatalf("read state: %v", err)
	}
	if len(st.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2 (%+v)", len(st.Artifacts), st.Artifacts)
	}
	a := st.Artifacts[0]
	if a.Path != "/a.md" || a.Op != "create" || a.Edits != 2 {
		t.Errorf("merged artifact wrong: %+v", a)
	}
	if !a.LastSeen.After(a.FirstSeen) {
		t.Errorf("LastSeen not advanced: %+v", a)
	}
	if st.Artifacts[1].Path != "/b.md" || st.Artifacts[1].Op != "edit" {
		t.Errorf("second artifact wrong: %+v", st.Artifacts[1])
	}
}

func TestApplyArtifacts_EvictsLeastRecentlyTouched(t *testing.T) {
	st := &SessionState{}
	base := time.Now()
	for i := 0; i < maxArtifacts; i++ {
		applyArtifacts(st, []ArtifactOp{{Op: "edit", Path: filepath.Join("/f", string(rune('a'+i%26)), time.Duration(i).String())}}, base.Add(time.Duration(i)*time.Second))
	}
	if len(st.Artifacts) != maxArtifacts {
		t.Fatalf("pre-cap artifacts = %d, want %d", len(st.Artifacts), maxArtifacts)
	}
	oldest := st.Artifacts[0].Path
	applyArtifacts(st, []ArtifactOp{{Op: "create", Path: "/overflow.md"}}, base.Add(time.Hour))
	if len(st.Artifacts) != maxArtifacts {
		t.Fatalf("post-cap artifacts = %d, want %d", len(st.Artifacts), maxArtifacts)
	}
	for _, a := range st.Artifacts {
		if a.Path == oldest {
			t.Errorf("least-recently-touched %q not evicted", oldest)
		}
	}
}

func TestFeaturePinSemantics(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// Auto-derived feature lands.
	if err := ApplyContractOnly(dir, "s-f", "claude", TurnContractUpdate{Feature: "Artifact Tracking"}, now); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadSessionState(dir, "s-f")
	if st.TurnContract.Feature != "artifact-tracking" || st.TurnContract.FeaturePinned {
		t.Fatalf("auto feature wrong: %+v", st.TurnContract)
	}

	// Pin overrides and locks.
	if err := ApplyContractOnly(dir, "s-f", "claude", TurnContractUpdate{Feature: "voice-babysit", PinFeature: true}, now); err != nil {
		t.Fatal(err)
	}
	st, _ = ReadSessionState(dir, "s-f")
	if st.TurnContract.Feature != "voice-babysit" || !st.TurnContract.FeaturePinned {
		t.Fatalf("pin not applied: %+v", st.TurnContract)
	}

	// Subsequent auto-derive is ignored while pinned.
	if err := ApplyContractOnly(dir, "s-f", "claude", TurnContractUpdate{Feature: "something-else"}, now); err != nil {
		t.Fatal(err)
	}
	st, _ = ReadSessionState(dir, "s-f")
	if st.TurnContract.Feature != "voice-babysit" {
		t.Fatalf("pinned feature overwritten by auto: %+v", st.TurnContract)
	}
}

func TestSanitizeFeatureSlug(t *testing.T) {
	cases := map[string]string{
		"Artifact Tracking":  "artifact-tracking",
		"FEATURE: foo_bar!":  "feature-foo-bar",
		"  voice  babysit  ": "voice-babysit",
		"---":                "",
		"":                   "",
	}
	for in, want := range cases {
		if got := SanitizeFeatureSlug(in); got != want {
			t.Errorf("SanitizeFeatureSlug(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("abcde-", 20)
	if got := SanitizeFeatureSlug(long); len(got) > 48 {
		t.Errorf("slug not capped: %d chars", len(got))
	}
}
