package decode

import (
	"encoding/json"
	"testing"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/extract"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const ctID = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA"

func bytesN(t *testing.T, n int, fill byte) xdr.ScVal {
	t.Helper()
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return bytes(b)
}

func ctKindsMap() map[string]config.ContractKind {
	return map[string]config.ContractKind{ctID: config.KindConfidentialToken}
}

// A confidential Transfer: [transfer, from, to] topics, ciphertexts in
// the data map, NO public amount.
func TestDeriveCTTransfer(t *testing.T) {
	ev := extract.Event{
		ID: "5-1-0", Ledger: 50, TxHash: "aa", ContractID: ctID, Name: "transfer",
		Topics: []xdr.ScVal{sym("transfer"), addr(t, 1), addr(t, 2)},
		Data: scmap(map[string]xdr.ScVal{
			"r_e_point": bytesN(t, 64, 0xAB),
			"v_tilde":   bytesN(t, 32, 0xCD),
			"sigma":     bytesN(t, 32, 0xEF),
		}),
	}
	d := Derive(ctKindsMap(), []extract.Event{ev})
	if len(d.CTEvents) != 1 {
		t.Fatalf("expected 1 ct event, got %d", len(d.CTEvents))
	}
	c := d.CTEvents[0]
	if c.Kind != "transfer" || len(c.Addresses) != 2 || c.AmountPublic != nil {
		t.Fatalf("unexpected: %+v", c)
	}
	var payload map[string]any
	if err := json.Unmarshal(c.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["v_tilde"]; !ok {
		t.Fatalf("payload lost ciphertexts: %v", payload)
	}
}

// A Deposit carries a PUBLIC i128 amount (tokens entering the wrapper).
func TestDeriveCTDepositPublicAmount(t *testing.T) {
	ev := extract.Event{
		ID: "5-1-1", Ledger: 51, TxHash: "bb", ContractID: ctID, Name: "deposit",
		Topics: []xdr.ScVal{sym("deposit"), addr(t, 1), addr(t, 2)},
		Data:   scmap(map[string]xdr.ScVal{"amount": i128(5000)}),
	}
	d := Derive(ctKindsMap(), []extract.Event{ev})
	if len(d.CTEvents) != 1 {
		t.Fatal("expected 1 ct event")
	}
	if d.CTEvents[0].AmountPublic == nil || *d.CTEvents[0].AmountPublic != "5000" {
		t.Fatalf("expected public amount 5000, got %+v", d.CTEvents[0].AmountPublic)
	}
}

// SpenderTransfer names three addresses — all must be queryable.
func TestDeriveCTSpenderTransferThreeAddresses(t *testing.T) {
	ev := extract.Event{
		ID: "5-1-2", Ledger: 52, TxHash: "cc", ContractID: ctID, Name: "spender_transfer",
		Topics: []xdr.ScVal{sym("spender_transfer"), addr(t, 1), addr(t, 2), addr(t, 3)},
		Data:   scmap(map[string]xdr.ScVal{"v_tilde": bytesN(t, 32, 1)}),
	}
	d := Derive(ctKindsMap(), []extract.Event{ev})
	if len(d.CTEvents) != 1 || len(d.CTEvents[0].Addresses) != 3 {
		t.Fatalf("expected 3 addresses, got %+v", d.CTEvents)
	}
}

// Unknown kinds stay raw-only, never fatal.
func TestDeriveCTUnknownKindIgnored(t *testing.T) {
	ev := extract.Event{
		ID: "5-1-3", Ledger: 53, ContractID: ctID, Name: "future_event",
		Topics: []xdr.ScVal{sym("future_event"), addr(t, 1)},
	}
	d := Derive(ctKindsMap(), []extract.Event{ev})
	if len(d.CTEvents) != 0 {
		t.Fatalf("unknown kind must not derive: %+v", d.CTEvents)
	}
}
