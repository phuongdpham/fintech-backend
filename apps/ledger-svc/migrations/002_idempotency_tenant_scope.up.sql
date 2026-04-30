-- 002_idempotency_tenant_scope.up.sql
--
-- Tenant-scoped idempotency. The previous schema treated idempotency_key
-- as globally unique, which let two tenants collide on the same string and
-- leak each other's transactions through the replay path. The replay query
-- (WHERE idempotency_key = $1) had no tenant filter either, so a duplicate
-- match could return another tenant's row.
--
-- Fix: tenant_id NOT NULL on transactions, and the UNIQUE constraint moves
-- from (idempotency_key) to (tenant_id, idempotency_key). The replay query
-- adds tenant_id to the WHERE clause as defense in depth even though the
-- composite UNIQUE already guarantees no cross-tenant match.
--
-- tenant_id is opaque text rather than UUID: we don't constrain the tenant
-- naming scheme in the ledger. The auth layer issues whatever token format
-- a deployment uses (UUID, slug, JWT sub). Length-capped at 128 to bound
-- index width.

ALTER TABLE transactions
    ADD COLUMN tenant_id VARCHAR(128) NOT NULL;

ALTER TABLE transactions
    DROP CONSTRAINT transactions_idempotency_key_key;

ALTER TABLE transactions
    ADD CONSTRAINT transactions_tenant_idem_key
    UNIQUE (tenant_id, idempotency_key);
