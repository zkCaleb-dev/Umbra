-- Token transfers (SEP-41 / Stellar Asset Contract), derived from raw
-- `transfer` events of contracts configured with kind "token". Amounts
-- are stored as decimal strings (i128 does not fit bigint).

CREATE TABLE token_transfers (
    event_id  text PRIMARY KEY REFERENCES events(id),
    token_id  text   NOT NULL,
    ledger    bigint NOT NULL,
    tx_hash   text   NOT NULL,
    from_addr text   NOT NULL,
    to_addr   text   NOT NULL,
    amount    text   NOT NULL
);

CREATE INDEX token_transfers_token_ledger_idx ON token_transfers (token_id, ledger);
CREATE INDEX token_transfers_from_idx ON token_transfers (token_id, from_addr, ledger);
CREATE INDEX token_transfers_to_idx   ON token_transfers (token_id, to_addr, ledger);
