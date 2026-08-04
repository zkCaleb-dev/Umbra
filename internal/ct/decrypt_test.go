package ct

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

// The fixture tuple every encrypt_* vector shares (testdata/README.md):
// vk = vk_from_sk(0xdead, 0xbeef), s = 0x12345, sigma = 0x01,
// sigma_a = 0x02, dvk = dvk_from_vk_op(vk, 0xabcd).
const fixtureVK = "0x208fbdb70d2faacf04f987b54f12aeeaeb432acc29d650c86ce0f6275b958eb8"
const fixtureDVK = "0x1a088264ebf7269160bbf34d5a3f94d7dec37efc609ef76c5dbfa8690af3eae9"

func TestDecryptBalanceRoundTrip(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				VNew  string `json:"v_new"`
				VK    string `json:"vk"`
				Sigma string `json:"sigma"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "encrypt_balance.json", &fx)
	for _, v := range fx.Vectors {
		got, err := DecryptBalance(mustBig(t, v.Inputs.VK), mustBig(t, v.Inputs.Sigma), mustBig(t, v.Output))
		if err != nil {
			t.Fatal(err)
		}
		if want := mustBig(t, v.Inputs.VNew); got.Cmp(want) != 0 {
			t.Fatalf("DecryptBalance = %v, want %v", got, want)
		}
	}
}

func TestDecryptAmountRoundTrip(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				VTransfer string `json:"v_transfer"`
				S         string `json:"s"`
				Sigma     string `json:"sigma"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "encrypt_amount.json", &fx)
	for _, v := range fx.Vectors {
		got, err := DecryptAmount(mustBig(t, v.Inputs.S), mustBig(t, v.Inputs.Sigma), mustBig(t, v.Output))
		if err != nil {
			t.Fatal(err)
		}
		if want := mustBig(t, v.Inputs.VTransfer); got.Cmp(want) != 0 {
			t.Fatalf("DecryptAmount = %v, want %v", got, want)
		}
	}
}

func TestDecryptAllowanceRoundTrip(t *testing.T) {
	var fx struct {
		Vectors []struct {
			Inputs struct {
				VA     string `json:"v_a"`
				DVK    string `json:"dvk"`
				SigmaA string `json:"sigma_a"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "encrypt_allowance.json", &fx)
	for _, v := range fx.Vectors {
		got, err := DecryptAllowance(mustBig(t, v.Inputs.DVK), mustBig(t, v.Inputs.SigmaA), mustBig(t, v.Output))
		if err != nil {
			t.Fatal(err)
		}
		if want := mustBig(t, v.Inputs.VA); got.Cmp(want) != 0 {
			t.Fatalf("DecryptAllowance = %v, want %v", got, want)
		}
	}
}

func TestDerivedRandomnessMatchesTestdata(t *testing.T) {
	var spendFx struct {
		Vectors []struct {
			Inputs struct {
				VK    string `json:"vk"`
				Sigma string `json:"sigma"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "derive_spend_r.json", &spendFx)
	for _, v := range spendFx.Vectors {
		got := SpendRandomness(mustBig(t, v.Inputs.VK), mustBig(t, v.Inputs.Sigma))
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("SpendRandomness = %x, want %x", got, want)
		}
	}

	var blindFx struct {
		Vectors []struct {
			Inputs struct {
				S     string `json:"s"`
				Sigma string `json:"sigma"`
			} `json:"inputs"`
			Output string `json:"output"`
		} `json:"vectors"`
	}
	readFixture(t, "derive_transfer_blind.json", &blindFx)
	for _, v := range blindFx.Vectors {
		got := TransferBlinding(mustBig(t, v.Inputs.S), mustBig(t, v.Inputs.Sigma))
		if want := mustBig(t, v.Output); got.Cmp(want) != 0 {
			t.Fatalf("TransferBlinding = %x, want %x", got, want)
		}
	}
}

// TestSpenderTransferRecipientEndToEnd replays the full recipient-side
// pipeline against the pinned witness of
// circuits/spender_transfer/src/tests.nr: recover s from (vk_B, R_e),
// open v_tilde, re-derive the transfer blinding, and reproduce the
// on-chain commitment C_transfer — the exact path `umbra view` runs for
// an inbound spender_transfer.
func TestSpenderTransferRecipientEndToEnd(t *testing.T) {
	vkRecipient := big.NewInt(0xfeed)
	sigmaA := big.NewInt(0x01)
	rE := &Point{
		X: mustBig(t, "0x114ed4fcf2c57014eb678c577aa02f30ef590b713d7a6a5e87702d1c7f71957f"),
		Y: mustBig(t, "0x07a70cf826350d4f438c7a3c5e8761b0ae6cb63de757f0c96815f4057b9205f4"),
	}
	if !onCurve(rE) {
		t.Fatal("pinned R_e not on curve")
	}
	vTilde := mustBig(t, "0x0b3b7be1cd27249ec6b32b4ecb840079e0354b8675e94aade6519e5428473ffa")

	s, err := SharedSecret(vkRecipient, rE)
	if err != nil {
		t.Fatal(err)
	}
	v, err := DecryptAmount(s, sigmaA, vTilde)
	if err != nil {
		t.Fatal(err)
	}
	if v.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("recipient decrypted %v, want 100", v)
	}

	cTransfer := Commit(v, TransferBlinding(s, sigmaA))
	wantC := &Point{
		X: mustBig(t, "0x26677e8f24cbbc929b8be4a8d470d4a0e54a3c8a351ceef295e6b99b2898ed1d"),
		Y: mustBig(t, "0x089153eeedb04e49b206f7121341fdcb842a6ca19fb0f938167834dd10d42a97"),
	}
	if !cTransfer.Equal(wantC) {
		t.Fatal("re-derived C_transfer does not match the circuit's commitment")
	}

	// The spender's view of the same operation: the new allowance under
	// (dvk_i, sigma_a_new).
	vA, err := DecryptAllowance(big.NewInt(0x44767669), big.NewInt(0x02),
		mustBig(t, "0x1d2e286a8d510a7c4164d0142ceea3202a957d49248426435f311a348f681147"))
	if err != nil {
		t.Fatal(err)
	}
	if vA.Cmp(big.NewInt(900)) != 0 {
		t.Fatalf("new allowance decrypted %v, want 900", vA)
	}
}

// TestTransferSenderCheckpoint opens the sender-side b_tilde pinned by
// circuits/transfer/src/tests.nr: v_new = 900 under (vk, sigma).
func TestTransferSenderCheckpoint(t *testing.T) {
	v, err := DecryptBalance(mustBig(t, fixtureVK), big.NewInt(0x01),
		mustBig(t, "0x04d1659db899a50a94dcfc54a18b8adaf6e9e8e3046bd11893fbd4a86a7c5738"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Cmp(big.NewInt(900)) != 0 {
		t.Fatalf("checkpoint decrypted %v, want 900", v)
	}
}

func TestWrongKeyIsFlagged(t *testing.T) {
	_, err := DecryptBalance(big.NewInt(0xbad), big.NewInt(0x01),
		mustBig(t, "0x04d1659db899a50a94dcfc54a18b8adaf6e9e8e3046bd11893fbd4a86a7c5738"))
	if !errors.Is(err, ErrNotReadable) {
		t.Fatalf("expected ErrNotReadable, got %v", err)
	}
}

// TestParsePayloadRealEvent parses the verbatim payload of a real
// set_spender event indexed from the deployed wrapper (event
// 3951710-11-0) — pinning the wire format: base64 BytesN, a 64-byte
// on-curve ephemeral, and the short field names of the deployed
// contract generation.
func TestParsePayloadRealEvent(t *testing.T) {
	raw := json.RawMessage(`{
		"r_e": "ET3j7hTnxVDPBcJ/nLClLQX0zd48c7jYVQhrG10Spe4cvDLDo2NUy2CNOk88vZ9fvARGyVpwUXR8NusUi7rpfg==",
		"sigma": "AIQfR1N60194f/NMDMePsmskjqf2+DNSIbvrjbeqzK0=",
		"b_aud_s": "KkInplZC0Eg+WBM7LzkyuTw/jauMNgHR0x/5Fnl240o=",
		"b_tilde": "EvuI1h+mJQjP9gOdstevU5128Tx1mEr/sRSvlLItVQw=",
		"v_aud_s": "Gv9Y72APNeAxDAMR1uyVffjFeU6B0kIOZSJE5ddLTzY=",
		"live_until_ledger": 4072667
	}`)
	p, err := ParsePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.RE == nil || p.Sigma == nil || p.BTilde == nil {
		t.Fatal("expected r_e, sigma and b_tilde to parse")
	}
	if p.LiveUntilLedger != 4072667 {
		t.Fatalf("live_until_ledger = %d", p.LiveUntilLedger)
	}
	if p.VTilde != nil || p.SigmaA != nil {
		t.Fatal("set_spender carries no v_tilde/sigma_a")
	}
	if !onCurve(p.RE) {
		t.Fatal("real event's R_e not on curve")
	}

	// Empty and null payloads (merge, deposit) must parse to zero values.
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		if _, err := ParsePayload(raw); err != nil {
			t.Fatalf("empty payload: %v", err)
		}
	}
	// Unknown fields must fail loudly, not silently drop ciphertext.
	if _, err := ParsePayload(json.RawMessage(`{"mystery": "AA=="}`)); err == nil {
		t.Fatal("unknown payload field should error")
	}
}
