package packaging

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseRequiresSafetyAttestationAndPreservesInactiveService(t *testing.T) {
	data, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(data)
	for _, required := range []string{
		"verify-artifact:",
		"verify-summarizer-safety:",
		"SummarizerSourcesContainNoProcessLaunchCapability",
		"verify-installed:",
		"summarizer_safety",
		"direct-api-only-v1",
		"service-install: install verify-installed",
		"source_revision=\"$$(git rev-parse --verify HEAD)\"",
		"test \"$(BUILD_REVISION)\" = \"$$source_revision\"",
		"if [ \"$$was_active\" = true ]; then",
	} {
		if !strings.Contains(makefile, required) {
			t.Fatalf("Makefile release safety is missing %q", required)
		}
	}
	// A fresh install must stay inactive, but a box already running arcmux from a
	// disabled unit must get enabled or it dies at the next reboot. Both live in
	// service-install, so check placement: every `enable` must sit inside the
	// was_active branch. A bare substring check cannot tell the two apart.
	guard := strings.Index(makefile, "if [ \"$$was_active\" = true ]; then")
	inactive := strings.Index(makefile, "echo \"installed systemd user unit; left it inactive\"")
	if guard < 0 || inactive < 0 || guard >= inactive {
		t.Fatal("service-install lost its was_active guard around the inactive path")
	}
	for _, at := range enableOffsets(makefile, "systemctl --user enable $(BINARY).service") {
		if at < guard || at > inactive {
			t.Fatalf("service-install enables the unit outside the was_active branch (offset %d)", at)
		}
	}
	if strings.Contains(makefile, "ALLOW_DIRTY_BUILD") {
		t.Fatal("release attestation exposes a dirty-build bypass")
	}
}

// enableOffsets returns every byte offset of needle in haystack.
func enableOffsets(haystack, needle string) []int {
	var out []int
	for at := 0; ; {
		i := strings.Index(haystack[at:], needle)
		if i < 0 {
			return out
		}
		out = append(out, at+i)
		at += i + len(needle)
	}
}
