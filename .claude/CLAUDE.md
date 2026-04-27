# fintech-backend — project instructions

Polyglot Nx monorepo around a double-entry distributed ledger.
Target: 5K+ TPS, ACID compliance, idempotent writes, exact-once event semantics.
gRPC-only services; a BFF (GraphQL) will land separately.

## Stack pins (do not silently bump)

- Go 1.26
- PostgreSQL 18 (uses native `uuidv7()` — no pgcrypto needed)
- Redis 8
- Redpanda (Kafka API wire-compatible) — `confluent-kafka-go` requires `CGO_ENABLED=1`
- Nx + pnpm + Buf

## Load-bearing invariants

These are why the service exists. Code MUST preserve them; flag any change that touches them.

1. **Double-entry sum-zero.** Every committed transaction's journal entries sum to zero. Enforced at three layers:
   - Domain: `Transaction.AssertBalanced()` (preflight, before DB).
   - Repository: same assert inside the SERIALIZABLE tx.
   - Reconciler: `SUM(amount) = 0` invariant query, paging-grade alert on failure.
2. **Idempotency.** `transactions.idempotency_key UNIQUE` is the durable backstop. Redis SETNX is the fast-path; on Redis fault the usecase fails open and Postgres' unique constraint catches duplicates.
3. **Atomicity of ledger + outbox.** Ledger inserts and the corresponding outbox event commit in the SAME Postgres transaction. The outbox worker drains via `FOR UPDATE SKIP LOCKED` — never publish to Kafka from the request path.
4. **Money is `NUMERIC(19,4)` end-to-end.** Never `FLOAT` / `DOUBLE`. Wire format is decimal-as-string (proto3 `string amount`).
5. **Currency is locked to the account.** Composite FK `(account_id, currency) REFERENCES accounts(id, currency)`. A journal entry can't be booked in a non-native currency.

## Architecture

Clean Architecture / Hexagonal layout per service:

```
apps/<svc>/
  cmd/<binary>/main.go      DI wiring, signal-driven graceful shutdown
  internal/
    domain/                 Entities + ports. Zero infra deps.
    usecase/                Orchestration (idempotency, balance assertion).
    repository/             Postgres adapters. InTx wrapper, 40001 retry.
    infrastructure/         Redis, Kafka, outbox worker, reconciler.
    transport/grpc/         Server + interceptors + handler.
    observability/          OTel SDK bootstrap (no os.Getenv — takes Config).
    config/                 Single env contract via caarlos0/env/v11 + godotenv.
  migrations/               golang-migrate SQL files.
```

Interceptor chain (order matters): `Recovery → RequestID → Logging → Auth`.
OTel uses `StatsHandler(otelgrpc.NewServerHandler())`, not the legacy interceptor.

## Repo conventions

- **Per-service Makefile + .env.example.** Root Makefile owns shared infra (compose, monorepo nx, proto-gen, stack-up); service Makefiles own build/test/run/migrate/smoke for that service. Future services get their own pair — never extend root.
- **Env layering.** Root `.env` = shared infra coords (DATABASE_URL, REDIS_URL, KAFKA_BROKERS, POSTGRES_*). Per-service `.env` = app-internal knobs (workers, auth, OTel). Loaded by both Make (for `$(VAR)` substitution) and binaries (via `internal/config.LoadDotenv`). `APP_ENV=production` skips dotenv entirely.
- **No `os.Getenv` outside `internal/config`.** Subsystems (observability, etc.) take typed config structs as parameters.
- **Generated code is committed.** `libs/go/proto-gen/` ships in the repo so clones don't need `buf` installed. Regenerate with `make proto-gen` when proto changes.
- **`go.work` + `go.work.sum` are committed.** Workspace IS the contract.
- **Table-driven tests.** Default everywhere; `t.Run(tc.name, ...)` per case.
- **DDD-style domain.** Aggregates, value objects, sentinel errors. Push back on anemic models.

## Run cheatsheet

```sh
# Boot shared infra + apply ledger-svc migrations
make stack-up

# Build / test
make -C apps/ledger-svc build
make -C apps/ledger-svc test

# Run server (foreground, ctrl-c)
make -C apps/ledger-svc run-server

# Migrations
make -C apps/ledger-svc migrate-up
make -C apps/ledger-svc migrate-version

# Smoke (prints grpcurl examples)
make -C apps/ledger-svc smoke

# Tear down (drops pgdata volume)
make stack-down
```

## CGO

`confluent-kafka-go` links against `librdkafka` — `nx run ledger-svc:build` plumbs `CGO_ENABLED=1`. Bare `go build ./...` from CGO=0 environments will fail; that's expected.

## Go conventions — write idiomatic Go 1.26

When writing or scaffolding Go in this repo, default to modern stdlib and language features. Don't reach for third-party libs for things stdlib now covers, and don't carry forward pre-Go-1.21 patterns.

**Stdlib first (Go 1.21+):**
- `slices` — `slices.Contains`, `Sort`, `SortFunc`, `Index`, `Equal`, `Concat`, `Chunk`, `BinarySearch`. Don't hand-roll loops for these.
- `maps` — `maps.Clone`, `Copy`, `Keys`, `Values`, `Equal`. Don't write a key-extraction loop.
- `cmp` — `cmp.Compare`, `cmp.Or`. For ordering and zero-value-fallback chains.
- `min`, `max`, `clear` — language builtins, not function calls. Use them.
- `errors.Join` — accumulate multi-error from a loop instead of `fmt.Errorf("a: %w; b: %w", ...)` strings.

**Language features:**
- Range-over-func iterators (Go 1.23+) — write `iter.Seq[T]` / `iter.Seq2[K,V]` for streaming APIs. Consumers use `for x := range myIter { ... }`. Beats hand-rolled callback-style or channel-based iteration.
- `for i := range N` (Go 1.22+) — integer range. Don't write `for i := 0; i < n; i++` anymore for plain counts.
- Loop variable scoping is per-iteration as of Go 1.22 — don't write `i := i` shadow lines, and don't capture loop vars defensively. (If you see `i := i` in this codebase, it's stale and should go.)

**HTTP / net:**
- `net/http.ServeMux` path patterns (Go 1.22+) — `mux.HandleFunc("GET /users/{id}", h)`. Don't pull in chi/gorilla for plain routing unless you need middleware composition the stdlib doesn't have.
- `context.AfterFunc` (Go 1.21+) — registers a callback when ctx is done. Cleaner than `go func() { <-ctx.Done(); cleanup() }()`.

**Concurrency:**
- `sync.OnceFunc`, `sync.OnceValue`, `sync.OnceValues` (Go 1.21+) — beats `sync.Once` + global state for memoized init.
- `errgroup.Group` with `WithContext` from `golang.org/x/sync` is fine; stdlib doesn't have an equivalent.

**Testing:**
- `t.Context()` (Go 1.24+) — auto-cancelled at test end. Beats manual `context.WithCancel(context.Background())` per test.
- `testing/synctest` (Go 1.24+ experimental, 1.25 GA) — use for time-dependent tests instead of real clock + sleeps.

**go.mod:**
- Use the `toolchain` directive when pinning a specific Go patch — `go 1.26` declares the language version, `toolchain go1.26.x` declares the build version. They're separate concerns now.

**When in doubt:**
- Check the Go release notes for 1.21 → current before reaching for a third-party package: <https://go.dev/doc/devel/release>. The "What's new in Go 1.X" sections list every new stdlib API.
- My (Claude's) knowledge cutoff is January 2026 — for anything that landed in Go 1.26.x patch releases or later, treat my suggestions as a starting point and verify against current release notes via WebFetch.

## Pending work

- BFF (GraphQL) — separate service that fronts ledger-svc + future gateway-svc.
- JWKS-backed JWT verifier replacing `DevTokenVerifier`.
- Integration tests via `testcontainers-go` (Postgres + Redis + Redpanda).
- Kafka header trace propagation (currently W3C TraceContext flows through gRPC metadata only).
- `otelpgx` for SQL spans.

## When in doubt

- The domain layer is the source of truth for invariants — read it before changing repository or usecase code.
- Migration changes against an already-applied schema MUST be a new file, not an edit. Editing 001 is only OK in dev with no committed data (we did exactly this for the v7 default change).
- New service? Use the `/scaffold-service` skill instead of copy-pasting from ledger-svc.
