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
	if strings.Contains(makefile, "systemctl --user enable $(BINARY).service; \\") {
		t.Fatal("service-install still unconditionally enables an inactive service")
	}
	if strings.Contains(makefile, "ALLOW_DIRTY_BUILD") {
		t.Fatal("release attestation exposes a dirty-build bypass")
	}
}
