package decode

import (
	"testing"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/extract"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const tokenID = "CCUUDM434BMZMYWYDITHFXHDMIVTGGD6T2I5UKNX5BSLXLW7HVR4MCGZ"

func addr(t *testing.T, seed byte) xdr.ScVal {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = seed
	acc, err := strkey.Encode(strkey.VersionByteAccountID, raw)
	if err != nil {
		t.Fatal(err)
	}
	var accountID xdr.AccountId
	if err := accountID.SetAddress(acc); err != nil {
		t.Fatal(err)
	}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &accountID}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func i128(lo uint64) xdr.ScVal {
	v := xdr.Int128Parts{Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &v}
}

func str(s string) xdr.ScVal {
	v := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}
}

func TestDeriveTokenTransferClassicShape(t *testing.T) {
	// SAC shape: [transfer, from, to, sep0011-asset], data = i128.
	ev := extract.Event{
		ID: "9-0-0", Ledger: 9, ContractID: tokenID, Name: "transfer",
		Topics: []xdr.ScVal{sym("transfer"), addr(t, 1), addr(t, 2), str("EURC:GB3Q...")},
		Data:   i128(1234567),
	}
	kinds := map[string]config.ContractKind{tokenID: config.KindToken}
	d := Derive(kinds, []extract.Event{ev})
	if len(d.Transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(d.Transfers))
	}
	tr := d.Transfers[0]
	if tr.Amount != "1234567" || tr.TokenID != tokenID || tr.From == tr.To {
		t.Fatalf("unexpected transfer: %+v", tr)
	}
}

func TestDeriveTokenTransferMuxedShape(t *testing.T) {
	// Muxed extension: data = map{amount: i128, to_muxed_id: ...}.
	ev := extract.Event{
		ID: "9-0-1", Ledger: 9, ContractID: tokenID, Name: "transfer",
		Topics: []xdr.ScVal{sym("transfer"), addr(t, 1), addr(t, 2)},
		Data:   scmap(map[string]xdr.ScVal{"amount": i128(42), "to_muxed_id": u32(7)}),
	}
	kinds := map[string]config.ContractKind{tokenID: config.KindToken}
	d := Derive(kinds, []extract.Event{ev})
	if len(d.Transfers) != 1 || d.Transfers[0].Amount != "42" {
		t.Fatalf("unexpected: %+v", d.Transfers)
	}
}

func TestDeriveTokenIgnoresNonTransferEvents(t *testing.T) {
	ev := extract.Event{
		ID: "9-0-2", Ledger: 9, ContractID: tokenID, Name: "mint",
		Topics: []xdr.ScVal{sym("mint"), addr(t, 1)},
		Data:   i128(1),
	}
	kinds := map[string]config.ContractKind{tokenID: config.KindToken}
	d := Derive(kinds, []extract.Event{ev})
	if len(d.Transfers) != 0 {
		t.Fatalf("mint must not derive a transfer: %+v", d.Transfers)
	}
}
