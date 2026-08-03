// Package extract turns a ledger's close meta into the raw contract
// events Umbra persists. It applies the correctness checks the pipeline
// depends on:
//
//   - only events from SUCCESSFUL transactions (failed transactions still
//     ship diagnostic events in their meta);
//   - only real contract events (ContractEventTypeContract) with a
//     non-nil ContractId;
//   - only contracts present in the configured watch set.
//
// It never interprets payloads beyond a readable JSON render — the raw
// XDR stays authoritative and decoders downstream are replaceable.
package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	sdkingest "github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Event is one extracted contract event: persistence fields plus the
// decoded topic/data ScVals for downstream decoders.
type Event struct {
	ID             string
	Ledger         uint32
	LedgerClosedAt time.Time
	TxHash         string
	TxIndex        uint32
	EventIndex     uint32
	ContractID     string
	Name           string // topic[0] symbol when present

	Topics []xdr.ScVal
	Data   xdr.ScVal
	RawXDR []byte
}

// FromLedger extracts the watched contract events from one ledger.
func FromLedger(ctx context.Context, passphrase string, watched map[string]struct{},
	meta xdr.LedgerCloseMeta) ([]Event, error) {

	reader, err := sdkingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, meta)
	if err != nil {
		return nil, fmt.Errorf("creating transaction reader for ledger %d: %w",
			meta.LedgerSequence(), err)
	}
	defer reader.Close() //nolint:errcheck

	var out []Event
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tx, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("reading transaction in ledger %d: %w",
				meta.LedgerSequence(), err)
		}
		// Failed transactions do not change contract state; their meta can
		// still carry (diagnostic) events. Skip them entirely.
		if !tx.Result.Successful() {
			continue
		}
		evs, err := fromTransaction(tx, watched)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return out, nil
}

// fromTransaction walks one transaction's operations and returns the
// watched events. EventIndex is a per-transaction running counter across
// all operations, matching how Soroban tooling numbers events.
func fromTransaction(tx sdkingest.LedgerTransaction, watched map[string]struct{}) ([]Event, error) {
	ledgerSeq := tx.Ledger.LedgerSequence()
	closedAt := time.Unix(tx.Ledger.LedgerCloseTime(), 0).UTC()
	txHash := tx.Hash.HexString()

	var out []Event
	var eventIdx uint32

	opCount := uint32(len(tx.Envelope.Operations()))
	for opIdx := uint32(0); opIdx < opCount; opIdx++ {
		events, err := tx.GetContractEventsForOperation(opIdx)
		if err != nil {
			return nil, fmt.Errorf("events for ledger=%d tx=%d op=%d: %w",
				ledgerSeq, tx.Index, opIdx, err)
		}
		for _, ev := range events {
			idx := eventIdx
			eventIdx++

			if ev.Type != xdr.ContractEventTypeContract || ev.ContractId == nil {
				continue
			}
			contractID, err := strkey.Encode(strkey.VersionByteContract, ev.ContractId[:])
			if err != nil {
				return nil, fmt.Errorf("encoding contract id at ledger=%d tx=%d event=%d: %w",
					ledgerSeq, tx.Index, idx, err)
			}
			if _, ok := watched[contractID]; !ok {
				continue
			}

			body := ev.Body.MustV0()
			raw, err := ev.MarshalBinary()
			if err != nil {
				return nil, fmt.Errorf("marshaling event at ledger=%d tx=%d event=%d: %w",
					ledgerSeq, tx.Index, idx, err)
			}

			out = append(out, Event{
				ID:             fmt.Sprintf("%d-%d-%d", ledgerSeq, tx.Index, idx),
				Ledger:         ledgerSeq,
				LedgerClosedAt: closedAt,
				TxHash:         txHash,
				TxIndex:        tx.Index,
				EventIndex:     idx,
				ContractID:     contractID,
				Name:           firstTopicSymbol(body.Topics),
				Topics:         body.Topics,
				Data:           body.Data,
				RawXDR:         raw,
			})
		}
	}
	return out, nil
}

// firstTopicSymbol returns topic[0] when it is a Symbol ("" otherwise) —
// the soroban-sdk `#[contractevent]` macro puts the event name there.
func firstTopicSymbol(topics []xdr.ScVal) string {
	if len(topics) == 0 {
		return ""
	}
	if sym, ok := topics[0].GetSym(); ok {
		return string(sym)
	}
	return ""
}
