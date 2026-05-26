package store

import (
	"encoding/binary"
	"encoding/json"

	bolt "go.etcd.io/bbolt"
)

// putInbox writes one InboxMsg into the supplied bucket using a sortable
// time-prefixed key. Used by every per-session inbox write surface.
//
// Pre-C3 this helper was shared by elon/manager/ic inbox writers; today
// only session_inbox.go calls it (those role-class inboxes are gone). Kept
// as a top-level helper so future per-key inbox surfaces (e.g. a global
// dead-letter queue) can reuse it without re-deriving the key format.
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
