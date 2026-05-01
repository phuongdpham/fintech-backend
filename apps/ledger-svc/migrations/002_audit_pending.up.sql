-- 002_audit_pending.up.sql
--
-- Async audit staging.
--
-- Before: the request tx wrote to audit_log directly. Each commit did
--   SELECT entry_hash FROM audit_log WHERE tenant_id=$1 ORDER BY ... LIMIT 1
--   INSERT INTO audit_log (..., prev_hash, entry_hash)
-- inside the SERIALIZABLE block. Concurrent commits for the same tenant
-- 40001-conflicted on the audit chain head — the per-tenant chain became
-- the throughput ceiling at sustained 10K TPS.
--
-- After: the request tx inserts one row here (atomic with the ledger
-- write), and a single-writer AuditWorker drains audit_pending in id
-- order, computes the per-tenant SHA-256 chain, and forwards rows into
-- audit_log. Chain serialization moves to a place where it's free
-- (one writer, one tx per batch) instead of a place where it isn't
-- (many concurrent SERIALIZABLE writers).
--
-- Trade-off accepted: audit_log is no longer atomic with the ledger row.
-- A request that commits at T1 appears in audit_log at T1+lag (typical
-- ~50–200ms). The reconciler alerts on audit_pending rows older than N
-- seconds — that's the upper bound, enforced operationally. Atomicity
-- at the per-row level is preserved: the audit_pending row commits in
-- the same Postgres tx as the ledger write, so we cannot end up with a
-- ledger row that has no audit trail enqueued.
--
-- Recovery on worker outage: rows back up here, the lag alert fires,
-- worker restarts and drains. No data loss; chain integrity is preserved
-- because the worker computes prev_hash from audit_log (the durable
-- chain) on every batch, not from in-memory state.
--
-- Partitioning: HASH-only (MODULUS 16 on tenant_id). No RANGE: in steady
-- state rows live here for seconds, so range-by-month is partition-
-- management cost on a near-empty queue. Hash spreads concurrent inserts
-- across 16 physical leaves, matching the journal_entries / audit_log
-- subpartition layout for cache locality with the rest of the write
-- path.

CREATE TABLE audit_pending (
    id              UUID            NOT NULL DEFAULT uuidv7(),
    enqueued_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    occurred_at     TIMESTAMPTZ     NOT NULL,
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
    -- (tenant_id, id) leading: exactly the access pattern the worker
    -- uses ("all rows for tenant X in id order"), so no extra index is
    -- needed for the drain path. id is uuidv7 — time-ordered prefix
    -- gives append-on-the-right inside each partition.
    PRIMARY KEY (tenant_id, id)
) PARTITION BY HASH (tenant_id);

DO $$
BEGIN
    FOR i IN 0..15 LOOP
        EXECUTE format(
            'CREATE TABLE audit_pending_h%s PARTITION OF audit_pending
                FOR VALUES WITH (MODULUS 16, REMAINDER %s)',
            lpad(i::text, 2, '0'), i
        );
    END LOOP;
END $$;

-- Lag-alert query: SELECT MIN(enqueued_at) FROM audit_pending. With this
-- index Postgres can serve the alert from a 1-row index seek per
-- partition (16 cheap lookups merged) instead of a parallel seq scan.
CREATE INDEX idx_audit_pending_enqueued ON audit_pending (enqueued_at);
