-- 001_init_ledger.down.sql
--
-- Reverse of 001_init_ledger.up.sql. Order matters — drop dependents first.
-- Indexes are dropped implicitly with their tables.

-- Drop dependents first. Partitions cascade with the parent; explicit
-- DROPs of audit_log_y* aren't necessary.
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS journal_entries;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS accounts;

-- pgcrypto is left in place; other migrations may depend on it.
