-- Dynamic contract registry. deployments.json seeds this table on every
-- boot (source=config, authoritative for its ids); POST /v1/contracts
-- adds rows at runtime (source=api) with no restart. The watch set the
-- pipeline filters with is built from this table, not from the file.
CREATE TABLE contracts (
    id           text PRIMARY KEY,
    kind         text NOT NULL,
    start_ledger bigint NOT NULL DEFAULT 0,
    label        text NOT NULL DEFAULT '',
    source       text NOT NULL DEFAULT 'api', -- config | api
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Per-contract event-name rollups for the status surface: which kinds a
-- contract actually emits is the signal that a decoder is missing them.
CREATE INDEX events_contract_name_idx ON events (contract_id, event_name);
