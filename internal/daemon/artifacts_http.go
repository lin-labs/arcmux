package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
)

// The artifacts endpoint answers "what did this conversation produce?" — the
// hook-captured file operations from the session state doc, enriched at query
// time with git status from the session cwd, plus a computed FOCUS suggestion
// (the file a voice caller most likely wants: the most recently touched
// artifact within the window, with the largest recent file as the alternate).

type artifactEntry struct {
	Path      string    `json:"path"`
	Op        string    `json:"op"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Edits     int       `json:"edits"`
	// SizeBytes is the file's current on-disk size (-1 when it no longer
	// exists — deleted or never landed).
	SizeBytes int64 `json:"size_bytes"`
	// GitStatus is the porcelain XY code when the file is dirty in the
	// session cwd's repo ("M", "A", "??", …); empty for clean/untracked-by-git
	// locations such as vault docs.
	GitStatus string `json:"git_status,omitempty"`
}

type gitFileEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type artifactsFocus struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type sessionArtifactsResponse struct {
	SessionID string          `json:"session_id"`
	Name      string          `json:"name,omitempty"`
	Project   string          `json:"project,omitempty"`
	Platform  bool            `json:"platform,omitempty"`
	Feature   string          `json:"feature,omitempty"`
	Archived  bool            `json:"archived,omitempty"`
	Artifacts []artifactEntry `json:"artifacts"`
	// GitExtra lists dirty/untracked repo files NOT captured as artifacts —
	// typically Bash-mediated changes (generators, mv, redirects).
	GitExtra []gitFileEntry  `json:"git_extra,omitempty"`
	Focus    *artifactsFocus `json:"focus,omitempty"`
	AltFocus *artifactsFocus `json:"alt_focus,omitempty"`
}

// handleSessionArtifacts serves GET /session/artifacts?name=<session name or
// id>[&window=2h]. Live sessions resolve by name (or id); ended sessions still
// answer from their archived state doc when queried by session id.
func (h *HTTPServer) handleSessionArtifacts(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	window := 2 * time.Hour
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("bad window %q (want e.g. 2h, 30m)", v)})
			return
		}
		window = d
	}

	resp := sessionArtifactsResponse{Artifacts: []artifactEntry{}}
	stateDir := h.daemon.cfg.Hooks.SessionStateDir

	var st *hooks.SessionState
	var cwd string
	if sess := h.findByName(name); sess != nil {
		snap := sess.Snapshot()
		resp.SessionID = snap.ID
		resp.Name = snap.Name
		resp.Project = snap.ProjectSlug
		resp.Platform = snap.Platform
		resp.Feature = snap.Feature
		cwd = snap.CWD
		st, _ = hooks.ReadSessionState(stateDir, snap.ID)
	} else {
		// Historical lookup: the daemon no longer tracks the session, but its
		// (archived) state doc still holds the artifact record.
		st, _ = hooks.ReadSessionState(stateDir, name)
		if st == nil {
			if data, err := os.ReadFile(hooks.ArchivedSessionStatePath(stateDir, name)); err == nil {
				st = parseSessionStateDoc(data)
				resp.Archived = true
			}
		}
		if st == nil {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("no session or state doc named %q", name)})
			return
		}
		resp.SessionID = st.SessionID
	}

	if st != nil {
		if st.TurnContract != nil && resp.Feature == "" {
			resp.Feature = st.TurnContract.Feature
		}
		for _, a := range st.Artifacts {
			entry := artifactEntry{
				Path: a.Path, Op: a.Op, FirstSeen: a.FirstSeen, LastSeen: a.LastSeen,
				Edits: a.Edits, SizeBytes: -1,
			}
			if fi, err := os.Stat(a.Path); err == nil && !fi.IsDir() {
				entry.SizeBytes = fi.Size()
			}
			resp.Artifacts = append(resp.Artifacts, entry)
		}
	}

	// Best-effort git enrichment: dirty state for captured artifacts, plus
	// repo files changed outside the file tools. Never fails the query.
	if cwd != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if statuses := gitDirtyFiles(ctx, cwd); len(statuses) > 0 {
			captured := make(map[string]bool, len(resp.Artifacts))
			for i := range resp.Artifacts {
				captured[resp.Artifacts[i].Path] = true
				if s, ok := statuses[resp.Artifacts[i].Path]; ok {
					resp.Artifacts[i].GitStatus = s
				}
			}
			for path, status := range statuses {
				if captured[path] {
					continue
				}
				resp.GitExtra = append(resp.GitExtra, gitFileEntry{Path: path, Status: status})
				if len(resp.GitExtra) >= 50 {
					break
				}
			}
		}
	}

	resp.Focus, resp.AltFocus = computeFocus(resp.Artifacts, window, time.Now())
	writeJSON(w, http.StatusOK, resp)
}

// parseSessionStateDoc decodes a raw state doc, tolerating partial content.
func parseSessionStateDoc(data []byte) *hooks.SessionState {
	var st hooks.SessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	return &st
}

// computeFocus picks the artifact a caller most likely wants: the most
// recently touched artifact within the window (falling back to all artifacts
// when the window is empty), with the largest recently-touched file as the
// alternate when it differs.
func computeFocus(artifacts []artifactEntry, window time.Duration, now time.Time) (*artifactsFocus, *artifactsFocus) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	cutoff := now.Add(-window)
	recent := make([]artifactEntry, 0, len(artifacts))
	for _, a := range artifacts {
		if !a.LastSeen.Before(cutoff) {
			recent = append(recent, a)
		}
	}
	scope := "recent"
	if len(recent) == 0 {
		recent = artifacts
		scope = "overall"
	}
	latest := recent[0]
	largest := recent[0]
	for _, a := range recent[1:] {
		if a.LastSeen.After(latest.LastSeen) {
			latest = a
		}
		if a.SizeBytes > largest.SizeBytes {
			largest = a
		}
	}
	focus := &artifactsFocus{
		Path:   latest.Path,
		Reason: fmt.Sprintf("most recently updated (%s, %s ago)", scope, humanAgo(now.Sub(latest.LastSeen))),
	}
	if largest.Path != latest.Path && largest.SizeBytes > 0 {
		return focus, &artifactsFocus{
			Path:   largest.Path,
			Reason: fmt.Sprintf("largest %s file (%d KB)", scope, (largest.SizeBytes+1023)/1024),
		}
	}
	return focus, nil
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
}

// gitDirtyFiles returns absolute path -> porcelain status for the repo
// containing cwd. Empty on any failure (not a repo, no git, timeout).
func gitDirtyFiles(ctx context.Context, cwd string) map[string]string {
	rootOut, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	root := strings.TrimSpace(string(rootOut))
	if root == "" {
		return nil
	}
	statusOut, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	statuses := map[string]string{}
	for _, line := range strings.Split(string(statusOut), "\n") {
		if len(line) < 4 {
			continue
		}
		code := strings.TrimSpace(line[:2])
		path := strings.TrimSpace(line[3:])
		// Renames are "old -> new"; the new path is the live artifact.
		if _, after, ok := strings.Cut(path, " -> "); ok {
			path = after
		}
		path = strings.Trim(path, `"`)
		if code == "" || path == "" {
			continue
		}
		statuses[filepath.Join(root, path)] = code
	}
	return statuses
}

// handleSessionSummarize serves POST /session/summarize?name=<n>[&wait=1].
// Triggers the on-demand goal-model pass that writes overall_goal + feature to
// the session's state doc — the recovery path for a babysit call that finds no
// briefing. Default is fire-and-forget ({"started": true}); wait=1 blocks
// (bounded) and returns the refreshed turn_contract.
func (h *HTTPServer) handleSessionSummarize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST only"})
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name"})
		return
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("no session named %q", name)})
		return
	}
	snap := sess.Snapshot()
	wait := r.URL.Query().Get("wait") == "1" || r.URL.Query().Get("wait") == "true"

	if !wait {
		go func() {
			ctx, cancel := context.WithTimeout(h.daemon.ctx, 120*time.Second)
			defer cancel()
			if _, err := h.daemon.SummarizeSessionByID(ctx, snap.ID); err != nil && err != ErrSummarizeInFlight {
				h.daemon.logger.Warn("background summarize failed", "session_id", snap.ID, "error", err)
			}
		}()
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": snap.ID, "name": snap.Name, "started": true,
			"note": "summarizing in background; re-read the state doc or /sessions shortly",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	goal, err := h.daemon.SummarizeSessionByID(ctx, snap.ID)
	if err == ErrSummarizeInFlight {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	resp := map[string]any{"session_id": snap.ID, "name": snap.Name, "overall_goal": goal}
	if st, rerr := hooks.ReadSessionState(h.daemon.cfg.Hooks.SessionStateDir, snap.ID); rerr == nil && st != nil {
		resp["turn_contract"] = st.TurnContract
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSessionFeature serves POST /session/feature?name=<n>&feature=<slug>
// [&pin=0]. An explicit set pins by default (auto-derivation stops); pass
// pin=0 to record a suggestion the summarizer may still refine.
func (h *HTTPServer) handleSessionFeature(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST only"})
		return
	}
	name := r.URL.Query().Get("name")
	rawFeature := r.URL.Query().Get("feature")
	if name == "" || rawFeature == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing name or feature"})
		return
	}
	slug := hooks.SanitizeFeatureSlug(rawFeature)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("feature %q has no usable characters", rawFeature)})
		return
	}
	pin := true
	if v := r.URL.Query().Get("pin"); v == "0" || v == "false" {
		pin = false
	}
	sess := h.findByName(name)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: fmt.Sprintf("no session named %q", name)})
		return
	}
	snap := sess.Snapshot()
	update := hooks.TurnContractUpdate{Feature: slug, PinFeature: pin, Source: "http"}
	if err := hooks.ApplyContractOnly(h.daemon.cfg.Hooks.SessionStateDir, snap.ID, snap.Agent, update, time.Now()); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	// Report the EFFECTIVE feature: a non-pin set against a pinned contract is
	// ignored by the single writer, and the response must say so.
	effective, pinned := slug, pin
	if st, err := hooks.ReadSessionState(h.daemon.cfg.Hooks.SessionStateDir, snap.ID); err == nil && st != nil && st.TurnContract != nil {
		effective, pinned = st.TurnContract.Feature, st.TurnContract.FeaturePinned
	}
	sess.SetFeature(effective)
	// Converge the tmux name now rather than waiting for the next sweep tick.
	go h.daemon.featureSweep()
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": snap.ID, "name": snap.Name, "feature": effective, "pinned": pinned,
	})
}
