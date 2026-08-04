# Handoff — build `umbra view`

Self-contained brief for a fresh session. Everything needed to start is
here; the crypto research is already done and transcribed below.

---

## 1. What Umbra is (30 seconds)

A durable, verifiable event index for privacy protocols on Stellar
(Nethermind's Stellar Private Payments + OpenZeppelin Confidential
Tokens). Go + PostgreSQL, Apache-2.0, single binary that serves a status
UI at `GET /`, a REST API under `/v1/`, and a bootnode-compatible
JSON-RPC facade at `POST /`.

- Repo: `~/Developer/Personal/Code/umbra` (github.com/zkCaleb-dev/umbra)
- Live: https://umbra-production-d30f.up.railway.app (Railway, project `umbra`)
- Local: `docker compose up` (Postgres + umbra on :8080)
- Built for a Stellar hackathon sub-lane on privacy wallet experiences.
  Deadline: Thursday 2026-08-06. Thursday is video + submission ONLY.

Already shipped: SPP decoders, CT decoder + per-address history,
Poseidon2 checkpoints (recomputed Merkle root == on-chain root),
retention clamp with honest gap evidence, per-contract backfill,
`umbra rederive` (+ automatic re-derivation on every boot), status page.

---

## 2. The task: `umbra view`

**Problem, observed live:** a teammate ran confidential-token operations,
pointed his recovery tool at Umbra, got his full history — and could not
read a single amount, because nothing exists between "ciphertexts served"
and "keys in hand". The only decrypting client in the world is
OpenZeppelin's hosted demo.

**Deliverable:** `umbra view` — given a Stellar secret key and a token
contract, fetch the account's history from Umbra's API and print the
DECRYPTED statement (per-operation amounts + current balance). Then a
`/view` page doing the same in-browser (Go→WASM), where Freighter signs
the derivation message and keys never leave the client.

**Why it wins the bounty:** the sub-lane's example #1 is "a wallet that
shows a user's CT balance, readable in-wallet". Umbra already
over-delivers example #3 (the indexer). `umbra view` makes the project
both: the durable index AND the minimal wallet experience proving it,
demoed with the team's own real history.

**Non-negotiable design property:** the server never decrypts. Umbra
serves ciphertexts; the client holds keys. That separation is the pitch —
do not add a server-side "decrypt for me" endpoint.

---

## 3. Crypto spec (fully researched — do not re-derive)

Sources, if verification is needed (clone fresh, the old scratchpad is gone):

```bash
git clone --depth 1 -b feat/confidential-verifier-ultrahonk \
  https://github.com/OpenZeppelin/stellar-contracts.git
# packages/tokens/src/confidential/circuits/lib/src/lib.nr   <- the crypto lib
# packages/tokens/src/confidential/circuits/lib/src/tests.nr <- TEST VECTORS
# packages/tokens/src/confidential/docs/SDK.md               <- key derivation
```

Toolchain already installed on this machine (pinned to OZ's CI):
`~/.nargo/bin/nargo` 1.0.0-beta.11, `~/.bb/bb` 0.87.0. Their circuits
compile and our generated VK matched their committed VK byte for byte,
so the toolchain is known-good.

### 3.1 Poseidon2 — CT uses a DIFFERENT variant than Umbra's existing one

`internal/poseidon2` (already in the repo, validated against SPP) is
**t=2 compression**. CT needs a **t=4 sponge**:

- rate 3, capacity 1, state `[0, 0, 0, iv]` where `iv = len(inputs) * 2^64`
  (`POSEIDON2_IV_BASE = 18446744073709551616`)
- absorb 3 elements per permutation (`state[i] += input[i]`, then permute)
- if a remainder block exists (or input is empty), absorb it and permute
- squeeze `state[0]`
- BN254 scalar field; same round-constant family as the existing t=2 port,
  but t=4 needs the 4-element external matrix (M4) and its own constants —
  extract them the same way the existing `internal/poseidon2/constants.go`
  was generated.

**Every hash goes through the domain funnel** — the tag is always the
first absorbed element:

```
poseidon_with_domain(d, inputs) = sponge([d] ++ inputs)
```

### 3.2 Domain separation tags (integers, cross-language contract)

```
1  ADDRESS                          8  ENCRYPTED_ALLOWANCE
2  VIEWING_KEY                      9  ALLOWANCE_RANDOMNESS
3  DELEGATION_VIEWING_KEY          10  ESCROWED_DELEGATION_VIEWING_KEY
4  SPEND_RANDOMNESS                11  AUDITOR_SENDER
5  TRANSFER_BLINDING               12  AUDITOR_RECIPIENT
6  TRANSFER_AMOUNT                 13  ECDH_SHARED_SECRET
7  ENCRYPTED_BALANCE
```

### 3.3 Grumpkin curve

`y² = x³ − 17`. Its scalar field is BN254's base field, so gnark-crypto
already has the arithmetic — instantiate with these generators:

```
G.x = 0x083e7911d835097629f0067531fc15cafd79a89beecb39903f69572c636f4a5a
G.y = 0x1a7f5efaad7f315c25a918f30cc8d7333fccab7ad7c90f14de81bcc528f9935d
H.x = 0x054aa86a73cb8a34525e5bbed6e43ba1198e860f5f3950268f71df4591bde402
H.y = 0x209dcfbf2cfb57f9f6046f44d71ac6faf87254afc7407c04eb621a6287cac126
```

Primitives: `commit(v, r) = v*G + r*H`; `scalar_mul(s, P)`;
`ecdh(scalar, P) = poseidon_with_domain(13, [S.x, S.y])` where `S = scalar*P`
(both coordinates are absorbed — x-only would collapse P and −P).

### 3.4 Key derivation (SDK.md §5)

```
addr_f  = poseidon_with_domain(1, [lo(addr), hi(addr)])   # 56-char strkey → field
msg     = "openzeppelin/confidential-token/v1/sk" \n <contract strkey> \n <account strkey>
          (151 bytes, ASCII)
root    = Ed25519-Sign(sk_ed, SHA256("Stellar Signed Message:\n" || msg))   # SEP-0053, 64 bytes
sk      = RejectionSample( HKDF-SHA512(IKM=root,
                                       salt="openzeppelin/confidential-token/v1/sk",
                                       info=be32(addr_f) || be32(acct_f) || le4(j)) )
          # RS: take 32-byte output, clear top 2 bits, accept iff in [1, r), else j++
vk      = poseidon_with_domain(2, [sk, addr_f])       # reject if vk == 0
PVK     = vk * H                                       # public viewing key
dvk_i   = poseidon_with_domain(3, [vk, op_i])          # delegation viewing key
```

The SEP-0053 envelope is MANDATORY even when the raw secret is available
(clients must be interoperable with wallet-prompt enrollment).

### 3.5 Decryption — additive masking, so it is subtraction

| Want | Formula | Event fields |
|---|---|---|
| Own new balance | `v = b_tilde − poseidon_with_domain(7, [vk, sigma])` | `b_tilde`, `sigma` |
| Received amount | `s = ecdh(vk, R_e)`; `v = v_tilde − poseidon_with_domain(6, [s, sigma])` | `v_tilde`, `r_e` |
| Spender allowance | `v = a_tilde − poseidon_with_domain(8, [dvk, sigma_a])` | `a_tilde`, `sigma_a` |
| Transfer blinding | `r = poseidon_with_domain(5, [s, sigma])` | — |
| Spend randomness | `r' = poseidon_with_domain(4, [vk, sigma])` | — |
| Escrowed dvk | `dvk = escrowed − poseidon_with_domain(10, [s, op_i])` | `set_spender` payload |

Auditor channel uses a two-squeeze sponge:
`sponge_squeeze_2(d, s, sigma) = poseidon2_permutation([d, s, sigma, 3*2^64], 4)[0..2]`
— index 0 = amount mask, index 1 = balance/randomness mask, with
`d = 11` (sender channel) or `d = 12` (recipient channel).

**Note on `sigma`:** it is the per-operation salt. Confirm how it reaches
the client for each event kind (it may be an explicit field or bound to
the operation) by reading the corresponding circuit's `main.nr` and the
contract's `emit_*` in `packages/tokens/src/confidential/mod.rs`.

### 3.6 Validation strategy (do this, do not skip)

`circuits/lib/src/tests.nr` holds Noir test vectors. Run them with
`nargo test` inside `circuits/`, capture expected values, and use them as
Go fixtures. This is exactly how the existing t=2 port was validated (it
reproduced the contract's 33-entry zero-hash chain). Do not ship an
unvalidated port.

---

## 4. Real test data (the ground truth)

Teammate's own confidential token wrapper on testnet, already indexed:

```
token    CANJZVFDJ2ARRHHCPTBIZ2O3N45KWCZJY2Q4ZUPWZW6T7TKNDBNZOQ4D
account  GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU
```

```bash
curl -s "https://umbra-production-d30f.up.railway.app/v1/ct/CANJZVFDJ2ARRHHCPTBIZ2O3N45KWCZJY2Q4ZUPWZW6T7TKNDBNZOQ4D/history/GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU" | python3 -m json.tool
```

29 events: 1 register, 7 deposit (public amounts — free sanity check),
7 merge, 7 set_spender, 7 spender_transfer. **`spender_transfer` support
is required from day one** — it is what the teammate actually used, and
it is the kind his tool could not read.

Also indexed: OpenZeppelin's official demo token
`CBF64DEOVQAXJFBSNGFEUT2AH4H7K5JBY3ZYJ5GVEINMNSDISWRG5N3F` (115+ events,
plain `transfer` flows — good for the simpler path first).

**Oracle for correctness:** the teammate can open OZ's hosted demo with
his key and read his real balance/amounts. `umbra view` must print the
same numbers. Ask the user to get those figures.

---

## 5. Suggested build order

1. `internal/ct/poseidon2_t4.go` — sponge + `poseidon_with_domain`,
   validated against Noir test vectors.
2. `internal/ct/grumpkin.go` — curve, `commit`, `scalar_mul`, `ecdh`.
3. `internal/ct/keys.go` — strkey→field, SEP-0053 message, HKDF, `sk`/`vk`/`dvk`.
4. `internal/ct/decrypt.go` — per-event-kind decryption (balance, received
   amount, allowance), including `spender_transfer`.
5. `cmd/umbra/view.go` — `umbra view --account S… --token C…` → statement.
   Fetch from the API (default to the public instance), decrypt locally.
6. Then (next day): `/view` page, Go→WASM, Freighter `signMessage`.

Fallback if the t=4 sponge fights back: ship CLI-only with the auditor
channel (same math, different tag) — still proves the point.

---

## 6. House rules

- Commit messages: no AI attribution/co-author trailers, imperative mood,
  explain WHY. Prose comments that explain reasoning, not restatements.
- Verify against reality, not just unit tests — this project's habit has
  been to reconcile against an independent oracle (the RPC, the contract's
  on-chain root, the demo app) before claiming something works.
- The user reads code carefully and values honesty about what is unverified.
- Spanish in conversation; English in code, comments, docs and commits.
