package store

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("not found")

// PutTeam upserts a team document.
func (d *DB) PutTeam(t Team) error {
	if t.ID == "" {
		return errors.New("team.ID required")
	}
	t.UpdatedAt = time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = t.UpdatedAt
	}
	val, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTeams)).Put([]byte(t.ID), val)
	})
}

// GetTeam returns the team by ID or ErrNotFound.
func (d *DB) GetTeam(id string) (Team, error) {
	var t Team
	err := d.b.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketTeams)).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &t)
	})
	return t, err
}

// ListTeams returns teams optionally filtered by state ("" = all).
func (d *DB) ListTeams(state string) ([]Team, error) {
	var out []Team
	err := d.b.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTeams)).ForEach(func(_, v []byte) error {
			var t Team
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if state == "" || t.State == state {
				out = append(out, t)
			}
			return nil
		})
	})
	return out, err
}
