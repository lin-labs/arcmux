package hooks

import (
	"os"
	"path/filepath"
)

// MigrateLegacySessionState moves per-session state docs (*.json, including
// the archived/ subdirectory) from the legacy application-named dir
// (~/data/arcmux/sessions) into the protocol dir (~/data/mux/sessions). It is
// the coded migration for the dir rename, run on every daemon startup when
// the config is on defaults: idempotent (a missing legacy dir or an empty
// sweep is fine), and it never overwrites a file that already exists at the
// destination (the new location is authoritative once written). Returns the
// number of files moved.
func MigrateLegacySessionState(legacyDir, dir string) (int, error) {
	if legacyDir == "" || dir == "" || legacyDir == dir {
		return 0, nil
	}
	moved := 0
	var firstErr error
	for _, sub := range []string{"", "archived"} {
		src := filepath.Join(legacyDir, sub)
		matches, err := filepath.Glob(filepath.Join(src, "*.json"))
		if err != nil || len(matches) == 0 {
			continue
		}
		dst := filepath.Join(dir, sub)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, m := range matches {
			target := filepath.Join(dst, filepath.Base(m))
			if _, err := os.Stat(target); err == nil {
				continue // destination is authoritative
			}
			if err := os.Rename(m, target); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			moved++
		}
	}
	return moved, firstErr
}
