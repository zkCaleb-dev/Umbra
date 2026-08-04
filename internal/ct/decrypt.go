package ct

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Event payloads reach the client exactly as Umbra's decoder rendered
// the contract's data map: BytesN as std base64, U256 as unpadded 0x
// hex. Field names are the DEPLOYED contract generation's short names;
// the current OpenZeppelin branch has since renamed them, so each field
// accepts both spellings.

// Payload is the ciphertext material of one confidential-token event.
// Only the fields the event kind carries are non-nil.
type Payload struct {
	RE              *Point   // ephemeral public key R_e (64-byte point)
	Sigma           *big.Int // per-operation salt
	SigmaA          *big.Int // delegation salt (pre-transfer) on spender_transfer
	BTilde          *big.Int // masked new spendable balance (checkpoint)
	VTilde          *big.Int // masked transferred amount
	LiveUntilLedger uint32   // set_spender only
}

// payloadAliases maps every accepted JSON key to its canonical field.
var payloadAliases = map[string]string{
	"r_e": "r_e", "r_e_point": "r_e",
	"sigma":   "sigma",
	"sigma_a": "sigma_a",
	"b_tilde": "b_tilde",
	"v_tilde": "v_tilde",
	"live_until_ledger": "live_until_ledger",
	// Auditor-channel fields: recognized so unknown-key detection below
	// stays meaningful, but not decrypted — the statement never has the
	// auditor's secret.
	"b_aud_s": "", "b_tilde_aud_s": "",
	"v_aud_s": "", "v_tilde_aud_s": "",
	"v_aud_r": "", "v_tilde_aud_r": "",
	"r_aud_r": "", "r_tilde_aud_r": "",
	"a_aud_s": "", "a_tilde_aud_s": "",
	"amount": "", "auditor_id": "",
}

// ParsePayload decodes an event payload as served by
// GET /v1/ct/{token}/history/{address}. nil input yields an empty
// payload (merge, deposit).
func ParsePayload(raw json.RawMessage) (*Payload, error) {
	p := &Payload{}
	if len(raw) == 0 || string(raw) == "null" {
		return p, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("payload is not an object: %w", err)
	}
	for key, val := range fields {
		canonical, known := payloadAliases[key]
		if !known {
			return nil, fmt.Errorf("unrecognized payload field %q — deployed contract generation drifted?", key)
		}
		var err error
		switch canonical {
		case "r_e":
			p.RE, err = parsePoint(val)
		case "sigma":
			p.Sigma, err = parseFieldElement(val)
		case "sigma_a":
			p.SigmaA, err = parseFieldElement(val)
		case "b_tilde":
			p.BTilde, err = parseFieldElement(val)
		case "v_tilde":
			p.VTilde, err = parseFieldElement(val)
		case "live_until_ledger":
			err = json.Unmarshal(val, &p.LiveUntilLedger)
		}
		if err != nil {
			return nil, fmt.Errorf("payload field %s: %w", key, err)
		}
	}
	return p, nil
}

// parseBytes accepts the two renderings of on-chain byte values: std
// base64 (BytesN) and unpadded 0x hex (U256), returning exactly want
// bytes big-endian.
func parseBytes(raw json.RawMessage, want int) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("expected a string value: %w", err)
	}
	if strings.HasPrefix(s, "0x") {
		n, ok := new(big.Int).SetString(s[2:], 16)
		if !ok {
			return nil, fmt.Errorf("bad hex %q", s)
		}
		if n.BitLen() > want*8 {
			return nil, fmt.Errorf("value exceeds %d bytes", want)
		}
		return n.FillBytes(make([]byte, want)), nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bad base64: %w", err)
	}
	if len(b) != want {
		return nil, fmt.Errorf("expected %d bytes, got %d", want, len(b))
	}
	return b, nil
}

// parseFieldElement reads a canonical Fr element (32 bytes big-endian,
// value < r — SDK.md §4.2 says reject non-canonical at the boundary).
func parseFieldElement(raw json.RawMessage) (*big.Int, error) {
	b, err := parseBytes(raw, 32)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(b)
	if n.Cmp(FrModulus) >= 0 {
		return nil, fmt.Errorf("non-canonical field element")
	}
	return n, nil
}

// parsePoint reads a 64-byte be(x)‖be(y) Grumpkin point and insists it
// is on the curve. (The all-zero identity encoding never appears in
// event ephemerals; treat it as invalid here.)
func parsePoint(raw json.RawMessage) (*Point, error) {
	b, err := parseBytes(raw, 64)
	if err != nil {
		return nil, err
	}
	p := &Point{X: new(big.Int).SetBytes(b[:32]), Y: new(big.Int).SetBytes(b[32:])}
	if !onCurve(p) {
		return nil, fmt.Errorf("point not on Grumpkin")
	}
	return p, nil
}

// ErrNotReadable flags a decryption whose plaintext is implausibly
// large. Masks are uniform in [0, r), so decrypting with the wrong key
// yields ~254-bit noise; genuine values are bounded by total supply
// (< 2^127). The false-accept probability is ~2^-127.
var ErrNotReadable = errors.New("ciphertext does not decrypt to a plausible value under this key")

var plausibleBound = new(big.Int).Lsh(big.NewInt(1), 127)

func plausible(v *big.Int) (*big.Int, error) {
	if v.Cmp(plausibleBound) >= 0 {
		return nil, ErrNotReadable
	}
	return v, nil
}

// SharedSecret is the recipient side of the ephemeral ECDH:
// s = poseidon(δ_ecdh, (vk·R_e).x, (vk·R_e).y).
func SharedSecret(vk *big.Int, rE *Point) (*big.Int, error) {
	return ecdh(vk, rE)
}

// DecryptBalance opens a checkpoint: v = b̃ − poseidon(δ_bal, vk, σ) mod r.
func DecryptBalance(vk, sigma, bTilde *big.Int) (*big.Int, error) {
	v := new(big.Int).Sub(bTilde, withDomain(domainEncryptedBalance, vk, sigma))
	return plausible(v.Mod(v, FrModulus))
}

// DecryptAmount opens a transferred amount for its recipient:
// v = ṽ − poseidon(δ_amt, s, σ) mod r. On spender_transfer the salt is
// the event's sigma_a.
func DecryptAmount(s, sigma, vTilde *big.Int) (*big.Int, error) {
	v := new(big.Int).Sub(vTilde, withDomain(domainTransferAmount, s, sigma))
	return plausible(v.Mod(v, FrModulus))
}

// DecryptAllowance opens a delegation allowance:
// v_a = ã − poseidon(δ_allow, dvk, σ_a) mod r.
func DecryptAllowance(dvk, sigmaA, aTilde *big.Int) (*big.Int, error) {
	v := new(big.Int).Sub(aTilde, withDomain(domainEncryptedAllow, dvk, sigmaA))
	return plausible(v.Mod(v, FrModulus))
}

// TransferBlinding derives the recipient-side Pedersen randomness of an
// inbound transfer: r = poseidon(δ_blind, s, σ). Never transmitted.
func TransferBlinding(s, sigma *big.Int) *big.Int {
	return withDomain(domainTransferBlinding, s, sigma)
}

// SpendRandomness derives the checkpoint blinding:
// r = poseidon(δ_spend, vk, σ). Never transmitted.
func SpendRandomness(vk, sigma *big.Int) *big.Int {
	return withDomain(domainSpendRandomness, vk, sigma)
}

// Commit exposes the Pedersen commitment for consistency checks against
// the on-chain C_spend / C_receive points (SDK.md §10.6).
func Commit(v, r *big.Int) *Point { return commit(v, r) }

// Equal reports whether two points are the same group element.
func (p *Point) Equal(q *Point) bool {
	if p.Inf || q.Inf {
		return p.Inf == q.Inf
	}
	return p.X.Cmp(q.X) == 0 && p.Y.Cmp(q.Y) == 0
}

// AddPoints exposes group addition for accumulator reconciliation.
func AddPoints(p, q *Point) *Point { return add(p, q) }

// PointFromBytes decodes a 64-byte be(x)‖be(y) point; 64 zero bytes is
// the identity (SDK.md §4.2).
func PointFromBytes(b []byte) (*Point, error) {
	if len(b) != 64 {
		return nil, fmt.Errorf("expected 64 bytes, got %d", len(b))
	}
	p := &Point{X: new(big.Int).SetBytes(b[:32]), Y: new(big.Int).SetBytes(b[32:])}
	if p.X.Sign() == 0 && p.Y.Sign() == 0 {
		return identity(), nil
	}
	if !onCurve(p) {
		return nil, fmt.Errorf("point not on Grumpkin")
	}
	return p, nil
}
