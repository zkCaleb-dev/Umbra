package view

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/zkCaleb-dev/umbra/internal/ct"
)

// confidentialAccount is the on-chain ConfidentialAccount record
// (storage.rs) reduced to what verification needs.
type confidentialAccount struct {
	viewingKey *ct.Point // PVK = vk·H
	spendable  *ct.Point // C_spend
	receiving  *ct.Point // C_receive
}

// fetchConfidentialAccount reads the persistent
// ConfidentialTokenStorageKey::Account(account) entry of the token
// contract via getLedgerEntries.
func fetchConfidentialAccount(ctx context.Context, rpcURL, token, account string) (*confidentialAccount, uint32, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, token)
	if err != nil {
		return nil, 0, fmt.Errorf("decoding token id: %w", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	contractAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}

	var aid xdr.AccountId
	if err := aid.SetAddress(account); err != nil {
		return nil, 0, fmt.Errorf("decoding account: %w", err)
	}
	accountAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}

	// Key: enum-with-payload contracttype → Vec[Symbol("Account"), Address].
	sym := xdr.ScSymbol("Account")
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &accountAddr},
	}
	vp := &vec
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   contractAddr,
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	bin, err := lk.MarshalBinary()
	if err != nil {
		return nil, 0, err
	}

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getLedgerEntries",
		"params": map[string]any{"keys": []string{base64.StdEncoding.EncodeToString(bin)}},
	})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck

	var out struct {
		Result struct {
			Entries []struct {
				XDR string `json:"xdr"`
			} `json:"entries"`
			LatestLedger uint32 `json:"latestLedger"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("decoding getLedgerEntries: %w", err)
	}
	if out.Error != nil {
		return nil, 0, fmt.Errorf("getLedgerEntries: %s", out.Error.Message)
	}
	if len(out.Result.Entries) == 0 {
		return nil, 0, fmt.Errorf("account is not registered on this token")
	}

	entryBin, err := base64.StdEncoding.DecodeString(out.Result.Entries[0].XDR)
	if err != nil {
		return nil, 0, err
	}
	var led xdr.LedgerEntryData
	if err := led.UnmarshalBinary(entryBin); err != nil {
		return nil, 0, fmt.Errorf("unmarshaling entry: %w", err)
	}
	cd, ok := led.GetContractData()
	if !ok {
		return nil, 0, fmt.Errorf("entry is not contract data")
	}
	acct, err := parseConfidentialAccount(cd.Val)
	if err != nil {
		return nil, 0, err
	}
	return acct, out.Result.LatestLedger, nil
}

// parseConfidentialAccount walks the contracttype struct's ScMap.
func parseConfidentialAccount(val xdr.ScVal) (*confidentialAccount, error) {
	m, ok := val.GetMap()
	if !ok || m == nil {
		return nil, fmt.Errorf("ConfidentialAccount is not a map")
	}
	acct := &confidentialAccount{}
	for _, kv := range *m {
		name, ok := kv.Key.GetSym()
		if !ok {
			continue
		}
		// The deployed generation names the commitments *_balance; the
		// current branch renamed them *_commitment. Accept both.
		var dst **ct.Point
		switch string(name) {
		case "viewing_public_key":
			dst = &acct.viewingKey
		case "spendable_commitment", "spendable_balance":
			dst = &acct.spendable
		case "receiving_commitment", "receiving_balance":
			dst = &acct.receiving
		default:
			continue
		}
		b, ok := kv.Val.GetBytes()
		if !ok {
			return nil, fmt.Errorf("%s is not bytes", name)
		}
		p, err := ct.PointFromBytes(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		*dst = p
	}
	if acct.viewingKey == nil || acct.spendable == nil || acct.receiving == nil {
		found := make([]string, 0, len(*m))
		for _, kv := range *m {
			if name, ok := kv.Key.GetSym(); ok {
				found = append(found, string(name))
			}
		}
		return nil, fmt.Errorf("ConfidentialAccount entry is missing expected fields; on-chain keys: %v", found)
	}
	return acct, nil
}
