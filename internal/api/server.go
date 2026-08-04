// Package api serves Umbra's read-only HTTP surface.
//
// Privacy stance: responses are range-shaped (download whole ranges,
// trial-decrypt locally) so the server cannot learn which notes belong
// to whom, and client addresses are NOT logged unless the operator opts
// in with UMBRA_LOG_CLIENT_IP=true.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/ingest"
	"github.com/zkCaleb-dev/umbra/internal/registry"
	"github.com/zkCaleb-dev/umbra/internal/store"
	"github.com/zkCaleb-dev/umbra/internal/verify"
)

const (
	defaultPageSize = 500
	maxPageSize     = 5000
)

//go:embed dashboard.html
var dashboardHTML []byte

// Server is the HTTP read API plus the one write it allows: contract
// registration.
type Server struct {
	cfg *config.Config
	st  *store.Store
	ing *ingest.Ingester
	reg *registry.Registry

	cpMu    sync.Mutex
	cpCache map[string]cachedCheckpoint
}

// cachedCheckpoint memoizes a pool's verification verdict. Recomputing
// the root is O(leaves) plus two RPC round trips, so serving it
// uncached on a public endpoint would be a denial-of-service surface.
// A verdict stays valid while the leaf set is unchanged and young.
type cachedCheckpoint struct {
	leafCount int
	at        time.Time
	cp        *verify.Checkpoint
}

const checkpointTTL = 60 * time.Second

// New builds the server.
func New(cfg *config.Config, st *store.Store, ing *ingest.Ingester, reg *registry.Registry) *Server {
	return &Server{cfg: cfg, st: st, ing: ing, reg: reg, cpCache: map[string]cachedCheckpoint{}}
}

// Run serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/pools", s.handlePools)
	mux.HandleFunc("GET /v1/pools/{id}/leaves", s.handleLeaves)
	mux.HandleFunc("GET /v1/pools/{id}/outputs", s.handleOutputs)
	mux.HandleFunc("GET /v1/pools/{id}/nullifiers", s.handleNullifiers)
	mux.HandleFunc("GET /v1/registry/{address}", s.handleRegistry)
	mux.HandleFunc("GET /v1/contracts/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /v1/tokens/{id}/transfers", s.handleTransfers)
	mux.HandleFunc("GET /v1/pools/{id}/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("GET /v1/ct/{token}/history/{address}", s.handleCTHistory)
	mux.HandleFunc("POST /v1/contracts", s.handleRegisterContract)
	// Bootnode-compatible JSON-RPC surface (see jsonrpc.go).
	mux.HandleFunc("POST /{$}", s.handleJSONRPC)
	// Human-facing status page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})

	srv := &http.Server{
		Addr:              s.cfg.APIBind,
		Handler:           s.withCommon(mux),
		ReadHeaderTimeout: 10 * time.Second, // gosec G112
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("api listening", "bind", s.cfg.APIBind)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// withCommon adds CORS (public read API) and optional access logging.
func (s *Server) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.cfg.LogClientIP {
			slog.Info("request", "path", r.URL.Path, "remote", r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Ping(r.Context()); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	type statusResp struct {
		Network   string           `json:"network"`
		Ingestion *ingest.Status   `json:"ingestion,omitempty"`
		Gaps      []store.GapItem  `json:"gaps"`
		Contracts []map[string]any `json:"contracts"`
	}
	resp := statusResp{Network: s.cfg.Network}
	if st, ok := s.ing.Status(); ok {
		resp.Ingestion = &st
	}
	gaps, err := s.st.Gaps(r.Context(), s.cfg.Network)
	if err != nil {
		s.fail(w, "listing gaps", err)
		return
	}
	resp.Gaps = gaps

	// Per-contract observability: coverage floor and which event names
	// were actually seen — unrecognized names are the tell that a
	// decoder ignores something the contract emits.
	names, err := s.st.EventNameCounts(r.Context())
	if err != nil {
		s.fail(w, "counting event names", err)
		return
	}
	for _, ct := range s.reg.Snapshot().Contracts {
		entry := map[string]any{
			"id": ct.ID, "kind": ct.Kind, "label": ct.Label,
			"start_ledger": ct.StartLedger, "source": ct.Source,
		}
		if covered, known, err := s.st.CoveredFrom(r.Context(), ct.ID); err == nil && known {
			entry["covered_from"] = covered
		}
		if n := names[ct.ID]; len(n) > 0 {
			entry["events"] = n
		}
		resp.Contracts = append(resp.Contracts, entry)
	}
	s.ok(w, resp)
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	type poolResp struct {
		ID          string           `json:"id"`
		Label       string           `json:"label,omitempty"`
		StartLedger uint32           `json:"start_ledger"`
		Stats       *store.PoolStats `json:"stats"`
	}
	out := []poolResp{}
	for _, ct := range s.reg.Snapshot().Contracts {
		if ct.Kind != config.KindSPPPool {
			continue
		}
		stats, err := s.st.Stats(r.Context(), ct.ID)
		if err != nil {
			s.fail(w, "computing pool stats", err)
			return
		}
		out = append(out, poolResp{ID: ct.ID, Label: ct.Label, StartLedger: ct.StartLedger, Stats: stats})
	}
	s.ok(w, out)
}

func (s *Server) handleLeaves(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, false)
}

func (s *Server) handleOutputs(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, true)
}

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, includeOutputs bool) {
	poolID, ok := s.knownPool(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown pool", http.StatusNotFound)
		return
	}
	from := queryUint(r, "from_index", 0)
	limit := queryLimit(r)
	page, err := s.st.Leaves(r.Context(), poolID, from, limit, includeOutputs)
	if err != nil {
		s.fail(w, "querying leaves", err)
		return
	}
	// A full page of an append-only stream is immutable forever: let
	// CDNs and clients cache it. Partial pages are still growing.
	if page.NextIndex != nil {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.ok(w, page)
}

func (s *Server) handleNullifiers(w http.ResponseWriter, r *http.Request) {
	poolID, ok := s.knownPool(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown pool", http.StatusNotFound)
		return
	}
	since := queryUint(r, "since_ledger", 0)
	limit := queryLimit(r)
	page, err := s.st.Nullifiers(r.Context(), poolID, since, limit)
	if err != nil {
		s.fail(w, "querying nullifiers", err)
		return
	}
	s.ok(w, page)
}

func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	entry, found, err := s.st.RegistryLookup(r.Context(), r.PathValue("address"))
	if err != nil {
		s.fail(w, "registry lookup", err)
		return
	}
	if !found {
		http.Error(w, "address not registered", http.StatusNotFound)
		return
	}
	s.ok(w, entry)
}

// handleEvents serves any configured contract's raw event stream — the
// generic surface: add a contract with kind "raw" and its full history
// is queryable here, no decoder required.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, watched := s.watchedContract(id); !watched {
		http.Error(w, "contract not indexed", http.StatusNotFound)
		return
	}
	ok, cursor := store.ParseEventCursor(r.URL.Query().Get("cursor"))
	if !ok {
		http.Error(w, "malformed cursor (want <ledger>-<tx>-<event>)", http.StatusBadRequest)
		return
	}
	page, err := s.st.Events(r.Context(), id, cursor, queryLimit(r))
	if err != nil {
		s.fail(w, "querying events", err)
		return
	}
	s.ok(w, page)
}

// handleTransfers serves decoded transfers of a kind:"token" contract,
// optionally filtered by ?address= (sender or recipient).
func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if kind, watched := s.watchedContract(id); !watched || kind != config.KindToken {
		http.Error(w, "token not indexed", http.StatusNotFound)
		return
	}
	page, err := s.st.Transfers(r.Context(), id,
		r.URL.Query().Get("address"), queryUint(r, "since_ledger", 0), queryLimit(r))
	if err != nil {
		s.fail(w, "querying transfers", err)
		return
	}
	s.ok(w, page)
}

// handleCTHistory serves a confidential token's events touching one
// address: kinds, public amounts where they exist, and the ciphertext
// payload the wallet decrypts locally.
func (s *Server) handleCTHistory(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if kind, watched := s.watchedContract(token); !watched || kind != config.KindConfidentialToken {
		http.Error(w, "confidential token not indexed", http.StatusNotFound)
		return
	}
	page, err := s.st.CTHistory(r.Context(), token, r.PathValue("address"),
		queryUint(r, "since_ledger", 0), queryLimit(r))
	if err != nil {
		s.fail(w, "querying ct history", err)
		return
	}
	s.ok(w, page)
}

// watchedContract reports whether id is registered, and its kind.
func (s *Server) watchedContract(id string) (config.ContractKind, bool) {
	kind, ok := s.reg.Snapshot().Kinds[id]
	return kind, ok
}

// handleCheckpoint recomputes the pool's Merkle root from the indexed
// leaves and compares it against the contract's current on-chain root —
// the "don't trust, verify" endpoint. Both inputs are public: anyone can
// repeat the computation from /leaves and getLedgerEntries.
func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	poolID, ok := s.knownPool(r.PathValue("id"))
	if !ok {
		http.Error(w, "unknown pool", http.StatusNotFound)
		return
	}
	commitments, err := s.st.AllLeafCommitments(r.Context(), poolID)
	if err != nil {
		s.fail(w, "loading leaves", err)
		return
	}

	s.cpMu.Lock()
	if c, hit := s.cpCache[poolID]; hit && c.leafCount == len(commitments) &&
		time.Since(c.at) < checkpointTTL {
		s.cpMu.Unlock()
		s.ok(w, c.cp)
		return
	}
	s.cpMu.Unlock()

	leaves := make([]*big.Int, len(commitments))
	for i, c := range commitments {
		n, ok := new(big.Int).SetString(strings.TrimPrefix(c, "0x"), 16)
		if !ok {
			s.fail(w, "parsing commitment", fmt.Errorf("bad commitment %q at leaf %d", c, i))
			return
		}
		leaves[i] = n
	}
	// Try the whole RPC pool, not just the primary.
	var cp *verify.Checkpoint
	for _, rpcURL := range s.cfg.RPCURLs {
		cp, err = verify.Verify(r.Context(), rpcURL, poolID, leaves)
		if err == nil {
			break
		}
	}
	if err != nil {
		s.fail(w, "verifying checkpoint", err)
		return
	}
	s.cpMu.Lock()
	s.cpCache[poolID] = cachedCheckpoint{leafCount: len(commitments), at: time.Now(), cp: cp}
	s.cpMu.Unlock()
	s.ok(w, cp)
}

// knownPool validates a path pool id against the registered spp-pool set
// — the API never turns user input into arbitrary queries.
func (s *Server) knownPool(id string) (string, bool) {
	if kind, ok := s.reg.Snapshot().Kinds[id]; ok && kind == config.KindSPPPool {
		return id, true
	}
	return "", false
}

func (s *Server) ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encoding response", "err", err)
	}
}

// fail logs the real error and answers 500 without leaking internals.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func queryUint(r *http.Request, key string, def uint32) uint32 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return def
	}
	return uint32(v)
}

func queryLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultPageSize
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultPageSize
	}
	if v > maxPageSize {
		return maxPageSize
	}
	return v
}
