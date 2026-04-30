-- 003_account_tenant_ownership.up.sql
--
-- Tenant ownership at the account level. 002 stopped tenants from sharing
-- an idempotency_key namespace; this stops them from booking transfers
-- against each other's accounts.
--
-- Enforcement is in the schema, not the application:
--   * accounts.tenant_id is the owning tenant.
--   * journal_entries.tenant_id is denormalized.
--   * Two composite FKs make the invariant unforgeable from app code:
--       (account_id, tenant_id) -> accounts(id, tenant_id)
--           => leg can only reference an account belonging to its tenant.
--       (transaction_id, tenant_id) -> transactions(id, tenant_id)
--           => leg's tenant must match its owning transaction's tenant.
--     Transitively: every leg's account belongs to the transaction's tenant.
--
-- Pattern matches the existing (account_id, currency) FK that locks each
-- leg's currency to its account's native currency.
--
-- VARCHAR(128) mirrors transactions.tenant_id from migration 002 so the FK
-- columns are type-compatible.

ALTER TABLE accounts
    ADD COLUMN tenant_id VARCHAR(128) NOT NULL;

ALTER TABLE accounts
    ADD CONSTRAINT accounts_id_tenant_key UNIQUE (id, tenant_id);

ALTER TABLE transactions
    ADD CONSTRAINT transactions_id_tenant_key UNIQUE (id, tenant_id);

ALTER TABLE journal_entries
    ADD COLUMN tenant_id VARCHAR(128) NOT NULL;

ALTER TABLE journal_entries
    ADD CONSTRAINT fk_je_acct_tenant
        FOREIGN KEY (account_id, tenant_id)
        REFERENCES accounts (id, tenant_id);

ALTER TABLE journal_entries
    ADD CONSTRAINT fk_je_tx_tenant
        FOREIGN KEY (transaction_id, tenant_id)
        REFERENCES transactions (id, tenant_id);
