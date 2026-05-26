package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestForProject(t *testing.T) {
	p := ForProject("/tmp/data", "/tmp/vault", "myproj")

	wantEphemeral := "/tmp/data/arcmux/myproj"
	if p.EphemeralRoot != wantEphemeral {
		t.Errorf("EphemeralRoot = %q, want %q", p.EphemeralRoot, wantEphemeral)
	}

	wantVault := "/tmp/vault/Projects/myproj"
	if p.VaultRoot != wantVault {
		t.Errorf("VaultRoot = %q, want %q", p.VaultRoot, wantVault)
	}

	if !strings.HasSuffix(p.StateBolt, "state.bolt") {
		t.Errorf("StateBolt path %q missing state.bolt suffix", p.StateBolt)
	}

	if filepath.Dir(p.StateBolt) != wantEphemeral {
		t.Errorf("StateBolt dir = %q, want %q", filepath.Dir(p.StateBolt), wantEphemeral)
	}
}

func TestValidate(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if _, err := Validate(""); err == nil {
			t.Error("Validate(\"\") should error")
		}
	})
	t.Run("with slash", func(t *testing.T) {
		if _, err := Validate("foo/bar"); err == nil {
			t.Error("Validate with slash should error")
		}
	})
	t.Run("with dotdot", func(t *testing.T) {
		if _, err := Validate(".."); err == nil {
			t.Error("Validate(\"..\") should error")
		}
	})
	t.Run("valid", func(t *testing.T) {
		got, err := Validate("my-project-1")
		if err != nil {
			t.Errorf("Validate(\"my-project-1\") errored: %v", err)
		}
		if got != "my-project-1" {
			t.Errorf("Validate returned %q, want %q", got, "my-project-1")
		}
	})
}
