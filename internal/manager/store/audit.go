package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// AppendAudit writes an audit entry. Key composes timestamp + sequence so
// identical-nano writes don't collide.
func (d *DB) AppendAudit(e AuditEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAudit))
		seq, err := b.NextSequence()
		if err != nil {
			return fmt.Errorf("audit nextseq: %w", err)
		}
		key := auditKey(e.Timestamp, seq)
		val, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put(key, val)
	})
}

// RecentAudit returns up to n entries, newest-first.
func (d *DB) RecentAudit(n int) ([]AuditEntry, error) {
	if n <= 0 {
		return nil, nil
	}
	out := make([]AuditEntry, 0, n)
	err := d.b.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketAudit)).Cursor()
		for k, v := c.Last(); k != nil && len(out) < n; k, v = c.Prev() {
			var e AuditEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func auditKey(ts time.Time, seq uint64) []byte {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(ts.UnixNano()))
	binary.BigEndian.PutUint64(buf[8:], seq)
	return buf[:]
}
