# Scaling ledger-svc to 10K+ TPS — issue plan

Source conversation: 2026-05-01 architecture review. Items (1)–(4) of the
plan broken into 8 individual GitHub issues with concrete acceptance
criteria. Item (5) (Citus / distributed-SQL fork) deliberately deferred
until 1–4 measure out.

Dependencies: Issue 1 must land before any of 2–8 can be measured for
success. Issues 2/3/4 are independent (parallelizable). Issues 5/6/7 are
independent. Issue 8 is gated on confirming pre-release migration
consolidation still applies at PR time.

---

## Issue 1 — Build sustained-load harness + capture 10K TPS baseline

**Context.** Every scaling decision after this depends on knowing where
the current single-PG design actually breaks, what the breakage looks
like, and which symptom appears first. Speculation gives the wrong
answer surprisingly often.

**Topology — hybrid Go seed + k6 run.** k6 owns load generation
(open-loop scheduler, HdrHistogram, Prometheus remote-write, ramp DSL,
distributed scaling via k6 Operator). A small Go binary owns
deterministic seeding and emits a fixtures file that k6 reads — keeps
UUID derivation in lock-step with server types in the same repo,
without porting SHA-256 derivation to JS. Generator runs in its own
process so its GC cannot pollute server pprof on the same host.

**Scope.**
- `cmd/loadtest/main.go seed` (Go): creates N tenants × M accounts via
  direct pgx insert with deterministic SHA-256-derived UUIDs. Idempotent
  via `ON CONFLICT DO NOTHING`. Writes
  `apps/ledger-svc/load/fixtures.json` with the resulting (tenant,
  account_id) tuples for k6.
- `apps/ledger-svc/load/k6/transfer.js`: k6 scenario using xk6-grpc.
  Reads fixtures, picks tenant + accounts with Pareto skew (80/20),
  builds Transfer requests with `x-tenant-id` / `x-actor-subject`
  headers. Constant-arrival-rate executor for honest open-loop pacing.
- `apps/ledger-svc/load/docker-compose.yml`: k6 + Prometheus +
  Grafana sidecar stack, separate from the `make stack-up` infra.
  Grafana dashboards (latency p50/95/99/999, RPS achieved vs target,
  gRPC code breakdown) committed at `load/k6/dashboards/`.
- Ramp: 1K → 12K TPS over 30 min via k6 stages, plus 5-min sustained
  at the identified ceiling.
- `make -C apps/ledger-svc loadtest-seed` and `make … loadtest-run`
  Make targets. Runs against `make stack-up`'d local stack.

**Acceptance criteria.**
- Reproducible: a fresh clone runs
  `make stack-up && make loadtest-seed && make loadtest-run` and gets a
  result. Single-machine, no cloud dependency.
- k6 emits Prometheus metrics consumable by the local Prom; Grafana
  shows the four core panels.
- Report at `apps/ledger-svc/docs/baseline-2026-05.md` captures: ceiling
  TPS, p50/p95/p99/p999 at each ramp step, top-3 bottlenecks with
  profiler evidence (server pprof snapshots taken during the run),
  recommendation on which of issues 2–10 to prioritize.
- Run is repeatable: ceiling within ±10% across two back-to-back runs.

**Out of scope.** Production-hardware-equivalent run (separate, needs
real infra). Long-duration soak (endurance, separate problem).
k6 Operator / distributed load generation (single-box single-k6 is
enough until we hit one box's NIC ceiling — flagged as future work).

---

## Issue 2 — Tune pgxpool for 10K TPS sustained write rate

**Context.** Default pool size (`DATABASE_MAX_CONNS=20`) is too small
for the target throughput. Symptom is invisible to PG: latency spikes
attributed to "the database" actually live in the application's pool
wait queue.

**Scope.**
- Make pool sizing config-driven via `internal/config` (no `os.Getenv`
  outside that package).
- Defaults updated: `MaxConns=120`, `MinConns=20`,
  `MaxConnIdleTime=30m`, `MaxConnLifetime=1h`, `AcquireTimeout=500ms`.
- Sizing formula documented in `.env.example`:
  `MaxConns ≥ TPS × p99_tx_seconds × 2`.
- New metric: `pgxpool_acquire_wait_seconds` histogram,
  `pgxpool_acquired_conns` gauge.

**Acceptance criteria.**
- Pool knobs exposed via env: `DATABASE_MAX_CONNS`, `DATABASE_MIN_CONNS`,
  `DATABASE_ACQUIRE_TIMEOUT`, `DATABASE_CONN_MAX_LIFE`,
  `DATABASE_CONN_MAX_IDLE`. Loaded by `caarlos0/env/v11`, documented in
  `.env.example`.
- At 10K TPS sustained (issue 1 harness), zero `pool acquire timeout`
  errors over a 5-min window.
- `pgxpool_acquire_wait_seconds` p99 < 5 ms at 10K TPS sustained.
- `AcquireTimeout` strictly less than the default client deadline;
  misconfiguration trips a startup error, not a runtime surprise.

**Out of scope.** Per-tenant pool quotas (issue 5). PgBouncer (separate
architectural call — when SERIALIZABLE retry is in play,
transaction-pool mode is wrong).

---

## Issue 3 — Set GOMEMLIMIT + GOGC for ledger-server

**Context.** At ~10–12 KB allocated per transfer × 10K TPS = 100+ MB/s
allocation rate, Go's GC assist time becomes a meaningful slice of
request latency. GOMEMLIMIT (Go 1.19+) caps heap growth and smooths GC
behavior; GOGC=200 trades memory for fewer cycles. Both are
container-aware via cgroup info.

**Scope.**
- Apply GOMEMLIMIT from cgroup memory limit in `cmd/ledger-server/main.go`.
- Default `GOGC=200`.
- Both overridable via env (`GOMEMLIMIT`, `GOGC`) for k8s-managed
  scenarios.
- Log applied values at startup.

**Acceptance criteria.**
- Service logs `applied GOMEMLIMIT=<bytes>` and `applied GOGC=<value>` at
  startup.
- Under 10K TPS sustained: pprof CPU profile shows
  `runtime.gcAssistAlloc` < 5%.
- Under 12K TPS sustained for 5 min: heap stays within 90% of GOMEMLIMIT,
  no OOM, GC frequency < 10 cycles/sec.
- If `GOMEMLIMIT` env is set, it overrides cgroup-derived value.

**Out of scope.** `sync.Pool` for hot allocations (created only if pprof
demands it after this lands).

---

## Issue 4 — Swap shopspring/decimal → cockroachdb/apd on hot path

**Context.** `shopspring/decimal` heap-allocates on every arithmetic op.
At ~6 decimal ops per `Transfer` × 10K TPS = 60K allocations/sec on
money math alone — measurable GC pressure even after issue 3.
`cockroachdb/apd` does in-place arithmetic via `*Decimal` receivers;
benchmarked allocation reduction is consistently 70–85%.

**Scope.**
- Replace `decimal.Decimal` with `*apd.Decimal` in
  `internal/domain/money.go` and propagate through usecase / repo /
  handler.
- One canonical `apd.Context` (precision=22, rounding=HalfEven) matching
  `NUMERIC(19,4)` semantics — define once in domain, no per-call
  construction.
- Decimal ↔ proto3 string round-trip: keep on-the-wire format
  byte-identical to current. No-op at the API boundary.
- Add benchmarks: `BenchmarkTransferRequest_Allocs` before and after.

**Acceptance criteria.**
- All existing decimal/money/transaction tests pass unchanged.
- New table-driven tests cover: precision boundary
  (`NUMERIC(19,4)` max, ±9999999999999999.9999), HalfEven rounding at
  .5 boundary, malformed input rejection, leading/trailing zeros,
  scientific-notation refusal.
- Bench shows ≥ 50% reduction in `B/op` and ≥ 50% in `allocs/op` for
  `Transfer` end-to-end.
- API contract unchanged: same string format in/out.

**Out of scope.** Switching to `int64` cents internally.

---

## Issue 9 — Swap encoding/json → goccy/go-json on hot path

**Context.** Audit `after_state` and outbox `payload` JSON-marshal on every
write. At 10K TPS that's ~20K marshals/sec — `encoding/json`'s
reflection + per-call allocations are measurable GC pressure even after
issues 3 and 4. `goccy/go-json` is a drop-in replacement with the same
API, ~30–40% fewer allocations and ~2× marshal throughput, no behavior
change for our shapes.

**Scope.**
- Replace `encoding/json` imports with `github.com/goccy/go-json`
  across the service. API is identical.
- Verify wire-format byte-identity for the audit canonical-ization path.
  (Audit chain hashing today uses a hand-rolled length-prefixed binary
  format — not JSON — so the hash chain is unaffected. Confirm by
  re-running audit chain tests.)
- Bench `BenchmarkTransfer_Allocs` before/after.

**Acceptance criteria.**
- All existing tests pass.
- Audit hash-chain verifier passes against a chain produced before the
  swap and a chain produced after — proves the canonical bytes path is
  truly JSON-independent.
- Bench shows ≥ 30% reduction in `allocs/op` on `Transfer` end-to-end.
- No behavior change at the API boundary: same JSON output for outbox
  payloads.

**Out of scope.** `easyjson` code-gen for specific types (queue if
profile after this lands still shows JSON marshal in the top 5
allocators). `bytedance/sonic` (JIT, x86-64-favored — defer until
multi-arch story is settled).

---

## Issue 10 — Move outbox payload from JSONB to proto bytea

**Context.** Outbox payload is JSON-marshaled in the request path,
JSON-parsed by the worker. We already have proto-gen for the wire shape
that consumers expect on Kafka. Storing the proto-serialized bytes
directly in the outbox row eliminates the marshal+parse cycle entirely
on the write path; the worker forwards bytes to Kafka with no
re-encoding. JSONB's "easy to `SELECT … WHERE payload->>'type'`" win
loses to the throughput argument at 10K TPS.

**Scope.**
- Define proto messages for outbox events
  (`fintech.ledger.v1.LedgerEvent` and concrete event types) if not
  already in proto-gen. Reuse existing types where they fit.
- Migrate `outbox_events.payload` `JSONB → BYTEA`. Add
  `event_type VARCHAR(80)` if `aggregate_type` doesn't already
  disambiguate.
- Update repository write path to `proto.Marshal` instead of
  `json.Marshal`.
- Update outbox worker to forward raw bytes to Kafka producer (no
  decode-then-encode).
- Pre-release: apply via re-init migration (consistent with the
  consolidation rule). At PR time, confirm that's still the right call.

**Acceptance criteria.**
- Outbox write path: no JSON marshal in the per-request CPU profile.
- Outbox worker drain path: zero JSON parse work in the per-event path.
- Kafka payload bytes are valid `LedgerEvent` proto-decodable by an
  external consumer (verified via a small consumer-side decode test).
- `BenchmarkTransfer_Allocs` shows further reduction beyond issue 9
  (specifically the bytes attributable to outbox-payload marshal).
- Schema invariants still hold: outbox writes commit in the same tx as
  ledger writes; FOR UPDATE SKIP LOCKED still drains correctly.

**Out of scope.** Switching `audit_log.{before_state, after_state}` to
bytea — audit JSONB is intentional for compliance jq access; the
throughput cost there is the right trade-off. Versioning the proto
schema for backward compat (pre-release; greenfield consumer set).

---

## Issue 5 — Per-tenant rate limit interceptor

**Context.** Without admission control, one tenant doing 8K TPS starves
the other 9999 tenants — pgxpool fills with that tenant's waiters and
everyone else queues behind. Today the noisy-neighbor blast radius is
the cluster.

**Scope.**
- New interceptor `RateLimit` in
  `internal/transport/grpc/interceptors/`. Slots in chain *after*
  `EdgeIdentity` (needs `Claims.Tenant`).
- Per-tenant token bucket with steady-state + burst configurable per
  tier. Default: 100 TPS / 200 burst. Tier map env-driven for now
  (`TENANT_TIER_MAP=tenant-a:premium,tenant-b:standard`). Promote to
  DB-backed lookup later.
- Rejected requests get `RESOURCE_EXHAUSTED` with retry-after hint in
  trailers.
- Impl: `golang.org/x/time/rate.Limiter` per tenant, sharded `sync.Map`.

**Acceptance criteria.**
- Table-driven test: under-limit pass, over-limit reject, burst
  absorption, **tenant isolation**.
- Metric `grpc_ratelimit_rejected_total{tenant,method,tier}` lands in
  `/metrics`.
- Memory bound: with 100K idle tenants, limiter map stays under 50 MB
  (drop entries idle > 5 min).
- Load test: one hot tenant at 5K TPS, 100 normal tenants at 50 TPS
  each. Hot tenant throttled; the 100 others' p99 stays within ±10% of
  single-tenant baseline.

**Out of scope.** Cluster-wide (cross-pod) rate limiting via Redis.

---

## Issue 6 — Bounded global admission queue with immediate shed

**Context.** Per-tenant rate limits don't protect against thundering
herds across tenants — N legitimate tenants each within their own quota
can still collectively overload PG. Need global semaphore cap on
concurrent in-flight RPCs with immediate rejection past the cap.

**Scope.**
- New interceptor `Admission` in chain *before* `RateLimit`.
- Global semaphore sized at `2 × DATABASE_MAX_CONNS`.
- On `Acquire` failure: `RESOURCE_EXHAUSTED` immediately, no queueing.
  Caller's deadline honored: if remaining deadline < expected wait,
  reject upfront.
- Metric: `grpc_admission_inflight` gauge,
  `grpc_admission_rejected_total{method}` counter.

**Acceptance criteria.**
- At semaphore saturation, additional requests reject in < 1 ms.
- Load test at 15K offered TPS (50% over capacity): served TPS plateaus
  near capacity, rejected TPS visible in metric, p99 of *served*
  requests stays under steady-state SLO. **No queueing tail** — bimodal
  distribution.
- Configurable via `GRPC_MAX_INFLIGHT` env; default
  `2 × DATABASE_MAX_CONNS`.

**Out of scope.** Priority lanes. Adaptive admission (LIFO, CoDel).

---

## Issue 7 — Per-tenant retry circuit breaker (retry depth already capped)

**Context.** `retry.RunInTx` already caps at 5 attempts; that's fine for
isolated 40001s. The meltdown scenario is *correlated* 40001 storms
under hot-account contention — every retry conflicts with another
retry, CPU saturates. Need per-tenant-scoped circuit breaker that trips
when sustained retry rate exceeds threshold.

**Scope.**
- Lower default `MaxAttempts` to 3 (review with team — 5 is
  back-pressure-friendly but doubles the storm surface). Configurable
  via `DB_TX_RETRY_MAX`.
- Track per-tenant-per-method retry rate via rolling 30s window. When
  > 5% of recent attempts hit cap-reached, trip breaker for that tenant
  for 1s, half-open after, close on first success.
- Metrics: `db_tx_retries_total{tenant,method,outcome}`,
  `db_tx_retry_circuit_state{tenant,method}` (gauge: 0=closed,
  1=half-open, 2=open).
- Config: `DB_TX_RETRY_CIRCUIT_THRESHOLD`,
  `DB_TX_RETRY_CIRCUIT_COOLDOWN`.

**Acceptance criteria.**
- Test: artificial 40001 forced via fixture (`pgtest`), breaker trips
  after threshold, recovers after cooldown half-open success, stays open
  if half-open trial fails.
- Load test: hot-account scenario at 5K TPS targeting one account. CPU
  with breaker on vs off — measurable difference.
- Existing `retry.go` audited; PR documents previous behavior so the
  change is reviewable.

**Out of scope.** Replacing SERIALIZABLE for hot accounts (architectural
— snapshot+delta balance, gated on product needing sufficient-funds
gating).

---

## Issue 8 — Hash-subpartition journal_entries + audit_log by tenant_id

**Context.** Current monthly RANGE partitioning means every write lands
on the *current* month's partition — single PG table, single set of
B-tree leaves, index-page contention even before SERIALIZABLE conflicts.
Hash-subpartitioning by `tenant_id` within each month-range parent
spreads writes across N tables; expected N× headroom on hot-month index
contention.

**Scope.**
- Schema:
  `PARTITION BY RANGE (created_at) → SUBPARTITION BY HASH (tenant_id) WITH 16 SUBPARTITIONS`
  for `journal_entries` and `audit_log`.
- Apply via re-init of consolidated migration (pre-release, no committed
  data — confirm at PR time before proceeding; if not, this becomes a
  multi-step online-repartitioning issue, much larger).
- Verify composite-FK constraints still hold (PG requires the partition
  key to be in PK / UNIQUE constraints — `tenant_id` already in
  journal_entries' identity).
- Update partition-manager doc to seed both range parents AND hash
  subpartitions (96 subpartitions: 6 months × 16 hashes).

**Acceptance criteria.**
- Migration replaces existing partition definition;
  `make stack-down && make stack-up` rebuilds clean.
- `\d+ journal_entries` shows 96 leaf partitions; `\d+ audit_log`
  similarly.
- Load test (1K active tenants): write distribution across 16 hash
  subpartitions of the current month within ±15%; no subpart taking >
  1.5× average.
- INSERT past last seeded month still hard-fails (no default partition
  — preserve fail-loud).
- Audit hash-chain still passes verifier.

**Out of scope.** Online repartitioning. pg_partman cron. Cross-shard
rebalancing (Citus territory).

---

## What this stack of 8 buys, and what it doesn't

After all 8 land + measured at 10K TPS:
- Single-PG design **survives** sustained 10K TPS without melting under
  hot accounts, noisy neighbors, retry storms, GC pauses, or pool
  starvation.
- p99 latency stays under the SLO under bursts up to ~12K TPS.
- One tenant cannot take down the cluster.

What it does **not** give you:
- Horizontal scale past single-PG fsync ceiling (~10–15K commit/sec on
  best HW). For 50K+ TPS or geographic distribution, Citus or
  CockroachDB / Yugabyte. That's the architectural fork in the original
  plan's item (5), deliberately not in this scope.
- Cross-region durability without async-replication data loss windows.
- Real-time reconciliation past current scale (already noted as a
  separate problem).
