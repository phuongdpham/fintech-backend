-- 003_account_tenant_ownership.down.sql
--
-- Reversal drops tenant ownership. Only safe in single-tenant deployments.

ALTER TABLE journal_entries DROP CONSTRAINT fk_je_tx_tenant;
ALTER TABLE journal_entries DROP CONSTRAINT fk_je_acct_tenant;
ALTER TABLE journal_entries DROP COLUMN tenant_id;

ALTER TABLE transactions DROP CONSTRAINT transactions_id_tenant_key;

ALTER TABLE accounts DROP CONSTRAINT accounts_id_tenant_key;
ALTER TABLE accounts DROP COLUMN tenant_id;
