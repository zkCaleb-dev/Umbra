// Package ingest drives the pipeline: fetch ledgers from the RPC pool,
// extract watched events, derive rows, and commit everything atomically
// per ledger. One goroutine, one writer.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/extract"
	"github.com/zkCaleb-dev/umbra/internal/registry"
	"github.com/zkCaleb-dev/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// progressLogEvery controls how often catch-up progress (rate + timing
// breakdown) is logged.
const progressLogEvery = 1000

// Status is a snapshot for /v1/status, maintained by the loop.
type Status struct {
	LastLedger      uint32    `json:"last_ledger"`
	LastLedgerHash  string    `json:"last_ledger_hash"`
	LastClosedAt    time.Time `json:"last_ledger_closed_at"`
	ProtocolVersion uint32    `json:"protocol_version"`
	Endpoint        string    `json:"rpc_endpoint"` // host only, no credentials
	UpdatedAt       time.Time `json:"updated_at"`
}

// Ingester runs the ledger loop.
type Ingester struct {
	cfg *config.Config
	st  *store.Store
	reg *registry.Registry
	// reconcileCh wakes the coverage reconciler when the watch set grows
	// at runtime (buffered: a burst of registrations coalesces into one
	// pass, which recomputes pending work from the coverage table anyway).
	reconcileCh chan struct{}

	mu        sync.RWMutex
	status    Status
	statusSet bool
}

// New builds an Ingester. The registry is the live watch set — the loop
// reads a fresh snapshot per ledger, so runtime registrations take
// effect without a restart.
func New(cfg *config.Config, st *store.Store, reg *registry.Registry) *Ingester {
	return &Ingester{
		cfg:         cfg,
		st:          st,
		reg:         reg,
		reconcileCh: make(chan struct{}, 1),
	}
}

// NotifyChange wakes the coverage reconciler (registry change callback).
// Never blocks.
func (i *Ingester) NotifyChange() {
	select {
	case i.reconcileCh <- struct{}{}:
	default:
	}
}

// Status returns the latest published snapshot, if any. Safe for any
// number of concurrent readers.
func (i *Ingester) Status() (Status, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.status, i.statusSet
}

func (i *Ingester) publishStatus(s Status) {
	i.mu.Lock()
	i.status, i.statusSet = s, true
	i.mu.Unlock()
}

// Run ingests ledgers until ctx is done. On backend errors it rotates
// through the configured RPC endpoints; when the whole pool fails it
// returns (the supervisor restarts the process, retrying from the top).
func (i *Ingester) Run(ctx context.Context) error {
	start, prevHash, err := i.resume(ctx)
	if err != nil {
		return err
	}
	slog.Info("ingestion starting", "from_ledger", start,
		"contracts", len(i.reg.Snapshot().Contracts))

	// Contracts added after the fact — at boot via the file, at runtime
	// via POST /v1/contracts — get their missing history from a single
	// reconciler goroutine; the live loop is never blocked by it.
	go i.coverageReconciler(ctx, start)

	endpointIdx := 0
	// coldFailures counts consecutive endpoints that could not serve even
	// ONE ledger from `start`. Once the whole pool has refused cold, the
	// cause may be retention (start below every window) — resolved by
	// clamping WITH gap evidence, never silently.
	coldFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rpcURL := i.cfg.RPCURLs[endpointIdx]
		slog.Info("connecting ledger backend", "endpoint", endpointHost(rpcURL))

		n, err := i.runOnEndpoint(ctx, rpcURL, &start, &prevHash)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		slog.Error("endpoint failed, rotating", "endpoint", endpointHost(rpcURL),
			"ledgers_ingested", n, "err", err)

		if n > 0 {
			coldFailures = 0
		} else {
			coldFailures++
		}
		if coldFailures >= len(i.cfg.RPCURLs) {
			clamped, cerr := i.clampStart(ctx, start)
			switch {
			case cerr == nil:
				// Deliberate discontinuity: the gap is recorded, so the
				// hash chain restarts at the clamped ledger.
				start, prevHash = clamped, ""
				coldFailures = 0
			default:
				// Not a retention problem (or nothing answered): an
				// outage. A durable index RIDES OUT outages — exiting
				// here caused restart storms (and each boot re-ran
				// startup work). Keep retrying at the backoff cap.
				slog.Error("whole RPC pool failing; holding and retrying",
					"err", cerr)
				if coldFailures > 8 {
					coldFailures = 8 // keep the backoff capped, avoid overflow
				}
			}
		}

		endpointIdx = (endpointIdx + 1) % len(i.cfg.RPCURLs)
		// Exponential backoff between rotations so a rate-limited pool is
		// not hammered (cap 30s).
		delay := min(time.Duration(1<<min(coldFailures, 5))*time.Second, 30*time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// runOnEndpoint drives one backend until error/cancel. Returns how many
// ledgers were committed. start/prevHash advance as ledgers commit so a
// rotation resumes exactly where the previous endpoint stopped.
func (i *Ingester) runOnEndpoint(ctx context.Context, rpcURL string, start *uint32, prevHash *string) (int, error) {
	backend := ledgerbackend.NewRPCLedgerBackend(ledgerbackend.RPCLedgerBackendOptions{
		RPCServerURL: rpcURL,
		BufferSize:   uint32(i.cfg.GetLedgersLimit),
		// Without an explicit client the SDK dials with no timeout and a
		// hung getLedgers blocks GetLedger forever.
		HttpClient: &http.Client{Timeout: i.cfg.LedgerFetchTimeout},
	})
	defer backend.Close() //nolint:errcheck

	if err := backend.PrepareRange(ctx, ledgerbackend.UnboundedRange(*start)); err != nil {
		return 0, fmt.Errorf("preparing range from %d: %w", *start, err)
	}

	committed := 0
	// Timing breakdown, logged every progressLogEvery ledgers so the
	// catch-up bottleneck (RPC fetch vs DB commit) is measurable in prod.
	var fetchDur, storeDur time.Duration
	windowStart := time.Now()

	for seq := *start; ; seq++ {
		if err := ctx.Err(); err != nil {
			return committed, err
		}
		t0 := time.Now()
		meta, err := backend.GetLedger(ctx, seq)
		if err != nil {
			return committed, fmt.Errorf("fetching ledger %d: %w", seq, err)
		}
		fetchDur += time.Since(t0)
		if err := i.checkContinuity(meta, *prevHash); err != nil {
			// A fallback serving a different chain must never poison the
			// index: hard error, do not advance.
			return committed, err
		}
		t1 := time.Now()
		if err := i.processLedger(ctx, meta); err != nil {
			return committed, err
		}
		storeDur += time.Since(t1)
		hash := meta.LedgerHash().HexString()
		*start, *prevHash = seq+1, hash
		committed++

		if committed%progressLogEvery == 0 {
			elapsed := time.Since(windowStart)
			slog.Info("catch-up progress",
				"ledger", seq,
				"rate_per_s", fmt.Sprintf("%.1f", float64(progressLogEvery)/elapsed.Seconds()),
				"fetch_pct", fmt.Sprintf("%.0f", 100*fetchDur.Seconds()/elapsed.Seconds()),
				"process_store_pct", fmt.Sprintf("%.0f", 100*storeDur.Seconds()/elapsed.Seconds()))
			fetchDur, storeDur, windowStart = 0, 0, time.Now()
		}

		i.publishStatus(Status{
			LastLedger:      seq,
			LastLedgerHash:  hash,
			LastClosedAt:    time.Unix(meta.ClosedAt().Unix(), 0).UTC(),
			ProtocolVersion: uint32(meta.LedgerHeaderHistoryEntry().Header.LedgerVersion),
			Endpoint:        endpointHost(rpcURL),
			UpdatedAt:       time.Now().UTC(),
		})
	}
}

// processLedger extracts, derives and commits one ledger atomically.
// The watch set is re-read per ledger: one atomic pointer load, so a
// contract registered a second ago is already in this ledger's filter.
func (i *Ingester) processLedger(ctx context.Context, meta xdr.LedgerCloseMeta) error {
	snap := i.reg.Snapshot()
	events, err := extract.FromLedger(ctx, i.cfg.Passphrase(), snap.Watched, meta)
	if err != nil {
		return fmt.Errorf("extracting ledger %d: %w", meta.LedgerSequence(), err)
	}

	rows, derived, err := i.renderRows(events, snap.Kinds)
	if err != nil {
		return err
	}
	seq := meta.LedgerSequence()
	if err := i.st.WriteLedger(ctx, i.cfg.Network, seq, meta.LedgerHash().HexString(), rows, derived); err != nil {
		return fmt.Errorf("committing ledger %d: %w", seq, err)
	}
	if len(rows) > 0 {
		slog.Info("ledger committed", "ledger", seq, "events", len(rows),
			"leaves", len(derived.Leaves), "nullifiers", len(derived.Nullifiers),
			"registrations", len(derived.Registry))
	}
	return nil
}

// checkContinuity enforces the parent-hash chain PERMANENTLY (not just
// after rotations): meta's PreviousLedgerHash must equal the hash of the
// last committed ledger.
func (i *Ingester) checkContinuity(meta xdr.LedgerCloseMeta, prevHash string) error {
	if prevHash == "" {
		return nil // first ledger ever: nothing to chain against
	}
	parent := xdr.Hash(meta.LedgerHeaderHistoryEntry().Header.PreviousLedgerHash).HexString()
	if parent != prevHash {
		return fmt.Errorf("chain discontinuity at ledger %d: parent hash %s != expected %s",
			meta.LedgerSequence(), parent, prevHash)
	}
	return nil
}

// resume loads the cursor; without one it starts at the configured
// deployment ledger.
func (i *Ingester) resume(ctx context.Context) (uint32, string, error) {
	ledger, hash, ok, err := i.st.Cursor(ctx, i.cfg.Network)
	if err != nil {
		return 0, "", fmt.Errorf("loading cursor: %w", err)
	}
	if ok {
		slog.Info("resuming from persisted cursor", "last_ledger", ledger)
		return ledger + 1, hash, nil
	}
	start := i.reg.Snapshot().MinStart
	if start == 0 {
		return 0, "", fmt.Errorf("no cursor and no contract declares start_ledger — refusing to guess")
	}
	return start, "", nil
}

// endpointHost reduces a URL to its host for logs: API keys ride in RPC
// URL paths (e.g. Validation Cloud), so full URLs are secrets.
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid-url"
	}
	return u.Host
}
