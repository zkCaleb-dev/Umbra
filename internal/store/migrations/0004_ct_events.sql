-- Confidential Token (OpenZeppelin) events, derived from raw events of
-- contracts configured with kind "confidential-token".
--
-- CT is confidentiality, not anonymity: addresses are public, amounts
-- travel as ciphertexts. So the useful index is "every movement
-- touching address X, with its ciphertexts" — the wallet decrypts
-- amounts locally with its keys. `addresses` carries every address the
-- event names (from/to/account/spender) for a single GIN-indexed
-- membership query; `payload` carries the full ciphertext material
-- (r_e_point, v_tilde, sigma, auditor envelopes...) as rendered JSON.

CREATE TABLE ct_events (
    event_id      text PRIMARY KEY REFERENCES events(id),
    token_id      text   NOT NULL,
    ledger        bigint NOT NULL,
    tx_hash       text   NOT NULL,
    kind          text   NOT NULL, -- register|deposit|merge|withdraw|transfer|spender_transfer|set_spender|revoke_spender
    addresses     text[] NOT NULL,
    amount_public text,            -- only deposit/withdraw carry a public i128
    payload       jsonb
);

CREATE INDEX ct_events_token_ledger_idx ON ct_events (token_id, ledger);
CREATE INDEX ct_events_addresses_idx ON ct_events USING gin (addresses);
