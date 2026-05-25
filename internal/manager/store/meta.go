package store

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ProjectMeta is a per-project singleton record holding launcher-time facts
// that downstream substrate (pulse, future heartbeats, future state-of-
// substrate dumps) needs to locate the Elon pane after the launcher exits.
// The audit log is append-only and not a stable lookup key; ProjectMeta is
// the small mutable header the rest of the substrate reads from.
type ProjectMeta struct {
	ElonPaneRef      string    `json:"elon_pane_ref"`
	ElonSurfaceRef   string    `json:"elon_surface_ref"`
	ElonWorkspaceRef string    `json:"elon_workspace_ref"`
	UpdatedAt        time.Time `json:"updated_at"`
}

const metaProjectKey = "project-meta"

// PutProjectMeta upserts the singleton project meta record.
func (d *DB) PutProjectMeta(m ProjectMeta) error {
	m.UpdatedAt = time.Now()
	val, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(metaProjectKey), val)
	})
}

// GetProjectMeta returns the singleton project meta or ErrNotFound when the
// launcher hasn't written one yet (i.e., manager-mode never started for this
// project on this machine).
func (d *DB) GetProjectMeta() (ProjectMeta, error) {
	var m ProjectMeta
	err := d.b.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketMeta)).Get([]byte(metaProjectKey))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &m)
	})
	if err != nil && errors.Is(err, ErrNotFound) {
		return ProjectMeta{}, ErrNotFound
	}
	return m, err
}
