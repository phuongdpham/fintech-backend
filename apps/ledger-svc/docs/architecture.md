# ledger-svc — architecture (current)

Snapshot of HEAD (`43d86d4`). Reflects what's actually shipped: async
audit, `pgx.SendBatch` on the write path, split request/worker
pgxpools, per-(tenant,method) circuit breaker, Pareto-skewed load
profile validated to ~2.4K sustained TPS at p50=297ms on a
workstation, with the 5K+ project target met on bigger iron.

## System view

```mermaid
flowchart TB
  classDef ext fill:#fafafa,stroke:#888,color:#333
  classDef pg  fill:#e8f5e9,stroke:#2e7d32
  classDef rd  fill:#ffebee,stroke:#c62828
  classDef rp  fill:#fff8e1,stroke:#ef6c00
  classDef rec fill:#f3e5f5,stroke:#6a1b9a
  classDef tbl fill:#f1f8e9,stroke:#558b2f,stroke-dasharray:3 3

  subgraph clients["Edge"]
    direction LR
    BFF["BFF / gRPC client<br/>forwards x-tenant-id<br/>x-actor-subject<br/>x-actor-session"]
    K6["k6 load driver"]
  end
  class BFF,K6 ext

  subgraph svc["ledger-svc — :9090 gRPC, :9100 Prometheus"]
    direction TB

    subgraph transport["Transport"]
      INTRC["Interceptor chain<br/>Recovery · RequestID · Logging<br/>EdgeIdentity (parse tenant/actor)<br/>RateLimit (per-tenant token bucket)<br/>Admission (1000 in-flight cap)<br/>otelgrpc StatsHandler"]
      HND["LedgerHandler<br/>Transfer · GetTransaction"]
    end

    subgraph use["Usecase"]
      TUC["TransferUsecase<br/>1. validate<br/>2. AssertBalanced (preflight)<br/>3. Redis SETNX idempotency<br/>4. LedgerRepo.ExecuteTransfer<br/>5. mark COMPLETED / FAILED"]
    end

    subgraph repo["Repository"]
      direction LR
      LR["LedgerRepo<br/>InTx: SERIALIZABLE + 40001 retry<br/>per-(tenant,method) breaker<br/>pgx.Batch — 5 INSERTs / 1 RTT"]
      OR["OutboxRepo<br/>FOR UPDATE SKIP LOCKED"]
      AR["AuditDrainRepo<br/>per-tenant SHA-256 chain<br/>single-writer drain"]
    end

    subgraph workers["In-process workers"]
      direction LR
      OWP["OutboxWorker<br/>4 goroutines<br/>poll · publish · mark PUBLISHED"]
      AWP["AuditWorker<br/>1 goroutine · batch=1000<br/>poll=50ms · idle=500ms"]
    end

    subgraph pools["pgxpools (split by role)"]
      direction LR
      REQ["requestPool<br/>500 conns · 500ms acquire<br/>fail-fast on saturation"]
      WRK["workerPool<br/>16 conns · 30s acquire<br/>patient drain"]
    end

    KP["Kafka producer<br/>idempotent · acks=all"]
    OBS["Observability<br/>OTel propagator + optional OTLP<br/>Prom: pool, breaker, audit lag,<br/>db_tx_outcomes, admission, ratelimit"]
  end

  subgraph data["Data plane"]
    direction TB
    PG[("PostgreSQL 18<br/>max_connections=700<br/>shared_buffers=4GB<br/>uuidv7 native")]
    RD[("Redis 8<br/>SETNX idempotency fast-path<br/>fail-open on fault")]
    RP[("Redpanda — Kafka API<br/>topic: fintech.ledger.transactions")]
  end
  class PG pg
  class RD rd
  class RP rp

  subgraph schema["Postgres schema (selected)"]
    direction TB
    AC["accounts<br/>UNIQUE (id, currency)<br/>UNIQUE (id, tenant_id)"]
    TX["transactions<br/>UNIQUE (tenant_id, idempotency_key)"]
    JE["journal_entries<br/>RANGE month → HASH tenant 16<br/>composite FKs lock currency + tenant"]
    OB["outbox_events<br/>partial idx WHERE status=PENDING"]
    AP["audit_pending<br/>HASH tenant 16<br/>queue: insert + delete only"]
    AL["audit_log<br/>RANGE month → HASH tenant 16<br/>per-tenant SHA-256 chain"]
  end
  class AC,TX,JE,OB,AP,AL tbl

  PG --- AC
  PG --- TX
  PG --- JE
  PG --- OB
  PG --- AP
  PG --- AL

  REC["reconciler (separate binary)<br/>SUM(amount)=0 invariant query<br/>stale outbox sweep<br/>cron / daemon"]
  class REC rec

  BFF -->|gRPC| INTRC
  K6  -->|gRPC| INTRC
  INTRC --> HND
  HND --> TUC
  TUC <-->|idempotency state| RD
  TUC --> LR

  LR -->|"SendBatch (1 RTT)"| REQ
  REQ --> PG

  AWP --> AR --> WRK
  OWP --> OR --> WRK
  WRK --> PG

  OWP -->|publish PENDING batch| KP
  KP  --> RP

  REC -->|admin path| PG
```

## Request hot path — `Transfer` RPC

```mermaid
sequenceDiagram
    autonumber
    actor C as BFF / client
    participant GW as gRPC interceptors
    participant UC as TransferUsecase
    participant RD as Redis
    participant LR as LedgerRepo
    participant PG as Postgres
    participant AW as AuditWorker
    participant OW as OutboxWorker
    participant K as Redpanda

    C->>GW: Transfer (x-tenant-id, x-actor-subject, x-actor-session)
    GW->>GW: Recovery → RequestID → Logging
    GW->>GW: EdgeIdentity → RateLimit → Admission
    GW->>UC: handler.Transfer
    UC->>UC: validate + AssertBalanced (preflight)
    UC->>RD: SETNX idempotency_key = PROCESSING

    alt replay (key already PROCESSING/COMPLETED)
      RD-->>UC: existing state
      UC-->>C: prior Transaction (Replayed=true)
    else first time
      UC->>LR: ExecuteTransfer

      LR->>PG: BEGIN ISOLATION LEVEL SERIALIZABLE
      Note over LR,PG: pgx.Batch + tx.SendBatch — single round-trip
      LR->>PG: INSERT transactions<br/>INSERT journal_entries × 2<br/>INSERT outbox_events<br/>INSERT audit_pending
      PG-->>LR: 5 results pipelined back
      LR->>PG: COMMIT
      PG-->>LR: ok / 40001 (retry budget)

      LR-->>UC: *domain.Transaction
      UC->>RD: SET state = COMPLETED
      UC-->>C: Transaction
    end

    rect rgba(200,230,201,0.35)
      Note over AW,K: Out-of-band, separate workerPool, no client deadline
      AW->>PG: SELECT audit_pending FOR UPDATE SKIP LOCKED
      AW->>PG: per-tenant SHA-256 chain<br/>INSERT audit_log, DELETE pending
      OW->>PG: SELECT outbox_events WHERE status='PENDING'<br/>FOR UPDATE SKIP LOCKED
      OW->>K: Produce (idempotent, keyed by aggregate_id)
      OW->>PG: UPDATE outbox_events SET status='PUBLISHED'
    end
```

## Load-bearing invariants

These are enforced by the architecture above; touching any of them
needs the `scaling-roadmap-10k.md` trade-off conversation, not just a
code change.

1. **Double-entry sum-zero.** Every committed transaction's journal
   entries sum to zero. Three layers: `domain.AssertBalanced`
   (preflight), the same assert inside the SERIALIZABLE tx, and the
   reconciler's `SUM(amount)=0` invariant query.
2. **Idempotency.** `transactions.idempotency_key` is composite-UNIQUE
   per tenant. Redis SETNX is the fast-path; on Redis fault the
   usecase fails open and PG's unique constraint catches the
   duplicate.
3. **Atomicity of ledger + outbox + audit_pending.** All five INSERTs
   commit in one Postgres tx. Ledger durability ⟺ outbox event
   durability ⟺ audit row enqueued. The audit *chain* is async (worker
   forwards `audit_pending` → `audit_log`), but the *enqueue* is in
   the same tx — there is no committed ledger row without a queued
   audit row.
4. **Money is `NUMERIC(19,4)`** end-to-end. Wire format is
   decimal-as-string (proto3 `string amount`).
5. **Currency is locked to the account.** Composite FK
   `(account_id, currency) → accounts(id, currency)` makes a
   non-native-currency journal entry unrepresentable.

## Why two pgxpools

Splitting the connection pool by role (`requestPool` vs `workerPool`)
prevents two failure-mode pairs from interfering:

- **Request path saturating starves the workers.** Without a split, a
  request burst at 250–500 conn ceiling would block outbox + audit
  drain on `Acquire`. Result: events back up in `outbox_events` and
  `audit_pending`, audit lag grows, alerts fire — even though PG
  itself is fine.
- **Slow Kafka publish starves the request path.** OutboxWorker holds
  a conn during `ProduceBatch`. If Kafka blips, all 4 workers can hold
  their conns for seconds. With a shared pool, request-path acquires
  start timing out at 500 ms and surface as `ResourceExhausted` to
  callers — cascading a transient Kafka issue into the user-facing
  hot path.

The split costs a few extra connections (`16 + 500` vs `500`) and
keeps each pool's failure mode local.

## Why async audit

Original design had the audit row + per-tenant SHA-256 chain compute
*inside* the SERIALIZABLE request tx. That introduced a per-tenant
read of `MAX(audit_log) WHERE tenant_id=$1` on every commit, which
became the throughput ceiling at ~5K TPS — hot-tenant chain heads
serialized concurrent commits.

Current design: request tx writes a tiny `audit_pending` row (no chain
state). A single-writer `AuditWorker` drains `audit_pending` → chains
SHA-256 → inserts to `audit_log` → deletes the pending row, all in
one READ COMMITTED tx per batch. Single-writer is by design — the
chain head needs a single owner; multi-worker would require an
advisory lock per tenant and we already paid that cost once and
reverted it (commit `5d291e9`).

Trade-off: `audit_log` is no longer atomic with the ledger row.
Bounded operationally by `audit_pending_lag_seconds`; in load tests
the bound has been 0–1 s.

## File map

| Concern | File |
|---|---|
| gRPC server + interceptor wiring | `internal/transport/grpc/server.go` |
| Edge identity, rate limit, admission | `internal/transport/grpc/interceptors/` |
| Usecase orchestration | `internal/usecase/transfer.go` |
| Repository write path (SendBatch) | `internal/repository/ledger_repo.go` |
| Audit drain (worker-side repo) | `internal/repository/audit_drain.go` |
| Audit chain hashing | `internal/audit/hasher.go` |
| Outbox repo | `internal/repository/outbox_repo.go` |
| pgxpool wrapper + retry/breaker | `internal/repository/{pool,tx,retry,breaker}.go` |
| Audit worker loop | `internal/infrastructure/audit_worker.go` |
| Outbox worker loop | `internal/infrastructure/outbox_worker.go` |
| Kafka producer | `internal/infrastructure/kafka_producer.go` |
| Redis idempotency store | `internal/infrastructure/idempotency_redis.go` |
| Domain (entities, ports, sentinel errors) | `internal/domain/` |
| Config (env contract) | `internal/config/config.go` |
| Observability (OTel + Prom) | `internal/observability/` |
| Migrations | `migrations/001_init_ledger.up.sql`<br/>`migrations/002_audit_pending.up.sql` |
| Reconciler binary | `cmd/reconciler/main.go` |
| Server binary (DI wiring) | `cmd/server/main.go` |
| Load harness | `cmd/loadtest/`<br/>`load/k6/{transfer,transfer-bench}.js` |

## What's not in this picture

- **BFF (GraphQL)** — separate service, will land in front of
  ledger-svc + future gateway-svc. Today direct gRPC clients (k6,
  smoke tests) mint the headers themselves.
- **JWKS-backed JWT verifier** — pending. Today's edge auth trusts
  forwarded headers (`DevTokenVerifier`); production needs a real
  verifier.
- **Multi-replica scale.** Architecture supports it (outbox uses
  `FOR UPDATE SKIP LOCKED`; audit worker singleton stays singleton via
  the chain head ownership invariant), but untested under load.
- **Sharding** (Citus / app-level shard router). The roadmap calls
  this Tier 3 — month-2 territory, only after Tier 4 (isolation
  review) measures out.
