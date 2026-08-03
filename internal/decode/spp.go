// Package decode turns raw watched events into Umbra's derived rows.
//
// Decoders are intentionally forgiving: SPP is a work-in-progress
// protocol, so an event that does not match the expected shape is
// SKIPPED (with a log), never fatal — the raw event is already persisted
// and derived tables can be rebuilt once a decoder learns the new shape.
package decode

import (
	"fmt"
	"log/slog"

	"github.com/Trustless-Work/umbra/internal/config"
	"github.com/Trustless-Work/umbra/internal/extract"
	"github.com/Trustless-Work/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Event names emitted by the SPP contracts (topic[0], snake_case — the
// soroban-sdk `#[contractevent]` convention; verified against the SPP
// SDK's own parser in sdk/state/src/events_parsers.rs).
const (
	nameNewCommitment = "new_commitment_event"
	nameNewNullifier  = "new_nullifier_event"
	namePublicKey     = "public_key_event"
)

// Derive maps a ledger's extracted events to derived rows according to
// each contract's configured kind.
func Derive(kinds map[string]config.ContractKind, events []extract.Event) store.Derived {
	var d store.Derived
	for i := range events {
		ev := &events[i]
		switch kinds[ev.ContractID] {
		case config.KindSPPPool:
			derivePool(ev, &d)
		case config.KindSPPRegistry:
			deriveRegistry(ev, &d)
		default:
			// raw / ASP kinds: raw storage only (ASP derived views are a
			// follow-up; their events already land in `events`).
		}
	}
	return d
}

func derivePool(ev *extract.Event, d *store.Derived) {
	switch ev.Name {
	case nameNewCommitment:
		leaf, err := parseNewCommitment(ev)
		if err != nil {
			skip(ev, err)
			return
		}
		d.Leaves = append(d.Leaves, leaf)
	case nameNewNullifier:
		n, err := parseNewNullifier(ev)
		if err != nil {
			skip(ev, err)
			return
		}
		d.Nullifiers = append(d.Nullifiers, n)
	}
}

func deriveRegistry(ev *extract.Event, d *store.Derived) {
	if ev.Name != namePublicKey {
		return
	}
	r, err := parsePublicKey(ev)
	if err != nil {
		skip(ev, err)
		return
	}
	d.Registry = append(d.Registry, r)
}

// parseNewCommitment: topics = [Symbol(name), commitment: U256],
// data = map{index: u32, encrypted_output: bytes}.
func parseNewCommitment(ev *extract.Event) (store.Leaf, error) {
	if len(ev.Topics) < 2 {
		return store.Leaf{}, fmt.Errorf("expected 2 topics, got %d", len(ev.Topics))
	}
	commitment, ok := ev.Topics[1].GetU256()
	if !ok {
		return store.Leaf{}, fmt.Errorf("commitment topic is not U256")
	}
	data, err := dataMap(ev)
	if err != nil {
		return store.Leaf{}, err
	}
	idxVal, ok := data["index"]
	if !ok {
		return store.Leaf{}, fmt.Errorf("data has no index field")
	}
	idx, ok := idxVal.GetU32()
	if !ok {
		return store.Leaf{}, fmt.Errorf("index is not u32")
	}
	outVal, ok := data["encrypted_output"]
	if !ok {
		return store.Leaf{}, fmt.Errorf("data has no encrypted_output field")
	}
	out, ok := outVal.GetBytes()
	if !ok {
		return store.Leaf{}, fmt.Errorf("encrypted_output is not bytes")
	}
	return store.Leaf{
		PoolID:          ev.ContractID,
		LeafIndex:       uint32(idx),
		Commitment:      extract.U256Hex(commitment),
		EncryptedOutput: out,
		Ledger:          ev.Ledger,
	}, nil
}

// parseNewNullifier: topics = [Symbol(name), nullifier: U256], no data.
func parseNewNullifier(ev *extract.Event) (store.Nullifier, error) {
	if len(ev.Topics) < 2 {
		return store.Nullifier{}, fmt.Errorf("expected 2 topics, got %d", len(ev.Topics))
	}
	nullifier, ok := ev.Topics[1].GetU256()
	if !ok {
		return store.Nullifier{}, fmt.Errorf("nullifier topic is not U256")
	}
	return store.Nullifier{
		PoolID:    ev.ContractID,
		Nullifier: extract.U256Hex(nullifier),
		Ledger:    ev.Ledger,
	}, nil
}

// parsePublicKey: topics = [Symbol(name), owner: Address],
// data = map{encryption_key: bytes, note_key: bytes}.
func parsePublicKey(ev *extract.Event) (store.RegistryKey, error) {
	if len(ev.Topics) < 2 {
		return store.RegistryKey{}, fmt.Errorf("expected 2 topics, got %d", len(ev.Topics))
	}
	addr, ok := ev.Topics[1].GetAddress()
	if !ok {
		return store.RegistryKey{}, fmt.Errorf("owner topic is not an address")
	}
	owner, err := extract.AddressString(addr)
	if err != nil {
		return store.RegistryKey{}, fmt.Errorf("rendering owner address: %w", err)
	}
	data, err := dataMap(ev)
	if err != nil {
		return store.RegistryKey{}, err
	}
	encVal, ok := data["encryption_key"]
	if !ok {
		return store.RegistryKey{}, fmt.Errorf("data has no encryption_key field")
	}
	enc, ok := encVal.GetBytes()
	if !ok {
		return store.RegistryKey{}, fmt.Errorf("encryption_key is not bytes")
	}
	noteVal, ok := data["note_key"]
	if !ok {
		return store.RegistryKey{}, fmt.Errorf("data has no note_key field")
	}
	note, ok := noteVal.GetBytes()
	if !ok {
		return store.RegistryKey{}, fmt.Errorf("note_key is not bytes")
	}
	return store.RegistryKey{
		RegistryID:    ev.ContractID,
		Owner:         owner,
		EncryptionKey: enc,
		NoteKey:       note,
		Ledger:        ev.Ledger,
	}, nil
}

// dataMap returns the event data as a symbol-keyed ScVal map.
func dataMap(ev *extract.Event) (map[string]xdr.ScVal, error) {
	m, ok := ev.Data.GetMap()
	if !ok || m == nil {
		return nil, fmt.Errorf("event data is not a map")
	}
	out := make(map[string]xdr.ScVal, len(*m))
	for _, entry := range *m {
		if sym, ok := entry.Key.GetSym(); ok {
			out[string(sym)] = entry.Val
		}
	}
	return out, nil
}

func skip(ev *extract.Event, err error) {
	slog.Warn("skipping undecodable event (raw copy is persisted)",
		"event_id", ev.ID, "contract", ev.ContractID, "name", ev.Name, "err", err)
}
