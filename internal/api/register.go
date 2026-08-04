package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/registry"
)

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
		Label       string `json:"label"`
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

	// Default the start ledger to the oldest history the RPC pool still
	// serves — register-and-save-everything-reachable is the point.
	start := req.StartLedger
	backfill := "none (live indexing from now)"
	if start == 0 {
		if oldest := s.ing.PoolOldest(r.Context()); oldest > 0 {
			start = oldest
		}
	}
	if start > 0 {
		backfill = "scheduled"
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
