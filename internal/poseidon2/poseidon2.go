// Package poseidon2 implements the exact Poseidon2 compression the SPP
// pool contract uses for its commitment Merkle tree (BN254 scalar
// field, t=2, d=5, RF=8, RP=56 — see constants.go for provenance).
//
// Faithfully ported from the Horizen Labs reference implementation
// vendored in the SPP repo (poseidon2/src/poseidon2/poseidon2.rs), whose
// semantics Soroban's poseidon2_permutation host function follows:
//
//	state ← M_E · input
//	4 × (add rc; x^5 both; M_E)          external, first half
//	56 × (state0 += rc; state0^5; M_I)   internal
//	4 × (add rc; x^5 both; M_E)          external, second half
//
// with M_E = [[2,1],[1,2]] and M_I = [[2,1],[1,3]] for t=2, and the
// contract's compression step: out[0] + left (mod p).
//
// Implemented over math/big on purpose: zero extra dependencies and
// trivially auditable against the Rust source. Fast enough for
// checkpointing (thousands of hashes); swap to gnark-crypto's fr if a
// pool ever grows past millions of leaves.
package poseidon2

import (
	"fmt"
	"math/big"
)

// Modulus is the BN254 scalar field prime r.
var Modulus, _ = new(big.Int).SetString(
	"30644e72e131a029b85045b68181585d2833e84879b9709143e1f593f0000001", 16)

var (
	ext1  [4][2]*big.Int
	inter [56]*big.Int
	ext2  [4][2]*big.Int
	// Zeroes are the per-level empty-subtree hashes as big ints.
	Zeroes [33]*big.Int
)

func init() {
	mustHex := func(s string) *big.Int {
		n, ok := new(big.Int).SetString(s, 16)
		if !ok {
			panic("poseidon2: bad embedded constant " + s)
		}
		return n
	}
	for i := range rcExternal1 {
		ext1[i][0], ext1[i][1] = mustHex(rcExternal1[i][0]), mustHex(rcExternal1[i][1])
	}
	for i := range rcInternal {
		inter[i] = mustHex(rcInternal[i])
	}
	for i := range rcExternal2 {
		ext2[i][0], ext2[i][1] = mustHex(rcExternal2[i][0]), mustHex(rcExternal2[i][1])
	}
	for i := range ZeroHashes {
		Zeroes[i] = mustHex(ZeroHashes[i])
	}
}

// Compress hashes two field elements into one, exactly as the pool
// contract's poseidon2_compress: permutation output[0] + left, mod p.
// Inputs must be canonical field elements (< Modulus).
func Compress(left, right *big.Int) (*big.Int, error) {
	if left.Sign() < 0 || left.Cmp(Modulus) >= 0 || right.Sign() < 0 || right.Cmp(Modulus) >= 0 {
		return nil, fmt.Errorf("poseidon2 inputs must be in [0, p)")
	}
	s0 := new(big.Int).Set(left)
	s1 := new(big.Int).Set(right)

	// Initial external linear layer.
	matmulExternal(s0, s1)

	for r := 0; r < 4; r++ {
		addMod(s0, ext1[r][0])
		addMod(s1, ext1[r][1])
		sbox5(s0)
		sbox5(s1)
		matmulExternal(s0, s1)
	}
	for r := 0; r < 56; r++ {
		addMod(s0, inter[r])
		sbox5(s0)
		matmulInternal(s0, s1)
	}
	for r := 0; r < 4; r++ {
		addMod(s0, ext2[r][0])
		addMod(s1, ext2[r][1])
		sbox5(s0)
		sbox5(s1)
		matmulExternal(s0, s1)
	}

	// Compression: truncate to out[0] and add the left input.
	out := s0.Add(s0, left)
	out.Mod(out, Modulus)
	return out, nil
}

// matmulExternal applies M_E = circ(2,1) in place:
// s0' = 2*s0 + s1, s1' = s0 + 2*s1.
func matmulExternal(s0, s1 *big.Int) {
	sum := new(big.Int).Add(s0, s1)
	s0.Add(s0, sum).Mod(s0, Modulus)
	s1.Add(s1, sum).Mod(s1, Modulus)
}

// matmulInternal applies M_I = [[2,1],[1,3]] in place:
// s0' = 2*s0 + s1, s1' = s0 + 3*s1.
func matmulInternal(s0, s1 *big.Int) {
	sum := new(big.Int).Add(s0, s1)
	s0.Add(s0, sum).Mod(s0, Modulus)
	s1.Lsh(s1, 1).Add(s1, sum).Mod(s1, Modulus)
}

func addMod(s, c *big.Int) {
	s.Add(s, c).Mod(s, Modulus)
}

func sbox5(s *big.Int) {
	sq := new(big.Int).Mul(s, s)
	sq.Mod(sq, Modulus)
	q := new(big.Int).Mul(sq, sq)
	q.Mod(q, Modulus)
	s.Mul(s, q).Mod(s, Modulus)
}
