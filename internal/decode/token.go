package decode

import (
	"fmt"

	"github.com/zkCaleb-dev/umbra/internal/extract"
	"github.com/zkCaleb-dev/umbra/internal/store"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// nameTransfer is the SEP-41/SAC transfer event (CAP-46-6): topics =
// [Symbol(transfer), from: Address, to: Address, (SAC adds a 4th
// SEP-0011 asset topic)], data = amount i128 — or, since the muxed
// accounts extension, a map carrying an "amount" entry.
const nameTransfer = "transfer"

func deriveToken(ev *extract.Event, d *store.Derived) {
	if ev.Name != nameTransfer {
		// mint/burn/clawback/approve land raw only for now.
		return
	}
	t, err := parseTransfer(ev)
	if err != nil {
		skip(ev, err)
		return
	}
	d.Transfers = append(d.Transfers, t)
}

func parseTransfer(ev *extract.Event) (store.TokenTransfer, error) {
	if len(ev.Topics) < 3 {
		return store.TokenTransfer{}, fmt.Errorf("expected at least 3 topics, got %d", len(ev.Topics))
	}
	from, err := topicAddress(ev.Topics[1])
	if err != nil {
		return store.TokenTransfer{}, fmt.Errorf("from: %w", err)
	}
	to, err := topicAddress(ev.Topics[2])
	if err != nil {
		return store.TokenTransfer{}, fmt.Errorf("to: %w", err)
	}
	amount, err := transferAmount(ev.Data)
	if err != nil {
		return store.TokenTransfer{}, err
	}
	return store.TokenTransfer{
		EventID: ev.ID,
		TokenID: ev.ContractID,
		Ledger:  ev.Ledger,
		TxHash:  ev.TxHash,
		From:    from,
		To:      to,
		Amount:  amount,
	}, nil
}

func topicAddress(v xdr.ScVal) (string, error) {
	addr, ok := v.GetAddress()
	if !ok {
		return "", fmt.Errorf("topic is not an address")
	}
	return extract.AddressString(addr)
}

// transferAmount accepts the classic shape (data = i128) and the muxed
// extension (data = map with an "amount" i128 entry).
func transferAmount(data xdr.ScVal) (string, error) {
	if p, ok := data.GetI128(); ok {
		return extract.I128String(p), nil
	}
	if m, ok := data.GetMap(); ok && m != nil {
		for _, entry := range *m {
			if sym, ok := entry.Key.GetSym(); ok && string(sym) == "amount" {
				if p, ok := entry.Val.GetI128(); ok {
					return extract.I128String(p), nil
				}
			}
		}
	}
	return "", fmt.Errorf("transfer data carries no i128 amount")
}
