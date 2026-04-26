-- 001_init_ledger.up.sql
--
-- Double-entry ledger schema.
--
-- Invariants enforced here (DB layer):
--   * idempotency_key on transactions is durably unique — last-line-of-defense
--     idempotency barrier when the Redis fast-path expires or is unavailable.
--   * journal_entries.amount uses NUMERIC(19,4) for exact decimal arithmetic;
--     never use FLOAT/DOUBLE for money.
--   * Composite FK (account_id, currency) -> accounts(id, currency) prevents
--     a journal entry from being booked against an account in a non-native
--     currency. Requires the matching UNIQUE constraint on accounts below.
--
-- Invariants NOT enforced here (must live in the application / Phase 4):
--   * SUM(amount) = 0 across legs of the same transaction.
--   * Per-account balance non-negativity, where applicable.
--
-- ID column choice: uuidv7() (Postgres 18 native) instead of v4. The
-- 48-bit ms timestamp prefix makes inserts append to the rightmost
-- B-tree leaf instead of scattering — material WAL + buffer-cache wins
-- on hot indexes (transactions.id, journal_entries.id, outbox_events.id)
-- under the plan's 5K TPS write load. App code generates ids via
-- uuid.NewV7 in Go; these defaults are belt-and-braces for ad-hoc inserts.

CREATE TABLE accounts (
    id          UUID         PRIMARY KEY DEFAULT uuidv7(),
    currency    VARCHAR(3)   NOT NULL,
    status      VARCHAR(20)  NOT NULL DEFAULT 'ACTIVE'
                             CHECK (status IN ('ACTIVE', 'FROZEN', 'CLOSED')),
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Required to satisfy journal_entries' composite FK below. PG demands the
    -- referenced columns be covered by a UNIQUE/PK constraint *together*.
    UNIQUE (id, currency)
);

CREATE TABLE transactions (
    id              UUID         PRIMARY KEY DEFAULT uuidv7(),
    idempotency_key VARCHAR(255) UNIQUE NOT NULL,
    status          VARCHAR(20)  NOT NULL
                                 CHECK (status IN ('COMMITTED', 'REVERSED')),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE journal_entries (
    id             UUID            PRIMARY KEY DEFAULT uuidv7(),
    transaction_id UUID            NOT NULL REFERENCES transactions(id),
    account_id     UUID            NOT NULL REFERENCES accounts(id),
    amount         NUMERIC(19, 4)  NOT NULL,
    currency       VARCHAR(3)      NOT NULL,
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_acct_curr
        FOREIGN KEY (account_id, currency)
        REFERENCES accounts (id, currency)
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
