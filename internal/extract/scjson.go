package extract

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TopicsJSON renders the event's topics as a JSON array.
func (e *Event) TopicsJSON() ([]byte, error) {
	arr := make([]any, len(e.Topics))
	for i, t := range e.Topics {
		arr[i] = safeScValToAny(t)
	}
	return json.Marshal(arr)
}

// DataJSON renders the event's data payload as JSON.
func (e *Event) DataJSON() ([]byte, error) {
	return json.Marshal(safeScValToAny(e.Data))
}

// safeScValToAny is the panic boundary around the render: the SDK's
// generated accessors (even the checked Get* forms) dereference their
// arm pointers unchecked, so a structurally invalid ScVal panics deep
// inside xdr. Malformed data must degrade to a fallback render — the raw
// XDR is persisted regardless — never kill the ingestion loop.
func safeScValToAny(v xdr.ScVal) (out any) {
	defer func() {
		if r := recover(); r != nil {
			out = map[string]string{"render_error": fmt.Sprintf("%v", r)}
		}
	}()
	return scValToAny(v)
}

// scValToAny renders an ScVal into JSON-friendly Go values. Lossy by
// design (the raw XDR is stored alongside): bytes → base64, big ints →
// decimal strings, U256 → 0x hex (field elements read naturally). It
// only uses the checked getters — a malformed or zero-valued ScVal must
// degrade to the XDR fallback, never panic the pipeline.
func scValToAny(v xdr.ScVal) any {
	switch v.Type {
	case xdr.ScValTypeScvBool:
		if b, ok := v.GetB(); ok {
			return b
		}
	case xdr.ScValTypeScvVoid:
		return nil
	case xdr.ScValTypeScvU32:
		if n, ok := v.GetU32(); ok {
			return uint64(n)
		}
	case xdr.ScValTypeScvI32:
		if n, ok := v.GetI32(); ok {
			return int64(n)
		}
	case xdr.ScValTypeScvU64:
		if n, ok := v.GetU64(); ok {
			return fmt.Sprintf("%d", n)
		}
	case xdr.ScValTypeScvI64:
		if n, ok := v.GetI64(); ok {
			return fmt.Sprintf("%d", n)
		}
	case xdr.ScValTypeScvTimepoint:
		if n, ok := v.GetTimepoint(); ok {
			return fmt.Sprintf("%d", n)
		}
	case xdr.ScValTypeScvDuration:
		if n, ok := v.GetDuration(); ok {
			return fmt.Sprintf("%d", n)
		}
	case xdr.ScValTypeScvU128:
		if p, ok := v.GetU128(); ok {
			return u128String(p)
		}
	case xdr.ScValTypeScvI128:
		if p, ok := v.GetI128(); ok {
			return i128String(p)
		}
	case xdr.ScValTypeScvU256:
		if p, ok := v.GetU256(); ok {
			return U256Hex(p)
		}
	case xdr.ScValTypeScvI256:
		if p, ok := v.GetI256(); ok {
			return i256String(p)
		}
	case xdr.ScValTypeScvBytes:
		if b, ok := v.GetBytes(); ok {
			return base64.StdEncoding.EncodeToString(b)
		}
	case xdr.ScValTypeScvString:
		if s, ok := v.GetStr(); ok {
			return string(s)
		}
	case xdr.ScValTypeScvSymbol:
		if s, ok := v.GetSym(); ok {
			return string(s)
		}
	case xdr.ScValTypeScvVec:
		if vec, ok := v.GetVec(); ok && vec != nil {
			arr := make([]any, len(*vec))
			for i, item := range *vec {
				arr[i] = scValToAny(item)
			}
			return arr
		}
	case xdr.ScValTypeScvMap:
		if m, ok := v.GetMap(); ok && m != nil {
			out := make(map[string]any, len(*m))
			for _, entry := range *m {
				key := ""
				if sym, ok := entry.Key.GetSym(); ok {
					key = string(sym)
				} else {
					key = fmt.Sprintf("%v", scValToAny(entry.Key))
				}
				out[key] = scValToAny(entry.Val)
			}
			return out
		}
	case xdr.ScValTypeScvAddress:
		if a, ok := v.GetAddress(); ok {
			if s, err := AddressString(a); err == nil {
				return s
			}
		}
	}
	// Fallback: raw XDR of the value.
	if raw, err := v.MarshalBinary(); err == nil {
		return map[string]string{"xdr": base64.StdEncoding.EncodeToString(raw)}
	}
	return map[string]string{"xdr": ""}
}

// AddressString renders an ScAddress as its strkey.
func AddressString(a xdr.ScAddress) (string, error) {
	return a.String()
}

// U256Hex renders U256 parts as a 0x-prefixed hex string (no padding) —
// the natural shape for BN254 field elements like commitments and
// nullifiers.
func U256Hex(p xdr.UInt256Parts) string {
	n := u256Big(p)
	return "0x" + n.Text(16)
}

func u256Big(p xdr.UInt256Parts) *big.Int {
	n := new(big.Int).SetUint64(uint64(p.HiHi))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.HiLo)))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.LoHi)))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.LoLo)))
	return n
}

func u128String(p xdr.UInt128Parts) string {
	n := new(big.Int).SetUint64(uint64(p.Hi))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.Lo)))
	return n.String()
}

func i128String(p xdr.Int128Parts) string {
	n := new(big.Int).SetInt64(int64(p.Hi))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.Lo)))
	return n.String()
}

func i256String(p xdr.Int256Parts) string {
	n := new(big.Int).SetInt64(int64(p.HiHi))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.HiLo)))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.LoHi)))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.LoLo)))
	return n.String()
}
