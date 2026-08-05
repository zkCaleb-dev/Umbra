package ct

import (
	"encoding/hex"
	"fmt"
	"math/big"
)

// RegisterWitnessTOML devuelve el Prover.toml del circuito register para
// (seed, token): sk privado + Y/PVK/addr_f/acct_f públicos, en el orden
// de parámetros de main().
func RegisterWitnessTOML(seed, token string) (string, error) {
	k, err := DeriveKeys(seed, token)
	if err != nil {
		return "", err
	}
	addrF, err := AddressToField(token)
	if err != nil {
		return "", err
	}
	acctF, err := AddressToField(k.Account)
	if err != nil {
		return "", err
	}
	y := scalarMul(k.SK, genH)
	h := func(n *big.Int) string { return "0x" + n.Text(16) }
	return fmt.Sprintf(
		"sk = \"%s\"\ny_x = \"%s\"\ny_y = \"%s\"\npvk_x = \"%s\"\npvk_y = \"%s\"\naddr_f = \"%s\"\n_acct_f = \"%s\"\n",
		h(k.SK), h(y.X), h(y.Y), h(k.PVK.X), h(k.PVK.Y), h(addrF), h(acctF)), nil
}

// RegisterPoints devuelve Y y PVK como be(x)||be(y) hex (los del payload).
func RegisterPoints(seed, token string) (yHex, pvkHex string, err error) {
	k, err := DeriveKeys(seed, token)
	if err != nil {
		return "", "", err
	}
	y := scalarMul(k.SK, genH)
	return hex.EncodeToString(y.Bytes()), hex.EncodeToString(k.PVK.Bytes()), nil
}
