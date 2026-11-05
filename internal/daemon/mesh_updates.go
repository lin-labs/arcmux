package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// updates.list serves per-session JSON records written by `mission-control
// watch`, so a Mac can pull them over the mesh.
const (
	updatesHintsFile   = "hints.json"
	updatesMonitorFile = "monitor.json"
)

var updatesUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type updatesListParams struct {
	Since  string        `json:"since,omitempty"`
	Cursor string        `json:"cursor,omitempty"`
	Hints  []surfaceHint `json:"hints,omitempty"`
}

type surfaceHint struct {
	MoshPort    int    `json:"mosh_port"`
	SurfaceID   string `json:"surface_id"`
	WorkspaceID string `json:"workspace_id"`
}

type updatesFile struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

// updatesListResult carries free text (session intents); unlike other mesh
// wire types, its own scope updates.read gates it to your own devices.
type updatesListResult struct {
	Files      []updatesFile   `json:"files"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Monitor    json.RawMessage `json:"monitor,omitempty"`
	Skipped    []string        `json:"skipped,omitempty"`
}

func (d *Daemon) listUpdates(dir string, p updatesListParams) (updatesListResult, error) {
	result := updatesListResult{Files: []updatesFile{}}
	if dir == "" {
		return result, errors.New("mesh.updates_dir is not configured")
	}
	var since time.Time
	if p.Since != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, p.Since); err != nil {
			return result, errors.New("since must be RFC 3339")
		}
	}
	for _, h := range p.Hints {
		if !updatesUUID.MatchString(h.SurfaceID) || h.MoshPort < 1 || h.MoshPort > 65535 {
			return result, errors.New("invalid surface hint")
		}
	}
	if len(p.Hints) > 0 {
		if err := writeUpdatesHints(dir, p.Hints); err != nil {
			return result, fmt.Errorf("write hints: %w", err)
		}
	}
	if monitor, err := os.ReadFile(filepath.Join(dir, updatesMonitorFile)); err == nil && json.Valid(monitor) {
		result.Monitor = monitor
	}
	entries, err := os.ReadDir(dir) // sorted by name
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}
	fits := func(r updatesListResult) bool {
		b, err := json.Marshal(r)
		return err == nil && len(b) <= meshPageResultBudget
	}
	if !fits(result) {
		result.Monitor = nil
	}
	last := ""
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") ||
			name == updatesHintsFile || name == updatesMonitorFile || name <= p.Cursor {
			continue
		}
		info, err := e.Info()
		if err != nil || (!since.IsZero() && !info.ModTime().After(since)) {
			continue
		}
		skip := func() updatesListResult {
			r := result
			r.Skipped = append(append([]string(nil), result.Skipped...), name)
			return r
		}
		var next updatesListResult
		var data []byte
		if info.Size() <= meshPageResultBudget {
			data, _ = os.ReadFile(filepath.Join(dir, name))
		}
		if data == nil || !json.Valid(data) {
			next = skip()
		} else {
			next = result
			next.Files = append(append([]updatesFile(nil), result.Files...), updatesFile{Name: name, Data: data})
			if !fits(next) && len(result.Files) == 0 {
				next = skip() // a lone record that cannot fit is skipped, not retried forever
			}
		}
		if !fits(next) {
			result.NextCursor = last
			break
		}
		result, last = next, name
	}
	return result, nil
}

func writeUpdatesHints(dir string, hints []surfaceHint) error {
	b, err := json.Marshal(hints)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hints-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, updatesHintsFile))
}
