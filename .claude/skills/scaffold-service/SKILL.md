---
name: scaffold-service
description: Bootstrap a new Go service under apps/ following the ledger-svc pattern (Makefile, .env.example, project.json, internal/config, go.mod, registered in go.work + nx + pnpm-workspace).
---

# scaffold-service

Use this when adding a new deployable Go service to the monorepo (e.g. BFF, gateway-svc-real, payments-svc).
The user typically invokes this as `/scaffold-service <name>`. Treat the argument as the service name —
kebab-case, no `apps/` prefix.

## When NOT to use this skill

- Not for adding a library (`libs/go/<x>`). Libraries are simpler — just `go mod init` + add to `go.work`.
- Not for sub-binaries within an existing service (that's `cmd/<binary>` inside the service).
- Not for tweaking an existing service — edit its files directly.

## Pre-flight checks (run FIRST, before any writes)

1. Confirm `apps/<name>/` doesn't already exist. If it does, stop and ask the user — don't overwrite.
2. Confirm the user told you whether the service needs **migrations**. If unclear, ask:
   > "Will this service own its own Postgres schema (migrations directory + migrate target), or is it stateless?"
   - Migrations service → ledger-svc-shape (with `cmd/migrate`, `migrations/`, migrate-* Makefile targets).
   - Stateless service → trim to `cmd/server` only; no migrate binary, no migrations dir.
3. Confirm whether the service speaks **gRPC**. If yes, it'll need proto definitions in `shared/proto/fintech/<name>/v1/` — flag this to the user and ask if they want stubs scaffolded too.

## Files to create

Replace `<name>` and `<NAME>` (uppercase env-var prefix) consistently. The module path follows the existing convention: `github.com/phuongdpham/fintech/apps/<name>`.

### 1. `apps/<name>/go.mod`

Keep deps minimal at scaffold time. The user will `go get` what they need.

```go
module github.com/phuongdpham/fintech/apps/<name>

go 1.26
```

### 2. `apps/<name>/cmd/server/main.go`

Mirrors ledger-svc's structure: `main` → `loadConfig` → `run(ctx, cfg, log)` with signal-driven shutdown.
Keep the body minimal at scaffold time — DI wiring grows as the user adds adapters.

### 3. `apps/<name>/internal/config/config.go`

Use the same shape as `apps/ledger-svc/internal/config/config.go`:
- `caarlos0/env/v11` + `joho/godotenv`
- Nested substructs for each subsystem
- `Load(LoadOptions{ServiceEnvFile: "apps/<name>/.env"})`
- Required fields use `env:"KEY,required"`
- Cross-field rules in `Validate()`

### 4. `apps/<name>/.env.example`

Service-internal knobs only — never put DATABASE_URL / REDIS_URL / KAFKA_BROKERS here (those live in root `.env`). Start from this template:

```env
APP_ENV=development
LOG_LEVEL=debug

GRPC_ADDR=:9091   # use a different port from ledger-svc (:9090)
GRPC_SHUTDOWN_TIMEOUT=30s

# OpenTelemetry
OTEL_SERVICE_NAME=<name>
OTEL_TRACES_SAMPLER=always_on
OTEL_TRACES_SAMPLER_ARG=0.1
OBS_TRACE_DEBUG=false
```

### 5. `apps/<name>/Makefile`

Copy `apps/ledger-svc/Makefile` verbatim, then:
- Replace `ledger-svc` → `<name>` in the `cd $(ROOT) && pnpm exec nx run <name>:build` lines.
- Replace `LEDGER_BIN` / `RECON_BIN` / `MIGRATE_BIN` to match this service's actual binaries.
- If stateless: remove the `##@ Migrations` section entirely and the `MIGRATE_BIN` variable.
- If no reconciler: drop the `run-reconciler` target.
- Update the `smoke` target's gRPC examples to match this service's actual RPCs (or stub it with a TODO).

### 6. `apps/<name>/project.json`

Mirror `apps/ledger-svc/project.json`. Critical: keep `CGO_ENABLED: "1"` in build/test env IF the service depends on `confluent-kafka-go`. Otherwise drop it.

```json
{
  "name": "<name>",
  "$schema": "../../node_modules/nx/schemas/project-schema.json",
  "targets": {
    "build": {
      "executor": "nx:run-commands",
      "options": {
        "command": "go build -o ../../dist/<name> ./cmd/server",
        "cwd": "apps/<name>",
        "env": { "CGO_ENABLED": "0" }
      }
    },
    "test": {
      "executor": "nx:run-commands",
      "options": {
        "command": "go test -v ./...",
        "cwd": "apps/<name>",
        "env": { "CGO_ENABLED": "0" }
      }
    }
  }
}
```

## Files to edit

### 7. `go.work` — add the new module

```diff
 use (
   ./apps/ledger-svc
   ./apps/gateway-svc
   ./apps/compliance-cli
+  ./apps/<name>
   ./libs/go/finance
   ...
 )
```

### 8. `pnpm-workspace.yaml` — already covers `apps/*` via glob. No edit needed unless the file uses explicit paths.

### 9. **Conditional: root `Makefile`** — only if the service has migrations.

Add ONE line to the `stack-up` target:

```diff
 stack-up: compose-up wait-postgres
 	$(MAKE) -C apps/ledger-svc migrate-up
+	$(MAKE) -C apps/<name> migrate-up
```

Don't auto-enumerate (no `for d in apps/*`) — explicit registration is intentional, so adding a service is a deliberate two-line PR rather than auto-magic.

### 10. **Conditional: `shared/proto/fintech/<name>/v1/<name>.proto`** — if gRPC.

Stub a starter `.proto` file. After creation, run `make proto-gen` and verify `libs/go/proto-gen/fintech/<name>/v1/` contains the generated stubs.

## Verification

After scaffolding, run:

```sh
cd apps/<name> && go mod tidy
make -C apps/<name> build   # should compile to dist/<name>
make -C apps/<name> test    # should pass with zero tests (no failures)
```

If any of those fail, do NOT report success — fix the scaffold first.

## Reporting back

Tell the user:
1. What was created (list of files).
2. Whether they need to manually run `go get` for any deps (proto-gen, otel, etc. — the scaffold only ships a bare `go.mod`).
3. The next step: implement actual handlers, wire DI in `main.go`, add domain types.

Don't oversell: the scaffold is plumbing, not a working service.
