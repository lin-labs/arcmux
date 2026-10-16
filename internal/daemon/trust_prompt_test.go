package daemon

import (
	"reflect"
	"testing"
)

// Screens captured live on 2026-10-15 (arcmux-c4i).
const claudeTrustScreen = ` Accessing workspace:
 /home/blin/data/mc2-spike/trust-6wSE
 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team).
 Claude Code'll be able to read, edit, and execute files here.
 Security guide
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel`

const codexTrustScreen = `  Folder access
  /home/blin/data/mc2-spike/ctrust-lCn3
  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings.
› 1. Trust and continue
  2. Back to Agent Command Center
  enter continue · esc back`

func TestTrustPromptKeys(t *testing.T) {
	for _, tc := range []struct {
		name, screen string
		want         []string
		wantErr      bool
	}{
		{"claude cursor on No moves down to Yes", claudeTrustScreen, []string{"Down", "Enter"}, false},
		{"codex cursor already on Trust", codexTrustScreen, []string{"Enter"}, false},
		{"cursor below Yes moves up", " ❯ 1. Yes, proceed\n   2. No, exit", []string{"Enter"}, false},
		{"yes above cursor", "   Yes, I trust this folder\n ❯ No, exit", []string{"Up", "Enter"}, false},
		{"no menu keeps configured response", "Do you trust the files in this folder? Press Enter", []string{"Enter"}, false},
		{"menu without affirmative option refuses", " ❯ No, exit\n   Cancel", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustPromptKeys(tc.screen, "Enter")
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("trustPromptKeys() = %v, %v; want %v, err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestIsTrustPrompt(t *testing.T) {
	for _, screen := range []string{claudeTrustScreen, codexTrustScreen, "Do you trust the files in this folder?"} {
		if !isTrustPrompt(screen, "do you trust") {
			t.Fatalf("not detected as trust prompt:\n%s", screen)
		}
	}
	if isTrustPrompt("❯ Try \"fix lint errors\"", "do you trust") {
		t.Fatal("ready prompt misdetected as trust prompt")
	}
}
