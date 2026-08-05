//go:build js && wasm

// The /view page's crypto core: internal/ct compiled to WebAssembly.
// Derived keys live ONLY inside this module's memory — JavaScript hands
// in a seed or a wallet signature and gets back the account id and a
// decrypted statement, never sk/vk. The server side of /view serves
// ciphertexts and public on-chain points; everything readable is opened
// here, in the visitor's browser.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"syscall/js"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/ct"
)

var keys *ct.Keys

func main() {
	js.Global().Set("umbraDeriveFromSeed", js.FuncOf(deriveFromSeed))
	js.Global().Set("umbraDerivationMessage", js.FuncOf(derivationMessage))
	js.Global().Set("umbraDeriveFromSignature", js.FuncOf(deriveFromSignature))
	js.Global().Set("umbraStatement", js.FuncOf(statement))
	select {}
}

func fail(err error) map[string]any { return map[string]any{"error": err.Error()} }

// deriveFromSeed(seed, contract) -> {account} | {error}
func deriveFromSeed(_ js.Value, args []js.Value) any {
	k, err := ct.DeriveKeys(args[0].String(), args[1].String())
	if err != nil {
		return fail(err)
	}
	keys = k
	return map[string]any{"account": k.Account}
}

// derivationMessage(contract, account) -> the SEP-0053 message a wallet
// must sign; its signature is the derivation root.
func derivationMessage(_ js.Value, args []js.Value) any {
	return ct.DerivationMessage(args[0].String(), args[1].String())
}

// deriveFromSignature(signatureB64, contract, account) -> {account} | {error}
func deriveFromSignature(_ js.Value, args []js.Value) any {
	sig, err := base64.StdEncoding.DecodeString(args[0].String())
	if err != nil {
		return fail(fmt.Errorf("signature is not base64: %w", err))
	}
	if len(sig) != 64 {
		return fail(fmt.Errorf("expected a 64-byte ed25519 signature, got %d bytes", len(sig)))
	}
	k, err := ct.DeriveKeysFromRoot(sig, args[1].String(), args[2].String())
	if err != nil {
		return fail(err)
	}
	keys = k
	return map[string]any{"account": k.Account}
}

// Wire shapes mirror the API responses (see internal/view).
type historyItem struct {
	EventID        string          `json:"event_id"`
	Ledger         uint32          `json:"ledger"`
	LedgerClosedAt time.Time       `json:"ledger_closed_at"`
	TxHash         string          `json:"tx_hash"`
	Kind           string          `json:"kind"`
	Addresses      []string        `json:"addresses"`
	AmountPublic   *string         `json:"amount_public"`
	Payload        json.RawMessage `json:"payload"`
}

type onChain struct {
	LatestLedger uint32 `json:"latest_ledger"`
	ViewingKey   string `json:"viewing_public_key"`
	Spendable    string `json:"spendable_commitment"`
	Receiving    string `json:"receiving_commitment"`
}

var debits = map[string]bool{
	"deposit/sender": true, "withdraw/sender": true, "transfer/sender": true,
	"set_spender/owner": true,
}

var credits = map[string]bool{
	"deposit/recipient": true, "transfer/recipient": true,
	"spender_transfer/recipient": true, "revoke_spender/owner": true,
}

// statement(eventsJSON, onchainJSON) -> statement JSON string | {error}.
// eventsJSON is the merged `events` array of the history pages;
// onchainJSON is the /v1/ct/{token}/account/{address} response ("" to
// skip verification).
func statement(_ js.Value, args []js.Value) any {
	if keys == nil {
		return fail(fmt.Errorf("derive keys first"))
	}
	var items []historyItem
	if err := json.Unmarshal([]byte(args[0].String()), &items); err != nil {
		return fail(fmt.Errorf("parsing history: %w", err))
	}
	events := make([]ct.Event, 0, len(items))
	for _, it := range items {
		ev := ct.Event{
			ID: it.EventID, Ledger: it.Ledger, ClosedAt: it.LedgerClosedAt,
			TxHash: it.TxHash, Kind: it.Kind, Addresses: it.Addresses,
		}
		if it.AmountPublic != nil {
			n, ok := new(big.Int).SetString(*it.AmountPublic, 10)
			if !ok {
				return fail(fmt.Errorf("event %s: bad public amount", it.EventID))
			}
			ev.AmountPublic = n
		}
		p, err := ct.ParsePayload(it.Payload)
		if err != nil {
			return fail(fmt.Errorf("event %s: %w", it.EventID, err))
		}
		ev.Payload = p
		events = append(events, ev)
	}

	st, err := ct.BuildStatement(keys.Account, keys.VK, events)
	if err != nil {
		return fail(err)
	}

	out := map[string]any{
		"account":  keys.Account,
		"entries":  renderEntries(st),
		"warnings": st.Warnings,
	}
	setAmount := func(key string, v *big.Int) {
		if v != nil {
			out[key] = v.String()
		}
	}
	setAmount("spendable", st.Spendable)
	setAmount("pending", st.Pending)
	if st.Spendable != nil && st.Pending != nil {
		out["total"] = new(big.Int).Add(st.Spendable, st.Pending).String()
	}

	if raw := args[1].String(); raw != "" {
		out["verify"] = verify(st, raw)
	}

	blob, err := json.Marshal(out)
	if err != nil {
		return fail(err)
	}
	return string(blob)
}

func renderEntries(st *ct.Statement) []any {
	entries := make([]any, 0, len(st.Entries))
	for _, e := range st.Entries {
		entry := map[string]any{
			"ledger": e.Ledger, "kind": e.Kind, "role": e.Role, "note": e.Note,
		}
		if !e.ClosedAt.IsZero() {
			entry["time"] = e.ClosedAt.UTC().Format("2006-01-02 15:04")
		}
		if e.Amount != nil {
			entry["amount"] = e.Amount.String()
			switch {
			case debits[e.Kind+"/"+e.Role]:
				entry["direction"] = "-"
			case credits[e.Kind+"/"+e.Role]:
				entry["direction"] = "+"
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// verify reconciles the statement against the public on-chain record:
// derived PVK vs registered viewing key, and Commit(v, r) vs
// C_spend / C_receive (SDK.md §10.6).
func verify(st *ct.Statement, raw string) map[string]any {
	var oc onChain
	if err := json.Unmarshal([]byte(raw), &oc); err != nil {
		return map[string]any{"available": false, "detail": "malformed on-chain record"}
	}
	point := func(b64 string) (*ct.Point, error) {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, err
		}
		return ct.PointFromBytes(b)
	}
	pvk, err1 := point(oc.ViewingKey)
	spend, err2 := point(oc.Spendable)
	receive, err3 := point(oc.Receiving)
	if err1 != nil || err2 != nil || err3 != nil {
		return map[string]any{"available": false, "detail": "malformed on-chain points"}
	}

	verdict := func(ok bool) string {
		if ok {
			return "match"
		}
		return "mismatch"
	}
	out := map[string]any{
		"available":     true,
		"latest_ledger": oc.LatestLedger,
		"pvk":           verdict(keys.PVK.Equal(pvk)),
	}
	if st.Spendable != nil {
		out["spend"] = verdict(ct.Commit(st.Spendable, st.SpendR).Equal(spend))
	}
	if st.Pending != nil {
		out["receive"] = verdict(ct.Commit(st.Pending, st.ReceiveR).Equal(receive))
	}
	return out
}
