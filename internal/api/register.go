package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/registry"
)

// ledgersPerDay assumes the network's 5-second close time.
const ledgersPerDay = 17280

// ledgersAgo returns the ledger roughly n ledgers before the tip (1 when
// that underflows, 0 when the tip is not known yet — live-only then).
func (s *Server) ledgersAgo(n uint32) uint32 {
	st, ok := s.ing.Status()
	if !ok {
		return 0
	}
	if st.LastLedger <= n {
		return 1
	}
	return st.LastLedger - n
}

// ledgerAround converts an approximate date to a ledger, with a one-day
// margin so "more or less when I deployed" still lands before the fact.
func (s *Server) ledgerAround(since string) (uint32, error) {
	t, err := time.Parse("2006-01-02", since)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, since); err != nil {
			return 0, fmt.Errorf("since must be a date like 2026-07-01")
		}
	}
	age := time.Since(t)
	if age < 0 {
		return 0, fmt.Errorf("since is in the future")
	}
	return s.ledgersAgo(uint32(age.Hours()/24*ledgersPerDay) + ledgersPerDay), nil
}

// handleRegisterContract adds a contract to the watch set at runtime.
// Deliberately open (this is a testnet trial instance): anyone can point
// Umbra at their contract and get live indexing immediately plus
// whatever history the RPC pool still retains. Kind is optional — when
// omitted the contract is classified from its on-chain spec.
func (s *Server) handleRegisterContract(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		StartLedger uint32 `json:"start_ledger"`
		// Since is a user-friendly alternative to start_ledger: an
		// approximate date (2026-07-01 or RFC3339) history should reach
		// back to. Converted to a ledger with a one-day safety margin.
		Since string `json:"since"`
		Label string `json:"label"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed JSON body", http.StatusBadRequest)
		return
	}
	if err := registry.ValidateContractID(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Label) > 120 {
		http.Error(w, "label too long (max 120)", http.StatusBadRequest)
		return
	}

	kind := config.ContractKind(req.Kind)
	classification := "explicit"
	if kind == "" {
		detected, detail, err := registry.Classify(r.Context(), s.cfg.RPCURLs, req.ID)
		if err != nil {
			http.Error(w, "could not classify contract: "+err.Error(), http.StatusBadGateway)
			return
		}
		kind = detected
		classification = "auto: " + detail
	}

	// Depth: explicit start_ledger wins, then an approximate date, then
	// 30 days. The default is bounded on purpose — this endpoint is
	// open, and unbounded depth would grow the database on anyone's
	// whim; full depth stays available by asking for it explicitly
	// (docs/ARCHIVE-BACKFILL.md documents the policy).
	start := req.StartLedger
	if start == 0 && req.Since != "" {
		var err error
		if start, err = s.ledgerAround(req.Since); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if start == 0 {
		start = s.ledgersAgo(30 * ledgersPerDay)
	}
	backfill := "none (live indexing from now)"
	if start > 0 {
		backfill = "scheduled — newest history lands first"
		if oldest := s.ing.PoolOldest(r.Context()); oldest > start {
			backfill += "; the part below the RPC window is replayed from the public history archives (deep ranges take hours, once)"
		}
	}

	created, err := s.reg.Register(r.Context(), registry.Entry{Contract: config.Contract{
		ID: req.ID, Kind: kind, StartLedger: start, Label: req.Label,
	}})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := map[string]any{
		"id": req.ID, "kind": kind, "start_ledger": start,
		"label": req.Label, "source": "api", "classification": classification,
		"backfill": backfill,
	}
	if !created {
		existing, _ := s.watchedContract(req.ID)
		http.Error(w, "contract already registered with kind "+string(existing), http.StatusConflict)
		return
	}
	slog.Info("contract registered via API", "contract", req.ID, "kind", kind,
		"start_ledger", start, "classification", classification)
	w.WriteHeader(http.StatusCreated)
	s.ok(w, resp)
}
