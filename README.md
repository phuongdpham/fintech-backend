# fintech-backend

Polyglot Nx monorepo for a banking OS. Currently houses **ledger-svc**, a double-entry distributed ledger over gRPC. Targets 5K+ TPS with ACID compliance, idempotent writes, and exact-once event semantics via the Transactional Outbox pattern.

## What's inside

| Path | What it is | Status |
| --- | --- | --- |
| `apps/ledger-svc` | Go gRPC ledger service: Transfer / GetTransaction RPCs, Postgres-backed double-entry book, Redis idempotency, in-process Outbox relay worker, anti-entropy reconciler daemon | Working |
| `apps/gateway-svc` | Payments gateway | Placeholder |
| `apps/compliance-cli` | Compliance tooling | Placeholder |
| `libs/go/finance` | Shared double-entry helpers | Stub |
| `libs/go/kafkautil` | Kafka helpers | Stub |
| `libs/go/logger` | `slog` wrappers | Stub |
| `libs/go/proto-gen` | Generated gRPC stubs (committed) | Tracks `shared/proto` |
| `shared/proto/` | Protobuf source, Buf-managed | — |

## Stack

- **Go** 1.26
- **PostgreSQL** 18 (uses native `uuidv7()`)
- **Redis** 8
- **Redpanda** (Kafka API wire-compatible)
- **Nx** + **pnpm** + **Buf**

`confluent-kafka-go` links against `librdkafka`; builds need `CGO_ENABLED=1`. `nx run` plumbs that automatically — bare `go build` from a CGO-disabled environment will fail by design.

## Quick start

```sh
# 1. Host deps (Arch Linux)
sudo pacman -S go protobuf buf
corepack enable pnpm
pnpm install

# 2. Env files (root = shared infra; per-service = app-internal knobs)
cp .env.example .env
cp apps/ledger-svc/.env.example apps/ledger-svc/.env

# 3. Boot shared infra (Postgres, Redis, Redpanda) + apply migrations
make stack-up

# 4. Run the gRPC server (foreground; ctrl-c to stop)
make -C apps/ledger-svc run-server

# 5. In a second terminal — print example grpcurl invocations
make -C apps/ledger-svc smoke
```

Run `make help` from root or any service directory for the full target list.

## Architecture

Each service follows Clean Architecture / hexagonal layout:

```
apps/<svc>/
  cmd/<binary>/main.go    DI wiring + signal-driven graceful shutdown
  internal/
    domain/               Entities + ports. Zero infrastructure dependencies.
    usecase/              Application logic. Idempotency orchestration.
    repository/           Postgres adapters (pgxpool + Serializable + 40001 retry).
    infrastructure/       Redis, Kafka, outbox worker, reconciler.
    transport/grpc/       gRPC server + interceptor chain.
    observability/        OpenTelemetry SDK bootstrap.
    config/               Env contract via caarlos0/env/v11 + godotenv.
  migrations/             golang-migrate SQL.
```

Interceptor chain: `Recovery → RequestID → Logging → Auth`. OpenTelemetry uses
`StatsHandler(otelgrpc.NewServerHandler())` for richer span coverage than the
legacy interceptor would provide.

## Notable invariants

The ledger has correctness constraints that the code enforces in multiple layers:

- **Double-entry sum-zero.** Every committed transaction's journal entries sum to zero. Asserted in domain (preflight), in the SERIALIZABLE repository transaction, and in the reconciler's invariant query. A SUM≠0 finding is a paging-grade incident.
- **Layered idempotency.** Redis `SETNX` fast-path with 24-hour TTL; Postgres `UNIQUE(idempotency_key)` is the durable backstop. Redis fault → fail-open, PG catches duplicates.
- **Atomic ledger + events.** Outbox rows are written in the same Serializable Postgres transaction as the journal entries. The relay worker drains via `FOR UPDATE SKIP LOCKED`. Direct publish from the request path is forbidden.
- **`NUMERIC(19,4)` end-to-end.** Money never touches `FLOAT`/`DOUBLE`. Wire format is decimal-as-string (proto3 `string amount`) for cross-language safety.
- **UUIDv7 primary keys** on hot indexes (transactions, journal_entries, outbox_events). The 48-bit ms-timestamp prefix gives B-tree write locality, avoiding the leaf-thrash UUIDv4 causes under load.
- **Currency locked to account.** Composite FK `(account_id, currency) → accounts(id, currency)` prevents booking a journal entry against an account in a non-native currency.

## Repo conventions

- **Per-service Makefile + .env.example.** Root Makefile owns shared infra (compose, wait-postgres, monorepo nx, proto-gen). Service-specific verbs (build, run, migrate, smoke) live in `apps/<svc>/Makefile`. New services add their own pair — never extend root.
- **Env layering.** Root `.env` carries shared infra coords (DATABASE_URL, REDIS_URL, KAFKA_BROKERS, POSTGRES_*). Per-service `.env` carries app-internal knobs (workers, auth, OTel). Both files are loaded by the binary at runtime via `godotenv`. `APP_ENV=production` skips dotenv entirely.
- **Generated code is committed.** `libs/go/proto-gen/` ships in the repo; clones don't need `buf` installed. Regenerate with `make proto-gen` after editing `.proto` files.
- **Workspace files are committed.** `go.work` + `go.work.sum` are the cross-module contract.
- **Table-driven tests** are the default style across the codebase.
- **No `os.Getenv` outside `internal/config`.** Subsystems take typed config structs.

## Testing

```sh
make -C apps/ledger-svc test     # one service
make test                        # whole monorepo via nx run-many
```

Current coverage: domain invariants, repository (mocked pgx), usecase orchestration, transport handlers, interceptor chain, OpenTelemetry bootstrap, config parsing/validation. Integration tests via `testcontainers-go` are pending.

The smoke target (`make -C apps/ledger-svc smoke`) prints `grpcurl` invocations for the live service.

## CI

`.github/workflows/ci.yml` runs on every push and PR to `main`:

- `pnpm install`
- `buf lint` + `buf generate`
- `nx affected --target=test,build`
- `govulncheck ./...`

## Layout

```
.
├── apps/
│   ├── ledger-svc/              # The ledger service (the real one)
│   ├── gateway-svc/             # placeholder
│   └── compliance-cli/          # placeholder
├── libs/go/                     # shared Go libraries
├── shared/proto/                # Protobuf source (Buf-managed)
├── .github/workflows/           # CI
├── docker-compose.yml           # Postgres / Redis / Redpanda
├── postgresql.conf              # tuning + listen_addresses
├── go.work                      # Go workspace root
├── nx.json                      # Nx config
├── Makefile                     # root orchestration
├── README.md
└── LICENSE                      # Apache-2.0
```

## Pending

- BFF (GraphQL) fronting ledger-svc + future services.
- JWKS-backed JWT verifier (replacing the dev-token-string verifier).
- Integration tests via `testcontainers-go`.
- Kafka header trace propagation (currently W3C TraceContext flows through gRPC metadata only).
- `otelpgx` for automatic SQL span instrumentation.

## License

Apache License 2.0 — see [LICENSE](./LICENSE). Apache-2.0 was chosen over MIT
for its explicit patent grant, which matters in fintech where contributors
and downstream users want clear protection against patent claims.
