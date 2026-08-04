package ct

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

// mustBig parses a 0x-prefixed (or bare) hex string from the fixtures.
func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	if len(s) > 1 && s[0] == 'H' { // the ecdh fixture names the generator
		t.Fatalf("mustBig cannot resolve symbolic value %q", s)
	}
	if len(s) >= 2 && s[:2] == "0x" {
		s = s[2:]
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		t.Fatalf("bad hex in fixture: %q", s)
	}
	return n
}

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
}

// TestPermutationMatchesNoirSmokeTest pins the raw permutation to the
// vector in noir-lang/noir v1.0.0-beta.11
// acvm-repo/bn254_blackbox_solver/src/poseidon2.rs (mod test).
func TestPermutationMatchesNoirSmokeTest(t *testing.T) {
	state := [4]*big.Int{big.NewInt(0), big.NewInt(0), big.NewInt(0), big.NewInt(0)}
	permute(&state)
	want := [4]string{
		"18dfb8dc9b82229cff974efefc8df78b1ce96d9d844236b496785c698bc6732e",
		"095c230d1d37a246e8d2d5a63b165fe0fade040d442f61e25f0590e5fb76f839",
		"0bb9545846e1afa4fa3c97414a60a20fc4949f537a68cceca34c5ce71e28aa59",
		"18a4f34c9c6f99335ff7638b82aeed9018026618358873c982bbdde265b2ed6d",
	}
	for i := range want {
		w, _ := new(big.Int).SetString(want[i], 16)
		if state[i].Cmp(w) != 0 {
			t.Fatalf("permute([0,0,0,0])[%d] = %x, want %s", i, state[i], want[i])
		}
	}
}

// TestSpongeMatchesNoirHashSmokeTest pins the sponge for a 4-element
// input (two permutations) to the fixed-length hash vector in the same
// Noir source file.
func TestSpongeMatchesNoirHashSmokeTest(t *testing.T) {
	got := sponge([]*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3), big.NewInt(4)})
	want, _ := new(big.Int).SetString(
		"130bf204a32cac1f0ace56c78b731aa3809f06df2731ebcf6b3464a15788b1b9", 16)
	if got.Cmp(want) != 0 {
		t.Fatalf("sponge([1,2,3,4]) = %x, want %x", got, want)
	}
}

// TestSpongeEmptyInput mirrors lib.nr's
// sponge_empty_input_applies_squeeze_permutation: the empty input still
// runs one permutation, so sponge([]) == permute([0,0,0,0])[0].
func TestSpongeEmptyInput(t *testing.T) {
	got := sponge(nil)
	want, _ := new(big.Int).SetString(
		"18dfb8dc9b82229cff974efefc8df78b1ce96d9d844236b496785c698bc6732e", 16)
	if got.Cmp(want) != 0 {
		t.Fatalf("sponge([]) = %x, want %x", got, want)
	}
}

func TestWithDomainMatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				Domain string   `json:"domain"`
				Inputs []string `json:"inputs"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "poseidon_with_domain.json", &fx)
	for _, v := range fx.Vectors {
		d := mustBig(t, v.Inputs.Domain).Uint64()
		inputs := make([]*big.Int, len(v.Inputs.Inputs))
		for i, s := range v.Inputs.Inputs {
			inputs[i] = mustBig(t, s)
		}
		got := withDomain(d, inputs...)
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("withDomain(%d, %v) = %x, want %x", d, v.Inputs.Inputs, got, want)
		}
	}
}

func TestSpongeSqueeze2MatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				D     string `json:"d"`
				S     string `json:"s"`
				Sigma string `json:"sigma"`
			} `json:"inputs"`
			Output []string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "sponge_squeeze_2.json", &fx)
	for _, v := range fx.Vectors {
		d := mustBig(t, v.Inputs.D).Uint64()
		s, sigma := mustBig(t, v.Inputs.S), mustBig(t, v.Inputs.Sigma)
		lane0, lane1 := spongeSqueeze2(d, s, sigma)
		if want := mustBig(t, v.Output[0]); lane0.Cmp(want) != 0 {
			t.Fatalf("spongeSqueeze2(%d)[0] = %x, want %x", d, lane0, want)
		}
		if want := mustBig(t, v.Output[1]); lane1.Cmp(want) != 0 {
			t.Fatalf("spongeSqueeze2(%d)[1] = %x, want %x", d, lane1, want)
		}
		// Lane 0 must equal the single-squeeze funnel on the same inputs
		// (lib.nr sponge_squeeze_2_first_matches_poseidon_with_domain).
		if single := withDomain(d, s, sigma); single.Cmp(lane0) != 0 {
			t.Fatalf("squeeze2 lane 0 diverges from withDomain: %x vs %x", lane0, single)
		}
	}
}
