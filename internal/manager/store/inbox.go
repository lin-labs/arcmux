package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrManagerInboxMissing is returned when a manager-inbox operation targets a
// team slug whose nested bucket has not been created (i.e., the team was
// never spawned, or its sub-bucket was deleted). Callers should call
// EnsureManagerInbox first, or treat this as "the team does not exist".
var ErrManagerInboxMissing = errors.New("manager inbox bucket missing")

// PushElonInbox enqueues a message to Elon's inbox.
func (d *DB) PushElonInbox(m InboxMsg) error {
	return d.push(BucketInboxElon, m)
}

// PeekElonInbox returns up to n messages oldest-first without removing them.
func (d *DB) PeekElonInbox(n int) ([]InboxMsg, error) {
	return d.peek(BucketInboxElon, n)
}

// AckElonInbox removes a single message by ID.
func (d *DB) AckElonInbox(id string) error {
	return d.ack(BucketInboxElon, id)
}

// EnsureManagerInbox creates the nested inbox bucket for a team slug. Safe to
// call repeatedly — idempotent. teamspawn.Spawn calls this immediately after
// PutTeam so the inbox is ready before any vision push.
func (d *DB) EnsureManagerInbox(team string) error {
	if team == "" {
		return fmt.Errorf("EnsureManagerInbox: team required")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		parent := tx.Bucket([]byte(BucketInboxManagers))
		if parent == nil {
			return fmt.Errorf("parent bucket %s missing", BucketInboxManagers)
		}
		_, err := parent.CreateBucketIfNotExists([]byte(team))
		return err
	})
}

// HasManagerInbox reports whether the nested inbox bucket exists for a team.
func (d *DB) HasManagerInbox(team string) bool {
	var ok bool
	_ = d.b.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket([]byte(BucketInboxManagers))
		if parent == nil {
			return nil
		}
		ok = parent.Bucket([]byte(team)) != nil
		return nil
	})
	return ok
}

// PushManagerInbox enqueues a message to a team's manager inbox. Returns
// ErrManagerInboxMissing if the sub-bucket has not been created.
func (d *DB) PushManagerInbox(team string, m InboxMsg) error {
	if team == "" {
		return fmt.Errorf("PushManagerInbox: team required")
	}
	if m.ID == "" {
		return fmt.Errorf("InboxMsg.ID required")
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := managerBucket(tx, team)
		if err != nil {
			return err
		}
		return putInbox(b, m)
	})
}

// PeekManagerInbox returns up to n messages oldest-first from a team's
// manager inbox. Returns ErrManagerInboxMissing if the sub-bucket has not
// been created.
func (d *DB) PeekManagerInbox(team string, n int) ([]InboxMsg, error) {
	if team == "" {
		return nil, fmt.Errorf("PeekManagerInbox: team required")
	}
	if n <= 0 {
		return nil, nil
	}
	out := make([]InboxMsg, 0, n)
	err := d.b.View(func(tx *bolt.Tx) error {
		b, err := managerBucket(tx, team)
		if err != nil {
			return err
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil && len(out) < n; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

// AckManagerInbox removes a single message by ID from a team's manager inbox.
func (d *DB) AckManagerInbox(team, id string) error {
	if team == "" {
		return fmt.Errorf("AckManagerInbox: team required")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := managerBucket(tx, team)
		if err != nil {
			return err
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				continue
			}
			if m.ID == id {
				return b.Delete(k)
			}
		}
		return ErrNotFound
	})
}

// managerBucket resolves the per-team nested inbox bucket within the
// inbox-managers parent. Returns ErrManagerInboxMissing if either the parent
// or the sub-bucket is absent.
func managerBucket(tx *bolt.Tx, team string) (*bolt.Bucket, error) {
	parent := tx.Bucket([]byte(BucketInboxManagers))
	if parent == nil {
		return nil, ErrManagerInboxMissing
	}
	b := parent.Bucket([]byte(team))
	if b == nil {
		return nil, fmt.Errorf("%w: team %q", ErrManagerInboxMissing, team)
	}
	return b, nil
}

// ErrICInboxMissing is returned when a per-IC inbox operation targets a slot
// whose nested bucket has not been created (slot was never spawned, or its
// sub-bucket was dropped). icspawn.Spawn calls EnsureICInbox at slot create,
// so an active slot always has its inbox bucket ready.
var ErrICInboxMissing = errors.New("ic inbox bucket missing")

// EnsureICInbox creates the nested inbox bucket for a slot id. Safe to call
// repeatedly — idempotent. icspawn.Spawn calls this immediately after PutSlot
// so the inbox is ready before any manager push.
func (d *DB) EnsureICInbox(slot string) error {
	if slot == "" {
		return fmt.Errorf("EnsureICInbox: slot required")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		parent := tx.Bucket([]byte(BucketInboxICs))
		if parent == nil {
			return fmt.Errorf("parent bucket %s missing", BucketInboxICs)
		}
		_, err := parent.CreateBucketIfNotExists([]byte(slot))
		return err
	})
}

// HasICInbox reports whether the nested inbox bucket exists for a slot.
func (d *DB) HasICInbox(slot string) bool {
	var ok bool
	_ = d.b.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket([]byte(BucketInboxICs))
		if parent == nil {
			return nil
		}
		ok = parent.Bucket([]byte(slot)) != nil
		return nil
	})
	return ok
}

// PushICInbox enqueues a message to a slot's per-IC inbox. Returns
// ErrICInboxMissing if the sub-bucket has not been created.
func (d *DB) PushICInbox(slot string, m InboxMsg) error {
	if slot == "" {
		return fmt.Errorf("PushICInbox: slot required")
	}
	if m.ID == "" {
		return fmt.Errorf("InboxMsg.ID required")
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := icInboxBucket(tx, slot)
		if err != nil {
			return err
		}
		return putInbox(b, m)
	})
}

// PeekICInbox returns up to n messages oldest-first from a slot's per-IC
// inbox. Returns ErrICInboxMissing if the sub-bucket has not been created.
func (d *DB) PeekICInbox(slot string, n int) ([]InboxMsg, error) {
	if slot == "" {
		return nil, fmt.Errorf("PeekICInbox: slot required")
	}
	if n <= 0 {
		return nil, nil
	}
	out := make([]InboxMsg, 0, n)
	err := d.b.View(func(tx *bolt.Tx) error {
		b, err := icInboxBucket(tx, slot)
		if err != nil {
			return err
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil && len(out) < n; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

// AckICInbox removes a single message by ID from a slot's per-IC inbox.
func (d *DB) AckICInbox(slot, id string) error {
	if slot == "" {
		return fmt.Errorf("AckICInbox: slot required")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := icInboxBucket(tx, slot)
		if err != nil {
			return err
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				continue
			}
			if m.ID == id {
				return b.Delete(k)
			}
		}
		return ErrNotFound
	})
}

// icInboxBucket resolves the per-slot nested inbox bucket within the
// inbox-ics parent. Returns ErrICInboxMissing if either the parent or the
// sub-bucket is absent.
func icInboxBucket(tx *bolt.Tx, slot string) (*bolt.Bucket, error) {
	parent := tx.Bucket([]byte(BucketInboxICs))
	if parent == nil {
		return nil, ErrICInboxMissing
	}
	b := parent.Bucket([]byte(slot))
	if b == nil {
		return nil, fmt.Errorf("%w: slot %q", ErrICInboxMissing, slot)
	}
	return b, nil
}

// putInbox writes one InboxMsg into the supplied bucket using a sortable
// time-prefixed key. Shared by Elon and manager inbox writes.
func putInbox(b *bolt.Bucket, m InboxMsg) error {
	seq, err := b.NextSequence()
	if err != nil {
		return err
	}
	var keybuf [16]byte
	binary.BigEndian.PutUint64(keybuf[:8], uint64(m.ReceivedAt.UnixNano()))
	binary.BigEndian.PutUint64(keybuf[8:], seq)
	val, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return b.Put(keybuf[:], val)
}

func (d *DB) push(bucket string, m InboxMsg) error {
	if m.ID == "" {
		return fmt.Errorf("InboxMsg.ID required")
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s missing", bucket)
		}
		return putInbox(b, m)
	})
}

func (d *DB) peek(bucket string, n int) ([]InboxMsg, error) {
	if n <= 0 {
		return nil, nil
	}
	out := make([]InboxMsg, 0, n)
	err := d.b.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucket)).Cursor()
		for k, v := c.First(); k != nil && len(out) < n; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

func (d *DB) ack(bucket, id string) error {
	return d.b.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				continue
			}
			if m.ID == id {
				return b.Delete(k)
			}
		}
		return ErrNotFound
	})
}
