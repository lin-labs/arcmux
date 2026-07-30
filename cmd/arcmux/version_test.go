package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestVersionJSONAttestsBuildRevisionAndSummarizerSafety(t *testing.T) {
	oldRevision, oldTime := buildRevision, buildTime
	buildRevision, buildTime = "abc123", "2026-07-30T18:00:00Z"
	t.Cleanup(func() { buildRevision, buildTime = oldRevision, oldTime })

	var output bytes.Buffer
	if err := cmdVersion([]string{"--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version          string `json:"version"`
		Revision         string `json:"revision"`
		BuildTime        string `json:"build_time"`
		SummarizerSafety string `json:"summarizer_safety"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != version || got.Revision != "abc123" || got.BuildTime != "2026-07-30T18:00:00Z" ||
		got.SummarizerSafety != summarizerSafetyCapability {
		t.Fatalf("version attestation=%+v", got)
	}
}
