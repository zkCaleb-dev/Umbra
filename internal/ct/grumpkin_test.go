package ct

import (
	"math/big"
	"testing"
)

type pointFixture struct {
	X string `json:"x"`
	Y string `json:"y"`
}

func (pf pointFixture) point(t *testing.T) *Point {
	t.Helper()
	return &Point{X: mustBig(t, pf.X), Y: mustBig(t, pf.Y)}
}

func TestGeneratorsOnCurve(t *testing.T) {
	if !onCurve(genG) || !onCurve(genH) {
		t.Fatal("G or H fails the curve equation")
	}
}

func TestScalarMulMatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				Scalar string `json:"scalar"`
				Point  string `json:"point"`
			} `json:"inputs"`
			Output pointFixture `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "scalar_mul.json", &fx)
	for _, v := range fx.Vectors {
		if v.Inputs.Point != "H" {
			t.Fatalf("unexpected symbolic point %q", v.Inputs.Point)
		}
		got := scalarMul(mustBig(t, v.Inputs.Scalar), genH)
		want := v.Output.point(t)
		if got.Inf || got.X.Cmp(want.X) != 0 || got.Y.Cmp(want.Y) != 0 {
			t.Fatalf("scalarMul(%s, H) = (%x, %x), want (%x, %x)",
				v.Inputs.Scalar, got.X, got.Y, want.X, want.Y)
		}
	}
}

func TestCommitMatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				Value      string `json:"value"`
				Randomness string `json:"randomness"`
			} `json:"inputs"`
			Output pointFixture `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "commit.json", &fx)
	for _, v := range fx.Vectors {
		got := commit(mustBig(t, v.Inputs.Value), mustBig(t, v.Inputs.Randomness))
		want := v.Output.point(t)
		if got.Inf || got.X.Cmp(want.X) != 0 || got.Y.Cmp(want.Y) != 0 {
			t.Fatalf("commit(%s, %s) mismatch", v.Inputs.Value, v.Inputs.Randomness)
		}
	}
}

func TestPVKMatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				VK string `json:"vk"`
			} `json:"inputs"`
			Output pointFixture `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "pvk_from_vk.json", &fx)
	for _, v := range fx.Vectors {
		got := scalarMul(mustBig(t, v.Inputs.VK), genH)
		want := v.Output.point(t)
		if got.Inf || got.X.Cmp(want.X) != 0 || got.Y.Cmp(want.Y) != 0 {
			t.Fatalf("PVK = vk·H mismatch for vk %s", v.Inputs.VK)
		}
	}
}

func TestECDHMatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				Scalar string `json:"scalar"`
				Point  string `json:"point"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "ecdh.json", &fx)
	for _, v := range fx.Vectors {
		if v.Inputs.Point != "H" {
			t.Fatalf("unexpected symbolic point %q", v.Inputs.Point)
		}
		got, err := ecdh(mustBig(t, v.Inputs.Scalar), genH)
		if err != nil {
			t.Fatal(err)
		}
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("ecdh(%s, H) = %x, want %x", v.Inputs.Scalar, got, want)
		}
	}
}

// TestECDHSymmetric mirrors lib.nr ecdh_symmetric:
// ecdh(a, b·H) == ecdh(b, a·H).
func TestECDHSymmetric(t *testing.T) {
	a, b := big.NewInt(123457), big.NewInt(987643)
	left, err := ecdh(a, scalarMul(b, genH))
	if err != nil {
		t.Fatal(err)
	}
	right, err := ecdh(b, scalarMul(a, genH))
	if err != nil {
		t.Fatal(err)
	}
	if left.Cmp(right) != 0 {
		t.Fatal("ECDH is not symmetric")
	}
	// And the negated-point secret must differ (ecdh_binds_negated_key).
	negated, err := ecdh(a, neg(scalarMul(b, genH)))
	if err != nil {
		t.Fatal(err)
	}
	if left.Cmp(negated) == 0 {
		t.Fatal("ECDH collapses P and −P")
	}
}

// TestCommitHomomorphic mirrors lib.nr commit_homomorphic:
// C(100,42) + C(250,7) == C(350,49).
func TestCommitHomomorphic(t *testing.T) {
	sum := add(commit(big.NewInt(100), big.NewInt(42)), commit(big.NewInt(250), big.NewInt(7)))
	want := commit(big.NewInt(350), big.NewInt(49))
	if sum.Inf || sum.X.Cmp(want.X) != 0 || sum.Y.Cmp(want.Y) != 0 {
		t.Fatal("Pedersen homomorphism broken")
	}
}

func TestAddIdentityAndInverse(t *testing.T) {
	p := scalarMul(big.NewInt(7), genG)
	if got := add(p, identity()); got.X.Cmp(p.X) != 0 || got.Y.Cmp(p.Y) != 0 {
		t.Fatal("P + O != P")
	}
	if got := add(p, neg(p)); !got.Inf {
		t.Fatal("P + (−P) != O")
	}
	// 2P via add(P,P) must match double-and-add's scalarMul(2, P).
	viaAdd := add(p, p)
	viaMul := scalarMul(big.NewInt(2), p)
	if viaAdd.X.Cmp(viaMul.X) != 0 || viaAdd.Y.Cmp(viaMul.Y) != 0 {
		t.Fatal("doubling paths disagree")
	}
}
