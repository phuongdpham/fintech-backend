-- 004_request_fingerprint.down.sql

ALTER TABLE transactions DROP COLUMN request_fingerprint;
