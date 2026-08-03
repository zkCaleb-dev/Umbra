-- Umbra initial schema.
--
-- Design rule: `events` is the raw, append-only truth (full XDR + a
-- readable render). Every other table is DERIVED from it and can be
-- rebuilt at any time. Nothing is ever deleted or destructively updated.

CREATE TABLE events (
    -- Deterministic id: <ledger>-<tx_index>-<event_index>. Makes ingestion
    -- and replay idempotent (ON CONFLICT DO NOTHING).
    id               text PRIMARY KEY,
    network          text        NOT NULL,
    ledger           bigint      NOT NULL,
    ledger_closed_at timestamptz NOT NULL,
    tx_hash          text        NOT NULL,
    tx_index         int         NOT NULL,
    event_index      int         NOT NULL,
    contract_id      text        NOT NULL,
    -- topic[0] symbol when present (e.g. "new_commitment_event").
    event_name       text        NOT NULL DEFAULT '',
    -- Readable render of topics/data. The raw XDR remains authoritative.
    topics           jsonb       NOT NULL DEFAULT '[]',
    data             jsonb,
    raw_xdr          bytea       NOT NULL,
    ingested_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_contract_order_idx
    ON events (contract_id, ledger, tx_index, event_index);
CREATE INDEX events_ledger_idx ON events (ledger);

-- Ingestion cursor: updated in the SAME transaction that writes the
-- ledger's events — crash-safe exactly-once without any broker.
CREATE TABLE cursor (
    network          text PRIMARY KEY,
    last_ledger      bigint NOT NULL,
    last_ledger_hash text   NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Evidence of ranges we could not ingest (provider retention, etc.).
-- An empty table is a completeness claim; rows are honest failure.
CREATE TABLE gaps (
    id          bigserial PRIMARY KEY,
    network     text   NOT NULL,
    from_ledger bigint NOT NULL,
    to_ledger   bigint NOT NULL,
    reason      text   NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);

-- ===== Derived views (SPP privacy pools) =====

-- Commitment tree leaves, in insertion order. leaf_index comes from the
-- contract's NewCommitmentEvent, so any hole in the sequence is
-- mathematical evidence of omission.
CREATE TABLE pool_leaves (
    pool_id          text   NOT NULL,
    leaf_index       bigint NOT NULL,
    commitment       text   NOT NULL, -- 0x-prefixed hex (BN254 field element)
    encrypted_output bytea  NOT NULL, -- opaque; wallets trial-decrypt locally
    ledger           bigint NOT NULL,
    PRIMARY KEY (pool_id, leaf_index)
);

-- Spent-note set.
CREATE TABLE pool_nullifiers (
    pool_id   text NOT NULL,
    nullifier text NOT NULL, -- 0x-prefixed hex
    ledger    bigint NOT NULL,
    PRIMARY KEY (pool_id, nullifier)
);

-- Latest registered keys per owner (the registry emits the full key set
-- on every registration; last write wins, history stays in `events`).
CREATE TABLE registry_keys (
    registry_id    text NOT NULL,
    owner          text NOT NULL, -- G... / C... strkey
    encryption_key bytea NOT NULL, -- X25519, 32 bytes
    note_key       bytea NOT NULL, -- BN254, 32 bytes
    ledger         bigint NOT NULL,
    PRIMARY KEY (registry_id, owner)
);
