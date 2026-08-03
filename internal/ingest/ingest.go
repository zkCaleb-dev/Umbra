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
	"time"

	"github.com/Trustless-Work/umbra/internal/config"
	"github.com/Trustless-Work/umbra/internal/decode"
	"github.com/Trustless-Work/umbra/internal/extract"
	"github.com/Trustless-Work/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Status is a snapshot for /v1/status, maintained by the loop.
type Status struct {
	LastLedger     uint32    `json:"last_ledger"`
	LastLedgerHash string    `json:"last_ledger_hash"`
	LastClosedAt   time.Time `json:"last_ledger_closed_at"`
	Endpoint       string    `json:"rpc_endpoint"` // host only, no credentials
	UpdatedAt      time.Time `json:"updated_at"`
}

// Ingester runs the ledger loop.
type Ingester struct {
	cfg     *config.Config
	st      *store.Store
	watched map[string]struct{}
	kinds   map[string]config.ContractKind

	statusCh chan Status // latest-wins snapshot for the API
}

// New builds an Ingester.
func New(cfg *config.Config, st *store.Store) *Ingester {
	kinds := cfg.ContractKinds()
	watched := make(map[string]struct{}, len(kinds))
	for id := range kinds {
		watched[id] = struct{}{}
	}
	ing := &Ingester{
		cfg:      cfg,
		st:       st,
		watched:  watched,
		kinds:    kinds,
		statusCh: make(chan Status, 1),
	}
	return ing
}

// Status returns the latest published snapshot, if any.
func (i *Ingester) Status() (Status, bool) {
	select {
	case s := <-i.statusCh:
		// Put it back for the next reader.
		i.publishStatus(s)
		return s, true
	default:
		return Status{}, false
	}
}

func (i *Ingester) publishStatus(s Status) {
	select {
	case i.statusCh <- s:
	default:
		select { // replace the stale snapshot
		case <-i.statusCh:
		default:
		}
		select {
		case i.statusCh <- s:
		default:
		}
	}
}

// Run ingests ledgers until ctx is done. On backend errors it rotates
// through the configured RPC endpoints; when the whole pool fails it
// returns (the supervisor restarts the process, retrying from the top).
func (i *Ingester) Run(ctx context.Context) error {
	start, prevHash, err := i.resume(ctx)
	if err != nil {
		return err
	}
	slog.Info("ingestion starting", "from_ledger", start, "contracts", len(i.watched))

	endpointIdx := 0
	for attempt := 0; attempt < len(i.cfg.RPCURLs); attempt++ {
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
		endpointIdx = (endpointIdx + 1) % len(i.cfg.RPCURLs)
		if n > 0 {
			attempt = -1 // progress was made: reset the give-up counter
		}
	}
	return fmt.Errorf("every configured RPC endpoint failed")
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
	for seq := *start; ; seq++ {
		if err := ctx.Err(); err != nil {
			return committed, err
		}
		meta, err := backend.GetLedger(ctx, seq)
		if err != nil {
			return committed, fmt.Errorf("fetching ledger %d: %w", seq, err)
		}
		if err := i.checkContinuity(meta, *prevHash); err != nil {
			// A fallback serving a different chain must never poison the
			// index: hard error, do not advance.
			return committed, err
		}
		if err := i.processLedger(ctx, meta); err != nil {
			return committed, err
		}
		hash := meta.LedgerHash().HexString()
		*start, *prevHash = seq+1, hash
		committed++

		i.publishStatus(Status{
			LastLedger:     seq,
			LastLedgerHash: hash,
			LastClosedAt:   time.Unix(meta.ClosedAt().Unix(), 0).UTC(),
			Endpoint:       endpointHost(rpcURL),
			UpdatedAt:      time.Now().UTC(),
		})
	}
}

// processLedger extracts, derives and commits one ledger atomically.
func (i *Ingester) processLedger(ctx context.Context, meta xdr.LedgerCloseMeta) error {
	events, err := extract.FromLedger(ctx, i.cfg.Passphrase(), i.watched, meta)
	if err != nil {
		return fmt.Errorf("extracting ledger %d: %w", meta.LedgerSequence(), err)
	}

	rows := make([]store.RawEvent, 0, len(events))
	for idx := range events {
		ev := &events[idx]
		topics, err := ev.TopicsJSON()
		if err != nil {
			return fmt.Errorf("rendering topics of %s: %w", ev.ID, err)
		}
		data, err := ev.DataJSON()
		if err != nil {
			return fmt.Errorf("rendering data of %s: %w", ev.ID, err)
		}
		rows = append(rows, store.RawEvent{
			ID: ev.ID, Ledger: ev.Ledger, LedgerClosedAt: ev.LedgerClosedAt,
			TxHash: ev.TxHash, TxIndex: ev.TxIndex, EventIndex: ev.EventIndex,
			ContractID: ev.ContractID, Name: ev.Name,
			TopicsJSON: topics, DataJSON: data, RawXDR: ev.RawXDR,
		})
	}

	derived := decode.Derive(i.kinds, events)
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
	start := i.cfg.StartLedger()
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
