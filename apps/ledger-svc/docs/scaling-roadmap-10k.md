# Scaling roadmap — getting from 6.5K to 10K+ TPS

Companion to `load-test-2026-05-01.md`. The current ceiling is the
request pool + per-account contention, not audit chain. This document
ranks the next moves by cost and expected payoff, then closes with an
honest read on where 10K+ TPS sits in industry practice.

## Where we are

```
async audit:           ✅ shipped (112c849)
pool conn-release fix: ✅ shipped (dba9d5d)
sustained ceiling:     6.5K iters/s on single PG, single replica
next bottleneck:       request pool 250/250 + 40001 retries on hot accounts
```

## Tiered improvements

### Tier 1 — pool + PG tuning (hours, no code change)

**Hypothesis:** the current 250-conn pool + PG `max_connections=400` is
the proximate ceiling. PG can absorb more if asked.

| Change | Cost | Expected gain |
|---|---|---|
| `DATABASE_MAX_CONNS` 250 → 500 | env edit | +30–50% if PG holds |
| PG `max_connections` 400 → 700 | postgresql.conf | required to back the pool bump |
| `effective_io_concurrency` 200 → 256 | postgresql.conf | helps WAL + index flush |
| `synchronous_commit` `on` → `remote_apply` (replica) or `off` (only with replica + manual recovery) | postgresql.conf | 2–3× write throughput, **trades durability** |
| Bump `wal_buffers` to 64MB | postgresql.conf | smooths WAL pressure at peak |

`synchronous_commit = off` is the big lever but you lose at-most-N-ms
of committed data on a crash. Acceptable only if the upstream system
(BFF) can replay from idempotency keys. Ours can.

**Re-test exit criteria:** sustained 8K iters/s for 5 min, 40001 ratio
< 15%, audit lag < 1 s. If we plateau before 8K, the limit is PG itself
(WAL flush, index lock contention) and Tier 2 starts paying off.

### Tier 2 — reduce per-tx work (days, focused code)

The faster each tx commits, the more TPS one pool sustains.

| Change | Cost | Expected gain |
|---|---|---|
| `PgxPool.SendBatch` for the 5 INSERTs (txn + 2 legs + outbox + audit_pending) | 0.5d | 1 RTT instead of 5 — saves ~10ms p99 over LAN |
| Server-side prepared statements (`pgx` Describe/Bind cache) | 0.5d | parse cost off the hot path, ~3–5% savings |
| Drop `RETURNING id` on audit_pending insert (the worker doesn't need it) | already done | — |
| Re-validate that `goccy/go-json` and proto-bytea are still wins (they were) | 1d | re-measure |
| Batch outbox INSERTs across N transfers (windowed coalescing) | 2d | needs careful idempotency reasoning — risky |

The first item is the cheapest big win. pgx's batched protocol pipelines
all 5 statements into a single round-trip; under the load profile (LAN,
1ms RTT) we'd save ~5ms per tx, which at 500 conns shifts the pool
ceiling to ~12K TPS without sharding.

### Tier 3 — split or shard (weeks, structural)

Past about 8K TPS on a single PG with our schema, the next constraint
is contention on shared resources: the WAL, the per-relation lock
manager, autovacuum on a few hot pages. These don't tune away.

| Change | Cost | Expected gain |
|---|---|---|
| Read replica for `GetTransaction` / `GetTransactionByIdempotencyKey` | 1w | offloads ~30% of the read path |
| Citus / pg_partman on tenant_id (logical sharding within one cluster) | 3-4w | linear scale with shard count, IF tenant skew is manageable |
| Move ledger to TigerBeetle (purpose-built ledger engine) | 8w+ | step change — claims 1M+ TPS, but it's a separate system you operate |
| App-level shard router (one PG cluster per tenant tier) | 6w | most flexibility, most ops surface |
| CockroachDB / YugabyteDB / Spanner | 8w+ | global consistency, but write latency goes up |

The BFF is the natural place to put the shard router (it already knows
the tenant). The ledger-svc itself stays single-shard from its own
viewpoint; sharding is an infra concern.

### Tier 4 — kill 40001 at the source (weeks, schema)

Hot-account contention is fundamental: SERIALIZABLE transfers to the
same account write-skew-conflict no matter how big the pool is.

| Change | Cost | Expected gain |
|---|---|---|
| Per-account counter table updated by ON CONFLICT DO UPDATE (instead of journal_entries scan) | 1w | drops the read-skew window |
| Drop SERIALIZABLE → READ COMMITTED for the request tx (already balanced via domain.AssertBalanced) | 0.5d | -100% on 40001, but loses cross-tx invariant proof | 
| Pre-aggregate writes via in-memory queue per account (single-writer per hot account) | 2-3w | removes contention entirely; introduces a SPOF you have to engineer around |
| Move balance updates out of the request tx (fire-and-forget into a per-account stream) | 4w | architectural shift; adds settlement latency |

Dropping SERIALIZABLE is the cheapest answer and works **only if** you
trust `domain.AssertBalanced` at preflight + the audit chain to detect
any anomaly downstream. We do — the question is whether the regulator
does. Document the choice.

## Realistic order

1. Tier 1 (today): bump pool + PG conns, re-run, see if we cross 8K.
2. Tier 2 SendBatch (week 1): biggest mechanical lever still available.
3. Tier 4 isolation review (week 2): decide whether SERIALIZABLE is
   really required for the request path. The audit chain plus the
   reconciler invariant query already cover the failure modes
   SERIALIZABLE was buying us.
4. Tier 3 sharding (month 2+): only after we've measured the post-Tier-2
   ceiling. Premature sharding doubles ops surface for a problem that
   may not exist yet.

## Does anyone actually run a ledger at 10K+ TPS?

Honest read of public information:

| System | Ledger TPS (public/estimated) | Architecture notes |
|---|---|---|
| Visa | ~1.7K avg, claims **24K capacity** | Authorization peak is high, settlement ledger is batch — not the same number |
| Stripe | "thousands per second" globally; internal ledger ~5–15K at peak (estimate) | Heavily sharded by tenant; not single-PG |
| PayPal | ~700–1,000 sustained | |
| Adyen | Low thousands | |
| Square / Block | Hundreds | |
| Robinhood / IBKR | Matching engine = high; ledger settlement = batch | Conflated often |
| Binance | Matching ~1.4M/s; ledger settlement much lower | Same conflation |
| RTGS systems (FedNow, TARGET2) | Hundreds, with strict per-tx SLA | Throughput is not the goal |
| TigerBeetle (purpose-built) | Claims 1M+ in benchmarks | Specialized engine, not Postgres |

**Takeaway**:

- 10K+ TPS sustained on a single Postgres for a strongly-consistent
  double-entry ledger is unusual. Production systems that need that
  throughput almost always shard (Stripe-style multi-tenant
  partitioning) or move to a specialized engine (TigerBeetle, custom
  KV with append-only logs, etc.).
- Most fintech ledgers in production sit at 100–2,000 TPS sustained.
  The 10K target in this exercise is at the upper end of what teams
  build before reaching for sharding or a specialized engine.
- Where 10K+ does exist, it's almost always one of:
  - **Sharded Postgres** (Citus or app-level router)
  - **CockroachDB / YugabyteDB / Spanner** (distributed SQL, accepts
    higher per-tx latency for horizontal scale)
  - **TigerBeetle** (designed for this exact problem; trades general
    SQL for specialized ledger semantics)
  - **Event-sourced + projection** (the ledger is the append-only
    event log; balances are projections rebuilt as needed)

So the engineering exercise here — getting one PG node to 10K — is
genuinely valuable as a stress test of the architecture, but mirrors
the ceiling most teams hit *before* moving to one of the patterns
above. Hitting 8K on commodity hardware with this schema is already a
respectable single-node number; the path past that is structural, not
tuning.

## Recommended next decision

Given (a) async-audit shipped, (b) pool fix shipped, (c) we're at 6.5K
sustained on one node, the cheapest informative next step is:

- **Tier 1 today** (pool 500, PG 700) — answers whether the proximate
  ceiling is conn count or PG itself.
- **Tier 2 SendBatch this week** — independent of Tier 1, biggest
  mechanical lever before structural changes.
- **Stop and re-evaluate at 8K sustained.** Past 8K the right move is
  almost certainly to start the sharding conversation rather than
  squeeze another 20% out of one box.

Premature sharding is the most common scaling mistake in this
category. Single-PG to 8K is an honest answer for a v1; the
v2 conversation is about tenant-aware routing, not more knobs.
