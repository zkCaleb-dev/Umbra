package ingest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/decode"
	"github.com/zkCaleb-dev/umbra/internal/extract"
	"github.com/zkCaleb-dev/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Rederive rebuilds every derived table from the raw event log — the
// operation that makes decoders replaceable: configure a new kind (or
// fix a decoder), run `umbra rederive`, and history that was only
// stored raw becomes first-class derived data. Idempotent: existing
// rows conflict away, so running it twice is a no-op.
//
// Safe to run while the live service is up: it only performs the same
// idempotent inserts the loop performs.
func Rederive(ctx context.Context, cfg RederiveConfig) error {
	slog.Info("re-deriving from raw events", "network", cfg.Network)
	cursor := ""
	batches, derivedTotal := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := cfg.Store.EventsPage(ctx, cfg.Network, cursor, 2000)
		if err != nil {
			return fmt.Errorf("reading events page: %w", err)
		}
		if len(batch.Events) == 0 {
			break
		}
		events := make([]extract.Event, 0, len(batch.Events))
		for _, re := range batch.Events {
			var ev xdr.ContractEvent
			if err := ev.UnmarshalBinary(re.RawXDR); err != nil {
				slog.Warn("skipping undecodable stored event", "id", re.ID, "err", err)
				continue
			}
			body := ev.Body.MustV0()
			events = append(events, extract.Event{
				ID: re.ID, Ledger: re.Ledger, TxHash: re.TxHash,
				TxIndex: re.TxIndex, EventIndex: re.EventIndex,
				ContractID: re.ContractID,
				Name:       firstTopicName(body.Topics),
				Topics:     body.Topics, Data: body.Data, RawXDR: re.RawXDR,
			})
		}
		derived := decode.Derive(cfg.Kinds, events)
		if err := cfg.Store.WriteDerived(ctx, derived); err != nil {
			return fmt.Errorf("writing derived rows: %w", err)
		}
		batches++
		derivedTotal += len(derived.Leaves) + len(derived.Nullifiers) +
			len(derived.Registry) + len(derived.Transfers)

		if batch.NextCursor == "" {
			break
		}
		cursor = batch.NextCursor
	}
	slog.Info("re-derivation complete", "batches", batches, "derived_rows_written", derivedTotal)
	return nil
}

// RederiveConfig carries what Rederive needs.
type RederiveConfig struct {
	Store   *store.Store
	Network string
	Kinds   map[string]config.ContractKind
}

func firstTopicName(topics []xdr.ScVal) string {
	if len(topics) == 0 {
		return ""
	}
	if s, ok := topics[0].GetSym(); ok {
		return string(s)
	}
	return ""
}
