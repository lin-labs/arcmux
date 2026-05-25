package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PutContract upserts a contract document and (re)builds its indexes.
func (d *DB) PutContract(c Contract) error {
	if c.ID == "" {
		return errors.New("contract.ID required")
	}
	if c.State == "" {
		c.State = ContractPending
	}
	c.UpdatedAt = time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = c.UpdatedAt
	}

	return d.b.Update(func(tx *bolt.Tx) error {
		// Remove any prior index rows for this contract.
		if prior := tx.Bucket([]byte(BucketContracts)).Get([]byte(c.ID)); prior != nil {
			var old Contract
			if err := json.Unmarshal(prior, &old); err == nil {
				clearContractIndexes(tx, old)
			}
		}
		val, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketContracts)).Put([]byte(c.ID), val); err != nil {
			return err
		}
		return writeContractIndexes(tx, c)
	})
}

// GetContract returns the contract by ID or ErrNotFound.
func (d *DB) GetContract(id string) (Contract, error) {
	var c Contract
	err := d.b.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketContracts)).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &c)
	})
	return c, err
}

// ListContractsByTeam returns contract IDs for a team via the index.
func (d *DB) ListContractsByTeam(team string) ([]string, error) {
	return d.listSuffixes(BucketIdxTeamContract, team+"/")
}

// ListContractsByState returns contract IDs in a given state via the index.
func (d *DB) ListContractsByState(state string) ([]string, error) {
	return d.listSuffixes(BucketIdxState, state+"/")
}

// ListContracts scans BucketContracts and returns the full Contract structs,
// optionally filtered by team and/or state. An empty filter is a wildcard.
// Results are sorted by Priority desc (higher first), then ID asc — a stable
// order that matches how a human dispatcher would scan a queue. A v0 design
// choice: full-bucket scan with in-memory filter rather than index walk +
// per-ID GetContract, because (a) one read transaction is cheaper than N+1,
// (b) the bucket is small at this stage, (c) callers usually want the whole
// struct, not just the ID. Optimize when contract counts justify it.
func (d *DB) ListContracts(team, state string) ([]Contract, error) {
	var out []Contract
	err := d.b.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketContracts)).ForEach(func(_, raw []byte) error {
			var c Contract
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			if team != "" && c.Team != team {
				return nil
			}
			if state != "" && c.State != state {
				return nil
			}
			out = append(out, c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ChildrenOf returns contracts that depend on parent.
func (d *DB) ChildrenOf(parent string) ([]string, error) {
	return d.listSuffixes(BucketIdxDepsParent, parent+"/")
}

// ParentsOf returns contracts that child depends on.
func (d *DB) ParentsOf(child string) ([]string, error) {
	return d.listSuffixes(BucketIdxDepsChild, child+"/")
}

// TransitionContract changes a contract's state with validation and audit.
// Transitions into Ready or Working enforce that all DependsOn parents have
// completed.
func (d *DB) TransitionContract(id, newState, reason, by string) error {
	return d.b.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketContracts))
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var c Contract
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}

		if !validTransition(c.State, newState) {
			return fmt.Errorf("invalid transition %q → %q for contract %s", c.State, newState, id)
		}

		if newState == ContractWorking || newState == ContractReady {
			for _, parent := range c.DependsOn {
				p := bucket.Get([]byte(parent))
				if p == nil {
					return fmt.Errorf("dep %q missing for contract %s", parent, id)
				}
				var pc Contract
				if err := json.Unmarshal(p, &pc); err != nil {
					return err
				}
				if pc.State != ContractCompleted {
					return fmt.Errorf("dep %q not completed (state=%s) for contract %s", parent, pc.State, id)
				}
			}
		}

		clearContractIndexes(tx, c)
		c.State = newState
		c.UpdatedAt = time.Now()
		c.Audit = append(c.Audit, ContractAudit{
			Timestamp: c.UpdatedAt,
			State:     newState,
			By:        by,
			Reason:    reason,
		})

		val, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), val); err != nil {
			return err
		}
		return writeContractIndexes(tx, c)
	})
}

// validTransition implements the contract state machine guard.
func validTransition(from, to string) bool {
	if from == to {
		return false
	}
	allowed := map[string]map[string]bool{
		ContractPending:    {ContractReady: true, ContractCancelled: true},
		ContractReady:      {ContractWorking: true, ContractCancelled: true},
		ContractWorking:    {ContractBlocked: true, ContractValidating: true, ContractCancelled: true, ContractFailed: true},
		ContractBlocked:    {ContractWorking: true, ContractCancelled: true, ContractFailed: true},
		ContractValidating: {ContractCompleted: true, ContractWorking: true, ContractFailed: true},
		ContractCompleted:  {},
		ContractCancelled:  {},
		ContractFailed:     {},
	}
	next, ok := allowed[from]
	if !ok {
		return false
	}
	return next[to]
}

func writeContractIndexes(tx *bolt.Tx, c Contract) error {
	if c.Team != "" {
		key := []byte(c.Team + "/" + c.ID)
		if err := tx.Bucket([]byte(BucketIdxTeamContract)).Put(key, nil); err != nil {
			return err
		}
	}
	if c.State != "" {
		key := []byte(c.State + "/" + c.ID)
		if err := tx.Bucket([]byte(BucketIdxState)).Put(key, nil); err != nil {
			return err
		}
	}
	pkey := []byte(fmt.Sprintf("%02d/%s", c.Priority, c.ID))
	if err := tx.Bucket([]byte(BucketIdxPriority)).Put(pkey, nil); err != nil {
		return err
	}
	for _, p := range c.DependsOn {
		if err := tx.Bucket([]byte(BucketIdxDepsParent)).Put([]byte(p+"/"+c.ID), nil); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketIdxDepsChild)).Put([]byte(c.ID+"/"+p), nil); err != nil {
			return err
		}
	}
	return nil
}

func clearContractIndexes(tx *bolt.Tx, c Contract) {
	if c.Team != "" {
		_ = tx.Bucket([]byte(BucketIdxTeamContract)).Delete([]byte(c.Team + "/" + c.ID))
	}
	if c.State != "" {
		_ = tx.Bucket([]byte(BucketIdxState)).Delete([]byte(c.State + "/" + c.ID))
	}
	_ = tx.Bucket([]byte(BucketIdxPriority)).Delete([]byte(fmt.Sprintf("%02d/%s", c.Priority, c.ID)))
	for _, p := range c.DependsOn {
		_ = tx.Bucket([]byte(BucketIdxDepsParent)).Delete([]byte(p + "/" + c.ID))
		_ = tx.Bucket([]byte(BucketIdxDepsChild)).Delete([]byte(c.ID + "/" + p))
	}
}

// listSuffixes returns the suffix portion of keys that begin with prefix.
// Used to read the right-hand side of "<a>/<b>" composite index keys.
func (d *DB) listSuffixes(bucket, prefix string) ([]string, error) {
	var out []string
	err := d.b.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucket)).Cursor()
		bp := []byte(prefix)
		for k, _ := c.Seek(bp); k != nil && bytes.HasPrefix(k, bp); k, _ = c.Next() {
			out = append(out, string(k[len(bp):]))
		}
		return nil
	})
	return out, err
}
