// Package registry owns the dynamic watch set: which contracts Umbra
// indexes and with which decoder kind. The deployments file seeds it on
// boot; POST /v1/contracts grows it at runtime. Consumers (the ingest
// loop, backfill, the API) read an immutable snapshot via one atomic
// pointer load, so registration never blocks or races the hot path.
package registry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/store"
)

// MaxContracts is an operational safety valve, not access control: the
// registration endpoint is deliberately open (testnet trial), but a
// runaway loop hammering it must not grow the watch set without bound.
const MaxContracts = 512

// Entry is one watched contract plus its provenance.
type Entry struct {
	config.Contract
	Source string `json:"source"` // "config" | "api"
}

// Snapshot is an immutable view of the watch set. Never mutate a
// snapshot — build a new one and swap.
type Snapshot struct {
	Contracts []Entry
	Watched   map[string]struct{}
	Kinds     map[string]config.ContractKind
	// MinStart is the lowest nonzero start_ledger — where a cold boot
	// with no cursor begins.
	MinStart uint32
}

// Registry loads from and persists to the contracts table.
type Registry struct {
	st   *store.Store
	snap atomic.Pointer[Snapshot]

	mu       sync.Mutex // serializes Register
	onChange func()
}

// Load seeds the table from the deployments file (config source wins for
// its ids) and builds the first snapshot from the table — which may hold
// additional API-registered contracts from previous runs.
func Load(ctx context.Context, st *store.Store, seed []config.Contract) (*Registry, error) {
	for _, c := range seed {
		err := st.SeedContract(ctx, store.ContractRow{
			ID: c.ID, Kind: string(c.Kind), StartLedger: c.StartLedger, Label: c.Label,
		})
		if err != nil {
			return nil, err
		}
	}
	r := &Registry{st: st}
	if err := r.reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Snapshot returns the current immutable view.
func (r *Registry) Snapshot() *Snapshot { return r.snap.Load() }

// SetOnChange installs the callback fired after a successful
// registration (the ingester uses it to reconcile coverage). Set once,
// before the registry is shared.
func (r *Registry) SetOnChange(f func()) { r.onChange = f }

// Register validates and persists a new contract, swaps the snapshot,
// and pings the change callback. Returns created=false when the id was
// already registered.
func (r *Registry) Register(ctx context.Context, e Entry) (created bool, err error) {
	if err := ValidateContractID(e.ID); err != nil {
		return false, err
	}
	if !validKind(e.Kind) {
		return false, fmt.Errorf("unknown kind %q", e.Kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.Snapshot().Contracts) >= MaxContracts {
		return false, fmt.Errorf("registry is full (%d contracts) — this is a trial instance, ask the operator to raise the cap", MaxContracts)
	}
	created, err = r.st.InsertContract(ctx, store.ContractRow{
		ID: e.ID, Kind: string(e.Kind), StartLedger: e.StartLedger, Label: e.Label,
	})
	if err != nil {
		return false, err
	}
	if !created {
		return false, nil
	}
	if err := r.reload(ctx); err != nil {
		return true, err
	}
	if r.onChange != nil {
		r.onChange()
	}
	return true, nil
}

// reload rebuilds the snapshot from the table.
func (r *Registry) reload(ctx context.Context) error {
	rows, err := r.st.ListContracts(ctx)
	if err != nil {
		return err
	}
	snap := &Snapshot{
		Contracts: make([]Entry, 0, len(rows)),
		Watched:   make(map[string]struct{}, len(rows)),
		Kinds:     make(map[string]config.ContractKind, len(rows)),
	}
	for _, row := range rows {
		e := Entry{Contract: config.Contract{
			ID: row.ID, Kind: config.ContractKind(row.Kind),
			StartLedger: row.StartLedger, Label: row.Label,
		}, Source: row.Source}
		snap.Contracts = append(snap.Contracts, e)
		snap.Watched[e.ID] = struct{}{}
		snap.Kinds[e.ID] = e.Kind
		if e.StartLedger != 0 && (snap.MinStart == 0 || e.StartLedger < snap.MinStart) {
			snap.MinStart = e.StartLedger
		}
	}
	r.snap.Store(snap)
	return nil
}

// ValidateContractID checks the C… strkey shape (including checksum).
func ValidateContractID(id string) error {
	if len(id) != 56 || !strings.HasPrefix(id, "C") {
		return fmt.Errorf("contract id must be a 56-character C… strkey")
	}
	if _, err := strkey.Decode(strkey.VersionByteContract, id); err != nil {
		return fmt.Errorf("invalid contract strkey: %w", err)
	}
	return nil
}

func validKind(k config.ContractKind) bool {
	switch k {
	case config.KindSPPPool, config.KindSPPRegistry, config.KindSPPASPMembership,
		config.KindSPPASPNonMember, config.KindToken, config.KindConfidentialToken, config.KindRaw:
		return true
	}
	return false
}
