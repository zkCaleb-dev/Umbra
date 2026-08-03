package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Retention handling.
//
// The authority on which ledgers an endpoint can serve is getLedgers
// ITSELF, never getHealth: providers with data-lake backfill ("Infinite
// Scroll") serve ledgers far below the oldestLedger their getHealth
// reports. So Umbra never pre-checks retention — it simply attempts the
// range, and only when the FIRST fetch fails on every endpoint does it
// consult getHealth as an estimate of where ingestion can resume.
//
// When the whole pool refuses the requested start, Umbra clamps to the
// best (lowest) oldestLedger in the pool and records the skipped range
// in the gaps table: the index stays honest about what it does not
// cover, and a future archive backfill has its work-list.

// oldestLedger asks one endpoint's getHealth for its advertised oldest
// retained ledger. Only used as a clamp estimate after getLedgers
// refused — see the package comment above.
func (i *Ingester) oldestLedger(ctx context.Context, rpcURL string) (uint32, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getHealth",
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck

	var out struct {
		Result struct {
			OldestLedger uint32 `json:"oldestLedger"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decoding getHealth from %s: %w", endpointHost(rpcURL), err)
	}
	if out.Error != nil {
		return 0, fmt.Errorf("getHealth from %s: %s", endpointHost(rpcURL), out.Error.Message)
	}
	if out.Result.OldestLedger == 0 {
		return 0, fmt.Errorf("getHealth from %s reported no oldestLedger", endpointHost(rpcURL))
	}
	return out.Result.OldestLedger, nil
}

// clampStart is called when every endpoint failed to serve `want` as the
// first ledger. It picks the lowest advertised oldestLedger across the
// pool; if that is above `want`, it records the gap and returns the
// clamped start. If no endpoint even answers getHealth, or the pool
// claims it DOES cover `want` (meaning the failures were not about
// retention), it returns an error so the caller keeps treating this as
// an outage rather than silently skipping ledgers.
func (i *Ingester) clampStart(ctx context.Context, want uint32) (uint32, error) {
	best := uint32(0)
	bestHost := ""
	for _, rpcURL := range i.cfg.RPCURLs {
		oldest, err := i.oldestLedger(ctx, rpcURL)
		if err != nil {
			slog.Warn("getHealth probe failed", "endpoint", endpointHost(rpcURL), "err", err)
			continue
		}
		if best == 0 || oldest < best {
			best, bestHost = oldest, endpointHost(rpcURL)
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("no endpoint answered getHealth; cannot estimate a resume point")
	}
	if best <= want {
		return 0, fmt.Errorf("pool claims coverage of ledger %d (oldest %d on %s) yet getLedgers failed — treating as outage",
			want, best, bestHost)
	}
	if err := i.st.RecordGap(ctx, i.cfg.Network, want, best-1, "below provider retention at startup"); err != nil {
		return 0, fmt.Errorf("recording retention gap [%d,%d]: %w", want, best-1, err)
	}
	slog.Warn("requested start is below every endpoint's retention — clamping and recording gap",
		"wanted", want, "clamped_to", best, "endpoint", bestHost,
		"gap", fmt.Sprintf("[%d,%d]", want, best-1))
	return best, nil
}
