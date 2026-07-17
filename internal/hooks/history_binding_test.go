package hooks

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyCanonicalHistoryBindingRequiresExactFrontmatterIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	basename := "2026-07-16-exact-session.md"
	content := "---\nagent: codex\nconversation_id: native-conversation-123\ncwd: /same/repo\n---\n\nPRIVATE TRANSCRIPT BODY MUST NOT PARTICIPATE\n"
	if err := os.WriteFile(filepath.Join(root, basename), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := VerifyCanonicalHistoryBinding(root, basename, "native-conversation-123"); err != nil {
		t.Fatalf("VerifyCanonicalHistoryBinding: %v", err)
	}
	for _, mismatch := range []struct {
		basename       string
		conversationID string
	}{
		{basename: "../" + basename, conversationID: "native-conversation-123"},
		{basename: basename, conversationID: "other-conversation"},
		{basename: basename, conversationID: "native/conversation"},
	} {
		if err := VerifyCanonicalHistoryBinding(root, mismatch.basename, mismatch.conversationID); err == nil {
			t.Fatalf("unsafe or mismatched binding accepted: %+v", mismatch)
		}
	}
}

func TestVerifyCanonicalHistoryBindingRejectsSymlinkAndDuplicateIdentity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target.md")
	if err := os.WriteFile(target, []byte("---\nconversation_id: exact-id\n---\nsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCanonicalHistoryBinding(root, "link.md", "exact-id"); err == nil {
		t.Fatal("symlinked canonical history was accepted")
	}

	duplicate := "---\nconversation_id: exact-id\nconversation_id: exact-id\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(root, "duplicate.md"), []byte(duplicate), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCanonicalHistoryBinding(root, "duplicate.md", "exact-id"); err == nil {
		t.Fatal("duplicate conversation_id was accepted")
	}
}

func TestReadCanonicalHistoryFrontmatterStopsBeforeTranscriptBody(t *testing.T) {
	t.Parallel()
	frontmatter := "---\nconversation_id: exact-id\nagent: codex\n---\n"
	body := "RAW-PRIVATE-TRANSCRIPT-SENTINEL"
	reader := &countingByteReader{data: []byte(frontmatter + body)}
	fields, err := readCanonicalHistoryFrontmatter(reader)
	if err != nil {
		t.Fatal(err)
	}
	if fields["conversation_id"] != "exact-id" {
		t.Fatalf("frontmatter=%v", fields)
	}
	if reader.offset != len(frontmatter) {
		t.Fatalf("binding reader consumed %d bytes, want exact frontmatter length %d", reader.offset, len(frontmatter))
	}
	if strings.Contains(string(reader.data[:reader.offset]), body) {
		t.Fatal("binding reader consumed transcript body")
	}
}

type countingByteReader struct {
	data   []byte
	offset int
}

func (r *countingByteReader) ReadByte() (byte, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	b := r.data[r.offset]
	r.offset++
	return b, nil
}

func TestReadCanonicalHistoryFrontmatterRejectsOversizedOrUnclosedMetadata(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"conversation_id: no-opening-fence\n---\n",
		"---\nconversation_id: never-closed\n",
		"---\nconversation_id: " + strings.Repeat("x", maxCanonicalHistoryFrontmatterBytes) + "\n---\n",
	} {
		_, err := readCanonicalHistoryFrontmatter(&countingByteReader{data: []byte(input)})
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("invalid frontmatter error=%v input-prefix=%q", err, input[:min(len(input), 32)])
		}
	}
}
