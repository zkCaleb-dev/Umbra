// Package verify recomputes the SPP pool's commitment-tree root from
// Umbra's indexed leaves and checks it against the root the contract
// stores on-chain. This is the trust-minimization core: anyone can
// repeat both sides (the leaves are served by the API, the root is
// public contract storage), so serving a forged or incomplete history
// is detectable arithmetic, not an accusation.
package verify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/poseidon2"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ComputeRoot folds the ordered leaves into the tree root exactly like
// MerkleTreeWithHistory: a fixed-depth binary tree where absent nodes
// take the per-level zero hash.
func ComputeRoot(leaves []*big.Int, levels int) (*big.Int, error) {
	if levels <= 0 || levels > 32 {
		return nil, fmt.Errorf("levels must be in [1,32], got %d", levels)
	}
	if len(leaves) > 1<<levels {
		return nil, fmt.Errorf("%d leaves exceed tree capacity 2^%d", len(leaves), levels)
	}
	level := make([]*big.Int, len(leaves))
	copy(level, leaves)

	for l := 0; l < levels; l++ {
		next := make([]*big.Int, (len(level)+1)/2)
		for i := 0; i < len(next); i++ {
			left := level[2*i]
			right := poseidon2.Zeroes[l]
			if 2*i+1 < len(level) {
				right = level[2*i+1]
			}
			h, err := poseidon2.Compress(left, right)
			if err != nil {
				return nil, fmt.Errorf("level %d node %d: %w", l, i, err)
			}
			next[i] = h
		}
		if len(next) == 0 {
			next = []*big.Int{poseidon2.Zeroes[l+1]}
		}
		level = next
	}
	return level[0], nil
}

// OnChainTree is the pool's Merkle state read from contract storage.
type OnChainTree struct {
	Levels           uint32
	NextIndex        uint64
	CurrentRootIndex uint32
	Root             *big.Int
	LatestLedger     uint32
}

// ReadOnChainTree fetches Levels, NextIndex, CurrentRootIndex and the
// current Root from the pool contract's persistent storage via
// getLedgerEntries — public state anyone can re-read to audit us.
func ReadOnChainTree(ctx context.Context, rpcURL, poolID string) (*OnChainTree, error) {
	keys := []xdr.ScVal{
		enumKey("Levels"), enumKey("NextIndex"), enumKey("CurrentRootIndex"),
	}
	entries, latest, err := getLedgerEntries(ctx, rpcURL, poolID, keys)
	if err != nil {
		return nil, err
	}
	t := &OnChainTree{LatestLedger: latest}
	var haveLevels, haveNext, haveIdx bool
	for _, e := range entries {
		name := firstSym(e.key)
		switch name {
		case "Levels":
			if v, ok := e.val.GetU32(); ok {
				t.Levels, haveLevels = uint32(v), true
			}
		case "NextIndex":
			if v, ok := e.val.GetU64(); ok {
				t.NextIndex, haveNext = uint64(v), true
			}
		case "CurrentRootIndex":
			if v, ok := e.val.GetU32(); ok {
				t.CurrentRootIndex, haveIdx = uint32(v), true
			}
		}
	}
	if !haveLevels || !haveNext || !haveIdx {
		return nil, fmt.Errorf("pool %s: merkle storage keys missing (levels=%v next=%v idx=%v)",
			poolID, haveLevels, haveNext, haveIdx)
	}

	rootKey := tupleKey("Root", t.CurrentRootIndex)
	entries, _, err = getLedgerEntries(ctx, rpcURL, poolID, []xdr.ScVal{rootKey})
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if firstSym(e.key) == "Root" {
			if v, ok := e.val.GetU256(); ok {
				t.Root = u256ToBig(v)
			}
		}
	}
	if t.Root == nil {
		return nil, fmt.Errorf("pool %s: current root entry missing", poolID)
	}
	return t, nil
}

// Checkpoint is the verification verdict served by the API.
type Checkpoint struct {
	PoolID         string    `json:"pool_id"`
	LeafCount      int       `json:"leaf_count"`
	OnChainLeaves  uint64    `json:"onchain_next_index"`
	Levels         uint32    `json:"levels"`
	ComputedRoot   string    `json:"computed_root"`
	OnChainRoot    string    `json:"onchain_root"`
	Verified       bool      `json:"verified"`
	Detail         string    `json:"detail"`
	LatestLedger   uint32    `json:"onchain_latest_ledger"`
	ComputedAt     time.Time `json:"computed_at"`
}

// Verify recomputes the root from leaves and compares it with the chain.
// A count mismatch (chain ahead of the index, or vice versa) is reported
// as unverified with detail, not an error: it is the expected transient
// while catching up.
func Verify(ctx context.Context, rpcURL, poolID string, leaves []*big.Int) (*Checkpoint, error) {
	chain, err := ReadOnChainTree(ctx, rpcURL, poolID)
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{
		PoolID: poolID, LeafCount: len(leaves), OnChainLeaves: chain.NextIndex,
		Levels: chain.Levels, OnChainRoot: "0x" + chain.Root.Text(16),
		LatestLedger: chain.LatestLedger, ComputedAt: time.Now().UTC(),
	}
	if uint64(len(leaves)) != chain.NextIndex {
		cp.Detail = fmt.Sprintf("leaf count mismatch: index has %d, contract next_index is %d (catching up?)",
			len(leaves), chain.NextIndex)
		return cp, nil
	}
	root, err := ComputeRoot(leaves, int(chain.Levels))
	if err != nil {
		return nil, err
	}
	cp.ComputedRoot = "0x" + root.Text(16)
	if root.Cmp(chain.Root) == 0 {
		cp.Verified = true
		cp.Detail = "recomputed root matches the contract's current root"
	} else {
		cp.Detail = "ROOT MISMATCH — the index does not reproduce the on-chain tree"
	}
	return cp, nil
}

// ===== getLedgerEntries plumbing =====

type entry struct {
	key xdr.ScVal
	val xdr.ScVal
}

func getLedgerEntries(ctx context.Context, rpcURL, contractID string, keys []xdr.ScVal) ([]entry, uint32, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return nil, 0, fmt.Errorf("decoding contract id: %w", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}

	b64keys := make([]string, 0, len(keys))
	for _, k := range keys {
		lk := xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   addr,
				Key:        k,
				Durability: xdr.ContractDataDurabilityPersistent,
			},
		}
		bin, err := lk.MarshalBinary()
		if err != nil {
			return nil, 0, err
		}
		b64keys = append(b64keys, base64.StdEncoding.EncodeToString(bin))
	}

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getLedgerEntries",
		"params": map[string]any{"keys": b64keys},
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

	entries := make([]entry, 0, len(out.Result.Entries))
	for _, e := range out.Result.Entries {
		bin, err := base64.StdEncoding.DecodeString(e.XDR)
		if err != nil {
			return nil, 0, err
		}
		var led xdr.LedgerEntryData
		if err := led.UnmarshalBinary(bin); err != nil {
			return nil, 0, fmt.Errorf("unmarshaling entry: %w", err)
		}
		cd, ok := led.GetContractData()
		if !ok {
			continue
		}
		entries = append(entries, entry{key: cd.Key, val: cd.Val})
	}
	return entries, out.Result.LatestLedger, nil
}

// enumKey renders a unit contracttype enum variant: Vec[Symbol(name)].
func enumKey(name string) xdr.ScVal {
	sym := xdr.ScSymbol(name)
	vec := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

// tupleKey renders a one-field tuple variant: Vec[Symbol(name), U32(v)].
func tupleKey(name string, v uint32) xdr.ScVal {
	sym := xdr.ScSymbol(name)
	u := xdr.Uint32(v)
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvU32, U32: &u},
	}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func firstSym(v xdr.ScVal) string {
	vec, ok := v.GetVec()
	if !ok || vec == nil || len(*vec) == 0 {
		return ""
	}
	if s, ok := (*vec)[0].GetSym(); ok {
		return string(s)
	}
	return ""
}

func u256ToBig(p xdr.UInt256Parts) *big.Int {
	n := new(big.Int).SetUint64(uint64(p.HiHi))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.HiLo)))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.LoHi)))
	n.Lsh(n, 64).Or(n, new(big.Int).SetUint64(uint64(p.LoLo)))
	return n
}
