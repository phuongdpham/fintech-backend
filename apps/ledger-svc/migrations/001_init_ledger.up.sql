-- 001_init_ledger.up.sql
--
-- Double-entry ledger schema, full shape. Pre-release: this is the only
-- migration. When the service ships, additive changes go in 002+.
--
-- Invariants enforced at the DB layer:
--
--   * Tenant scoping. Every transaction and every journal entry carries a
--     tenant_id. Composite UNIQUE (tenant_id, idempotency_key) on
--     transactions means two tenants can use the same caller-supplied key
--     without colliding. Two composite FKs from journal_entries to
--     accounts and transactions enforce that a leg's account belongs to
--     the same tenant as the transaction it's part of — cross-tenant
--     transfers are unrepresentable, not just rejected by app code.
--
--   * Request fingerprint. transactions.request_fingerprint is a SHA-256
--     hex of the canonical request body. The replay path compares this
--     against the current request; mismatch surfaces as
--     ErrRequestFingerprintMismatch. Catches client bugs ("same key,
--     different body") and replay/MITM tampering.
--
--   * Currency lock. Composite FK (account_id, currency) -> accounts
--     prevents a journal entry from being booked in a non-native currency.
--
--   * Money is NUMERIC(19,4) end-to-end. Never FLOAT / DOUBLE.
--
-- Invariants NOT enforced here (live in the application):
--
--   * SUM(amount) = 0 across legs of the same transaction
--     (domain.AssertBalanced + reconciler invariant query).
--   * Per-account balance non-negativity, where applicable.
--
-- ID column choice: uuidv7() (Postgres 18 native) instead of v4. The
-- 48-bit ms timestamp prefix makes inserts append to the rightmost B-tree
-- leaf instead of scattering — material WAL + buffer-cache wins on hot
-- indexes under 5K TPS. App code generates ids via uuid.NewV7 in Go;
-- these defaults are belt-and-braces for ad-hoc inserts.

CREATE TABLE accounts (
    id          UUID            PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   VARCHAR(128)    NOT NULL,
    currency    VARCHAR(3)      NOT NULL,
    status      VARCHAR(20)     NOT NULL DEFAULT 'ACTIVE'
                                CHECK (status IN ('ACTIVE', 'FROZEN', 'CLOSED')),
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    -- (id, currency) feeds the journal_entries currency-lock FK.
    -- (id, tenant_id) feeds the journal_entries tenant-lock FK.
    -- PG requires the referenced columns to be covered by a UNIQUE/PK
    -- constraint together for each composite FK.
    UNIQUE (id, currency),
    UNIQUE (id, tenant_id)
);

CREATE TABLE transactions (
    id                  UUID            PRIMARY KEY DEFAULT uuidv7(),
    tenant_id           VARCHAR(128)    NOT NULL,
    idempotency_key     VARCHAR(255)    NOT NULL,
    request_fingerprint VARCHAR(64)     NOT NULL,
    status              VARCHAR(20)     NOT NULL
                                        CHECK (status IN ('COMMITTED', 'REVERSED')),
    created_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    -- Tenant-scoped uniqueness — globally-unique would let two tenants
    -- collide on the same caller-supplied string and leak each other's
    -- transactions through the replay path.
    CONSTRAINT transactions_tenant_idem_key UNIQUE (tenant_id, idempotency_key),
    -- Feeds the journal_entries tenant-lock FK to transactions.
    CONSTRAINT transactions_id_tenant_key   UNIQUE (id, tenant_id)
);

CREATE TABLE journal_entries (
    id             UUID            PRIMARY KEY DEFAULT uuidv7(),
    transaction_id UUID            NOT NULL REFERENCES transactions(id),
    tenant_id      VARCHAR(128)    NOT NULL,
    account_id     UUID            NOT NULL REFERENCES accounts(id),
    amount         NUMERIC(19, 4)  NOT NULL,
    currency       VARCHAR(3)      NOT NULL,
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_acct_curr
        FOREIGN KEY (account_id, currency)
        REFERENCES accounts (id, currency),
    -- Schema-enforced tenant isolation. Together these two FKs make a
    -- cross-tenant journal entry unrepresentable: the account must
    -- belong to the leg's tenant, AND the leg's tenant must match the
    -- owning transaction's tenant. Transitively, an account in tenant A
    -- can never be booked under a transaction owned by tenant B.
    CONSTRAINT fk_je_acct_tenant
        FOREIGN KEY (account_id, tenant_id)
        REFERENCES accounts (id, tenant_id),
    CONSTRAINT fk_je_tx_tenant
        FOREIGN KEY (transaction_id, tenant_id)
        REFERENCES transactions (id, tenant_id)
);

-- Transactional Outbox — atomically committed alongside ledger writes,
-- drained by the Outbox Relay Worker. Decouples DB durability from
-- Kafka availability and gives at-least-once delivery semantics.
CREATE TABLE outbox_events (
    id             UUID         PRIMARY KEY DEFAULT uuidv7(),
    aggregate_type VARCHAR(50)  NOT NULL,
    aggregate_id   UUID         NOT NULL,
    payload        JSONB        NOT NULL,
    status         VARCHAR(20)  NOT NULL DEFAULT 'PENDING'
                                CHECK (status IN ('PENDING', 'PUBLISHED')),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_journal_account ON journal_entries (account_id);
-- Partial index: hot path is "give me the next batch of unpublished events
-- in arrival order". Keeping it partial keeps the index tiny and writes cheap.
CREATE INDEX idx_outbox_pending  ON outbox_events  (created_at)
    WHERE status = 'PENDING';
