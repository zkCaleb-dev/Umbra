// Bootnode-compatible JSON-RPC facade.
//
// Serves the exact surface the Stellar Private Payments reference wallet
// expects from a "bootnode" (see NethermindEth/stellar-private-payments,
// tools/bootnode): POST / with getEvents and getLatestLedger, answers
// shaped like a real Stellar RPC, plus the two bootnode error codes —
// -32004 (page not indexed yet, retry) and -32002 (range within the
// wallet RPC's retention: continue there from error.data.fromLedger).
// Pointing the SPP app's bootnode URL at Umbra requires no app changes.
package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Bootnode-specific JSON-RPC error codes (same values as the reference
// implementation).
const (
	codeCacheMiss        = -32004
	codeRetentionHandoff = -32002
	codeInvalidParams    = -32602
	codeMethodNotFound   = -32601
	codeInternal         = -32603
	codeParse            = -32700
)

// getEvents page-size bounds, mirroring stellar-rpc defaults.
const (
	rpcDefaultLimit = 100
	rpcMaxLimit     = 10000
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type getEventsParams struct {
	StartLedger uint32          `json:"startLedger"`
	EndLedger   uint32          `json:"endLedger"`
	Filters     []eventFilter   `json:"filters"`
	Pagination  *rpcPagination  `json:"pagination"`
	XDRFormat   string          `json:"xdrFormat"`
}

type eventFilter struct {
	Type        string     `json:"type"`
	ContractIDs []string   `json:"contractIds"`
	Topics      [][]string `json:"topics"`
}

type rpcPagination struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

type rpcEvent struct {
	Type                     string   `json:"type"`
	Ledger                   uint32   `json:"ledger"`
	LedgerClosedAt           string   `json:"ledgerClosedAt"`
	ContractID               string   `json:"contractId"`
	ID                       string   `json:"id"`
	OperationIndex           uint32   `json:"operationIndex"`
	TransactionIndex         uint32   `json:"transactionIndex"`
	TxHash                   string   `json:"txHash"`
	InSuccessfulContractCall bool     `json:"inSuccessfulContractCall"`
	Topic                    []string `json:"topic"`
	Value                    string   `json:"value"`
}

type getEventsResult struct {
	Events                []rpcEvent `json:"events"`
	LatestLedger          uint32     `json:"latestLedger"`
	LatestLedgerCloseTime string     `json:"latestLedgerCloseTime"`
	OldestLedger          uint32     `json:"oldestLedger"`
	OldestLedgerCloseTime string     `json:"oldestLedgerCloseTime"`
	Cursor                string     `json:"cursor"`
}

// handleJSONRPC is the POST / entrypoint.
func (s *Server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeParse, Message: "parse error"}})
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "getLatestLedger":
		resp.Result, resp.Error = s.rpcGetLatestLedger()
	case "getEvents":
		resp.Result, resp.Error = s.rpcGetEvents(r, req.Params)
	default:
		resp.Error = &rpcError{Code: codeMethodNotFound, Message: "method not found"}
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("encoding jsonrpc response", "err", err)
	}
}

func (s *Server) rpcGetLatestLedger() (any, *rpcError) {
	st, ok := s.ing.Status()
	if !ok {
		return nil, &rpcError{Code: codeCacheMiss, Message: "bootnode warming up; retry later"}
	}
	return map[string]any{
		"id":              st.LastLedgerHash,
		"protocolVersion": st.ProtocolVersion,
		"sequence":        st.LastLedger,
	}, nil
}

func (s *Server) rpcGetEvents(r *http.Request, raw json.RawMessage) (any, *rpcError) {
	var p getEventsParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "malformed params"}
		}
	}
	if p.XDRFormat != "" && p.XDRFormat != "base64" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unsupported xdrFormat"}
	}

	// Filters: only configured contracts are served (the facade never
	// turns client input into arbitrary scans).
	contractIDs, topicFilters, errRPC := s.resolveFilters(p.Filters)
	if errRPC != nil {
		return nil, errRPC
	}

	limit := rpcDefaultLimit
	cursor := ""
	if p.Pagination != nil {
		if p.Pagination.Limit > 0 {
			limit = min(p.Pagination.Limit, rpcMaxLimit)
		}
		cursor = p.Pagination.Cursor
	}

	st, ok := s.ing.Status()
	if !ok {
		return nil, &rpcError{Code: codeCacheMiss, Message: "bootnode warming up; retry later"}
	}
	tip := st.LastLedger
	cutoff := uint32(0)
	if s.cfg.HandoffCutoffLedgers > 0 && tip > s.cfg.HandoffCutoffLedgers {
		cutoff = tip - s.cfg.HandoffCutoffLedgers
	}

	// Position: startLedger XOR cursor, like stellar-rpc.
	var afterL, afterT, afterE uint32
	var posLedger uint32
	switch {
	case cursor != "" && p.StartLedger != 0:
		return nil, &rpcError{Code: codeInvalidParams, Message: "startLedger and cursor are mutually exclusive"}
	case cursor != "":
		l, t, e, err := parseRPCCursor(cursor)
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		afterL, afterT, afterE = l, t, e
		posLedger = l
	case p.StartLedger != 0:
		// Inclusive start: strictly after the end of the previous ledger.
		afterL, afterT, afterE = p.StartLedger-1, 1<<20-1, 1<<32-1
		posLedger = p.StartLedger
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "startLedger or cursor is required"}
	}

	// Retention handoff: the requested position is recent enough for the
	// wallet's own RPC — tell it to continue there.
	if cutoff > 0 && posLedger >= cutoff {
		return nil, &rpcError{
			Code:    codeRetentionHandoff,
			Message: "Continue syncing on your RPC endpoint",
			Data:    map[string]any{"reason": "retention_threshold", "fromLedger": cutoff},
		}
	}

	// Not indexed yet (catching up): retry later.
	if posLedger > tip {
		return nil, &rpcError{Code: codeCacheMiss, Message: "cache miss; indexer may still be catching up"}
	}

	// Serve up to the tip, the handoff cutoff (exclusive) and endLedger.
	scanEnd := tip
	if cutoff > 0 {
		scanEnd = min(scanEnd, cutoff-1)
	}
	if p.EndLedger != 0 {
		if p.EndLedger < posLedger {
			return nil, &rpcError{Code: codeInvalidParams, Message: "endLedger is before the requested position"}
		}
		scanEnd = min(scanEnd, p.EndLedger)
	}

	rows, err := s.st.EventsAfter(r.Context(), contractIDs, afterL, afterT, afterE, scanEnd, limit)
	if err != nil {
		slog.Error("jsonrpc getEvents query", "err", err)
		return nil, &rpcError{Code: codeInternal, Message: "internal error"}
	}

	events := make([]rpcEvent, 0, len(rows))
	nextCursor := formatRPCCursor(scanEnd+1, 0, 0) // virtual: everything through scanEnd consumed (tx indexes are 1-based, so nothing at tx 0 is skipped)
	for _, row := range rows {
		ev, err := buildRPCEvent(row.RawXDR, row.Ledger, row.LedgerClosedAt, row.TxHash, row.TxIndex, row.EventIndex)
		if err != nil {
			slog.Error("jsonrpc event rebuild", "err", err, "ledger", row.Ledger)
			return nil, &rpcError{Code: codeInternal, Message: "internal error"}
		}
		if !matchTopics(topicFilters, ev.Topic) {
			continue
		}
		events = append(events, ev)
	}
	if len(rows) == limit {
		// Page full: resume right after the last SCANNED event (not the
		// last MATCHED one, or topic-filtered streams would skip ranges).
		last := rows[len(rows)-1]
		nextCursor = formatRPCCursor2(last.Ledger, last.TxIndex, last.EventIndex)
	}

	oldest, oldestTime, err := s.st.Coverage(r.Context(), s.cfg.Network, s.cfg.StartLedger())
	if err != nil {
		slog.Error("jsonrpc coverage query", "err", err)
		return nil, &rpcError{Code: codeInternal, Message: "internal error"}
	}
	oldestStr := ""
	if !oldestTime.IsZero() {
		oldestStr = strconv.FormatInt(oldestTime.Unix(), 10)
	}
	return getEventsResult{
		Events:                events,
		LatestLedger:          tip,
		LatestLedgerCloseTime: strconv.FormatInt(st.LastClosedAt.Unix(), 10),
		OldestLedger:          oldest,
		OldestLedgerCloseTime: oldestStr,
		Cursor:                nextCursor,
	}, nil
}

// resolveFilters validates the request filters and returns the contract
// set to query plus the topic filters to apply. No filters = all
// configured contracts.
func (s *Server) resolveFilters(filters []eventFilter) ([]string, [][]string, *rpcError) {
	if len(filters) == 0 {
		ids := make([]string, 0, len(s.cfg.Deployments.Contracts))
		for _, ct := range s.cfg.Deployments.Contracts {
			ids = append(ids, ct.ID)
		}
		return ids, nil, nil
	}
	seen := map[string]struct{}{}
	var ids []string
	var topics [][]string
	for _, f := range filters {
		if f.Type != "" && f.Type != "contract" {
			return nil, nil, &rpcError{Code: codeInvalidParams, Message: "unsupported filters"}
		}
		for _, id := range f.ContractIDs {
			if _, watched := s.watchedContract(id); !watched {
				return nil, nil, &rpcError{Code: codeInvalidParams, Message: "unsupported filters"}
			}
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		topics = append(topics, f.Topics...)
	}
	if len(ids) == 0 {
		return nil, nil, &rpcError{Code: codeInvalidParams, Message: "unsupported filters"}
	}
	return ids, topics, nil
}

// buildRPCEvent decodes the stored ContractEvent XDR back into the
// Stellar RPC wire shape (base64 topic segments + value).
func buildRPCEvent(rawXDR []byte, ledger uint32, closedAt time.Time, txHash string, txIndex, eventIndex uint32) (rpcEvent, error) {
	var ev xdr.ContractEvent
	if err := ev.UnmarshalBinary(rawXDR); err != nil {
		return rpcEvent{}, fmt.Errorf("unmarshaling stored event XDR: %w", err)
	}
	contractID := ""
	if ev.ContractId != nil {
		addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: ev.ContractId}
		s, err := addr.String()
		if err != nil {
			return rpcEvent{}, err
		}
		contractID = s
	}
	body := ev.Body.MustV0()
	topics := make([]string, 0, len(body.Topics))
	for _, t := range body.Topics {
		raw, err := t.MarshalBinary()
		if err != nil {
			return rpcEvent{}, err
		}
		topics = append(topics, base64.StdEncoding.EncodeToString(raw))
	}
	value, err := body.Data.MarshalBinary()
	if err != nil {
		return rpcEvent{}, err
	}
	return rpcEvent{
		Type:                     "contract",
		Ledger:                   ledger,
		LedgerClosedAt:           closedAt.UTC().Format(time.RFC3339),
		ContractID:               contractID,
		ID:                       formatRPCCursor2(ledger, txIndex, eventIndex),
		OperationIndex:           0,
		TransactionIndex:         txIndex,
		TxHash:                   txHash,
		InSuccessfulContractCall: true, // only successful txs are ingested
		Topic:                    topics,
		Value:                    base64.StdEncoding.EncodeToString(value),
	}, nil
}

// matchTopics applies RPC topic filters: a filter matches when every
// segment equals the event topic at that position or is "*"; a trailing
// "**" matches any remaining topics. Empty filter set matches all.
func matchTopics(filters [][]string, topics []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if matchOneTopicFilter(f, topics) {
			return true
		}
	}
	return false
}

func matchOneTopicFilter(filter, topics []string) bool {
	for i, seg := range filter {
		if seg == "**" {
			return true // matches any suffix, present or empty
		}
		if i >= len(topics) {
			return false
		}
		if seg != "*" && seg != topics[i] {
			return false
		}
	}
	return len(filter) == len(topics)
}

// ===== Cursor codec: "%019d-%010d" with TOID = ledger<<32 | tx<<12 =====

func formatRPCCursor2(ledger, txIndex, eventIndex uint32) string {
	toid := int64(ledger)<<32 | int64(txIndex)<<12
	return fmt.Sprintf("%019d-%010d", toid, eventIndex)
}

func formatRPCCursor(ledger, txIndex, eventIndex uint32) string {
	return formatRPCCursor2(ledger, txIndex, eventIndex)
}

func parseRPCCursor(cursor string) (ledger, txIndex, eventIndex uint32, err error) {
	parts := strings.SplitN(cursor, "-", 2)
	if len(parts) != 2 {
		return 0, 0, 0, fmt.Errorf("malformed cursor")
	}
	toid, err1 := strconv.ParseInt(parts[0], 10, 64)
	ev, err2 := strconv.ParseUint(parts[1], 10, 32)
	if err1 != nil || err2 != nil || toid < 0 {
		return 0, 0, 0, fmt.Errorf("malformed cursor")
	}
	return uint32(toid >> 32), uint32(toid >> 12 & (1<<20 - 1)), uint32(ev), nil
}
