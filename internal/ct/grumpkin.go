package ct

import (
	"fmt"
	"math/big"
)

// Grumpkin is y² = x³ − 17 over Fr (BN254's scalar field), forming a
// 2-cycle with BN254: coordinates live in Fr, scalars in Fq (DESIGN.md
// §2.2). Affine arithmetic over math/big is plenty for a statement's
// worth of operations.

// Point is an affine Grumpkin point. The identity is Inf=true (encoded
// on the wire as 64 zero bytes).
type Point struct {
	X, Y *big.Int
	Inf  bool
}

// Generators from the protocol's fixed derivation
// (derive_generators("DEFAULT_DOMAIN_SEPARATOR"), lib.nr): G commits
// values, H commits randomness and carries every public key.
var (
	genG = mustPoint(
		"083e7911d835097629f0067531fc15cafd79a89beecb39903f69572c636f4a5a",
		"1a7f5efaad7f315c25a918f30cc8d7333fccab7ad7c90f14de81bcc528f9935d")
	genH = mustPoint(
		"054aa86a73cb8a34525e5bbed6e43ba1198e860f5f3950268f71df4591bde402",
		"209dcfbf2cfb57f9f6046f44d71ac6faf87254afc7407c04eb621a6287cac126")
)

func mustPoint(xHex, yHex string) *Point {
	x, okX := new(big.Int).SetString(xHex, 16)
	y, okY := new(big.Int).SetString(yHex, 16)
	if !okX || !okY {
		panic("ct: bad embedded generator")
	}
	return &Point{X: x, Y: y}
}

// identity returns the group identity.
func identity() *Point { return &Point{Inf: true} }

// onCurve reports whether p satisfies y² = x³ − 17 with canonical
// coordinates. The identity is not on the affine curve.
func onCurve(p *Point) bool {
	if p.Inf {
		return false
	}
	if p.X.Sign() < 0 || p.X.Cmp(FrModulus) >= 0 || p.Y.Sign() < 0 || p.Y.Cmp(FrModulus) >= 0 {
		return false
	}
	y2 := new(big.Int).Mul(p.Y, p.Y)
	y2.Mod(y2, FrModulus)
	x3 := new(big.Int).Mul(p.X, p.X)
	x3.Mod(x3, FrModulus)
	x3.Mul(x3, p.X)
	x3.Sub(x3, big.NewInt(17))
	x3.Mod(x3, FrModulus)
	return y2.Cmp(x3) == 0
}

// neg returns −p.
func neg(p *Point) *Point {
	if p.Inf {
		return identity()
	}
	y := new(big.Int).Neg(p.Y)
	y.Mod(y, FrModulus)
	return &Point{X: new(big.Int).Set(p.X), Y: y}
}

// add returns p + q with full identity/doubling/inverse handling.
func add(p, q *Point) *Point {
	if p.Inf {
		return clonePoint(q)
	}
	if q.Inf {
		return clonePoint(p)
	}
	if p.X.Cmp(q.X) == 0 {
		if p.Y.Cmp(q.Y) != 0 {
			return identity() // q == −p
		}
		return double(p)
	}
	// λ = (y2 − y1) / (x2 − x1)
	num := new(big.Int).Sub(q.Y, p.Y)
	den := new(big.Int).Sub(q.X, p.X)
	den.Mod(den, FrModulus)
	den.ModInverse(den, FrModulus)
	lambda := num.Mul(num, den)
	lambda.Mod(lambda, FrModulus)
	return chord(p, q, lambda)
}

// double returns 2p.
func double(p *Point) *Point {
	if p.Inf || p.Y.Sign() == 0 {
		return identity()
	}
	// λ = 3x² / 2y (curve has a = 0)
	num := new(big.Int).Mul(p.X, p.X)
	num.Mod(num, FrModulus)
	num.Mul(num, big.NewInt(3))
	den := new(big.Int).Lsh(p.Y, 1)
	den.Mod(den, FrModulus)
	den.ModInverse(den, FrModulus)
	lambda := num.Mul(num, den)
	lambda.Mod(lambda, FrModulus)
	return chord(p, p, lambda)
}

// chord finishes an addition given λ: x3 = λ² − x1 − x2, y3 = λ(x1 − x3) − y1.
func chord(p, q *Point, lambda *big.Int) *Point {
	x3 := new(big.Int).Mul(lambda, lambda)
	x3.Sub(x3, p.X)
	x3.Sub(x3, q.X)
	x3.Mod(x3, FrModulus)
	y3 := new(big.Int).Sub(p.X, x3)
	y3.Mul(y3, lambda)
	y3.Sub(y3, p.Y)
	y3.Mod(y3, FrModulus)
	return &Point{X: x3, Y: y3}
}

func clonePoint(p *Point) *Point {
	if p.Inf {
		return identity()
	}
	return &Point{X: new(big.Int).Set(p.X), Y: new(big.Int).Set(p.Y)}
}

// scalarMul returns k·p by double-and-add. k is reduced mod q — the
// Grumpkin group order — so exact-integer inputs (committed values) and
// mod-q blindings both land on the right point.
func scalarMul(k *big.Int, p *Point) *Point {
	kq := new(big.Int).Mod(k, FqModulus)
	acc := identity()
	base := clonePoint(p)
	for i := 0; i < kq.BitLen(); i++ {
		if kq.Bit(i) == 1 {
			acc = add(acc, base)
		}
		base = double(base)
	}
	return acc
}

// commit is the Pedersen commitment Com(v, r) = v·G + r·H (DESIGN.md §2.3).
func commit(v, r *big.Int) *Point {
	return add(scalarMul(v, genG), scalarMul(r, genH))
}

// ecdh derives the shared-secret scalar poseidon(δ_ecdh, S.x, S.y) with
// S = k·p. Both coordinates are absorbed — an x-only extraction would
// collapse P and −P (lib.nr `ecdh`).
func ecdh(k *big.Int, p *Point) (*big.Int, error) {
	s := scalarMul(k, p)
	if s.Inf {
		return nil, fmt.Errorf("ecdh degenerates to the identity")
	}
	return withDomain(domainECDH, s.X, s.Y), nil
}
