-- 002_idempotency_tenant_scope.down.sql
--
-- Reversal is destructive: dropping tenant_id loses isolation. Only run
-- against a single-tenant deployment.

ALTER TABLE transactions
    DROP CONSTRAINT transactions_tenant_idem_key;

ALTER TABLE transactions
    ADD CONSTRAINT transactions_idempotency_key_key UNIQUE (idempotency_key);

ALTER TABLE transactions
    DROP COLUMN tenant_id;
