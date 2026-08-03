package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// LeafPage is one page of ordered commitment-tree leaves. Responses are
// range-shaped on purpose: clients download whole ranges and never ask
// "which of these are mine", so the server learns nothing about note
// ownership.
type LeafPage struct {
	PoolID    string     `json:"pool_id"`
	FromIndex uint32     `json:"from_index"`
	Leaves    []LeafItem `json:"leaves"`
	// NextIndex is the from_index for the next page, absent when the page
	// reached the current end of the tree.
	NextIndex *uint32 `json:"next_index,omitempty"`
}

// LeafItem is one leaf of the commitment tree.
type LeafItem struct {
	Index           uint32 `json:"index"`
	Commitment      string `json:"commitment"`
	EncryptedOutput []byte `json:"encrypted_output,omitempty"` // base64 in JSON
	Ledger          uint32 `json:"ledger"`
}

// Leaves returns leaves of pool from leaf_index >= from, capped at limit.
// includeOutputs selects the heavier encrypted_output column (the
// /outputs endpoint) versus commitments only (the /leaves endpoint).
func (s *Store) Leaves(ctx context.Context, poolID string, from uint32, limit int, includeOutputs bool) (*LeafPage, error) {
	col := "''::bytea"
	if includeOutputs {
		col = "encrypted_output"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT leaf_index, commitment, %s, ledger
		FROM pool_leaves WHERE pool_id=$1 AND leaf_index >= $2
		ORDER BY leaf_index LIMIT $3`, col),
		poolID, int64(from), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &LeafPage{PoolID: poolID, FromIndex: from, Leaves: []LeafItem{}}
	for rows.Next() {
		var it LeafItem
		var idx, ledger int64
		var out []byte
		if err := rows.Scan(&idx, &it.Commitment, &out, &ledger); err != nil {
			return nil, err
		}
		it.Index, it.Ledger = uint32(idx), uint32(ledger)
		if includeOutputs {
			it.EncryptedOutput = out
		}
		page.Leaves = append(page.Leaves, it)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(page.Leaves) == limit {
		next := page.Leaves[len(page.Leaves)-1].Index + 1
		page.NextIndex = &next
	}
	return page, nil
}

// NullifierPage is one page of spent nullifiers.
type NullifierPage struct {
	PoolID      string          `json:"pool_id"`
	SinceLedger uint32          `json:"since_ledger"`
	Nullifiers  []NullifierItem `json:"nullifiers"`
	NextLedger  *uint32         `json:"next_ledger,omitempty"`
}

// NullifierItem is one spent nullifier.
type NullifierItem struct {
	Nullifier string `json:"nullifier"`
	Ledger    uint32 `json:"ledger"`
}

// Nullifiers returns nullifiers spent at ledger >= since, ordered by
// ledger, capped at limit.
func (s *Store) Nullifiers(ctx context.Context, poolID string, since uint32, limit int) (*NullifierPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT nullifier, ledger FROM pool_nullifiers
		WHERE pool_id=$1 AND ledger >= $2
		ORDER BY ledger, nullifier LIMIT $3`,
		poolID, int64(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &NullifierPage{PoolID: poolID, SinceLedger: since, Nullifiers: []NullifierItem{}}
	for rows.Next() {
		var it NullifierItem
		var ledger int64
		if err := rows.Scan(&it.Nullifier, &ledger); err != nil {
			return nil, err
		}
		it.Ledger = uint32(ledger)
		page.Nullifiers = append(page.Nullifiers, it)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(page.Nullifiers) == limit {
		next := page.Nullifiers[len(page.Nullifiers)-1].Ledger + 1
		page.NextLedger = &next
	}
	return page, nil
}

// RegistryEntry is the latest registered key pair for an owner.
type RegistryEntry struct {
	Owner         string `json:"owner"`
	EncryptionKey []byte `json:"encryption_key"` // base64 in JSON
	NoteKey       []byte `json:"note_key"`
	Ledger        uint32 `json:"ledger"`
}

// RegistryLookup returns the latest keys registered for owner across any
// indexed registry, or ok=false.
func (s *Store) RegistryLookup(ctx context.Context, owner string) (*RegistryEntry, bool, error) {
	e := &RegistryEntry{Owner: owner}
	var ledger int64
	err := s.pool.QueryRow(ctx, `
		SELECT encryption_key, note_key, ledger FROM registry_keys
		WHERE owner=$1 ORDER BY ledger DESC LIMIT 1`, owner,
	).Scan(&e.EncryptionKey, &e.NoteKey, &ledger)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	e.Ledger = uint32(ledger)
	return e, true, nil
}

// PoolStats summarizes one pool's derived state.
type PoolStats struct {
	LeafCount      int64   `json:"leaf_count"`
	NullifierCount int64   `json:"nullifier_count"`
	MaxLeafIndex   *uint32 `json:"max_leaf_index,omitempty"`
}

// Stats returns pool counters. A leaf_count != max_leaf_index+1 exposes
// an ingestion hole — completeness is checkable in O(1).
func (s *Store) Stats(ctx context.Context, poolID string) (*PoolStats, error) {
	st := &PoolStats{}
	var maxIdx *int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), max(leaf_index) FROM pool_leaves WHERE pool_id=$1`, poolID,
	).Scan(&st.LeafCount, &maxIdx)
	if err != nil {
		return nil, err
	}
	if maxIdx != nil {
		v := uint32(*maxIdx)
		st.MaxLeafIndex = &v
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pool_nullifiers WHERE pool_id=$1`, poolID,
	).Scan(&st.NullifierCount); err != nil {
		return nil, err
	}
	return st, nil
}

// GapItem is one recorded ingestion gap.
type GapItem struct {
	FromLedger uint32    `json:"from_ledger"`
	ToLedger   uint32    `json:"to_ledger"`
	Reason     string    `json:"reason"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Gaps lists recorded gaps for the network (empty = completeness claim).
func (s *Store) Gaps(ctx context.Context, network string) ([]GapItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT from_ledger, to_ledger, reason, recorded_at
		FROM gaps WHERE network=$1 ORDER BY from_ledger`, network)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GapItem{}
	for rows.Next() {
		var g GapItem
		var f, t int64
		if err := rows.Scan(&f, &t, &g.Reason, &g.RecordedAt); err != nil {
			return nil, err
		}
		g.FromLedger, g.ToLedger = uint32(f), uint32(t)
		out = append(out, g)
	}
	return out, rows.Err()
}
