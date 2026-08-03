package store

import (
	"context"
	"fmt"
	"time"
)

// RPCEventRow is one raw event with everything the JSON-RPC facade needs
// to rebuild a Stellar-RPC-shaped event (topics/value come from RawXDR).
type RPCEventRow struct {
	Ledger         uint32
	LedgerClosedAt time.Time
	TxHash         string
	TxIndex        uint32
	EventIndex     uint32
	ContractID     string
	RawXDR         []byte
}

// EventsAfter returns events of the given contracts strictly after the
// (ledger, tx, event) position, up to and including endLedger, in
// stream order. Used by the bootnode-compatible getEvents.
func (s *Store) EventsAfter(ctx context.Context, contractIDs []string,
	ledger, txIndex, eventIndex uint32, endLedger uint32, limit int) ([]RPCEventRow, error) {

	rows, err := s.pool.Query(ctx, `
		SELECT ledger, ledger_closed_at, tx_hash, tx_index, event_index, contract_id, raw_xdr
		FROM events
		WHERE contract_id = ANY($1)
		  AND (ledger, tx_index, event_index) > ($2, $3, $4)
		  AND ledger <= $5
		ORDER BY ledger, tx_index, event_index
		LIMIT $6`,
		contractIDs, int64(ledger), int32(txIndex), int32(eventIndex), int64(endLedger), limit)
	if err != nil {
		return nil, fmt.Errorf("querying events after %d-%d-%d: %w", ledger, txIndex, eventIndex, err)
	}
	defer rows.Close()

	out := []RPCEventRow{}
	for rows.Next() {
		var r RPCEventRow
		var l int64
		var t, e int32
		if err := rows.Scan(&l, &r.LedgerClosedAt, &r.TxHash, &t, &e, &r.ContractID, &r.RawXDR); err != nil {
			return nil, err
		}
		r.Ledger, r.TxIndex, r.EventIndex = uint32(l), uint32(t), uint32(e)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Coverage reports the oldest ledger this index covers contiguously: the
// configured start, unless a retention clamp recorded a gap — then
// coverage begins right after the newest such gap. closedAt is the close
// time of the earliest stored event at/after that boundary (zero when no
// events are stored yet).
func (s *Store) Coverage(ctx context.Context, network string, configuredStart uint32) (oldest uint32, closedAt time.Time, err error) {
	var gapTo *int64
	err = s.pool.QueryRow(ctx,
		`SELECT max(to_ledger) FROM gaps WHERE network=$1`, network).Scan(&gapTo)
	if err != nil {
		return 0, time.Time{}, err
	}
	oldest = configuredStart
	if gapTo != nil && uint32(*gapTo)+1 > oldest {
		oldest = uint32(*gapTo) + 1
	}
	var ts *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT min(ledger_closed_at) FROM events WHERE network=$1 AND ledger >= $2`,
		network, int64(oldest)).Scan(&ts)
	if err != nil {
		return 0, time.Time{}, err
	}
	if ts != nil {
		closedAt = *ts
	}
	return oldest, closedAt, nil
}
