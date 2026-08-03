package verify

import (
	"math/big"
	"testing"

	"github.com/zkCaleb-dev/umbra/internal/poseidon2"
)

// An empty tree's root must equal the contract's initial root: the
// zero hash at the top level (MerkleTreeWithHistory.init stores
// zeros[levels] as Root(0)).
func TestComputeRootEmptyTree(t *testing.T) {
	root, err := ComputeRoot(nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if root.Cmp(poseidon2.Zeroes[10]) != 0 {
		t.Fatalf("empty root = %s, want zeroes[10] = %s",
			root.Text(16), poseidon2.Zeroes[10].Text(16))
	}
}

// One pair of leaves in a depth-2 tree, computed by hand with the same
// primitives: root = H(H(a,b), zeroes[1]).
func TestComputeRootOnePair(t *testing.T) {
	a, b := big.NewInt(7), big.NewInt(11)
	ab, err := poseidon2.Compress(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want, err := poseidon2.Compress(ab, poseidon2.Zeroes[1])
	if err != nil {
		t.Fatal(err)
	}
	got, err := ComputeRoot([]*big.Int{a, b}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("root = %s, want %s", got.Text(16), want.Text(16))
	}
}

// Odd leaf counts pad with the level-0 zero hash on the right.
func TestComputeRootOddLeafPadsWithZero(t *testing.T) {
	a := big.NewInt(7)
	az, err := poseidon2.Compress(a, poseidon2.Zeroes[0])
	if err != nil {
		t.Fatal(err)
	}
	want, err := poseidon2.Compress(az, poseidon2.Zeroes[1])
	if err != nil {
		t.Fatal(err)
	}
	got, err := ComputeRoot([]*big.Int{a}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("root = %s, want %s", got.Text(16), want.Text(16))
	}
}

func TestComputeRootCapacity(t *testing.T) {
	leaves := []*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3)}
	if _, err := ComputeRoot(leaves, 1); err == nil {
		t.Fatal("3 leaves must not fit a depth-1 tree")
	}
}
