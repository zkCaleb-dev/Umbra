# Recovering history beyond the RPC window

Every Stellar RPC keeps roughly **7 days** of history (~121,000 ledgers).
Events older than that are not served by any RPC, anywhere — for most
indexers that history is simply gone unless someone archived it in time.

Umbra recovers it anyway. This document explains what to provide, what
to expect, and why the defaults are what they are.

## Where old history actually lives

The [Stellar history archives](https://developers.stellar.org/docs/data/history-archives)
are public, free files holding **every transaction since the network's
genesis** (or, on testnet, since the last quarterly reset). They do not
contain events directly — events live in the ledger *meta*, which only
exists when transactions are executed.

So Umbra replays them: a captive `stellar-core` downloads the verified
state at a checkpoint and **re-executes the transactions**, regenerating
the meta and the events in it. Two properties fall out of that design:

- **Nothing is taken on trust.** The archive's hash chain is verified by
  core itself, and every event is recomputed by executing the same
  transactions the network executed — not served by a third party.
- **It is slow, once.** Re-executing months of ledgers takes hours. But
  a range only ever needs recovering once: after that Umbra has it
  archived, and every future consumer gets it instantly from the API.

## What you provide

1. **Your contract id** (`C…`) — required. Umbra classifies it from its
   on-chain spec automatically.
2. **A date** (optional): roughly when your contract's history starts —
   when you deployed it, or as far back as you care about. An exact
   date is not needed; Umbra adds a one-day margin. The API also
   accepts an exact `start_ledger` if you prefer.

Never keys. Umbra indexes and serves ciphertexts; decryption always
happens on your side.

## What to expect

| History | When you see it |
|---|---|
| Registration + classification | seconds |
| New events (live) | immediately |
| Last ~7 days (RPC range) | minutes to ~1 hour |
| Default depth (30 days) | ~1–2 hours, in background |
| Months of history (full archive replay) | hours — typically overnight |

Recovery runs **newest-first**: your recent history appears while older
ranges are still replaying, and `covered_from` in `/v1/status` shows how
far back coverage currently reaches. Close the tab and come back —
progress is persisted and interruptions resume where they left off.

## Why the 30-day default

The registration endpoint is deliberately open — no accounts, no API
keys — because this is a public trial instance. An *unbounded* default
depth would let a single request grow the database and burn hours of
compute on anyone's whim, so the default is bounded at 30 days as a
**cost control, not an access control**: full depth is available to
anyone simply by setting the date (or `start_ledger`) explicitly. Deep
jobs run serialized, one at a time, so a queue of requests degrades into
waiting, not into resource exhaustion.

On testnet the recoverable ceiling is the last network reset (testnet
wipes everything quarterly, archives included). On mainnet the archives
reach the 2015 genesis.

## Operator notes

The archive leg needs the `stellar-core` binary (shipped in the Docker
image) and scratch disk for bucket downloads (a few GB, ephemeral).

| Env | Default | Meaning |
|---|---|---|
| `UMBRA_ARCHIVE_BACKFILL` | `true` | Enable the captive-core leg (auto-disables with a warning if the binary is missing) |
| `UMBRA_CORE_BINARY` | `/usr/bin/stellar-core` | Core binary path |
| `UMBRA_ARCHIVE_URLS` | SDF archives of the network | Comma-separated history archive URLs |
| `UMBRA_CORE_STORAGE` | `/tmp/umbra-captive` | Bucket scratch space |

When the leg is disabled, ranges below RPC retention are recorded as
honest gaps (exactly the pre-archive behavior); a later pass with the
leg enabled recovers them and resolves the gap evidence.
