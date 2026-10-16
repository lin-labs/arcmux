package daemon

import (
	"fmt"
	"strings"
)

// knownTrustPhrases match folder-trust dialogs across agent versions
// (Claude Code 2.1.x and Codex 0.16x both say "trust this folder").
var knownTrustPhrases = []string{"trust this folder", "do you trust"}

// isTrustPrompt reports whether a trust dialog is on the bottom of the screen,
// where agents render it, so quoted dialog text in scrollback is ignored.
func isTrustPrompt(screen, pattern string) bool {
	lines := nonEmptyLines(screen)
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	tail := strings.Join(lines, "\n")
	if pattern != "" && containsFold(tail, pattern) {
		return true
	}
	for _, phrase := range knownTrustPhrases {
		if containsFold(tail, phrase) {
			return true
		}
	}
	return false
}

// trustPromptKeys returns the keys that accept the trust dialog on screen. In
// a cursor menu it moves to the affirmative option instead of trusting the
// default, which Claude Code 2.1.301 sets to "No, exit" (arcmux-c4i).
func trustPromptKeys(screen, response string) ([]string, error) {
	lines := nonEmptyLines(screen)
	cursor, column := -1, 0
	for i, line := range lines {
		if plain, ok := withoutCursor(line); ok {
			lines[i], cursor, column = plain, i, textColumn(plain)
		}
	}
	if cursor < 0 {
		if response == "" || response == "Enter" {
			return []string{"Enter"}, nil
		}
		return []string{response, "Enter"}, nil
	}
	for _, step := range []int{1, -1} {
		for i := cursor; i >= 0 && i < len(lines); i += step {
			if i != cursor && textColumn(lines[i]) != column {
				break
			}
			if isAffirmativeOption(lines[i][column:]) {
				return moveKeys(i - cursor), nil
			}
		}
	}
	return nil, fmt.Errorf("trust prompt menu has no affirmative option")
}

func moveKeys(offset int) []string {
	key := "Down"
	if offset < 0 {
		key, offset = "Up", -offset
	}
	keys := make([]string, 0, offset+1)
	for ; offset > 0; offset-- {
		keys = append(keys, key)
	}
	return append(keys, "Enter")
}

// withoutCursor replaces a menu cursor glyph with one space, keeping the
// option text in the same column as its uncursored siblings.
func withoutCursor(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	for _, glyph := range []string{"❯", "›"} {
		if strings.HasPrefix(trimmed, glyph+" ") {
			return strings.Replace(line, glyph, " ", 1), true
		}
	}
	return line, false
}

func textColumn(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func isAffirmativeOption(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if dot := strings.Index(text, ". "); dot > 0 && dot <= 2 {
		text = text[dot+2:]
	}
	return strings.HasPrefix(text, "yes") || strings.HasPrefix(text, "trust")
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, " "))
		}
	}
	return out
}
