package ct

import (
	"math/big"
	"testing"
)

// TestBuildStatementFullFlow replays a synthetic but cryptographically
// real lifecycle: every ciphertext below is produced with the same
// formulas the contract's circuits enforce, so the replay exercises the
// actual decrypt paths, role detection, ordering, dedup and blinding
// accumulation — and the final accumulators must re-commit to the same
// points a chain tracking this flow would hold.
func TestBuildStatementFullFlow(t *testing.T) {
	const account = "GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU"
	const spender = "CB3IFTSLLQIBRBDLOXQXXY4R4AQYR5RIB6UTSCNMXT5YR76EUIVORUKL"
	const other = "GDT4ZDLRSS6WMP3SQABR4XGX5AWMDE6LBQIDRH673GZSWZD5XV2UEJUF"

	vk := mustBig(t, fixtureVK)

	encBalance := func(v int64, sigma *big.Int) *big.Int {
		e := new(big.Int).Add(big.NewInt(v), withDomain(domainEncryptedBalance, vk, sigma))
		return e.Mod(e, FrModulus)
	}

	sigma1 := big.NewInt(0x11) // set_spender checkpoint salt
	events := []Event{
		{ID: "100-1-0", Ledger: 100, Kind: "register", Addresses: []string{account}},
		{ID: "101-1-0", Ledger: 101, Kind: "deposit", Addresses: []string{account, account},
			AmountPublic: big.NewInt(1000), Payload: &Payload{}},
		{ID: "102-1-0", Ledger: 102, Kind: "merge", Addresses: []string{account}, Payload: &Payload{}},
		// Escrow 300 to the spender: checkpoint says the new balance is 700.
		{ID: "103-1-0", Ledger: 103, Kind: "set_spender", Addresses: []string{account, spender},
			Payload: &Payload{BTilde: encBalance(700, sigma1), Sigma: sigma1, LiveUntilLedger: 4000}},
		// The spender pays a third party from the escrow: nothing the
		// owner can read, nothing changes in the owner's accumulators.
		{ID: "104-1-0", Ledger: 104, Kind: "spender_transfer", Addresses: []string{spender, account, other},
			Payload: &Payload{}},
		// Duplicate delivery must not double-apply.
		{ID: "101-1-0", Ledger: 101, Kind: "deposit", Addresses: []string{account, account},
			AmountPublic: big.NewInt(1000), Payload: &Payload{}},
	}
	// Deliberately shuffled: replay must reconstruct emission order.
	shuffled := []Event{events[3], events[0], events[5], events[2], events[4], events[1]}

	st, err := BuildStatement(account, vk, shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", st.Warnings)
	}
	if st.Spendable == nil || st.Spendable.Cmp(big.NewInt(700)) != 0 {
		t.Fatalf("spendable = %v, want 700", st.Spendable)
	}
	if st.Pending == nil || st.Pending.Sign() != 0 {
		t.Fatalf("pending = %v, want 0", st.Pending)
	}

	// The escrow entry must show the 300 delta (1000 → 700).
	var escrow *Entry
	for i := range st.Entries {
		if st.Entries[i].Kind == "set_spender" {
			escrow = &st.Entries[i]
		}
	}
	if escrow == nil || escrow.Amount == nil || escrow.Amount.Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("set_spender entry did not derive the escrowed 300: %+v", escrow)
	}

	// The checkpoint overwrote the blinding: it must be the derived
	// spend randomness, and the accumulators must re-commit.
	wantR := SpendRandomness(vk, sigma1)
	if st.SpendR.Cmp(wantR) != 0 {
		t.Fatal("spend blinding is not the derived checkpoint randomness")
	}
	if !Commit(st.Spendable, st.SpendR).Equal(Commit(big.NewInt(700), wantR)) {
		t.Fatal("spendable accumulator does not re-commit")
	}
	if !Commit(st.Pending, st.ReceiveR).Equal(identity()) {
		t.Fatal("empty receiving accumulator should commit to the identity")
	}
	// 6 raw events, 1 duplicate → 5 entries.
	if len(st.Entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(st.Entries))
	}
}

// TestBuildStatementRecipientInbound checks the recipient view of a
// transfer built with the pinned circuit witness: vk_B = 0xfeed
// receives 100 under sigma 0x01.
func TestBuildStatementRecipientInbound(t *testing.T) {
	const me = "GDT4ZDLRSS6WMP3SQABR4XGX5AWMDE6LBQIDRH673GZSWZD5XV2UEJUF"
	const sender = "GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU"
	vkB := big.NewInt(0xfeed)
	rE := &Point{
		X: mustBig(t, "0x114ed4fcf2c57014eb678c577aa02f30ef590b713d7a6a5e87702d1c7f71957f"),
		Y: mustBig(t, "0x07a70cf826350d4f438c7a3c5e8761b0ae6cb63de757f0c96815f4057b9205f4"),
	}
	vTilde := mustBig(t, "0x0b3b7be1cd27249ec6b32b4ecb840079e0354b8675e94aade6519e5428473ffa")

	st, err := BuildStatement(me, vkB, []Event{
		{ID: "200-1-0", Ledger: 200, Kind: "register", Addresses: []string{me}},
		{ID: "201-1-0", Ledger: 201, Kind: "transfer", Addresses: []string{sender, me},
			Payload: &Payload{RE: rE, Sigma: big.NewInt(0x01), VTilde: vTilde}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil || st.Pending.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("pending = %v, want 100", st.Pending)
	}
	// The receiving accumulator must re-commit to C_transfer (the same
	// commitment the chain added to C_receive).
	wantC := &Point{
		X: mustBig(t, "0x26677e8f24cbbc929b8be4a8d470d4a0e54a3c8a351ceef295e6b99b2898ed1d"),
		Y: mustBig(t, "0x089153eeedb04e49b206f7121341fdcb842a6ca19fb0f938167834dd10d42a97"),
	}
	if !Commit(st.Pending, st.ReceiveR).Equal(wantC) {
		t.Fatal("receiving accumulator does not re-commit to C_transfer")
	}
}
