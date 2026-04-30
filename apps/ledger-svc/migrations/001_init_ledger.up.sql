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

-- ----------------------------------------------------------------------------
-- Audit log
--
-- Every state-changing operation (transfer execution, future account/freeze
-- ops) appends one row here, in the SAME Postgres transaction as the ledger
-- write. Three guarantees this layer enforces:
--
--   * Tamper evidence — entry_hash chains backward via prev_hash
--     (per-tenant chain anchored at all-zeros GenesisHash).
--   * Atomic with ledger — committed-or-not-at-all with the underlying
--     state change. If the audit write fails, the ledger write rolls back.
--   * Append-only intent — production deployments revoke UPDATE/DELETE on
--     this table from the app role. The hash chain catches any in-flight
--     tampering even if a privileged role mutates rows.
--
-- Range-partitioned by month from day one. Adding partitioning to a non-
-- empty table later is painful; greenfield is the only cheap window.
-- A monthly partition manager (pg_cron / external) creates next-month's
-- partition before it's needed; this migration seeds the first six
-- partitions to bootstrap.
--
-- Per-tenant chain integrity: an audit gap in tenant A doesn't invalidate
-- tenant B's chain. The hot read path on every audit write is
-- "give me the latest entry_hash for tenant X", indexed below.
-- ----------------------------------------------------------------------------

CREATE TABLE audit_log (
    id              UUID            NOT NULL DEFAULT uuidv7(),
    occurred_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    tenant_id       VARCHAR(128)    NOT NULL,
    actor_subject   VARCHAR(255)    NOT NULL,
    actor_session   VARCHAR(255)    NOT NULL DEFAULT '',
    request_id      VARCHAR(64)     NOT NULL,
    trace_id        VARCHAR(32)     NOT NULL DEFAULT '',
    aggregate_type  VARCHAR(50)     NOT NULL,
    aggregate_id    UUID            NOT NULL,
    operation       VARCHAR(80)     NOT NULL,
    before_state    JSONB,
    after_state     JSONB,
    prev_hash       BYTEA           NOT NULL,
    entry_hash      BYTEA           NOT NULL,
    -- Partition key MUST be in the primary key for declarative partitioning.
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

-- Hot read path: "latest entry_hash for tenant X" on every audit write.
-- DESC order matches the lookup direction; (occurred_at, id) breaks ties
-- deterministically when two rows share the same nanosecond.
CREATE INDEX idx_audit_tenant_chain
    ON audit_log (tenant_id, occurred_at DESC, id DESC);

-- Common audit query patterns. Add more as they materialize.
CREATE INDEX idx_audit_aggregate
    ON audit_log (tenant_id, aggregate_type, aggregate_id, occurred_at);
CREATE INDEX idx_audit_actor
    ON audit_log (tenant_id, actor_subject, occurred_at);

-- Bootstrap partitions: current month + five ahead. Production needs a
-- partition manager to keep rolling; running short of partitions hard-
-- fails INSERTs (no default partition by design — silent fallback to a
-- catch-all partition would defeat the audit-row time guarantees).
CREATE TABLE audit_log_y2026m05 PARTITION OF audit_log FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE audit_log_y2026m06 PARTITION OF audit_log FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE audit_log_y2026m07 PARTITION OF audit_log FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');
CREATE TABLE audit_log_y2026m08 PARTITION OF audit_log FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE audit_log_y2026m09 PARTITION OF audit_log FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE audit_log_y2026m10 PARTITION OF audit_log FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
