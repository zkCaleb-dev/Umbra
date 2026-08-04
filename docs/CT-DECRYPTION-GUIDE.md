# Decrypting Umbra's confidential-token history

Implementation guide for a client that turns Umbra's ciphertext history
into readable balances and amounts. Written to be implemented directly —
every formula, constant, and encoding is stated, with a real payload and
a self-check that fails loudly if anything is wrong.

**Division of labour.** Umbra stores and serves ciphertexts and never
holds a key. The client holds the key material and does all decryption
locally. Do not send secret keys to any indexer, including this one.

Protocol: OpenZeppelin Confidential Tokens (`stellar-contracts`,
`packages/tokens/src/confidential`). Authoritative sources for every
claim here: `circuits/lib/src/lib.nr` (crypto library) and
`docs/SDK.md` §5 (key derivation).

---

## 1. Where the data comes from

```
GET https://umbra-production-d30f.up.railway.app/v1/ct/{token}/history/{address}
```

Returns every event naming that address, oldest first, each with a
`payload` object holding the raw ciphertext material.

Live example used throughout this document:

```
token   CANJZVFDJ2ARRHHCPTBIZ2O3N45KWCZJY2Q4ZUPWZW6T7TKNDBNZOQ4D
address GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU
```

### 1.1 Real payload (verbatim)

```json
{
  "event_id": "3968683-6-0",
  "ledger": 3968683,
  "tx_hash": "f26d27e8f6405ee4a2df9d8f449c19406139f2233e55156894ee2538e213d746",
  "kind": "set_spender",
  "addresses": [
    "GCRYH6M5YLTGZTCAALJPIJGQZY4Z6XFFUVTINCELQG4OGLADUBTAE3OU",
    "CBJGL4LY4MLH6WZRNDNBHOSF44YIMSFXUVEPGZR54X5LAC4JTT7BM3IK"
  ],
  "payload": {
    "r_e": "JUkFLMeS1hfqWn+9cNjEMuNiW4JpCWlWEwFYc4r6UA0ADJyX7O7QoV+7+eSyDIP2UZjRItGGBtx0U7/xfELKGQ==",
    "sigma": "ACOOr4P/eJo8Q00QzVlsGZDPiwUG3+9xwvTo09Euwyo=",
    "b_tilde": "IbU2owOBvQxEUEdsNYNZHtiZxa+NpAgMabRPeYV+2MY=",
    "b_aud_s": "Gyo+KbtzZSvyiyNd4Oi/AKCT5pCY6ysmtpqbdNob70Q=",
    "v_aud_s": "ARwkQo8NMQ6DzWIpxKc9aUzqN3Omoeh00NwaiCfdlF4=",
    "live_until_ledger": 4089640
  }
}
```

### 1.2 Encoding rules

| Shape | Meaning | How to read |
|---|---|---|
| 44-char base64 → 32 bytes | field element of BN254's scalar field `F_r` | big-endian integer, reduce mod `r` |
| 88-char base64 → 64 bytes | Grumpkin point | `x = bytes[0:32]`, `y = bytes[32:64]`, both big-endian |
| JSON number | plain integer (`auditor_id`, `live_until_ledger`) | as-is |
| `amount_public` (top level, string) | public amount, only on `deposit` / `withdraw` | decimal string, i128 |

Token amounts carry the wrapped SEP-41 token's decimals (7 for USDC):
`1000000000` = 100.0000000.

---

## 2. Cryptographic primitives

### 2.1 Poseidon2 — sponge, rate 3, capacity 1, state width 4

BN254 scalar field. **This is not the same variant as the t=2
compression used by SPP Merkle trees** — a t=2 implementation will
produce wrong values here.

```
sponge(inputs[M]):
    iv    = M * 2^64                       # 18446744073709551616
    state = [0, 0, 0, iv]
    for each full 3-element block:
        state[0] += in[i]; state[1] += in[i+1]; state[2] += in[i+2]
        state = poseidon2_permutation(state, width=4)
    if a remainder block exists (or M == 0):
        add the remaining elements into state[0..k]
        state = poseidon2_permutation(state, width=4)
    return state[0]

poseidon_with_domain(d, inputs) = sponge([d] ++ inputs)
```

Every hash in this protocol goes through `poseidon_with_domain`; the
domain tag is always the first absorbed element. The permutation is
standard Poseidon2 over BN254 with d=5 S-box, RF=8, RP=56, width 4.

### 2.2 Domain tags (integers, fixed by the protocol)

```
1  ADDRESS        4  SPEND_RANDOMNESS   7  ENCRYPTED_BALANCE    10 ESCROWED_DVK
2  VIEWING_KEY    5  TRANSFER_BLINDING  8  ENCRYPTED_ALLOWANCE  11 AUDITOR_SENDER
3  DELEGATION_VK  6  TRANSFER_AMOUNT    9  ALLOWANCE_RANDOMNESS 12 AUDITOR_RECIPIENT
                                                                13 ECDH_SHARED_SECRET
```

### 2.3 Grumpkin curve

`y² = x³ − 17`. Its scalar field is BN254's base field.

```
G.x = 0x083e7911d835097629f0067531fc15cafd79a89beecb39903f69572c636f4a5a
G.y = 0x1a7f5efaad7f315c25a918f30cc8d7333fccab7ad7c90f14de81bcc528f9935d
H.x = 0x054aa86a73cb8a34525e5bbed6e43ba1198e860f5f3950268f71df4591bde402
H.y = 0x209dcfbf2cfb57f9f6046f44d71ac6faf87254afc7407c04eb621a6287cac126

ecdh(scalar, P):
    S = scalar * P                                   # curve scalar multiplication
    return poseidon_with_domain(13, [S.x, S.y])      # BOTH coordinates
```

---

## 3. Key derivation (SDK.md §5)

From the account's Stellar ed25519 secret. Fully deterministic — same
inputs always yield the same keys, which is what makes recovery work.

```
address_to_field(strkey):                            # 56-char G... or C...
    decode strkey to its 32-byte ed25519/contract payload
    lo = big-endian integer of payload[16:32]
    hi = big-endian integer of payload[0:16]
    return poseidon_with_domain(1, [lo, hi])

addr_f = address_to_field(<token contract C...>)
acct_f = address_to_field(<account G...>)

msg  = "openzeppelin/confidential-token/v1/sk" || 0x0a
       || <token contract strkey> || 0x0a || <account strkey>       # 151 bytes ASCII
root = Ed25519_Sign(secret, SHA256("Stellar Signed Message:\n" || msg))   # SEP-0053, 64 bytes

for j = 0, 1, 2, ...:
    okm = HKDF-SHA512(IKM   = root,
                      salt  = "openzeppelin/confidential-token/v1/sk",
                      info  = be32(addr_f) || be32(acct_f) || le32_u32(j),
                      L     = 32)
    candidate = big-endian integer of okm, with the TOP 2 BITS CLEARED
    if 1 <= candidate < r:                           # r = BN254 scalar field order
        sk = candidate
        vk = poseidon_with_domain(2, [sk, addr_f])
        if vk != 0: break                            # both checks required

PVK = vk * H                                          # public viewing key
dvk_i = poseidon_with_domain(3, [vk, op_i])           # delegation key, per spender op
```

Notes that cause silent failures if ignored:

- The SEP-0053 envelope is **mandatory** even when the raw secret is in
  hand. Signing the message bytes directly (without the
  `"Stellar Signed Message:\n"` prefix and the SHA-256) yields a
  different root and therefore a different, wrong account.
- `be32` is 32-byte big-endian; the rejection counter `j` is **4-byte
  little-endian**.
- Clearing the top 2 bits before the range check is part of the
  procedure, not an optimization.
- Ed25519 signing must be deterministic RFC 8032 (it is, in every
  Stellar SDK) — otherwise the root changes on every run.

**Verify the derivation before trusting any amount:** compute `Y = sk*H`
and compare against the `Y` published in the account's on-chain
registration. A mismatch means the key material is wrong; it does not
mean the decryption is broken.

---

## 4. Decryption — the ciphertexts are additive masks

Every "encryption" here is `ciphertext = value + mask (mod r)`. To read a
value, **subtract the mask**. Results are small non-negative integers; a
result that looks like a huge 254-bit number means a wrong mask (wrong
key, wrong tag, wrong salt, or wrong Poseidon2 variant).

```
value = (ciphertext - mask) mod r
```

### 4.1 Which mask, per field

| Field | Value it hides | Mask | Salt field |
|---|---|---|---|
| `b_tilde` | owner's new spendable balance | `poseidon_with_domain(7, [vk, sigma])` | `sigma` |
| `v_tilde` | transfer amount | `poseidon_with_domain(6, [s, sigma_or_sigma_a])` where `s = ecdh(vk_recipient, R_e)` | see 4.2 |
| `a_tilde` | spender allowance | `poseidon_with_domain(8, [dvk, sigma_a])` | `sigma_a` |
| `v_aud_*`, `b_aud_*`, `a_aud_*` | auditor copies | `sponge_squeeze_2(11 or 12, s_aud, sigma)` | auditor keys only |

```
sponge_squeeze_2(d, s, sigma):
    state = poseidon2_permutation([d, s, sigma, 3 * 2^64], width=4)
    return [state[0], state[1]]     # [0] = amount mask, [1] = balance/allowance mask
```

### 4.2 Which salt, per event kind — the most common mistake

| kind | payload salt | notes |
|---|---|---|
| `transfer` | `sigma` | `v_tilde` amount, `b_tilde` sender's new balance |
| `withdraw` | `sigma` | public `amount_public` + `b_tilde` |
| `set_spender` | `sigma` | carries `b_tilde` (owner's balance) |
| `revoke_spender` | `sigma` | carries `b_tilde` |
| **`spender_transfer`** | **`sigma_a`** | **no `sigma` field, and no `b_tilde`** |
| `deposit` | — | amount is public in `amount_public` |
| `merge` | — | no payload; moves pending → spendable |
| `register` | — | `auditor_id` only |

In `spender_transfer`, `sigma_a` is the salt for the transfer blinding,
the encrypted amount **and** both auditor sponges. Using `sigma` there
(or expecting `b_tilde`) is why a client shows history but no amounts.

### 4.3 As the recipient of a transfer

`v_tilde` is masked under the ECDH secret between the sender's ephemeral
key and the recipient's viewing key:

```
R_e = point from the 64-byte r_e field
s   = ecdh(vk, R_e)                    # vk = the RECIPIENT's viewing key
amount = (v_tilde - poseidon_with_domain(6, [s, sigma_or_sigma_a])) mod r
```

A sender reading their own outgoing transfer uses the ephemeral scalar
they generated, which is not in the event — senders track their own
outgoing amounts locally. What the event always gives the sender is
`b_tilde`: their balance *after* the operation, which is usually the
number a UI actually needs.

---

## 5. Computing the current balance

`b_tilde` is the owner's spendable balance **after that operation**, so
the naive rule is: decrypt `b_tilde` from the most recent event that
carries one. But two kinds move funds without publishing a ciphertext:

- `deposit` — credits the **pending** balance (amount public).
- `merge` — moves pending → spendable, publishing nothing.

So the correct rule is:

```
1. Walk events oldest → newest, tracking the latest decrypted b_tilde.
2. After that event, add every deposit whose amount was merged
   afterwards (deposit → later merge), and note any deposit not yet
   merged as a separate "pending" figure.
3. spendable = last_b_tilde + merged_deposits_after_it
   pending   = deposits with no merge after them
```

Worked example — the reference account's tail:

```
3968683  set_spender    <- last b_tilde  (decrypt this one)
3968693  deposit 250000000
3968696  merge                     <- 25.0 folded into spendable
3968754  spender_transfer          <- spends from ALLOWANCE, not balance
3969934  deposit 250000000         <- not merged yet: pending 25.0
```

`spendable = decrypt(b_tilde @ 3968683) + 250000000`, `pending = 250000000`.

Note that `spender_transfer` reduces the **allowance**, not the owner's
spendable balance — do not subtract it.

---

## 6. Self-check before showing anything to a user

Run these in order; each isolates one failure mode.

1. **Poseidon2 vector.** Take any two field elements and compare your
   `poseidon_with_domain` output against the same call in OZ's Noir
   library (`nargo test` in `circuits/`, or a fixture). If this is wrong,
   nothing downstream can be right — the most likely cause is using the
   t=2 compression instead of the t=4 sponge, or omitting the
   `iv = M * 2^64` capacity value.
2. **Key derivation.** `Y = sk*H` must equal the registered `Y` on chain.
3. **Public amounts.** `deposit` events carry `amount_public` in the
   clear. Any balance you compute must be consistent with them — for the
   reference account, the deposits alone total 6 500 000 000 (650.0),
   so a spendable balance outside a plausible range of that is wrong.
4. **Sanity of magnitude.** Every decrypted amount must be a small
   integer (fits comfortably in 64 bits). A 254-bit result is a wrong
   mask, never a large balance.

---

## 7. Minimal implementation checklist

```
[ ] BN254 F_r arithmetic (mod r add/sub/mul)
[ ] Poseidon2 permutation, width 4, d=5, RF=8, RP=56
[ ] sponge (rate 3, cap 1, iv = M * 2^64) + poseidon_with_domain
[ ] Grumpkin point ops: scalar_mul, with G/H above
[ ] ecdh = poseidon_with_domain(13, [S.x, S.y])
[ ] strkey decode + address_to_field (tag 1, lo/hi split at byte 16)
[ ] SEP-0053 message + SHA-256 + ed25519 sign  -> root
[ ] HKDF-SHA512 + rejection sampling           -> sk, then vk
[ ] base64 -> 32-byte BE field / 64-byte point decoding
[ ] per-kind salt selection (sigma vs sigma_a)  <- see 4.2
[ ] balance walk with deposit/merge accounting  <- see 5
```

Language notes: Rust has `ark-bn254` + `ark-ff`; Go has `gnark-crypto`
(BN254 scalar field, and Grumpkin arithmetic lives in its base field);
TypeScript has `@noble/curves` + `@aztec/bb.js`. Whatever the language,
the Poseidon2 t=4 sponge is the piece to validate first — it is where
implementations silently diverge.
