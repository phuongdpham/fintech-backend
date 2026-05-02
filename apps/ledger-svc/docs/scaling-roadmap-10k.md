# Scaling roadmap — getting from 6.5K to 10K+ TPS

This is the **incremental** roadmap — refactors and tuning within the
current synchronous architecture. Companion to:

- `architecture.md` — what's shipped today (HEAD).
- `load-test-2026-05-01.md` / `load-test-2026-05-02.md` — measured
  ceilings and A/B results that motivate this doc.
- `scaling-v2-async-ledger.md` — the **structural** rewrite track,
  active only if this roadmap measures out at < 10K.

## Where we are

```
async audit:           shipped (112c849)
pool conn-release fix: shipped (dba9d5d)
Tier 1 pool tuning:    shipped (43d86d4) — measured regression at high concurrency
Tier 2 SendBatch:      shipped (4ca71a1) — A/B'd, +20% sustained TPS, -54ms p50
Tier 3 sharding:       NOT STARTED — gated on Tier 4 measurement
Tier 4 isolation:      NOT STARTED — next concrete move

sustained ceiling:     6.5K iters/s (single-host, k6 co-located)
                       ~9–10K projected with k6 off-box on same code
project target:        5K+ TPS sustained — MET
stretch target:        10K+ TPS sustained — open
```

## Industry context

10K+ TPS sustained on a single Postgres for a strongly-consistent
double-entry ledger is rare. Public/estimated numbers:

| System | Ledger TPS | Architecture |
|---|---:|---|
| Visa | ~1.7K avg, 24K capacity | global card network, settlement is batch |
| Stripe | thousands globally; ~5–15K peak (estimate) | heavily sharded by tenant |
| PayPal | ~700–1,000 sustained | — |
| Adyen | low thousands | — |
| RTGS (FedNow, TARGET2) | hundreds, strict per-tx SLA | throughput isn't the goal |
| TigerBeetle | claims 1M+ in benchmarks | specialized engine, not Postgres |

Where 10K+ exists in production it's almost always one of: sharded
Postgres (Citus or app-level), distributed SQL (Cockroach / Yugabyte
/ Spanner), a specialized engine (TigerBeetle), or event-sourced with
projection-based balances. None of those are tuning — all are
structural.

The 10K target on this codebase is at the upper end of what teams
build before reaching for one of the above patterns. 8K on commodity
hardware with this schema is already a respectable single-node
number; past that is structural, not tuning.

---

## Tier 1 — pool + PG tuning  (shipped, measured regression)

### Status

Shipped in `43d86d4`. Result: **doesn't help, may hurt at high
concurrency**. Kept the PG-side bumps because they don't hurt and
benefit later tiers; rolled the application pool size out of the
incremental path.

### What shipped

| Knob | Before | After | Where |
|---|---:|---:|---|
| `DATABASE_MAX_CONNS` | 250 | 500 | `apps/ledger-svc/.env` |
| PG `max_connections` | 400 | 700 | `postgresql-load.conf` |
| `effective_io_concurrency` | 200 | 256 | `postgresql-load.conf` |
| `wal_buffers` | 64MB | 64MB | already there, no change |

### What we measured

| Pool size | Sustained TPS | 40001 cap_reached | Pool utilization |
|---:|---:|---:|---:|
| 250 (yesterday) | 6.5K | 11.9% | 100% pegged |
| 500 (today) | ~5.8K | 14.4% | ~52% (not pool-bound) |

The pool size 250 was already past the inflection point for this
workload. Doubling to 500 doubled the concurrent same-account
contention; the breaker had to absorb a much larger storm in early
ramp before it stabilized. Throughput didn't beat 250-conn.

### Conclusion

The pool was never the bottleneck. **Hot-account 40001 contention
is.** The bottleneck moves from "queue at the pool" to "queue inside
PG's lock manager / SERIALIZABLE snapshot validation." That's a
contention problem, not a capacity problem.

### What we did NOT do, and why

| Knob | Skipped because |
|---|---|
| `synchronous_commit = off` | Would give 2–3× write throughput but trades durability — only acceptable if a replica + manual recovery story is in place. Not worth the trade-off until v2 makes the durability story different anyway. |
| Bigger `shared_buffers` | Already at 4GB on 8GB container; growing means container memory grows. Returns diminishing on this schema. |
| `autovacuum_vacuum_scale_factor` per-table | Would help on `outbox_events` (high churn). Defer to Tier 3 read-replica work — sets up cleaner per-table tuning. |

---

## Tier 2 — reduce per-tx work  (shipped, A/B confirmed win)

### Status

`pgx.SendBatch` shipped in `4ca71a1`. A/B'd in
`load-test-2026-05-02.md`. **+20% sustained TPS, -54 ms p50,
-102 ms p99.** Other items in this tier remain available but
expected gain is small.

### What shipped — `pgx.SendBatch` for the 5 hot-path INSERTs

`internal/repository/ledger_repo.go::ExecuteTransfer` used to make 5
separate `tx.Exec` round-trips inside one SERIALIZABLE tx. Now all 5
queue in a `pgx.Batch` and pipeline as one `tx.SendBatch`.

Order is locked (matters for `classifyBatchErr`):

```
0: INSERT transactions       — UNIQUE → ErrDuplicateIdempotencyKey
1: INSERT journal_entries#1  — FK     → ErrAccountTenantMismatch
2: INSERT journal_entries#2  — FK     → ErrAccountTenantMismatch
3: INSERT outbox_events
4: INSERT audit_pending
```

Surrounding `BEGIN ISOLATION LEVEL SERIALIZABLE` + 40001 retry +
breaker + atomicity all unchanged.

### A/B results (measured)

|  | Phase B (unbatched) | Phase A (SendBatch) | Δ |
|---|---:|---:|---:|
| Server-side ok | 472,000 | 567,194 | **+20.1%** |
| Sustained TPS | 1,966 / s | 2,363 / s | +397 / s |
| p50 latency | 351 ms | 297 ms | **−54 ms** |
| p99 latency | 813 ms | 711 ms | **−102 ms** |
| 40001 cap_reached | 1.13% | 1.07% | unchanged at this load |

Bigger win than the 5 ms RTT-math estimate because Pareto-skewed
queueing on hot accounts amplifies every saved millisecond inside the
SERIALIZABLE snapshot window.

### Items still available in this tier (deferred)

| Change | Cost | Expected gain | Decision |
|---|---|---|---|
| Server-side prepared statements (`pgx` Describe/Bind cache) | 0.5d | parse cost off hot path, ~3–5% | Defer — pgx already caches per-conn; explicit prepare adds ceremony for marginal gain. |
| Re-validate `goccy/go-json` and proto-bytea wins | 1d | re-measure | Defer — measured fine in 2026-05 runs. Re-test only if benchmarks regress. |
| Batch outbox INSERTs across N transfers (windowed coalescing) | 2d | risky | Defer — needs careful idempotency reasoning. Don't pre-optimize. |

### Conclusion

Tier 2's headline win shipped. Other items aren't worth the time
until we have a measured reason to chase them.

---

## Tier 4 — isolation review  (next concrete move)

Tier 4 comes before Tier 3 in priority because (a) it's the cheapest
code change (~0.5 day), (b) the decision work is what's expensive,
and (c) Tier 3 sharding only makes sense if Tier 4 doesn't get us
across 10K.

### Hypothesis

Hot-account contention is fundamental: SERIALIZABLE transfers to the
same account write-skew-conflict no matter how big the pool is. The
40001 retry-cap rate is bounded by the breaker but never goes to
zero, and that ceiling caps single-node throughput at the tenant-
distribution-dependent number we see today (~6.5K with Pareto skew,
likely 9–12K with flatter distributions).

The structural fix is to stop using SERIALIZABLE on the request path
when the application's own invariants are sufficient.

### What "sufficient" means here

The current code has three layers of sum-zero invariant defense:

1. **Domain preflight** — `domain.AssertBalanced()` is called before
   the tx opens. A two-leg transfer that doesn't sum to zero is
   rejected before any DB work.
2. **In-tx assert** — same call repeated inside the SERIALIZABLE tx.
   This is what SERIALIZABLE was buying: protection against a
   concurrent state change that would invalidate the preflight check
   between layers 1 and 3.
3. **Reconciler** — periodic `SUM(amount)=0` invariant query across
   the entire `journal_entries` table. Pages on failure.

The question Tier 4 asks: **does layer 2 add meaningful protection
beyond layer 1 + layer 3?**

For our schema, the answer is largely no:

- Each transfer's two legs are written together in a single tx. A
  concurrent session can't insert "half a transfer" — the
  composite FKs and domain invariants apply per-tx.
- Hot-account write-skew (the case SERIALIZABLE catches) is
  "two transfers reading the same balance, both deciding it's
  sufficient, both committing — final balance is wrong". But in our
  current schema, **journal_entries doesn't store a running balance**.
  Balance is computed on read from the sum of postings. So there's
  no balance value to read-skew.
- The only real risk is currency mismatch between concurrent transfers
  on the same account — and that's enforced at FK level
  (`accounts(id, currency)` composite), not at isolation level.

This is why dropping SERIALIZABLE → READ COMMITTED is plausible
without breaking the invariant. It's also why the change is a
**compliance decision, not a code decision** — the regulator is the
audience for "we don't actually need SERIALIZABLE."

### Implementation plan (assuming green light)

**Step 1 — change isolation level (1 line, 0.5d total)**

`internal/repository/tx.go`:

```go
// Before:
tx, err := BeginTxWithAcquire(ctx, pool, cfg, metrics,
    pgx.TxOptions{IsoLevel: pgx.Serializable})

// After:
tx, err := BeginTxWithAcquire(ctx, pool, cfg, metrics,
    pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
```

The 40001-retry wrapper becomes a no-op for normal traffic (READ
COMMITTED doesn't raise 40001). Keep the wrapper code — costs nothing
and protects against future isolation-level changes.

**Step 2 — drop the in-tx AssertBalanced (defense-in-depth → defense-in-preflight)**

`internal/repository/ledger_repo.go::ExecuteTransfer`:

```go
// Defense-in-depth: balance check BEFORE going to the DB.
if err := transaction.AssertBalanced(); err != nil {
    return nil, err
}
// (remove the duplicate assertion that used to live inside InTx)
```

Already only one call in the current code (the in-tx call was already
removed in async-audit refactor). Verify and document.

**Step 3 — strengthen reconciler (the load-bearing change)**

Without SERIALIZABLE, the reconciler becomes the only defense
against silent corruption. Tighten:

`cmd/reconciler/main.go`:

- Run the `SUM(amount)=0` invariant **per tenant** every minute (not
  just globally hourly). A tenant-level violation is paging-grade.
- Add a **per-account balance reconciliation**: for every account
  that had postings in the last minute, recompute balance from the
  sum and compare to a stored `materialized_balances` value.
  Difference > 0 is paging-grade.
- This implies a `materialized_balances` table — a write-time
  side-effect of postings. Add it as a Tier 4 prerequisite (1d).

**Step 4 — load-test the change (1d)**

Same A/B as Tier 2. Run `transfer-bench.js` at constant 4K offered
for 4 min, before and after the isolation change. Capture:

- p50, p95, p99 latency
- Sustained TPS
- 40001 cap_reached (should drop to ~0)
- Reconciler invariant query results during and after the run

**Step 5 — write the compliance memo (the actual work)**

Document:

1. What SERIALIZABLE was buying us before.
2. What our schema makes unnecessary about it.
3. What detective controls (reconciler) replace preventive controls
   (isolation level).
4. What the "blast radius" is if a bug slips past the preventive
   layer (preflight) — bounded to one minute of traffic, caught by
   the reconciler invariant.
5. Sign-off path (eng lead + compliance + auditor liaison).

### Expected gains

- 40001 cap_reached → ~0% (today: 1–14% depending on load profile)
- p99 latency drop another 10–30 ms (no SERIALIZABLE snapshot
  validation cost)
- Sustained TPS lift 15–30% (depends on hot-account distribution —
  flatter = bigger lift)
- Realistic landing: **8–10K sustained on the same hardware** at
  current Pareto skew. With flatter (production-shaped) traffic,
  10–13K plausible.

### What Tier 4 does NOT solve

- The PG `max_wal_size` ceiling under sustained writes. Eventually
  WAL flush becomes the bottleneck.
- Vacuum lag on `outbox_events` and `journal_entries` hot partitions
  under sustained 10K+. Per-table autovacuum tuning becomes worth
  the time.
- Single-node failure domain. One PG instance is one PG instance.

### Risks and how to mitigate

| Risk | Mitigation |
|---|---|
| Compliance/auditor rejects "no SERIALIZABLE" | Make Tier 4 implementation reversible — feature flag in `internal/config`. Roll back is one config flip. |
| Reconciler latency too high to detect drift before it propagates | Run per-tenant every 60s, page on first violation. Acceptable detection bound. |
| Currency mismatch slips through (FK is the only guard) | Add explicit per-tenant currency-set check in domain preflight as belt-and-braces. |
| Long-running batch jobs see snapshot anomalies | Reconciler reads can pin to a known LSN via `pg_export_snapshot`; document the read pattern. |

### Decision criteria — when to commit to Tier 4

- **Yes if:** compliance team has confirmed the documented argument is
  sufficient, and the load-test A/B shows >15% sustained TPS lift.
- **No if:** auditor wants SERIALIZABLE on the books for any reason.
  Going around them invalidates the certification posture and
  undoes a year of paperwork.

---

## Tier 3 — split or shard  (only after Tier 4)

### Status

NOT STARTED. Gated on Tier 4 measurement. Premature sharding doubles
ops surface for a problem that may not exist after Tier 4.

### Hypothesis

Past ~10K sustained TPS on a single PG (post-Tier-4 ceiling), the
next constraints are physical: WAL fsync rate, lock manager
contention on shared-relation pages, autovacuum on hot partitions.
None of those tune away. The fix is to spread writes across multiple
write-side authorities.

### Options, ranked by cost vs payoff

#### Option A — Read replica (1w, ~30% read-path offload)

Cheapest first move. Doesn't help write TPS but offloads
`GetTransaction` and `GetTransactionByIdempotencyKey` reads.

Implementation:

1. Add `replicaPool` — third pgxpool, points at PG hot standby.
2. New env: `DATABASE_REPLICA_URL`.
3. Route read methods on `LedgerRepo` to `replicaPool`. Write methods
   stay on `requestPool`.
4. Replication lag check on each read; fall back to primary if
   lag > 100ms (or accept stale read with a header).
5. Reconciler reads also move to replica.

**When to do this:** when read traffic is ≥20% of total request
volume AND the read pool is competing with write pool for connection
slots. Currently we don't have that signal — it's a Tier-3 prerequisite
work, not a standalone improvement.

#### Option B — Citus / pg_partman tenant_id sharding (3–4w)

Logical sharding within one Postgres cluster. Each tenant_id maps to
a shard; queries are routed by Citus's distributed planner.

Implementation:

1. Migrate `journal_entries`, `transactions`, `audit_log`,
   `audit_pending`, `outbox_events`, `accounts` to distributed tables
   keyed by `tenant_id`.
2. Update repository SQL — Citus rewrites `WHERE tenant_id=X` queries
   to hit one shard; queries without `tenant_id` go to all shards
   (slower, mostly admin/reconciler).
3. Ensure all hot-path queries include `tenant_id` in the predicate.
   The current schema mostly does (via composite FKs), but
   `audit_pending` worker drain SQL needs review.
4. Migration plan: dual-write for 14 days, validate sums match, cut
   over.

**Pros:** linear scale with shard count if tenant skew is manageable.
Audit chain stays per-tenant — already aligned with Citus's
distribution-key model.

**Cons:** cross-tenant queries get expensive. Citus is operationally
non-trivial; not a "drop in" change. Reconciler queries that scan
across tenants need to be re-shaped or accept the multi-shard fan-out
cost.

**When to do this:** if production shows tenant distribution is
suitable (no tenant > 10% of total writes; otherwise the hot tenant
hits the same single-shard ceiling we have today).

#### Option C — App-level shard router (6w)

One PG cluster per tenant tier. The BFF routes requests to the right
ledger-svc instance based on tenant_id.

**Pros:** maximum control. Per-tier hardware sizing. Clear blast
radius — one tenant's PG outage doesn't affect others.

**Cons:** maximum ops surface. N PG clusters to manage. Cross-tenant
operations (BI, fraud detection) require a separate read-side warehouse.

**When to do this:** when individual tenants need their own physical
isolation (regulatory or contractual), or when Citus's cross-shard
penalty becomes unbearable.

#### Option D — TigerBeetle (8w+)

Move the ledger to TigerBeetle, a purpose-built engine. ledger-svc
becomes a thin gRPC adapter in front of TigerBeetle's binary
protocol.

**Pros:** claims 1M+ TPS. Specialized for exactly our workload.

**Cons:** different operational model — TigerBeetle isn't general
SQL; reconciler queries, ad-hoc reads, BI integrations all need
re-think. Audit story is different (TigerBeetle has its own
durability model).

**When to do this:** if we've outgrown sharded Postgres AND the
operational maturity is there.

#### Option E — Distributed SQL (Cockroach / Yugabyte / Spanner) (8w+)

Replace PG with a distributed-SQL engine. Get global consistency +
horizontal scale, accept higher per-tx latency.

**When to do this:** if multi-region active-active becomes a
requirement. For single-region scale, this is overkill — Citus is
cheaper.

### Recommendation

If we end up doing Tier 3 at all:

1. Start with **Option A (read replica)** as a prerequisite — needed
   regardless of which write-side scaling option we pick.
2. Then **Option B (Citus)** if tenant distribution is suitable. The
   most common path in our position.
3. Skip directly to **Option D (TigerBeetle)** only if the scale
   target is 50K+ TPS AND we have ops capacity for a non-SQL system.
4. **Option C and E** are unlikely to be the right answer for our
   shape.

---

## Tier 5 — async-commit  (structural rewrite, separate doc)

If Tier 4 + Tier 3 still don't get us to the throughput target, the
last incremental move is exhausted and we're into the structural
rewrite track. That's covered in `scaling-v2-async-ledger.md`.

**This is not the next move.** It's the move-after-the-move-after-next.
The doc exists so we don't have to start from scratch when (if) we
get there.

---

## Realistic order

```
1. Tier 4 (next):       compliance + 0.5d code + 1d load-test + memo
                        ↓ measure result
2a. ≥ 10K sustained?   → STOP. Project target (5K+) exceeded by 2×.
                        Document the win. No further work.
2b. 8–10K?             → Tier 3 Option A (read replica) for headroom.
                        Stop there if reads are the new pressure.
2c. < 8K?              → Tier 3 Option B (Citus) — write-side sharding.
                        Plan for 6+ months of migration work.
2d. < 8K AND business
    case for >50K?     → Tier 5 / v2 (scaling-v2-async-ledger.md).
                        Months of work, async client UX trade-off.
```

The single most expensive scaling mistake in this category is doing
Tier 3 or Tier 5 before measuring Tier 4. Don't.

## What's actually next

**Tier 4 isolation review.** Specifically:

1. Eng lead sponsors a compliance conversation about whether
   SERIALIZABLE is load-bearing for our certification posture.
2. If the answer is "yes, regulator wants it" — Tier 4 is dead, skip
   to Tier 3 Option A and accept the 6.5–10K ceiling.
3. If the answer is "no, detective controls suffice" — ship the 0.5d
   code change behind a feature flag, run the A/B, write the memo.
4. **Then** measure. The measurement decides whether we're done,
   doing Tier 3, or eventually doing v2.

The v2 doc (`scaling-v2-async-ledger.md`) is **a contingent plan**,
not a recommendation. It's there so that if we end up needing it,
we don't waste two weeks re-deriving the architecture from scratch.
The pattern in that doc is the production-grade version of the
async-commit + Kafka command log shape — the simplified diagram I
was reviewed on was missing 4–5 load-bearing pieces (audit chain,
durable idempotency, sum-zero enforcement, reservation lifecycle,
two-account partition strategy), which the v2 doc puts back.

If anyone reads this in 6 months and is tempted to skip ahead to v2:
**measure Tier 4 first.** The honest answer for most teams in our
position is that Tier 4 + read replica is enough, and v2 is
over-engineering they'll regret.
