package decode

import (
	"testing"

	"github.com/Trustless-Work/umbra/internal/config"
	"github.com/Trustless-Work/umbra/internal/extract"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	poolID     = "CCG3ICXNCYWQIRUMUQEJZZIIF2DTXIY63UMVDJT2EJM7VZPE45W2XFLU"
	registryID = "CDMGLGZV2S4HW4WKW7ZAYICT73V57QNCVJ5K6A22DVPPJHIQPHFLSGRL"
)

func sym(s string) xdr.ScVal {
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
}

func u256(lo uint64) xdr.ScVal {
	v := xdr.UInt256Parts{LoLo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &v}
}

func u32(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

func bytes(b []byte) xdr.ScVal {
	v := xdr.ScBytes(b)
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &v}
}

func scmap(pairs map[string]xdr.ScVal) xdr.ScVal {
	m := xdr.ScMap{}
	for k, v := range pairs {
		m = append(m, xdr.ScMapEntry{Key: sym(k), Val: v})
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func kinds() map[string]config.ContractKind {
	return map[string]config.ContractKind{
		poolID:     config.KindSPPPool,
		registryID: config.KindSPPRegistry,
	}
}

func TestDeriveNewCommitment(t *testing.T) {
	ev := extract.Event{
		ID: "1-0-0", Ledger: 42, ContractID: poolID, Name: "new_commitment_event",
		Topics: []xdr.ScVal{sym("new_commitment_event"), u256(7)},
		Data: scmap(map[string]xdr.ScVal{
			"index":            u32(3),
			"encrypted_output": bytes([]byte{0xAA, 0xBB}),
		}),
	}
	d := Derive(kinds(), []extract.Event{ev})
	if len(d.Leaves) != 1 {
		t.Fatalf("expected 1 leaf, got %d", len(d.Leaves))
	}
	leaf := d.Leaves[0]
	if leaf.PoolID != poolID || leaf.LeafIndex != 3 || leaf.Commitment != "0x7" ||
		leaf.Ledger != 42 || len(leaf.EncryptedOutput) != 2 {
		t.Fatalf("unexpected leaf: %+v", leaf)
	}
}

func TestDeriveNewNullifier(t *testing.T) {
	ev := extract.Event{
		ID: "1-0-1", Ledger: 43, ContractID: poolID, Name: "new_nullifier_event",
		Topics: []xdr.ScVal{sym("new_nullifier_event"), u256(255)},
	}
	d := Derive(kinds(), []extract.Event{ev})
	if len(d.Nullifiers) != 1 {
		t.Fatalf("expected 1 nullifier, got %d", len(d.Nullifiers))
	}
	if d.Nullifiers[0].Nullifier != "0xff" || d.Nullifiers[0].Ledger != 43 {
		t.Fatalf("unexpected nullifier: %+v", d.Nullifiers[0])
	}
}

func TestDeriveMalformedEventIsSkippedNotFatal(t *testing.T) {
	// A commitment event missing its data map must be skipped (raw copy is
	// persisted anyway), never panic or abort the ledger.
	ev := extract.Event{
		ID: "1-0-2", Ledger: 44, ContractID: poolID, Name: "new_commitment_event",
		Topics: []xdr.ScVal{sym("new_commitment_event"), u256(1)},
		// Data left as zero value (void, not a map).
	}
	d := Derive(kinds(), []extract.Event{ev})
	if len(d.Leaves) != 0 || len(d.Nullifiers) != 0 {
		t.Fatalf("malformed event must derive nothing, got %+v", d)
	}
}

func TestDeriveIgnoresUnwatchedKind(t *testing.T) {
	// A pool-shaped event from a contract configured as raw derives nothing.
	rawKinds := map[string]config.ContractKind{poolID: config.KindRaw}
	ev := extract.Event{
		ID: "1-0-3", Ledger: 45, ContractID: poolID, Name: "new_nullifier_event",
		Topics: []xdr.ScVal{sym("new_nullifier_event"), u256(9)},
	}
	d := Derive(rawKinds, []extract.Event{ev})
	if len(d.Nullifiers) != 0 {
		t.Fatalf("raw kind must not derive, got %+v", d)
	}
}
