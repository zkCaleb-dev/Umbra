package ct

import (
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// Key derivation per SDK.md §4.9/§5: everything hangs off the SEP-0053
// signature of a fixed message, so any wallet that can sign a message
// can re-derive the same keys — the raw seed is never a substitute
// path, it must go through the same envelope.

const (
	derivationContext = "openzeppelin/confidential-token/v1/sk"
	sep53Prefix       = "Stellar Signed Message:\n"
	strkeyLen         = 56
	strkeyLimbLen     = 28
)

// AddressToField compresses a 56-character strkey into one Fr element:
// poseidon(δ_addr, lo, hi) where lo/hi read the lower/upper 28 ASCII
// bytes in little-endian order (SDK.md §4.9). It is used for the
// contract (addr_f), the account (acct_f) and the spender (op_i) alike.
func AddressToField(strkey string) (*big.Int, error) {
	if len(strkey) != strkeyLen {
		return nil, fmt.Errorf("strkey must be %d characters, got %d", strkeyLen, len(strkey))
	}
	buf := []byte(strkey)
	return withDomain(domainAddress, leLimb(buf[:strkeyLimbLen]), leLimb(buf[strkeyLimbLen:])), nil
}

// leLimb interprets 28 bytes as a little-endian integer.
func leLimb(le []byte) *big.Int {
	be := make([]byte, len(le))
	for i, b := range le {
		be[len(le)-1-i] = b
	}
	return new(big.Int).SetBytes(be)
}

// Keys is the derived hierarchy for one (contract, account) pair.
type Keys struct {
	Account string   // G... strkey the seed controls
	SK      *big.Int // spending secret scalar
	VK      *big.Int // viewing key (decrypts balances and incoming amounts)
	PVK     *Point   // vk·H, published on-chain at registration
}

// DeriveKeys derives sk/vk/PVK from a Stellar secret seed for the given
// confidential-token contract, via the mandatory SEP-0053 envelope
// (SDK.md §5.2 — mandatory even with the raw seed in hand, so CLI and
// wallet-prompt enrollments stay interoperable).
func DeriveKeys(secretSeed, contract string) (*Keys, error) {
	kp, err := keypair.ParseFull(secretSeed)
	if err != nil {
		return nil, fmt.Errorf("parsing secret seed: %w", err)
	}
	account := kp.Address()

	addrF, err := AddressToField(contract)
	if err != nil {
		return nil, fmt.Errorf("contract address: %w", err)
	}
	acctF, err := AddressToField(account)
	if err != nil {
		return nil, fmt.Errorf("account address: %w", err)
	}

	// root = Ed25519-Sign(SHA256(prefix ‖ context ‖ \n ‖ contract ‖ \n ‖ account))
	msg := derivationContext + "\n" + contract + "\n" + account
	digest := sha256.Sum256(append([]byte(sep53Prefix), msg...))
	root, err := kp.Sign(digest[:])
	if err != nil {
		return nil, fmt.Errorf("signing derivation message: %w", err)
	}

	sk, vk, err := rejectionSample(root, addrF, acctF)
	if err != nil {
		return nil, err
	}
	return &Keys{Account: account, SK: sk, VK: vk, PVK: scalarMul(vk, genH)}, nil
}

// rejectionSample runs SDK.md §5.1: HKDF-SHA512 with a counter in the
// info, clear the top 2 bits of each 32-byte candidate, accept the
// first in [1, r) whose vk is nonzero.
func rejectionSample(root []byte, addrF, acctF *big.Int) (sk, vk *big.Int, err error) {
	info := make([]byte, 0, 68)
	info = append(info, be32(addrF)...)
	info = append(info, be32(acctF)...)
	for j := uint32(0); j < 256; j++ {
		out, err := hkdf.Key(sha512.New, root, []byte(derivationContext),
			string(binary.LittleEndian.AppendUint32(info, j)), 32)
		if err != nil {
			return nil, nil, fmt.Errorf("hkdf: %w", err)
		}
		out[0] &= 0x3f // 254-bit candidate, big-endian
		cand := new(big.Int).SetBytes(out)
		if cand.Sign() == 0 || cand.Cmp(FrModulus) >= 0 {
			continue
		}
		vk := withDomain(domainViewingKey, cand, addrF)
		if vk.Sign() == 0 {
			continue
		}
		return cand, vk, nil
	}
	// Each draw fails with probability ~2^-2.6; 256 misses means the
	// inputs are broken, not that we were unlucky.
	return nil, nil, fmt.Errorf("rejection sampling did not converge")
}

// be32 encodes a field element as 32 big-endian bytes.
func be32(n *big.Int) []byte {
	return n.FillBytes(make([]byte, 32))
}

// DVK derives the delegation viewing key for one spender:
// dvk_i = poseidon(δ_dvk, vk, op_i) with op_i = AddressToField(spender).
func DVK(vk, opI *big.Int) *big.Int {
	return withDomain(domainDelegationVK, vk, opI)
}
