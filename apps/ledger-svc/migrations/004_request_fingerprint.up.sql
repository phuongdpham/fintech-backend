-- 004_request_fingerprint.up.sql
--
-- Bind every committed transaction to a hash of its originating request
-- payload. Two attacks/bugs this catches:
--
--   1. Client bug — same idempotency_key, different body. Today the
--      replay path silently returns the original transaction. After this,
--      the mismatch surfaces as FailedPrecondition.
--   2. Replay/MITM — attacker captures (key, body), modifies amount or
--      destination account, resends. Same fingerprint check rejects.
--
-- Hash: SHA-256 of "tenant_id|from_account_id|to_account_id|amount|
-- currency|idempotency_key", hex-encoded → 64 chars.
--
-- VARCHAR(64) is plenty; tightening to CHAR(64) would force every row to
-- pad on update, which we never do for this column anyway.

ALTER TABLE transactions
    ADD COLUMN request_fingerprint VARCHAR(64) NOT NULL;
