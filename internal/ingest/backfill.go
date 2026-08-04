package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/decode"
	"github.com/zkCaleb-dev/umbra/internal/extract"
	"github.com/zkCaleb-dev/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
)

// coverageReconciler owns all backfill work in one goroutine: a pass at
// startup, then another whenever NotifyChange signals that the watch
// set grew. Serializing here keeps concurrent registrations from
// double-fetching overlapping ranges — each pass recomputes what is
// pending from the coverage table, so coalesced signals lose nothing.
func (i *Ingester) coverageReconciler(ctx context.Context, loopStart uint32) {
	for {
		// A contract registered mid-run is only covered by the live loop
		// from the CURRENT position onward, not from where the loop
		// started — use the freshest cursor as the conservative bound.
		pos := loopStart
		if st, ok := i.Status(); ok {
			pos = st.LastLedger + 1
		}
		for _, job := range i.reconcileCoverage(ctx, pos) {
			job(ctx)
		}
		i.replayGaps(ctx)
		select {
		case <-ctx.Done():
			return
		case <-i.reconcileCh:
		}
	}
}

// replayGaps turns recorded gap evidence into recovered history: with
// the archive leg available, every gap range is replayed from the
// history archives for the FULL current watch set (a gap is a
// network-range statement, so any watched contract may have events
// there). Success resolves the evidence; failure leaves it honest and
// retries on the next pass. This is also how a clamped cold start heals
// itself — the live loop records the below-retention gap, then this
// pass recovers it.
func (i *Ingester) replayGaps(ctx context.Context) {
	if !i.archiveAvailable() {
		return
	}
	gaps, err := i.st.Gaps(ctx, i.cfg.Network)
	if err != nil {
		slog.Error("listing gaps for archive replay", "err", err)
		return
	}
	for _, gap := range gaps {
		if ctx.Err() != nil {
			return
		}
		snap := i.reg.Snapshot()
		ids := make([]string, 0, len(snap.Contracts))
		for _, ct := range snap.Contracts {
			ids = append(ids, ct.ID)
		}
		lower := func(coveredDownTo uint32) {
			for _, id := range ids {
				if err := i.st.SetCoveredFrom(ctx, id, coveredDownTo); err != nil {
					slog.Error("recording archive coverage", "contract", id, "err", err)
				}
			}
		}
		if err := i.archiveBackfill(ctx, ids, snap.Watched, gap.FromLedger, gap.ToLedger, lower); err != nil {
			slog.Error("gap archive replay failed; evidence stays recorded",
				"gap", fmt.Sprintf("[%d,%d]", gap.FromLedger, gap.ToLedger), "err", err)
		}
	}
}

// reconcileCoverage compares each registered contract's declared
// start_ledger with its recorded coverage and returns backfill jobs for
// the missing history:
//
//   - fresh database: every contract is covered from the loop's start —
//     record it and move on;
//   - contract added later (file edit or runtime registration): the
//     live loop only sees it from the current cursor onward, so the
//     range [start_ledger, cursor] is fetched by a bounded job that
//     lowers covered_from as it goes. Interrupted backfills resume on
//     the next pass or boot.
func (i *Ingester) reconcileCoverage(ctx context.Context, loopStart uint32) []func(context.Context) {
	type pending struct {
		id   string
		from uint32
	}
	var todo []pending
	var lo, hi uint32

	for _, ct := range i.reg.Snapshot().Contracts {
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
		i.runBackfillJob(jctx, ids, watched, lo, hi, func(coveredDownTo uint32) {
			for _, p := range todo {
				if err := i.st.SetCoveredFrom(jctx, p.id, coveredDownTo); err != nil {
					slog.Error("recording backfill coverage", "contract", p.id, "err", err)
				}
			}
		})
	}
	return []func(context.Context){job}
}

// backfillChunk is how many ledgers a backfill fetches before coverage
// is persisted. Chunks walk DOWNWARD (newest first): after each chunk
// the covered range grows monotonically toward the past, so an
// interruption at any point leaves complete, resumable coverage — and
// recent history (what wallets ask for first) lands first.
const backfillChunk = 2000

// runBackfillJob walks [lo, hi] in descending chunks, persisting
// progress after every chunk via lowerCoverage. When the low end has
// fallen below every endpoint's retention it clamps, records ONE gap
// for the truly unreachable part, and finishes — partial history is a
// result, not a failure.
func (i *Ingester) runBackfillJob(ctx context.Context, ids []string,
	watched map[string]struct{}, lo, hi uint32, lowerCoverage func(uint32)) {

	label := fmt.Sprintf("%d contracts", len(ids))
	started := time.Now()
	end := hi

	for end >= lo {
		if ctx.Err() != nil {
			return
		}
		chunkStart := lo
		if end >= backfillChunk && end-backfillChunk+1 > lo {
			chunkStart = end - backfillChunk + 1
		}

		var lastErr error
		done := false
		for _, rpcURL := range i.cfg.RPCURLs {
			if ctx.Err() != nil {
				return
			}
			if err := i.backfillRange(ctx, rpcURL, label, watched, chunkStart, end); err != nil {
				lastErr = err
				continue
			}
			done = true
			break
		}

		if done {
			lowerCoverage(chunkStart)
			if chunkStart == lo {
				slog.Info("backfill complete", "contracts", ids,
					"from", lo, "to", hi, "took", time.Since(started).Round(time.Second))
				return
			}
			end = chunkStart - 1
			continue
		}

		// Whole pool refused this chunk. Retention or outage?
		oldest := i.poolOldest(ctx)
		switch {
		case oldest > 0 && oldest > chunkStart:
			// The low end fell out of every retention window. Take what
			// the RPCs still serve, then hand the remainder to the
			// archive leg — or, without one, record the honest gap.
			if oldest <= end {
				if err := i.retryClamped(ctx, label, watched, oldest, end); err != nil {
					slog.Error("clamped backfill chunk failed", "err", err)
					return // resume next boot; coverage already reflects progress
				}
				lowerCoverage(oldest)
			}
			if i.archiveAvailable() {
				if err := i.archiveBackfill(ctx, ids, watched, lo, oldest-1, lowerCoverage); err != nil {
					slog.Error("archive backfill failed; range stays a gap until the next pass", "err", err)
					if err := i.st.RecordGap(ctx, i.cfg.Network, lo, oldest-1,
						"below provider retention; archive replay failed and will retry on the next reconcile pass"); err != nil {
						slog.Error("recording backfill gap", "err", err)
					}
				}
				return
			}
			if err := i.st.RecordGap(ctx, i.cfg.Network, lo, oldest-1,
				"below provider retention during backfill; events already stored in this range are kept"); err != nil {
				slog.Error("recording backfill gap", "err", err)
			}
			slog.Warn("backfill reached the retention wall",
				"contracts", ids, "unreachable", fmt.Sprintf("[%d,%d]", lo, oldest-1),
				"took", time.Since(started).Round(time.Second))
			return
		default:
			// Transient outage: wait and retry the SAME chunk. Never
			// abandon progress over a hiccup.
			slog.Error("backfill chunk failed on every endpoint; retrying",
				"chunk", fmt.Sprintf("[%d,%d]", chunkStart, end), "err", lastErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}
}

// retryClamped fetches one clamped range across the pool.
func (i *Ingester) retryClamped(ctx context.Context, label string,
	watched map[string]struct{}, from, to uint32) error {
	var lastErr error
	for _, rpcURL := range i.cfg.RPCURLs {
		if err := i.backfillRange(ctx, rpcURL, label, watched, from, to); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// PoolOldest returns the lowest oldestLedger any endpoint advertises (0
// when nothing answers) — the registration default for "save everything
// still reachable".
func (i *Ingester) PoolOldest(ctx context.Context) uint32 {
	return i.poolOldest(ctx)
}

// poolOldest returns the lowest oldestLedger any endpoint advertises
// (0 when nothing answers).
func (i *Ingester) poolOldest(ctx context.Context) uint32 {
	best := uint32(0)
	for _, rpcURL := range i.cfg.RPCURLs {
		oldest, err := i.oldestLedger(ctx, rpcURL)
		if err != nil {
			continue
		}
		if best == 0 || oldest < best {
			best = oldest
		}
	}
	return best
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
		rows, derived, err := i.renderRows(events, i.reg.Snapshot().Kinds)
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
// (shared by the live loop and backfill). kinds comes from the caller's
// registry snapshot so both paths derive with the current decoders.
func (i *Ingester) renderRows(events []extract.Event, kinds map[string]config.ContractKind) ([]store.RawEvent, store.Derived, error) {
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
	return rows, decode.Derive(kinds, events), nil
}
