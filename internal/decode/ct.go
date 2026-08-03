package decode

import (
	"encoding/json"
	"fmt"

	"github.com/zkCaleb-dev/umbra/internal/extract"
	"github.com/zkCaleb-dev/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Confidential Token (OpenZeppelin stellar-contracts, confidential
// package) events. Topic layout follows the #[contractevent] convention:
// topic[0] = snake_case name, then the #[topic] Address fields in
// declaration order; remaining fields ride in the data map.
//
// The decoder is deliberately shape-light: it classifies the kind,
// collects every Address topic (that is what history-by-address
// queries filter on), lifts the public amount when the event has one
// (deposit/withdraw), and stores the rest of the data map — the
// ciphertext material wallets decrypt locally — as JSON payload.
var ctKinds = map[string]bool{
	"register":         true,
	"deposit":          true,
	"merge":            true,
	"withdraw":         true,
	"transfer":         true,
	"spender_transfer": true,
	"set_spender":      true,
	"revoke_spender":   true,
}

func deriveCT(ev *extract.Event, d *store.Derived) {
	if !ctKinds[ev.Name] {
		return // unknown CT event kinds stay raw-only
	}
	row, err := parseCTEvent(ev)
	if err != nil {
		skip(ev, err)
		return
	}
	d.CTEvents = append(d.CTEvents, row)
}

func parseCTEvent(ev *extract.Event) (store.CTEvent, error) {
	// Every Address topic after the name participates in the event.
	addrs := []string{}
	for _, t := range ev.Topics[1:] {
		if a, ok := t.GetAddress(); ok {
			s, err := extract.AddressString(a)
			if err != nil {
				return store.CTEvent{}, fmt.Errorf("rendering topic address: %w", err)
			}
			addrs = append(addrs, s)
		}
	}
	if len(addrs) == 0 {
		return store.CTEvent{}, fmt.Errorf("ct event %s carries no address topics", ev.Name)
	}

	var amount *string
	var payload []byte
	if m, ok := ev.Data.GetMap(); ok && m != nil {
		fields := map[string]any{}
		for _, entry := range *m {
			sym, ok := entry.Key.GetSym()
			if !ok {
				continue
			}
			key := string(sym)
			if key == "amount" {
				if p, ok := entry.Val.GetI128(); ok {
					v := extract.I128String(p)
					amount = &v
					continue
				}
			}
			fields[key] = renderScVal(entry.Val)
		}
		if len(fields) > 0 {
			raw, err := json.Marshal(fields)
			if err != nil {
				return store.CTEvent{}, fmt.Errorf("rendering payload: %w", err)
			}
			payload = raw
		}
	}

	return store.CTEvent{
		EventID:      ev.ID,
		TokenID:      ev.ContractID,
		Ledger:       ev.Ledger,
		TxHash:       ev.TxHash,
		Kind:         ev.Name,
		Addresses:    addrs,
		AmountPublic: amount,
		Payload:      payload,
	}, nil
}

// renderScVal delegates to the extract package's safe renderer via a
// tiny wrapper event (keeps a single rendering codepath).
func renderScVal(v xdr.ScVal) any {
	e := extract.Event{Data: v}
	raw, err := e.DataJSON()
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}
