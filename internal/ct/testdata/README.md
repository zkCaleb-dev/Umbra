# Cross-language test vectors — provenance

Copied verbatim from `packages/tokens/src/confidential/circuits/lib/testdata/`
of openzeppelin/stellar-contracts, branch `feat/confidential-verifier-ultrahonk`
(commit `98090b3`). That directory is the protocol's declared cross-language
contract: a consumer in any language is correct iff it reproduces every output
in every file byte for byte.

The vectors share one fixture tuple (see that repo's testdata/README.md):
`sk = 0xdead`, `addr_f = 0xbeef`, `sigma = 0x01`, `sigma_a = 0x02`,
`op_i = 0xabcd`, `v = 1000`, `r = 42`, `v_transfer = 100`, `v_a = 500`,
`r_e = 0xfeedface`, `s = 0x12345`.

Do not edit by hand — re-copy from upstream and re-run `go test ./internal/ct`
if the upstream vectors ever version.
