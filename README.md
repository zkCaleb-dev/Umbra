# Umbra

**A durable, verifiable event index for privacy protocols on Stellar.**

Privacy wallets on Stellar have a data problem: RPC nodes only retain
contract events for ~7 days. A wallet that syncs later — or loses local
state — cannot rebuild its notes, and the reference mitigation (a
"bootnode" cache) asks users to trust an opaque server that could forge,
omit or censor history.

Umbra is the missing piece: an open-source index that follows the ledger
stream, stores every event of the configured privacy contracts in
PostgreSQL **forever**, and serves them through an API designed so that
**you don't have to trust it** — completeness and integrity are checkable
by the client.

Out of the box it indexes [Stellar Private Payments](https://github.com/NethermindEth/stellar-private-payments)
(Nethermind's privacy pools): commitment-tree leaves, spent nullifiers,
encrypted outputs and the public-key registry.

## Why you can point a wallet at it

- **Durable** — events are kept from each contract's deployment ledger
  onward, past any RPC retention window. Raw XDR is the source of truth;
  every derived view can be rebuilt from it.
- **Complete or honest** — ingestion processes whole ledgers with
  parent-hash continuity checks; anything skipped is recorded in a public
  `gaps` table. Commitment leaves carry the contract's own consecutive
  `index`, so a single omitted leaf is detectable arithmetic, not a
  matter of trust.
- **Private to query** — endpoints are range-shaped: wallets download
  whole ranges and trial-decrypt locally. The server is never asked
  "which notes are mine", and client IPs are not logged by default.
- **Yours if you want** — `docker compose up` gives you a sovereign copy
  of the whole index. Self-hosting is the strongest trust model, so we
  made it a one-liner.

## Live instance

A public instance indexes the SPP + Confidential Token testnet
deployments: **https://umbra-production-d30f.up.railway.app** — the
status page is the root URL, the REST API is under `/v1/`, and the
bootnode JSON-RPC facade answers `POST /`.

## Quickstart

```bash
git clone https://github.com/zkCaleb-dev/umbra
cd umbra
docker compose up --build -d
curl localhost:8080/v1/status
```

That starts Postgres + Umbra indexing the SPP testnet deployment
(configured in `deployments/testnet.json`) from the pools' deployment
ledger.

## API

| Endpoint | Answers |
|---|---|
| `GET /v1/pools` | Indexed pools + leaf/nullifier counters |
| `GET /v1/pools/{id}/leaves?from_index=&limit=` | Commitment-tree leaves in insertion order |
| `GET /v1/pools/{id}/outputs?from_index=&limit=` | Leaves incl. encrypted outputs (bulk note scanning) |
| `GET /v1/pools/{id}/nullifiers?since_ledger=&limit=` | Spent-note set |
| `GET /v1/registry/{address}` | Registered encryption + note public keys |
| `GET /v1/contracts/{id}/events?cursor=&limit=` | Raw event stream of ANY configured contract (readable render + XDR) |
| `GET /v1/tokens/{id}/transfers?address=&since_ledger=` | Decoded SEP-41/SAC transfers, filterable per address |
| `GET /v1/ct/{token}/history/{address}?since_ledger=&limit=` | Confidential-token events naming an address — ciphertext payloads served verbatim, decryption is the client's job |
| `GET /v1/status` | Cursor, RPC endpoint, gap evidence, contracts |
| `GET /healthz`, `GET /readyz` | Liveness / readiness |

Pagination: `next_index` / `next_ledger` in the response until absent.

## Configuration

Everything is environment + one JSON file:

| Variable | Default | Meaning |
|---|---|---|
| `UMBRA_NETWORK` | `testnet` | `testnet` or `pubnet` |
| `UMBRA_RPC_URLS` | SDF testnet | Comma-separated failover pool, first is primary |
| `DATABASE_URL` | — | Postgres DSN (required) |
| `UMBRA_DEPLOYMENTS` | `deployments/testnet.json` | Contracts to index |
| `UMBRA_API_BIND` | `:8080` | API listen address |
| `UMBRA_LEDGER_FETCH_TIMEOUT` | `60s` | Per-fetch bound before rotating endpoints |
| `UMBRA_LOG_CLIENT_IP` | `false` | Opt-IN access logging (privacy default: off) |

To index a different deployment (your own pools, another network), edit
the deployments file — each entry is `{id, kind, start_ledger, label}`
with kinds `spp-pool`, `spp-registry`, `spp-asp-membership`,
`spp-asp-non-membership`, `token` (SEP-41/SAC transfer decoding), or
`raw` (store any contract's events, served at /v1/contracts/:id/events).

## Why this exists

Every serious privacy chain ends up needing a specialized data layer
between the chain and its wallets — Bitcoin has Electrum servers, Zcash
has lightwalletd. Privacy wallets are blind by design: balances live in
encrypted blobs that only the owner can open, so a wallet must download
the *complete* event history of its protocol to find its notes, rebuild
the commitment tree and know what is spent. RPC nodes keep ~7 days.
That layer did not exist for Stellar's new privacy stack. Umbra is it.

General-purpose indexers can't credibly serve this niche: their business
model is metered, authenticated queries — and for a privacy wallet the
API key *is* your identity and your query pattern *is* your notes.
Umbra inverts that: anonymous range downloads, no client logs, and
self-hosting as a first-class path. Storage stays trivial by design —
indexing a protocol's contracts from their deployment ledger means even
a wildly successful pool measures in low gigabytes, not terabytes.

**Sustainability**: the public instance and the code are free ecosystem
infrastructure. The intended model is boring and proven — hosted SLA
tiers and dedicated verifiable instances for wallet teams, and
self-hosted deployment support for enterprises running confidential
tokens, who will never query a third party for their balances.

## Design

One goroutine drives the pipeline: fetch ledger → keep only events of
successful transactions from watched contracts → render + store raw XDR →
derive views → advance cursor. **Events, derived rows and the cursor
commit in a single Postgres transaction**, so ingestion is exactly-once
across crashes without any broker. On RPC failure the loop rotates
through the endpoint pool, re-checking chain continuity by parent hash so
a divergent endpoint cannot poison the index.

```
RPC pool ──getLedgers──► extract ──► decode ──► ┌─ one PG tx ─┐
 failover                (M2/M3      (SPP        │ events      │
 + parent-hash            checks)     views)     │ derived     │
   continuity                                    │ cursor      │
                                                 └── commit ───┘
```

## Roadmap

- **Bootnode-compatible JSON-RPC facade** (`getEvents`/`getLatestLedger`
  with `-32002` handoff) so the SPP reference app can use Umbra as a
  drop-in bootnode.
- **Verifiable checkpoints**: recompute the Poseidon2 commitment-tree
  root from indexed leaves and publish it against the on-chain root —
  integrity you can check, not assume.
- Confidential Tokens (OpenZeppelin) decoder: per-address transfer
  history with ciphertexts.
- Deep-history backfill via archive endpoints.

## License

[Apache-2.0](LICENSE)
