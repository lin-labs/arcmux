package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

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
