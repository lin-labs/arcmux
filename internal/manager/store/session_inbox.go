package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrSessionInboxMissing is returned when a per-session inbox operation
// targets a session name whose nested sub-bucket under BucketSessionInbox
// has not been created. Callers should call EnsureSessionInbox first.
//
// This mirrors ErrManagerInboxMissing / ErrICInboxMissing so the three
// nested-bucket surfaces behave identically.
var ErrSessionInboxMissing = errors.New("session inbox bucket missing")

// EnsureSessionInbox creates the nested inbox bucket for an arcmux session
// name. Safe to call repeatedly — idempotent. The C1 daemon calls this
// the first time it queues a message to a session that isn't ready, so a
// session that's always-ready never accumulates an empty sub-bucket.
func (d *DB) EnsureSessionInbox(name string) error {
	if name == "" {
		return fmt.Errorf("EnsureSessionInbox: name required")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		parent := tx.Bucket([]byte(BucketSessionInbox))
		if parent == nil {
			return fmt.Errorf("parent bucket %s missing", BucketSessionInbox)
		}
		_, err := parent.CreateBucketIfNotExists([]byte(name))
		return err
	})
}

// HasSessionInbox reports whether a nested inbox bucket exists for a
// session name. Test-friendly and lets callers distinguish "never queued"
// from "queue empty".
func (d *DB) HasSessionInbox(name string) bool {
	var ok bool
	_ = d.b.View(func(tx *bolt.Tx) error {
		parent := tx.Bucket([]byte(BucketSessionInbox))
		if parent == nil {
			return nil
		}
		ok = parent.Bucket([]byte(name)) != nil
		return nil
	})
	return ok
}

// PushSessionInbox enqueues a message into a session's inbox. Returns
// ErrSessionInboxMissing if the sub-bucket has not been created — callers
// who want push to imply ensure should call EnsureSessionInbox first.
func (d *DB) PushSessionInbox(name string, m InboxMsg) error {
	if name == "" {
		return fmt.Errorf("PushSessionInbox: name required")
	}
	if m.ID == "" {
		return fmt.Errorf("InboxMsg.ID required")
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := sessionInboxBucket(tx, name)
		if err != nil {
			return err
		}
		return putInbox(b, m)
	})
}

// PeekSessionInbox returns up to n messages oldest-first without removing
// them. If n <= 0, returns all queued messages.
func (d *DB) PeekSessionInbox(name string, n int) ([]InboxMsg, error) {
	if name == "" {
		return nil, fmt.Errorf("PeekSessionInbox: name required")
	}
	all := n <= 0
	cap := n
	if cap < 0 {
		cap = 0
	}
	out := make([]InboxMsg, 0, cap)
	err := d.b.View(func(tx *bolt.Tx) error {
		b, err := sessionInboxBucket(tx, name)
		if err != nil {
			return err
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil && (all || len(out) < n); k, v = c.Next() {
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

// AckSessionInbox removes a single message by ID from a session's inbox.
// Idempotent on missing IDs: returns nil when the message isn't found, so
// repeated ack calls don't error. This matches the C1 RPC's documented
// "AckInboxResponse.acked stays true on second call" contract.
//
// The ONE error case is ErrSessionInboxMissing — when the sub-bucket
// itself doesn't exist (the session was never sent to). That's surfaced
// so a caller can distinguish "you sent to a name we never knew" from
// "we knew this name, the message just isn't here anymore".
func (d *DB) AckSessionInbox(name, id string) error {
	if name == "" {
		return fmt.Errorf("AckSessionInbox: name required")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := sessionInboxBucket(tx, name)
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
		// Idempotent: a missing ID is not an error. The caller already
		// got the response "this message is no longer queued", which is
		// exactly what they wanted.
		return nil
	})
}

// DepthSessionInbox returns the number of unacked messages in a session's
// inbox. Returns ErrSessionInboxMissing if the sub-bucket isn't created.
func (d *DB) DepthSessionInbox(name string) (int, error) {
	if name == "" {
		return 0, fmt.Errorf("DepthSessionInbox: name required")
	}
	var n int
	err := d.b.View(func(tx *bolt.Tx) error {
		b, err := sessionInboxBucket(tx, name)
		if err != nil {
			return err
		}
		s := b.Stats()
		n = s.KeyN
		return nil
	})
	return n, err
}

// sessionInboxBucket resolves the per-session nested inbox bucket within
// the session-inbox parent. Mirrors managerBucket / icInboxBucket so the
// three nested surfaces behave identically.
func sessionInboxBucket(tx *bolt.Tx, name string) (*bolt.Bucket, error) {
	parent := tx.Bucket([]byte(BucketSessionInbox))
	if parent == nil {
		return nil, ErrSessionInboxMissing
	}
	b := parent.Bucket([]byte(name))
	if b == nil {
		return nil, fmt.Errorf("%w: session %q", ErrSessionInboxMissing, name)
	}
	return b, nil
}
