package poseidon2

import "testing"

// The pool contract ships 33 precomputed empty-subtree hashes where
// zeroes[i+1] = compress(zeroes[i], zeroes[i]). Reproducing the whole
// chain from zeroes[0] cross-validates this port against the on-chain
// implementation with 32 independent test vectors — if any round
// constant, matrix or the compression step were wrong, the chain would
// diverge at the first link.
func TestZeroHashChainMatchesContract(t *testing.T) {
	for i := 0; i < 32; i++ {
		got, err := Compress(Zeroes[i], Zeroes[i])
		if err != nil {
			t.Fatalf("level %d: %v", i, err)
		}
		if got.Cmp(Zeroes[i+1]) != 0 {
			t.Fatalf("level %d: compress(z,z) = %s, contract says %s",
				i, got.Text(16), Zeroes[i+1].Text(16))
		}
	}
}

func TestCompressRejectsOutOfField(t *testing.T) {
	if _, err := Compress(Modulus, Zeroes[0]); err == nil {
		t.Fatal("expected error for input >= modulus")
	}
}
