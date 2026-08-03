package extract

import (
	"encoding/json"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestU256Hex(t *testing.T) {
	cases := []struct {
		parts xdr.UInt256Parts
		want  string
	}{
		{xdr.UInt256Parts{}, "0x0"},
		{xdr.UInt256Parts{LoLo: 255}, "0xff"},
		{xdr.UInt256Parts{HiHi: 1}, "0x1000000000000000000000000000000000000000000000000"}, // 1 << 192
		{xdr.UInt256Parts{LoHi: 1, LoLo: 0}, "0x10000000000000000"},
	}
	for _, c := range cases {
		if got := U256Hex(c.parts); got != c.want {
			t.Errorf("U256Hex(%+v) = %s, want %s", c.parts, got, c.want)
		}
	}
}

func TestTopicsJSONRendersSymbolsAndU256(t *testing.T) {
	symv := xdr.ScSymbol("new_nullifier_event")
	u := xdr.UInt256Parts{LoLo: 16}
	e := Event{Topics: []xdr.ScVal{
		{Type: xdr.ScValTypeScvSymbol, Sym: &symv},
		{Type: xdr.ScValTypeScvU256, U256: &u},
	}}
	raw, err := e.TopicsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if arr[0] != "new_nullifier_event" || arr[1] != "0x10" {
		t.Fatalf("unexpected render: %v", arr)
	}
}

func TestDataJSONFallbackNeverPanics(t *testing.T) {
	// The zero ScVal (bool type with a nil pointer) is malformed. It must
	// degrade to the XDR fallback — never panic or error the pipeline.
	e := Event{}
	raw, err := e.DataJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("expected some render for malformed data")
	}
}
