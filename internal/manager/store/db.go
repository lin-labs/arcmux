// Package store implements the per-project coordination data store backed
// by bbolt. It owns project meta, audit log, and per-session inboxes —
// the generic substrate primitives. Role-class concepts (teams/contracts/
// slots/elon-vs-manager-vs-ic) used to live here and were removed in C3:
// arcmux is now a pure substrate librarian and leaves agent-class decisions
// to its callers (elonco, etc.).
package store

import (
	"encoding/binary"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// CurrentSchemaVersion is the on-disk schema version this binary expects.
//
// The C3 demolition dropped a number of role-specific buckets
// (teams/contracts/slots/elon-inbox/manager-inboxes/ic-inboxes plus the
// associated DAG/state/priority indices). Old dev-time state.bolt files
// still carry those buckets — they become orphaned dead data but do not
// affect correctness, so the schema version stays at 1. There is no
// migration: cutover throws away dev state.
const CurrentSchemaVersion uint64 = 1

// Bucket names.
const (
	// BucketAudit is the append-only project-wide audit log.
	BucketAudit = "audit"
	// BucketMeta holds the singleton ProjectMeta record (and the schema
	// version key).
	BucketMeta = "meta"

	// BucketSessionInbox (added in C1) is the parent bucket holding one
	// nested sub-bucket per arcmux Session — keyed by session name. Each
	// nested bucket stores InboxMsg JSON values under time-sortable keys.
	// Created lazily by EnsureSessionInbox; readers tolerate a missing
	// sub-bucket as "no queue exists yet".
	BucketSessionInbox = "session-inbox"

	// BucketBabysitContext (B3) holds ephemeral, TTL'd babysit call contexts
	// keyed by their opaque token. Values are JSON blobs owned by the daemon
	// package; the store treats them as opaque bytes. Expiry is enforced
	// lazily by the daemon on read.
	BucketBabysitContext = "babysit-context"
)

// AllBuckets lists buckets created on Open.
var AllBuckets = []string{
	BucketAudit,
	BucketMeta,
	BucketSessionInbox,
	BucketBabysitContext,
}

// DB wraps a bbolt handle with arcmux-specific helpers.
type DB struct {
	b *bolt.DB
}

// Open opens or creates a bbolt file at path and ensures schema is current.
func Open(path string) (*DB, error) {
	b, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("bbolt open %s: %w", path, err)
	}
	db := &DB{b: b}
	if err := db.init(); err != nil {
		_ = b.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the underlying file handle.
func (d *DB) Close() error { return d.b.Close() }

// Bolt exposes the raw handle for advanced callers (tests, snapshots).
func (d *DB) Bolt() *bolt.DB { return d.b }

func (d *DB) init() error {
	return d.b.Update(func(tx *bolt.Tx) error {
		for _, name := range AllBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}

		meta := tx.Bucket([]byte(BucketMeta))
		v := meta.Get([]byte("schema-version"))
		if v == nil {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], CurrentSchemaVersion)
			return meta.Put([]byte("schema-version"), buf[:])
		}
		got := binary.BigEndian.Uint64(v)
		if got != CurrentSchemaVersion {
			return fmt.Errorf("schema version mismatch: file=%d, binary=%d", got, CurrentSchemaVersion)
		}
		return nil
	})
}

// HasBucket reports whether a bucket exists. Test helper.
func (d *DB) HasBucket(name string) bool {
	var ok bool
	_ = d.b.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket([]byte(name)) != nil
		return nil
	})
	return ok
}

// SchemaVersion returns the persisted schema version.
func (d *DB) SchemaVersion() (uint64, error) {
	var v uint64
	err := d.b.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		raw := meta.Get([]byte("schema-version"))
		if len(raw) != 8 {
			return fmt.Errorf("schema-version missing or malformed")
		}
		v = binary.BigEndian.Uint64(raw)
		return nil
	})
	return v, err
}
