// Package ct implements the client-side cryptography of OpenZeppelin's
// Confidential Token extension for Stellar: the Poseidon2 t=4 sponge,
// Grumpkin curve arithmetic, the key-derivation hierarchy, and the
// per-event decryption formulas. Umbra's server serves ciphertexts
// verbatim; everything in this package runs on the client, next to the
// keys.
//
// The authoritative spec is the confidential-token branch of
// openzeppelin/stellar-contracts (docs/SDK.md, docs/DESIGN.md, and
// circuits/lib/src/lib.nr). Every primitive here is pinned to the
// cross-language vectors that repo commits under circuits/lib/testdata,
// mirrored in testdata/ — a port is correct iff it reproduces them
// byte for byte.
//
// Like internal/poseidon2 (the SPP t=2 compression), this is math/big
// on purpose: zero extra dependencies, trivially auditable against the
// Rust and Noir sources, and fast enough for statements of a few
// hundred events.
package ct

import "math/big"

// FrModulus is the BN254 scalar field prime r: the Poseidon2 field and
// Grumpkin's coordinate field.
var FrModulus, _ = new(big.Int).SetString(
	"30644e72e131a029b85045b68181585d2833e84879b9709143e1f593f0000001", 16)

// FqModulus is the BN254 base field prime q: Grumpkin's group order,
// i.e. the modulus for scalars and blinding accumulation (SDK.md §4.6).
var FqModulus, _ = new(big.Int).SetString(
	"30644e72e131a029b85045b68181585d97816a916871ca8d3c208c16d87cfd47", 16)

// Domain separation tags (DESIGN.md §2.6) — always the first absorbed
// sponge element, via withDomain.
const (
	domainAddress          = 1
	domainViewingKey       = 2
	domainDelegationVK     = 3
	domainSpendRandomness  = 4
	domainTransferBlinding = 5
	domainTransferAmount   = 6
	domainEncryptedBalance = 7
	domainEncryptedAllow   = 8
	domainAllowRandomness  = 9
	domainEscrowedDVK      = 10
	domainAuditorSender    = 11
	domainAuditorRecipient = 12
	domainECDH             = 13
)

var (
	ext1t4 [4][4]*big.Int
	intT4  [56]*big.Int
	ext2t4 [4][4]*big.Int
	diagT4 [4]*big.Int
)

func init() {
	mustHex := func(s string) *big.Int {
		n, ok := new(big.Int).SetString(s, 16)
		if !ok {
			panic("ct: bad embedded constant " + s)
		}
		return n
	}
	for i := range rcExternal1 {
		for j := range rcExternal1[i] {
			ext1t4[i][j] = mustHex(rcExternal1[i][j])
		}
	}
	for i := range rcInternal {
		intT4[i] = mustHex(rcInternal[i])
	}
	for i := range rcExternal2 {
		for j := range rcExternal2[i] {
			ext2t4[i][j] = mustHex(rcExternal2[i][j])
		}
	}
	for i := range matInternalDiag {
		diagT4[i] = mustHex(matInternalDiag[i])
	}
}

// permute runs the Poseidon2 t=4 permutation in place: initial external
// linear layer, 4 external rounds, 56 internal rounds, 4 external
// rounds — the exact algorithm behind Noir's poseidon2_permutation
// builtin (Barretenberg parameters, see constants.go).
func permute(s *[4]*big.Int) {
	matmul4(s)
	for r := 0; r < 4; r++ {
		for i := 0; i < 4; i++ {
			addModFr(s[i], ext1t4[r][i])
			sbox5Fr(s[i])
		}
		matmul4(s)
	}
	for r := 0; r < 56; r++ {
		addModFr(s[0], intT4[r])
		sbox5Fr(s[0])
		matmulInternal4(s)
	}
	for r := 0; r < 4; r++ {
		for i := 0; i < 4; i++ {
			addModFr(s[i], ext2t4[r][i])
			sbox5Fr(s[i])
		}
		matmul4(s)
	}
}

// matmul4 applies the external 4x4 matrix circ(5,7,1,3) in place, using
// Barretenberg's addition chain.
func matmul4(s *[4]*big.Int) {
	t0 := new(big.Int).Add(s[0], s[1])                  // A+B
	t1 := new(big.Int).Add(s[2], s[3])                  // C+D
	t2 := new(big.Int).Lsh(s[1], 1)                     // 2B
	t2.Add(t2, t1)                                      // 2B+C+D
	t3 := new(big.Int).Lsh(s[3], 1)                     // 2D
	t3.Add(t3, t0)                                      // 2D+A+B
	t4 := new(big.Int).Lsh(t1, 2)                       // 4C+4D
	t4.Add(t4, t3)                                      // A+B+4C+6D
	t5 := new(big.Int).Lsh(t0, 2)                       // 4A+4B
	t5.Add(t5, t2)                                      // 4A+6B+C+D
	s[0].Add(t3, t5).Mod(s[0], FrModulus)               // 5A+7B+C+3D
	s[1].Set(t5.Mod(t5, FrModulus))                     // 4A+6B+C+D
	s[2].Add(t2, t4).Mod(s[2], FrModulus)               // A+3B+5C+7D
	s[3].Set(t4.Mod(t4, FrModulus))                     // A+B+4C+6D
}

// matmulInternal4 applies the internal linear layer in place:
// s[i] = diag[i]*s[i] + sum(s).
func matmulInternal4(s *[4]*big.Int) {
	sum := new(big.Int).Add(s[0], s[1])
	sum.Add(sum, s[2]).Add(sum, s[3])
	for i := 0; i < 4; i++ {
		s[i].Mul(s[i], diagT4[i]).Add(s[i], sum).Mod(s[i], FrModulus)
	}
}

func addModFr(s, c *big.Int) {
	s.Add(s, c).Mod(s, FrModulus)
}

func sbox5Fr(s *big.Int) {
	sq := new(big.Int).Mul(s, s)
	sq.Mod(sq, FrModulus)
	q := new(big.Int).Mul(sq, sq)
	q.Mod(q, FrModulus)
	s.Mul(s, q).Mod(s, FrModulus)
}

// sponge absorbs inputs at rate 3 with IV = len(inputs)*2^64 in the
// capacity lane, and squeezes state[0] (lib.nr `sponge`). The trailing
// permutation always runs — also for the empty input.
func sponge(inputs []*big.Int) *big.Int {
	iv := new(big.Int).Lsh(big.NewInt(int64(len(inputs))), 64)
	state := [4]*big.Int{big.NewInt(0), big.NewInt(0), big.NewInt(0), iv}
	i := 0
	for {
		n := len(inputs) - i
		if n > 3 {
			n = 3
		}
		for j := 0; j < n; j++ {
			addModFr(state[j], inputs[i+j])
		}
		i += n
		permute(&state)
		if i >= len(inputs) {
			return state[0]
		}
	}
}

// withDomain is the single Poseidon entry point of the protocol:
// sponge([d] ++ inputs), the domain tag always absorbed first.
func withDomain(d uint64, inputs ...*big.Int) *big.Int {
	all := make([]*big.Int, 0, len(inputs)+1)
	all = append(all, new(big.Int).SetUint64(d))
	all = append(all, inputs...)
	return sponge(all)
}

// spongeSqueeze2 is the two-lane auditor sponge: one permutation of
// [d, s, sigma, 3*2^64], returning (state[0], state[1]) — lane 0 masks
// the amount, lane 1 the balance / transfer randomness (DESIGN.md §2.5).
func spongeSqueeze2(d uint64, s, sigma *big.Int) (*big.Int, *big.Int) {
	state := [4]*big.Int{
		new(big.Int).SetUint64(d),
		new(big.Int).Set(s),
		new(big.Int).Set(sigma),
		new(big.Int).Lsh(big.NewInt(3), 64),
	}
	permute(&state)
	return state[0], state[1]
}
