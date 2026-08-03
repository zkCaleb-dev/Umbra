package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EventItem is one raw event as served by the API. Topics and Data are
// the readable renders; RawXDR (base64 via JSON) remains authoritative.
type EventItem struct {
	ID             string          `json:"id"`
	Ledger         uint32          `json:"ledger"`
	LedgerClosedAt time.Time       `json:"ledger_closed_at"`
	TxHash         string          `json:"tx_hash"`
	Name           string          `json:"event_name,omitempty"`
	Topics         json.RawMessage `json:"topics"`
	Data           json.RawMessage `json:"data"`
	RawXDR         []byte          `json:"raw_xdr"`
}

// EventPage is one page of a contract's raw events in ledger order.
type EventPage struct {
	ContractID string      `json:"contract_id"`
	Events     []EventItem `json:"events"`
	// NextCursor resumes after the last event of this page; absent when
	// the page reached the end of the stored stream.
	NextCursor string `json:"next_cursor,omitempty"`
}

// eventCursor orders events by (ledger, tx_index, event_index) — the
// same triple the deterministic id is built from.
type eventCursor struct {
	ledger  int64
	txIndex int32
	event   int32
}

// ParseEventCursor parses a cursor ("ledger-tx-event", i.e. an event
// id). Empty input means "from the beginning".
func ParseEventCursor(raw string) (ok bool, c eventCursor) {
	if raw == "" {
		return true, eventCursor{-1, -1, -1}
	}
	parts := strings.Split(raw, "-")
	if len(parts) != 3 {
		return false, c
	}
	l, err1 := strconv.ParseInt(parts[0], 10, 64)
	t, err2 := strconv.ParseInt(parts[1], 10, 32)
	e, err3 := strconv.ParseInt(parts[2], 10, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return false, c
	}
	return true, eventCursor{l, int32(t), int32(e)}
}

// Events returns contract events strictly after the cursor position, in
// (ledger, tx_index, event_index) order, capped at limit.
func (s *Store) Events(ctx context.Context, contractID string, cursor eventCursor, limit int) (*EventPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, ledger, ledger_closed_at, tx_hash, event_name, topics, data, raw_xdr,
		       tx_index, event_index
		FROM events
		WHERE contract_id = $1
		  AND (ledger, tx_index, event_index) > ($2, $3, $4)
		ORDER BY ledger, tx_index, event_index
		LIMIT $5`,
		contractID, cursor.ledger, cursor.txIndex, cursor.event, limit)
	if err != nil {
		return nil, fmt.Errorf("querying events of %s: %w", contractID, err)
	}
	defer rows.Close()

	page := &EventPage{ContractID: contractID, Events: []EventItem{}}
	for rows.Next() {
		var it EventItem
		var ledger int64
		var txIdx, evIdx int32
		if err := rows.Scan(&it.ID, &ledger, &it.LedgerClosedAt, &it.TxHash, &it.Name,
			&it.Topics, &it.Data, &it.RawXDR, &txIdx, &evIdx); err != nil {
			return nil, err
		}
		it.Ledger = uint32(ledger)
		page.Events = append(page.Events, it)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(page.Events) == limit {
		page.NextCursor = page.Events[len(page.Events)-1].ID
	}
	return page, nil
}

// TransferItem is one decoded token transfer.
type TransferItem struct {
	EventID string `json:"event_id"`
	Ledger  uint32 `json:"ledger"`
	TxHash  string `json:"tx_hash"`
	From    string `json:"from"`
	To      string `json:"to"`
	Amount  string `json:"amount"`
}

// TransferPage is one page of a token's transfers in ledger order.
type TransferPage struct {
	TokenID     string         `json:"token_id"`
	Address     string         `json:"address,omitempty"`
	SinceLedger uint32         `json:"since_ledger"`
	Transfers   []TransferItem `json:"transfers"`
	NextLedger  *uint32        `json:"next_ledger,omitempty"`
}

// Transfers returns a token's transfers at ledger >= since, optionally
// filtered to those touching address (as sender or recipient).
func (s *Store) Transfers(ctx context.Context, tokenID, address string, since uint32, limit int) (*TransferPage, error) {
	query := `
		SELECT event_id, ledger, tx_hash, from_addr, to_addr, amount
		FROM token_transfers
		WHERE token_id = $1 AND ledger >= $2`
	args := []any{tokenID, int64(since)}
	if address != "" {
		query += ` AND (from_addr = $3 OR to_addr = $3)`
		args = append(args, address)
	}
	query += fmt.Sprintf(` ORDER BY ledger, event_id LIMIT %d`, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying transfers of %s: %w", tokenID, err)
	}
	defer rows.Close()

	page := &TransferPage{TokenID: tokenID, Address: address, SinceLedger: since, Transfers: []TransferItem{}}
	for rows.Next() {
		var it TransferItem
		var ledger int64
		if err := rows.Scan(&it.EventID, &ledger, &it.TxHash, &it.From, &it.To, &it.Amount); err != nil {
			return nil, err
		}
		it.Ledger = uint32(ledger)
		page.Transfers = append(page.Transfers, it)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(page.Transfers) == limit {
		next := page.Transfers[len(page.Transfers)-1].Ledger + 1
		page.NextLedger = &next
	}
	return page, nil
}

// CTHistoryItem is one confidential-token event touching an address.
type CTHistoryItem struct {
	EventID      string          `json:"event_id"`
	Ledger       uint32          `json:"ledger"`
	TxHash       string          `json:"tx_hash"`
	Kind         string          `json:"kind"`
	Addresses    []string        `json:"addresses"`
	AmountPublic *string         `json:"amount_public,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// CTHistoryPage pages a token's events for one address in ledger order.
type CTHistoryPage struct {
	TokenID     string          `json:"token_id"`
	Address     string          `json:"address"`
	SinceLedger uint32          `json:"since_ledger"`
	Events      []CTHistoryItem `json:"events"`
	NextLedger  *uint32         `json:"next_ledger,omitempty"`
}

// CTHistory returns confidential-token events of tokenID that name
// address, at ledger >= since. The server only ever filters by PUBLIC
// addresses — amounts stay ciphertext end to end.
func (s *Store) CTHistory(ctx context.Context, tokenID, address string, since uint32, limit int) (*CTHistoryPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, ledger, tx_hash, kind, addresses, amount_public, payload
		FROM ct_events
		WHERE token_id = $1 AND $2 = ANY(addresses) AND ledger >= $3
		ORDER BY ledger, event_id LIMIT $4`,
		tokenID, address, int64(since), limit)
	if err != nil {
		return nil, fmt.Errorf("querying ct history: %w", err)
	}
	defer rows.Close()

	page := &CTHistoryPage{TokenID: tokenID, Address: address, SinceLedger: since, Events: []CTHistoryItem{}}
	for rows.Next() {
		var it CTHistoryItem
		var ledger int64
		if err := rows.Scan(&it.EventID, &ledger, &it.TxHash, &it.Kind, &it.Addresses,
			&it.AmountPublic, &it.Payload); err != nil {
			return nil, err
		}
		it.Ledger = uint32(ledger)
		page.Events = append(page.Events, it)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(page.Events) == limit {
		next := page.Events[len(page.Events)-1].Ledger + 1
		page.NextLedger = &next
	}
	return page, nil
}
