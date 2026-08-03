package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/decode"
	"github.com/zkCaleb-dev/umbra/internal/extract"
	"github.com/zkCaleb-dev/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
)

// reconcileCoverage compares each configured contract's declared
// start_ledger with its recorded coverage and schedules backfills for
// the missing history. Called once at startup, before the live loop:
//
//   - fresh database: every contract is covered from the loop's start —
//     record it and move on;
//   - contract added to a running instance: the loop only sees it from
//     the current cursor onward, so the range [start_ledger, cursor] is
//     fetched by a bounded background job that lowers covered_from when
//     it finishes. Interrupted backfills resume on next boot.
func (i *Ingester) reconcileCoverage(ctx context.Context, loopStart uint32) []func(context.Context) {
	type pending struct {
		id   string
		from uint32
	}
	var todo []pending
	var lo, hi uint32

	for _, ct := range i.cfg.Deployments.Contracts {
		covered, known, err := i.st.CoveredFrom(ctx, ct.ID)
		if err != nil {
			slog.Error("reading coverage", "contract", ct.ID, "err", err)
			continue
		}
		want := ct.StartLedger
		if want == 0 {
			want = loopStart
		}
		if !known {
			// First sighting. On a fresh database the live loop covers it
			// from loopStart; on an existing database we cannot know, so
			// the conservative answer is "covered from the cursor onward"
			// and the range below is backfilled (idempotent writes make an
			// overlap harmless).
			if err := i.st.SetCoveredFrom(ctx, ct.ID, loopStart); err != nil {
				slog.Error("recording coverage", "contract", ct.ID, "err", err)
				continue
			}
			covered = loopStart
		}
		if want < covered {
			todo = append(todo, pending{id: ct.ID, from: want})
			if lo == 0 || want < lo {
				lo = want
			}
			if covered-1 > hi {
				hi = covered - 1
			}
		}
	}
	if len(todo) == 0 {
		return nil
	}

	// ONE job for the union range with every pending contract watched:
	// N contracts missing overlapping history cost one pass over the
	// ledgers, not N.
	watched := make(map[string]struct{}, len(todo))
	ids := make([]string, 0, len(todo))
	for _, p := range todo {
		watched[p.id] = struct{}{}
		ids = append(ids, p.id)
	}
	slog.Info("scheduling grouped backfill", "contracts", ids, "from", lo, "to", hi)

	job := func(jctx context.Context) {
		start := time.Now()
		for _, rpcURL := range i.cfg.RPCURLs {
			if err := jctx.Err(); err != nil {
				return
			}
			err := i.backfillRange(jctx, rpcURL, fmt.Sprintf("%d contracts", len(ids)), watched, lo, hi)
			if err == nil {
				for _, p := range todo {
					if err := i.st.SetCoveredFrom(jctx, p.id, p.from); err != nil {
						slog.Error("recording backfill coverage", "contract", p.id, "err", err)
					}
				}
				slog.Info("backfill complete", "contracts", ids,
					"from", lo, "to", hi, "took", time.Since(start).Round(time.Second))
				return
			}
			slog.Error("backfill endpoint failed, trying next",
				"endpoint", endpointHost(rpcURL), "err", err)
		}
		if err := i.st.RecordGap(jctx, i.cfg.Network, lo, hi,
			"backfill failed on every endpoint"); err != nil {
			slog.Error("recording backfill gap", "err", err)
		}
	}
	return []func(context.Context){job}
}

func (i *Ingester) backfillRange(ctx context.Context, rpcURL, contractID string,
	watched map[string]struct{}, from, to uint32) error {

	backend := ledgerbackend.NewRPCLedgerBackend(ledgerbackend.RPCLedgerBackendOptions{
		RPCServerURL: rpcURL,
		BufferSize:   uint32(i.cfg.GetLedgersLimit),
		HttpClient:   &http.Client{Timeout: i.cfg.LedgerFetchTimeout},
	})
	defer backend.Close() //nolint:errcheck

	if err := backend.PrepareRange(ctx, ledgerbackend.BoundedRange(from, to)); err != nil {
		return fmt.Errorf("preparing backfill range [%d,%d]: %w", from, to, err)
	}
	for seq := from; seq <= to; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta, err := backend.GetLedger(ctx, seq)
		if err != nil {
			return fmt.Errorf("fetching ledger %d: %w", seq, err)
		}
		events, err := extract.FromLedger(ctx, i.cfg.Passphrase(), watched, meta)
		if err != nil {
			return fmt.Errorf("extracting ledger %d: %w", seq, err)
		}
		if len(events) == 0 {
			continue
		}
		rows, derived, err := i.renderRows(events)
		if err != nil {
			return err
		}
		if err := i.st.WriteBackfill(ctx, i.cfg.Network, rows, derived); err != nil {
			return fmt.Errorf("writing backfill ledger %d: %w", seq, err)
		}
		slog.Info("backfill ledger stored", "contract", contractID, "ledger", seq, "events", len(rows))
	}
	return nil
}

// renderRows converts extracted events into store rows + derived rows
// (shared by the live loop and backfill).
func (i *Ingester) renderRows(events []extract.Event) ([]store.RawEvent, store.Derived, error) {
	rows := make([]store.RawEvent, 0, len(events))
	for idx := range events {
		ev := &events[idx]
		topics, err := ev.TopicsJSON()
		if err != nil {
			return nil, store.Derived{}, fmt.Errorf("rendering topics of %s: %w", ev.ID, err)
		}
		data, err := ev.DataJSON()
		if err != nil {
			return nil, store.Derived{}, fmt.Errorf("rendering data of %s: %w", ev.ID, err)
		}
		rows = append(rows, store.RawEvent{
			ID: ev.ID, Ledger: ev.Ledger, LedgerClosedAt: ev.LedgerClosedAt,
			TxHash: ev.TxHash, TxIndex: ev.TxIndex, EventIndex: ev.EventIndex,
			ContractID: ev.ContractID, Name: ev.Name,
			TopicsJSON: topics, DataJSON: data, RawXDR: ev.RawXDR,
		})
	}
	return rows, decode.Derive(i.kinds, events), nil
}
