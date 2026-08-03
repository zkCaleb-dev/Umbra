package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Coverage bookkeeping — see migrations/0003_contract_coverage.sql.

// CoveredFrom returns the ledger from which contractID's history is
// complete, or ok=false when the contract has never been registered.
func (s *Store) CoveredFrom(ctx context.Context, contractID string) (uint32, bool, error) {
	var v int64
	err := s.pool.QueryRow(ctx,
		`SELECT covered_from FROM contract_coverage WHERE contract_id=$1`, contractID,
	).Scan(&v)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uint32(v), true, nil
}

// SetCoveredFrom records (or lowers) a contract's coverage start.
func (s *Store) SetCoveredFrom(ctx context.Context, contractID string, from uint32) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contract_coverage (contract_id, covered_from)
		VALUES ($1,$2)
		ON CONFLICT (contract_id) DO UPDATE
		SET covered_from = LEAST(contract_coverage.covered_from, EXCLUDED.covered_from),
		    updated_at = now()`,
		contractID, int64(from))
	if err != nil {
		return fmt.Errorf("setting coverage of %s: %w", contractID, err)
	}
	return nil
}

// WriteBackfill persists events and derived rows WITHOUT touching the
// cursor — the write path for bounded historical jobs running beside
// the live loop. Idempotent like WriteLedger (same conflict targets).
func (s *Store) WriteBackfill(ctx context.Context, network string, events []RawEvent, d Derived) error {
	return s.writeRows(ctx, network, events, d)
}

// EventBatch is one page of raw events for re-derivation.
type EventBatch struct {
	Events     []ReplayEvent
	NextCursor string
}

// ReplayEvent is a stored event read back for re-derivation.
type ReplayEvent struct {
	ID         string
	Ledger     uint32
	TxHash     string
	TxIndex    uint32
	EventIndex uint32
	ContractID string
	RawXDR     []byte
}

// EventsPage reads stored events in stream order for re-derivation,
// starting strictly after cursor (an event id, empty = beginning).
func (s *Store) EventsPage(ctx context.Context, network, cursor string, limit int) (*EventBatch, error) {
	ok, c := ParseEventCursor(cursor)
	if !ok {
		return nil, fmt.Errorf("malformed rederive cursor %q", cursor)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, ledger, tx_hash, tx_index, event_index, contract_id, raw_xdr
		FROM events
		WHERE network = $1 AND (ledger, tx_index, event_index) > ($2, $3, $4)
		ORDER BY ledger, tx_index, event_index
		LIMIT $5`,
		network, c.ledger, c.txIndex, c.event, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	batch := &EventBatch{}
	for rows.Next() {
		var e ReplayEvent
		var l int64
		var t, ev int32
		if err := rows.Scan(&e.ID, &l, &e.TxHash, &t, &ev, &e.ContractID, &e.RawXDR); err != nil {
			return nil, err
		}
		e.Ledger, e.TxIndex, e.EventIndex = uint32(l), uint32(t), uint32(ev)
		batch.Events = append(batch.Events, e)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(batch.Events) == limit {
		batch.NextCursor = batch.Events[len(batch.Events)-1].ID
	}
	return batch, nil
}

// WriteDerived persists only derived rows (re-derivation path).
func (s *Store) WriteDerived(ctx context.Context, d Derived) error {
	return s.writeRows(ctx, "", nil, d)
}
