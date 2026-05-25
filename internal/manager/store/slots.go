package store

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PutSlot upserts an IC slot record and (re)builds its team index.
func (d *DB) PutSlot(s Slot) error {
	if s.ID == "" {
		return errors.New("slot.ID required")
	}
	if s.Team == "" {
		return errors.New("slot.Team required")
	}
	if s.State == "" {
		s.State = SlotActive
	}
	s.UpdatedAt = time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = s.UpdatedAt
	}

	return d.b.Update(func(tx *bolt.Tx) error {
		// Clear prior team index row in case the slot moved teams. Slot
		// reassignment between teams is not exposed via the v0 CLI but the
		// store would corrupt its index if a future caller did it without
		// this cleanup, so do it unconditionally.
		if prior := tx.Bucket([]byte(BucketSlots)).Get([]byte(s.ID)); prior != nil {
			var old Slot
			if err := json.Unmarshal(prior, &old); err == nil && old.Team != "" {
				_ = tx.Bucket([]byte(BucketIdxTeamSlot)).Delete([]byte(old.Team + "/" + old.ID))
			}
		}
		val, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketSlots)).Put([]byte(s.ID), val); err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketIdxTeamSlot)).Put([]byte(s.Team+"/"+s.ID), nil)
	})
}

// GetSlot returns a slot by ID or ErrNotFound.
func (d *DB) GetSlot(id string) (Slot, error) {
	var s Slot
	err := d.b.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketSlots)).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &s)
	})
	return s, err
}

// ListSlots scans BucketSlots and returns full Slot records, optionally
// filtered by team and/or state. Empty filters are wildcards. Mirrors the
// shape of ListContracts: full-bucket scan + in-memory filter, sorted by
// ID asc — the order a human dispatcher scans a team roster.
func (d *DB) ListSlots(team, state string) ([]Slot, error) {
	var out []Slot
	err := d.b.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketSlots)).ForEach(func(_, raw []byte) error {
			var s Slot
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			if team != "" && s.Team != team {
				return nil
			}
			if state != "" && s.State != state {
				return nil
			}
			out = append(out, s)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
