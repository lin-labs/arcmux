package hooks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxCanonicalHistoryFrontmatterBytes = 16 << 10

var canonicalConversationIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// ApplyVerifiedCanonicalHistoryBinding is the sole trusted writer for a
// canonical history reference. Verification and the fixed provenance stamp
// live behind one API so generic contract writers cannot manufacture proof.
func ApplyVerifiedCanonicalHistoryBinding(stateDir, sessionID, agent, historyRoot, basename, conversationID string, now time.Time) error {
	if err := VerifyCanonicalHistoryBinding(historyRoot, basename, conversationID); err != nil {
		return err
	}
	return mutateSessionState(stateDir, sessionID, func(st *SessionState) {
		st.SessionID = sessionID
		if agent != "" {
			st.Agent = agent
		}
		if st.CreatedAt.IsZero() {
			st.CreatedAt = now
		}
		if st.TurnContract == nil {
			st.TurnContract = &TurnContract{}
		}
		st.TurnContract.CanonicalHistory = &CanonicalHistoryBinding{
			Basename: basename, ConversationID: conversationID,
			Provenance: CanonicalHistoryBindingProvenance, UpdatedAt: now,
		}
		st.TurnContract.UpdatedAt = now
		st.UpdatedAt = now
	})
}

// VerifyCanonicalHistoryBinding proves that basename names one regular file
// directly under the canonical history root and that the file's frontmatter
// carries the exact native conversation identity supplied by the producer. It
// deliberately stops reading at the closing frontmatter fence; transcript body
// text is neither an input nor an output of history attribution.
func VerifyCanonicalHistoryBinding(historyRoot, basename, conversationID string) error {
	if err := validateCanonicalHistoryMetadata(basename, conversationID); err != nil {
		return err
	}
	if !filepath.IsAbs(historyRoot) || filepath.Clean(historyRoot) != historyRoot {
		return errors.New("canonical history root must be an absolute clean path")
	}
	resolvedRoot, err := filepath.EvalSymlinks(historyRoot)
	if err != nil {
		return fmt.Errorf("resolve canonical history root: %w", err)
	}
	root, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return fmt.Errorf("open canonical history root: %w", err)
	}
	defer root.Close()

	before, err := root.Lstat(basename)
	if err != nil {
		return fmt.Errorf("inspect canonical history: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return errors.New("canonical history must be a regular non-symlink file")
	}
	file, err := root.Open(basename)
	if err != nil {
		return fmt.Errorf("open canonical history: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return errors.New("canonical history changed while opening")
	}

	fields, err := readCanonicalHistoryFrontmatter(&fileByteReader{reader: file})
	if err != nil {
		return fmt.Errorf("read canonical history frontmatter: %w", err)
	}
	if fields["conversation_id"] != conversationID {
		return errors.New("canonical history conversation identity does not match the exact producer identity")
	}

	afterFile, fileErr := file.Stat()
	afterPath, pathErr := root.Lstat(basename)
	if fileErr != nil || pathErr != nil || afterPath.Mode()&os.ModeSymlink != 0 || !afterPath.Mode().IsRegular() ||
		!os.SameFile(opened, afterFile) || !os.SameFile(opened, afterPath) ||
		afterFile.Size() != opened.Size() || afterPath.Size() != opened.Size() ||
		!afterFile.ModTime().Equal(opened.ModTime()) || !afterPath.ModTime().Equal(opened.ModTime()) {
		return errors.New("canonical history changed while validating frontmatter")
	}
	return nil
}

func validateCanonicalHistoryMetadata(basename, conversationID string) error {
	if basename == "" || utf8.RuneCountInString(basename) > 255 || filepath.Base(basename) != basename ||
		basename == "." || basename == ".." || !strings.HasSuffix(basename, ".md") || strings.ContainsAny(basename, "/\\\x00\r\n") {
		return errors.New("canonical history basename must name one Markdown file directly under the history root")
	}
	for _, r := range basename {
		if unicode.IsControl(r) {
			return errors.New("canonical history basename contains control characters")
		}
	}
	if !canonicalConversationIDRe.MatchString(conversationID) {
		return errors.New("canonical history conversation identity is invalid")
	}
	return nil
}

func readCanonicalHistoryFrontmatter(reader io.ByteReader) (map[string]string, error) {
	total := 0
	readLine := func() (string, error) {
		var line strings.Builder
		for {
			if total >= maxCanonicalHistoryFrontmatterBytes {
				return "", errors.New("canonical history frontmatter exceeds size limit")
			}
			value, err := reader.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) && line.Len() > 0 {
					return strings.TrimSuffix(line.String(), "\r"), nil
				}
				return "", err
			}
			total++
			if value == '\n' {
				return strings.TrimSuffix(line.String(), "\r"), nil
			}
			line.WriteByte(value)
		}
	}

	first, err := readLine()
	if err != nil || first != "---" {
		return nil, errors.New("canonical history is missing opening frontmatter fence")
	}
	fields := make(map[string]string)
	for {
		line, err := readLine()
		if err != nil {
			return nil, errors.New("canonical history is missing closing frontmatter fence")
		}
		if line == "---" {
			return fields, nil
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("canonical history frontmatter repeats %q", key)
		}
		fields[key] = strings.TrimSpace(value)
	}
}

type fileByteReader struct {
	reader io.Reader
}

func (r *fileByteReader) ReadByte() (byte, error) {
	var value [1]byte
	_, err := io.ReadFull(r.reader, value[:])
	return value[0], err
}
