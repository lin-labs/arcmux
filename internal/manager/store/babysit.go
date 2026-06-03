package store

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// PutBabysitContext stores an opaque context blob under its token. Overwrites
// any existing value for the same token.
func (d *DB) PutBabysitContext(token string, value []byte) error {
	if token == "" {
		return fmt.Errorf("empty babysit context token")
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(BucketBabysitContext))
		if bkt == nil {
			return fmt.Errorf("bucket %s missing", BucketBabysitContext)
		}
		return bkt.Put([]byte(token), append([]byte(nil), value...))
	})
}

// GetBabysitContext returns the stored blob for token, or (nil, nil) when no
// such token exists. Expiry is the daemon's concern, not the store's.
func (d *DB) GetBabysitContext(token string) ([]byte, error) {
	var out []byte
	err := d.b.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(BucketBabysitContext))
		if bkt == nil {
			return nil
		}
		v := bkt.Get([]byte(token))
		if v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	return out, err
}

// DeleteBabysitContext removes a context token. Missing tokens are not an error.
func (d *DB) DeleteBabysitContext(token string) error {
	return d.b.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(BucketBabysitContext))
		if bkt == nil {
			return nil
		}
		return bkt.Delete([]byte(token))
	})
}
