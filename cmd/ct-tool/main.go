// ct-tool: operator helpers for a plain Confidential Token deployment —
// the crypto steps around the nargo/bb proving toolchain. Pairs with
// `umbra view` (the reading side). Testnet demo tooling.
package main

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/zkCaleb-dev/umbra/internal/ct"
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `ct-tool — confidential-token operator helpers

  ct-tool witness  <seed> <token>                 # → register Prover.toml (stdout)
  ct-tool envelope <seed> <token> <proof_file>    # → RegisterData XDR hex (stdout)`)
	os.Exit(2)
}

func symVal(s string) xdr.ScVal {
	y := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &y}
}
func bytesVal(b []byte) xdr.ScVal {
	sb := xdr.ScBytes(b)
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &sb}
}
func mapVal(m xdr.ScMap) xdr.ScVal {
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "witness":
		if len(os.Args) != 4 {
			usage()
		}
		toml, err := ct.RegisterWitnessTOML(os.Args[2], os.Args[3])
		die(err)
		fmt.Print(toml)
	case "envelope":
		if len(os.Args) != 5 {
			usage()
		}
		yHex, pvkHex, err := ct.RegisterPoints(os.Args[2], os.Args[3])
		die(err)
		proof, err := os.ReadFile(os.Args[4])
		die(err)
		yB, _ := hex.DecodeString(yHex)
		pvkB, _ := hex.DecodeString(pvkHex)
		// RegisterPayload map (keys ordenadas: pvk < y)
		payload := mapVal(xdr.ScMap{
			{Key: symVal("pvk"), Val: bytesVal(pvkB)},
			{Key: symVal("y"), Val: bytesVal(yB)},
		})
		// RegisterData map (payload < proof)
		data := mapVal(xdr.ScMap{
			{Key: symVal("payload"), Val: payload},
			{Key: symVal("proof"), Val: bytesVal(proof)},
		})
		out, err := data.MarshalBinary()
		die(err)
		fmt.Print(hex.EncodeToString(out))
	default:
		usage()
	}
}
