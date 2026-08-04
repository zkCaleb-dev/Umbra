package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	goxdr "github.com/stellar/go-xdr/xdr3"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/zkCaleb-dev/umbra/internal/config"
)

// Classification: a contract self-describes on-chain. SACs are built-in
// executables (no WASM) — always SEP-41 tokens. WASM contracts embed a
// SEP-48 contract spec in a `contractspecv0` custom section; its event
// and function names identify the protocol without any configuration.

// Classify infers a contract's kind from its on-chain executable/spec.
// The detail string explains the verdict for the API response and logs.
func Classify(ctx context.Context, rpcURLs []string, contractID string) (config.ContractKind, string, error) {
	var lastErr error
	for _, rpcURL := range rpcURLs {
		kind, detail, err := classifyOn(ctx, rpcURL, contractID)
		if err != nil {
			lastErr = err
			continue
		}
		return kind, detail, nil
	}
	return "", "", fmt.Errorf("classification failed on every RPC endpoint: %w", lastErr)
}

func classifyOn(ctx context.Context, rpcURL, contractID string) (config.ContractKind, string, error) {
	instanceVal, err := fetchInstance(ctx, rpcURL, contractID)
	if err != nil {
		return "", "", err
	}
	inst, ok := instanceVal.GetInstance()
	if !ok {
		return "", "", fmt.Errorf("contract instance entry has unexpected shape")
	}
	switch inst.Executable.Type {
	case xdr.ContractExecutableTypeContractExecutableStellarAsset:
		return config.KindToken, "stellar asset contract (built-in SEP-41)", nil
	case xdr.ContractExecutableTypeContractExecutableWasm:
		code, err := fetchCode(ctx, rpcURL, *inst.Executable.WasmHash)
		if err != nil {
			return "", "", err
		}
		spec, err := specFromWasm(code)
		if err != nil {
			return "", "", err
		}
		return classifySpec(spec)
	default:
		return "", "", fmt.Errorf("unknown executable type %d", inst.Executable.Type)
	}
}

// classifySpec applies protocol signatures over the spec's event names,
// falling back to function names for contracts compiled before event
// entries existed in the spec.
func classifySpec(entries []xdr.ScSpecEntry) (config.ContractKind, string, error) {
	events := map[string]bool{}
	functions := map[string]bool{}
	for _, e := range entries {
		switch e.Kind {
		case xdr.ScSpecEntryKindScSpecEntryEventV0:
			// The spec names the event STRUCT (CamelCase); on chain the
			// #[contractevent] macro emits its snake_case as topic[0].
			// Normalize so the signatures below live in the same world
			// as the decoders.
			events[toSnake(string(e.MustEventV0().Name))] = true
		case xdr.ScSpecEntryKindScSpecEntryFunctionV0:
			functions[string(e.MustFunctionV0().Name)] = true
		}
	}
	names := func(m map[string]bool, keys ...string) int {
		n := 0
		for _, k := range keys {
			if m[k] {
				n++
			}
		}
		return n
	}

	if len(events) > 0 {
		switch {
		case events["new_commitment_event"]:
			return config.KindSPPPool, "spec events: SPP pool commitments", nil
		case events["public_key_event"]:
			return config.KindSPPRegistry, "spec events: SPP key registry", nil
		case names(events, "register", "deposit", "merge", "set_spender", "spender_transfer") >= 3:
			return config.KindConfidentialToken, "spec events: confidential-token lifecycle", nil
		case events["transfer"]:
			return config.KindToken, "spec events: SEP-41 transfer", nil
		default:
			return config.KindRaw, fmt.Sprintf(
				"spec declares events %v but none match a known protocol — indexing raw", sorted(events)), nil
		}
	}

	// No event entries (older SDK): function-name heuristics.
	switch {
	case names(functions, "set_spender", "spender_transfer", "merge") >= 2:
		return config.KindConfidentialToken, "spec functions: confidential-token interface", nil
	case names(functions, "transfer", "balance", "decimals", "symbol") == 4:
		return config.KindToken, "spec functions: SEP-41 interface", nil
	default:
		return config.KindRaw, "spec matches no known protocol — indexing raw", nil
	}
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// toSnake converts a CamelCase identifier to snake_case, matching the
// soroban-sdk contractevent macro's topic derivation.
func toSnake(name string) string {
	var b []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b = append(b, '_')
			}
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

// ===== on-chain reads =====

func fetchInstance(ctx context.Context, rpcURL, contractID string) (xdr.ScVal, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return xdr.ScVal{}, fmt.Errorf("decoding contract id: %w", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	entry, err := getLedgerEntry(ctx, rpcURL, lk)
	if err != nil {
		return xdr.ScVal{}, err
	}
	cd, ok := entry.GetContractData()
	if !ok {
		return xdr.ScVal{}, fmt.Errorf("instance entry is not contract data")
	}
	return cd.Val, nil
}

func fetchCode(ctx context.Context, rpcURL string, hash xdr.Hash) ([]byte, error) {
	lk := xdr.LedgerKey{
		Type:         xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.LedgerKeyContractCode{Hash: hash},
	}
	entry, err := getLedgerEntry(ctx, rpcURL, lk)
	if err != nil {
		return nil, err
	}
	code, ok := entry.GetContractCode()
	if !ok {
		return nil, fmt.Errorf("code entry is not contract code")
	}
	return code.Code, nil
}

func getLedgerEntry(ctx context.Context, rpcURL string, lk xdr.LedgerKey) (xdr.LedgerEntryData, error) {
	bin, err := lk.MarshalBinary()
	if err != nil {
		return xdr.LedgerEntryData{}, err
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getLedgerEntries",
		"params": map[string]any{"keys": []string{base64.StdEncoding.EncodeToString(bin)}},
	})
	if err != nil {
		return xdr.LedgerEntryData{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(body))
	if err != nil {
		return xdr.LedgerEntryData{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return xdr.LedgerEntryData{}, err
	}
	defer resp.Body.Close() //nolint:errcheck

	var out struct {
		Result struct {
			Entries []struct {
				XDR string `json:"xdr"`
			} `json:"entries"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return xdr.LedgerEntryData{}, fmt.Errorf("decoding getLedgerEntries: %w", err)
	}
	if out.Error != nil {
		return xdr.LedgerEntryData{}, fmt.Errorf("getLedgerEntries: %s", out.Error.Message)
	}
	if len(out.Result.Entries) == 0 {
		return xdr.LedgerEntryData{}, fmt.Errorf("contract not found on chain (is it deployed on this network?)")
	}
	entryBin, err := base64.StdEncoding.DecodeString(out.Result.Entries[0].XDR)
	if err != nil {
		return xdr.LedgerEntryData{}, err
	}
	var led xdr.LedgerEntryData
	if err := led.UnmarshalBinary(entryBin); err != nil {
		return xdr.LedgerEntryData{}, fmt.Errorf("unmarshaling ledger entry: %w", err)
	}
	return led, nil
}

// ===== WASM custom-section walk =====

// specFromWasm extracts every ScSpecEntry from the `contractspecv0`
// custom section of a WASM module. The section holds a plain
// concatenation of XDR-encoded entries.
func specFromWasm(code []byte) ([]xdr.ScSpecEntry, error) {
	payload, err := customSection(code, "contractspecv0")
	if err != nil {
		return nil, err
	}
	var entries []xdr.ScSpecEntry
	d := goxdr.NewDecoder(bytes.NewReader(payload))
	for {
		var e xdr.ScSpecEntry
		if _, err := e.DecodeFrom(d, goxdr.DecodeDefaultMaxDepth); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("decoding spec entry %d: %w", len(entries), err)
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("contract spec section is empty")
	}
	return entries, nil
}

// customSection walks the WASM binary format (magic, version, then
// [id, size, payload] sections; id 0 = custom, payload = name + data)
// and returns the named custom section's data.
func customSection(code []byte, name string) ([]byte, error) {
	if len(code) < 8 || !bytes.Equal(code[:4], []byte{0x00, 0x61, 0x73, 0x6d}) {
		return nil, fmt.Errorf("not a WASM module")
	}
	r := bytes.NewReader(code[8:])
	for {
		id, err := r.ReadByte()
		if err == io.EOF {
			return nil, fmt.Errorf("WASM module has no %q custom section", name)
		}
		if err != nil {
			return nil, err
		}
		size, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, fmt.Errorf("malformed WASM section size: %w", err)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("truncated WASM section: %w", err)
		}
		if id != 0 {
			continue
		}
		pr := bytes.NewReader(payload)
		nameLen, err := binary.ReadUvarint(pr)
		if err != nil || nameLen > uint64(pr.Len()) {
			return nil, fmt.Errorf("malformed WASM custom section name")
		}
		sectionName := make([]byte, nameLen)
		if _, err := io.ReadFull(pr, sectionName); err != nil {
			return nil, err
		}
		if string(sectionName) == name {
			data := make([]byte, pr.Len())
			if _, err := io.ReadFull(pr, data); err != nil {
				return nil, err
			}
			return data, nil
		}
	}
}
