package registry

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/config"
)

// wasmWith builds a minimal WASM module holding one custom section.
func wasmWith(name string, payload []byte) []byte {
	var section bytes.Buffer
	section.WriteByte(byte(len(name)))
	section.WriteString(name)
	section.Write(payload)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, 0x00) // custom section id
	out = binary.AppendUvarint(out, uint64(section.Len()))
	return append(out, section.Bytes()...)
}

func TestCustomSectionWalk(t *testing.T) {
	code := wasmWith("contractspecv0", []byte{0xAA, 0xBB})
	got, err := customSection(code, "contractspecv0")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0xAA, 0xBB}) {
		t.Fatalf("payload = %x", got)
	}
	if _, err := customSection(code, "missing"); err == nil {
		t.Fatal("missing section should error")
	}
	if _, err := customSection([]byte{1, 2, 3}, "x"); err == nil {
		t.Fatal("non-wasm input should error")
	}
}

// TestClassifyLiveContracts classifies the deployed contracts Umbra
// already indexes — ground truth for the spec heuristics. Guarded:
//
//	UMBRA_LIVE_TEST=1 go test ./internal/registry -run Live -v
func TestClassifyLiveContracts(t *testing.T) {
	if os.Getenv("UMBRA_LIVE_TEST") == "" {
		t.Skip("set UMBRA_LIVE_TEST=1 to hit testnet RPC")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rpc := []string{"https://soroban-testnet.stellar.org"}

	cases := []struct {
		id   string
		want config.ContractKind
	}{
		{"CANJZVFDJ2ARRHHCPTBIZ2O3N45KWCZJY2Q4ZUPWZW6T7TKNDBNZOQ4D", config.KindConfidentialToken}, // TW wrapper
		{"CBF64DEOVQAXJFBSNGFEUT2AH4H7K5JBY3ZYJ5GVEINMNSDISWRG5N3F", config.KindConfidentialToken}, // OZ demo
		{"CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA", config.KindToken},             // USDC SAC
		{"CCG3ICXNCYWQIRUMUQEJZZIIF2DTXIY63UMVDJT2EJM7VZPE45W2XFLU", config.KindSPPPool},           // SPP XLM pool
	}
	for _, tc := range cases {
		kind, detail, err := Classify(ctx, rpc, tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		t.Logf("%s → %s (%s)", tc.id[:8], kind, detail)
		if kind != tc.want {
			t.Errorf("%s classified as %s, want %s (%s)", tc.id, kind, tc.want, detail)
		}
	}
}
