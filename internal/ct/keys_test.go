package ct

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// TestAddressToFieldMatchesTestdata pins the one primitive that has two
// independent implementations (contract-side Rust and every client) and
// no Noir reference — exactly why its vector file exists.
func TestAddressToFieldMatchesTestdata(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				Strkey string `json:"strkey"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "address_to_field.json", &fx)
	for _, v := range fx.Vectors {
		got, err := AddressToField(v.Inputs.Strkey)
		if err != nil {
			t.Fatal(err)
		}
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("AddressToField(%s) = %x, want %x", v.Inputs.Strkey, got, want)
		}
	}
}

func TestVKAndDVKMatchTestdata(t *testing.T) {
	var vkFx struct {
		Vectors []struct {
			Inputs struct {
				SK   string `json:"sk"`
				Wrap string `json:"wrap"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "vk_from_sk.json", &vkFx)
	for _, v := range vkFx.Vectors {
		got := withDomain(domainViewingKey, mustBig(t, v.Inputs.SK), mustBig(t, v.Inputs.Wrap))
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("vk_from_sk mismatch: %x", got)
		}
	}

	var dvkFx struct {
		Vectors []struct {
			Inputs struct {
				VK  string `json:"vk"`
				OpI string `json:"op_i"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "dvk_from_vk_op.json", &dvkFx)
	for _, v := range dvkFx.Vectors {
		got := DVK(mustBig(t, v.Inputs.VK), mustBig(t, v.Inputs.OpI))
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("dvk_from_vk_op mismatch: %x", got)
		}
	}
}

// TestDeriveKeysShape checks the derivation end to end against
// everything checkable without an on-chain oracle: determinism, the
// 151-byte SEP-0053 message (SDK.md §5.2), sk in [1, r), vk nonzero,
// and PVK on the curve. The HKDF path itself has no published vectors —
// its real gate is comparing PVK against a registered account's
// on-chain viewing_public_key.
func TestDeriveKeysShape(t *testing.T) {
	kp, err := keypair.FromRawSeed([32]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	})
	if err != nil {
		t.Fatal(err)
	}
	const contract = "CANJZVFDJ2ARRHHCPTBIZ2O3N45KWCZJY2Q4ZUPWZW6T7TKNDBNZOQ4D"

	msg := derivationContext + "\n" + contract + "\n" + kp.Address()
	if len(msg) != 151 {
		t.Fatalf("SEP-0053 message is %d bytes, spec says 151", len(msg))
	}

	k1, err := DeriveKeys(kp.Seed(), contract)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveKeys(kp.Seed(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if k1.SK.Cmp(k2.SK) != 0 || k1.VK.Cmp(k2.VK) != 0 {
		t.Fatal("derivation is not deterministic")
	}
	if k1.SK.Sign() <= 0 || k1.SK.Cmp(FrModulus) >= 0 {
		t.Fatalf("sk out of [1, r): %x", k1.SK)
	}
	if k1.VK.Sign() == 0 {
		t.Fatal("vk is zero")
	}
	if !onCurve(k1.PVK) {
		t.Fatal("PVK not on curve")
	}
	// A different contract must yield different keys (addr_f binding).
	k3, err := DeriveKeys(kp.Seed(), "CBF64DEOVQAXJFBSNGFEUT2AH4H7K5JBY3ZYJ5GVEINMNSDISWRG5N3F")
	if err != nil {
		t.Fatal(err)
	}
	if k1.SK.Cmp(k3.SK) == 0 {
		t.Fatal("sk does not depend on the contract address")
	}
}
