-- Per-contract coverage tracking. The global cursor says how far the
-- LOOP has ingested; this table says from which ledger each contract's
-- history is actually complete. A contract added to a running instance
-- starts covered only from the cursor — the gap down to its declared
-- start_ledger is backfilled by a background job that lowers
-- covered_from as it completes.

CREATE TABLE contract_coverage (
    contract_id  text PRIMARY KEY,
    covered_from bigint NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
