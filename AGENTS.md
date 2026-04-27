# AGENTS.md

## Scope and current reality
- This is an Nx-managed polyglot monorepo, but only `apps/ledger-svc` is production-grade; `apps/gateway-svc` and `apps/compliance-cli` are placeholders (`README.md`, `apps/*/main.go`).
- Treat `apps/ledger-svc` as the canonical pattern for new backend work.

## Big picture architecture (ledger-svc)
- Entrypoint (`apps/ledger-svc/cmd/server/main.go`) wires config, Postgres, Redis, Kafka producer, gRPC server, and an in-process outbox worker.
- Request flow: gRPC handler (`internal/transport/grpc/ledger_handler.go`) -> use case (`internal/usecase/transfer.go`) -> repository (`internal/repository/ledger_repo.go`).
- Idempotency is layered: Redis fast-path (`internal/infrastructure/redis_idempotency.go`) plus Postgres `UNIQUE(idempotency_key)` backstop (`migrations/001_init_ledger.up.sql`).
- Atomicity model: transaction row + journal legs + outbox row are committed in one Serializable PG tx (`internal/repository/ledger_repo.go`); do not publish Kafka directly from request path.
- Event delivery: outbox relay workers drain with `FOR UPDATE SKIP LOCKED` and publish to Kafka (`internal/repository/outbox_repo.go`, `internal/infrastructure/outbox_worker.go`).
- Anti-entropy: reconciler republishes stale pending outbox rows and runs ledger sum-zero checks (`internal/infrastructure/reconciler.go`).

## Non-negotiable invariants
- Double-entry must net to zero per transaction and globally (`internal/domain/transaction.go`, `internal/repository/outbox_repo.go`).
- Money stays decimal: Postgres `NUMERIC(19,4)` + proto amount as string (`migrations/001_init_ledger.up.sql`, `shared/proto/fintech/ledger/v1/ledger.proto`).
- UUIDv7 is used for hot write tables and app-generated IDs (`migrations/001_init_ledger.up.sql`, `internal/repository/ledger_repo.go`).
- Currency is account-bound via composite FK `(account_id, currency)` (`migrations/001_init_ledger.up.sql`).

## Developer workflows that matter
- Bootstrap infra + migrations:
  - `make stack-up` (root Makefile starts Postgres/Redis/Redpanda, waits for PG, runs ledger migrations).
- Run service binaries:
  - `make -C apps/ledger-svc run-server`
  - `make -C apps/ledger-svc run-reconciler`
- Test paths:
  - `make -C apps/ledger-svc test` (service)
  - `make test` (monorepo via Nx)
- Proto changes:
  - edit `shared/proto/**`, then run `make proto-gen` (generated code is committed under `libs/go/proto-gen`).

## Build/tooling conventions
- Prefer Nx targets over ad-hoc `go build/go test` (`apps/*/project.json`); `ledger-svc` build/test set `CGO_ENABLED=1` for `confluent-kafka-go`.
- Root Makefile owns shared infra and monorepo orchestration; service Makefiles own service-specific commands and migrations.
- CI gate (`.github/workflows/ci.yml`): `pnpm install`, `buf lint/generate`, `nx affected` build+test, `govulncheck`.

## Config and env rules
- Env layering is intentional: root `.env` (shared infra coords) + `apps/ledger-svc/.env` (service knobs).
- `internal/config` is the only env parsing boundary; avoid `os.Getenv` elsewhere.
- `APP_ENV=production` disables dotenv loading entirely (`internal/config/config.go`).

## Change playbook for agents
- For transfer-path changes, update all three: use case (`internal/usecase`), repo transaction/outbox write (`internal/repository`), and transport mapping/tests (`internal/transport/grpc`).
- If you touch idempotency semantics, validate Redis-fail-open and PG-duplicate replay behavior (`internal/usecase/transfer.go`, `internal/domain/errors.go`).
- If you change outbox payload/schema, update producer/consumer contract and proto-generated types together (`internal/usecase/events.go`, `shared/proto/**`, `libs/go/proto-gen/**`).

