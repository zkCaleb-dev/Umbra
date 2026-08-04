-- Gap evidence must be a fact, not a log line. Two corrections:
--
-- 1. Remove rows written by the all-or-nothing backfill bug (fixed in
--    the same release): every restart re-recorded the SAME range as
--    "backfill failed on every endpoint" even though most of it had
--    been (or later was) successfully stored. Those rows are noise from
--    a defect, not evidence — the fixed backfill re-records the truly
--    unreachable remainder precisely.
-- 2. Dedupe whatever else repeated, and make repeats structurally
--    impossible: one row per (network, range).

DELETE FROM gaps WHERE reason = 'backfill failed on every endpoint';

DELETE FROM gaps a USING gaps b
 WHERE a.id > b.id
   AND a.network = b.network
   AND a.from_ledger = b.from_ledger
   AND a.to_ledger = b.to_ledger;

CREATE UNIQUE INDEX gaps_range_uniq ON gaps (network, from_ledger, to_ledger);
