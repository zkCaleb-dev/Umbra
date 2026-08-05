package view

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestFetchConfidentialAccountLive reads the deployed wrapper's real
// ConfidentialAccount entry for a known registered account. Guarded so
// `go test ./...` stays hermetic — the CI habit here is unit tests
// offline, oracles on demand:
//
//	UMBRA_LIVE_TEST=1 go test ./internal/view -run Live -v
func TestFetchConfidentialAccountLive(t *testing.T) {
	if os.Getenv("UMBRA_LIVE_TEST") == "" {
		t.Skip("set UMBRA_LIVE_TEST=1 to hit testnet RPC")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acct, latest, err := FetchConfidentialAccount(ctx,
		"https://soroban-testnet.stellar.org",
		"CANJZVFDJ2ARRHHCPTBIZ2O3N45KWCZJY2Q4ZUPWZW6T7TKNDBNZOQ4D",
		"GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU")
	if err != nil {
		t.Fatal(err)
	}
	if latest == 0 {
		t.Fatal("latest ledger is zero")
	}
	// PointFromBytes already enforced on-curve; just confirm the record
	// is complete and non-identity where it must be.
	if acct.ViewingKey.Inf {
		t.Fatal("registered viewing key is the identity")
	}
	t.Logf("latest ledger %d; PVK=(%x, %x)", latest, acct.ViewingKey.X, acct.ViewingKey.Y)
	t.Logf("C_spend identity=%v, C_receive identity=%v", acct.Spendable.Inf, acct.Receiving.Inf)
}
