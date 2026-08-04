package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"

	"github.com/zkCaleb-dev/umbra/internal/extract"
)

// The archive leg: history below every RPC's retention window is not
// gone — the public history archives hold every transaction since the
// network's genesis (or last testnet reset). Captive stellar-core
// replays them, RE-EXECUTING the transactions and regenerating the
// ledger meta events live in. Nothing is taken on trust: catchup
// verifies the archive's hash chain and the events are recomputed, not
// served by a third party. Slow (a deep range is hours, once) but free,
// and once stored Umbra serves it forever.

// archiveSegment is how many ledgers one captive PrepareRange covers.
// Segments walk DOWNWARD like RPC backfill chunks (coverage grows
// monotonically toward the past, newest history lands first) while the
// replay inside each segment runs forward, the only direction core can.
// One captive instance spans all segments so downloaded buckets stay
// cached; the per-segment cost is a catchup re-apply, minutes not
// downloads.
const archiveSegment = 100_000

// archiveAvailable reports whether the captive-core leg can run.
func (i *Ingester) archiveAvailable() bool {
	if !i.cfg.ArchiveBackfill {
		return false
	}
	if _, err := os.Stat(i.cfg.CoreBinaryPath); err != nil {
		slog.Warn("archive backfill disabled: stellar-core binary not found",
			"path", i.cfg.CoreBinaryPath)
		return false
	}
	return true
}

// archiveBackfill replays [lo, hi] from the history archives,
// persisting coverage after each descending segment. Idempotent writes
// make a re-run of a half-finished segment harmless.
func (i *Ingester) archiveBackfill(ctx context.Context, ids []string,
	watched map[string]struct{}, lo, hi uint32, lowerCoverage func(uint32)) error {

	toml, err := ledgerbackend.NewCaptiveCoreToml(ledgerbackend.CaptiveCoreTomlParams{
		NetworkPassphrase:  i.cfg.Passphrase(),
		HistoryArchiveURLs: i.cfg.HistoryArchiveURLs(),
		CoreBinaryPath:     i.cfg.CoreBinaryPath,
		// Without unified events the replayed meta omits SAC transfers
		// that came from CLASSIC operations (payments) — the replay
		// would silently reproduce only the Soroban-side history. RPC
		// nodes run with these on; the replay must match them.
		EmitUnifiedEvents:                 true,
		EmitUnifiedEventsBeforeProtocol22: true,
	})
	if err != nil {
		return fmt.Errorf("building captive core config: %w", err)
	}
	backend, err := ledgerbackend.NewCaptive(ledgerbackend.CaptiveCoreConfig{
		BinaryPath:         i.cfg.CoreBinaryPath,
		NetworkPassphrase:  i.cfg.Passphrase(),
		HistoryArchiveURLs: i.cfg.HistoryArchiveURLs(),
		Toml:               toml,
		StoragePath:        i.cfg.CoreStoragePath,
		UserAgent:          "umbra-archive-backfill",
		Context:            ctx,
	})
	if err != nil {
		return fmt.Errorf("starting captive core: %w", err)
	}
	defer backend.Close() //nolint:errcheck

	started := time.Now()
	slog.Info("archive backfill starting", "contracts", ids,
		"from", lo, "to", hi, "segments", (hi-lo)/archiveSegment+1)

	end := hi
	for end >= lo {
		segStart := lo
		if end >= archiveSegment && end-archiveSegment+1 > lo {
			segStart = end - archiveSegment + 1
		}
		if err := i.replaySegment(ctx, backend, watched, segStart, end); err != nil {
			return fmt.Errorf("archive segment [%d,%d]: %w", segStart, end, err)
		}
		lowerCoverage(segStart)
		slog.Info("archive segment covered", "from", segStart, "to", end,
			"elapsed", time.Since(started).Round(time.Second))
		if segStart == lo {
			break
		}
		end = segStart - 1
	}

	// History recovered — any gap fully inside this range is no longer
	// honest evidence of absence.
	if err := i.st.ResolveGapsWithin(ctx, i.cfg.Network, lo, hi); err != nil {
		slog.Error("resolving gaps after archive backfill", "err", err)
	}
	slog.Info("archive backfill complete", "contracts", ids,
		"from", lo, "to", hi, "took", time.Since(started).Round(time.Second))
	return nil
}

// replaySegment streams one bounded range out of captive core.
func (i *Ingester) replaySegment(ctx context.Context, backend ledgerbackend.LedgerBackend,
	watched map[string]struct{}, from, to uint32) error {

	if err := backend.PrepareRange(ctx, ledgerbackend.BoundedRange(from, to)); err != nil {
		return fmt.Errorf("preparing range: %w", err)
	}
	for seq := from; seq <= to; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta, err := backend.GetLedger(ctx, seq)
		if err != nil {
			return fmt.Errorf("replaying ledger %d: %w", seq, err)
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
			return fmt.Errorf("writing archive ledger %d: %w", seq, err)
		}
		slog.Info("archive ledger stored", "ledger", seq, "events", len(rows))
	}
	return nil
}
