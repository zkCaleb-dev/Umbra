# Demo runbook — SPP reference client syncing through Umbra

Reproduces the drop-in demo: the official Stellar Private Payments CLI
detects that its RPC cannot serve the pool's history, falls back to
Umbra as its bootnode, syncs the historical range from it, and resumes
on the RPC after Umbra's retention handoff.

## Prerequisites

- Umbra running via `docker compose up` and caught up to tip.
- `stellar` CLI ≥ 27 (`stellar message sign` is needed for key derivation).
- The SPP repo built: `cargo build --release -p stellar-private-payments-cli`
  in a clone of NethermindEth/stellar-private-payments.
- **Until the upstream fix lands**: the SPP CLI stores the bootnode
  setting but never passes it to the SDK client (`cli/src/session.rs`
  hardcodes `None`). Apply the two-hunk patch in
  `docs/upstream/spp-cli-bootnode.patch` and rebuild.

## Forcing the "history lost" scenario on demand

The SPP testnet pools deployed ~2026-07-31; while that ledger is still
inside the RPC's 7-day window the client never needs a bootnode. Two
knobs recreate the post-retention world today:

1. Deployment override: copy `deployments/testnet/deployments.json`
   and set each pool's `deploymentLedger` to a value below the RPC's
   oldest ledger (e.g. 3700000). Factually honest: there are no pool
   events before the real deployment, so Umbra's answers stay truthful.
2. Demo handoff window so Umbra serves the real events (not only empty
   pages) before handing off:

```bash
UMBRA_LOG_CLIENT_IP=true UMBRA_HANDOFF_CUTOFF_LEDGERS=5000 docker compose up -d umbra
```

## Run

```bash
stellar keys generate demo --network testnet --fund

spp onboard --accept --no-register --account demo \
  --data-dir /tmp/spp-demo \
  --deployment /path/to/deployments-demo.json \
  --bootnode-url http://localhost:8080

spp overview --account demo --data-dir /tmp/spp-demo \
  --deployment /path/to/deployments-demo.json
```

## Expected output

```
INFO main RPC sync gap, trying bootnode at http://localhost:8080
INFO bootnode handoff, resuming on main RPC
  balance: 0 XLM
  balance: 0 EURC
```

and on Umbra's side (`docker compose logs umbra | grep '"msg":"request"'`)
the JSON-RPC pages served during catch-up.

Reset for a fresh take: `rm -rf /tmp/spp-demo` and repeat onboard.
