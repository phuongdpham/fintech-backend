# ledger-svc — service-scoped Makefile.
#
# Reachable two ways from anywhere:
#
#   make -C apps/ledger-svc <target>     # from repo root
#   cd apps/ledger-svc && make <target>  # from the service dir
#
# Both work because ROOT is resolved from this file's location, not CWD.
#
# Env layering (Make-side):
#   1. Root .env  — shared infra coords (DATABASE_URL, KAFKA_BROKERS, …).
#   2. Service .env (this dir) — service-internal knobs.
# Loaded in that order; service overrides root on collision (they shouldn't
# collide by partition rule, but order is deterministic if they ever do).
#
# Env layering (binary-side, at runtime via godotenv):
#   The same two files are read by internal/config.LoadDotenv. The Make-
#   side load is purely so $(DATABASE_URL) substitutes correctly into the
#   migrate target — Make can't read env vars the parent shell forgot to
#   export.
#
# CWD: run-* targets cd to ROOT before invoking the binary so godotenv's
# relative paths (./.env, apps/ledger-svc/.env) resolve correctly.

# ROOT = repo root, derived from this Makefile's path. Robust to any CWD.
ROOT       := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
SVC_DIR    := $(ROOT)/apps/ledger-svc
DIST       := $(ROOT)/dist
LEDGER_BIN := $(DIST)/ledger-svc
RECON_BIN  := $(DIST)/reconciler
MIGRATE_BIN:= $(DIST)/migrate

# Layer 1: root .env (shared infra). Required for $(DATABASE_URL) below.
ifneq (,$(wildcard $(ROOT)/.env))
include $(ROOT)/.env
export
endif

# Layer 2: service .env. Anything app-internal lives here.
ifneq (,$(wildcard $(SVC_DIR)/.env))
include $(SVC_DIR)/.env
export
endif

# Last-line-of-defense default if neither file is present (fresh clone).
# Matches docker-compose.yml. Overridable via shell or `make migrate-up DATABASE_URL=...`.
DATABASE_URL ?= postgres://phuong_dev:ledger_secret@localhost:5432/ledger_db?sslmode=disable

.DEFAULT_GOAL := help

##@ General
.PHONY: help
help:  ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nledger-svc targets:\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build
# nx is repo-root-anchored (pnpm-workspace.yaml lives at root) so build/test
# always cd there, regardless of where this Makefile was invoked from.
.PHONY: build
build:  ## Build ledger-svc binaries (server, reconciler, migrate)
	cd $(ROOT) && pnpm exec nx run ledger-svc:build

.PHONY: test
test:  ## Run ledger-svc tests
	cd $(ROOT) && pnpm exec nx run ledger-svc:test

.PHONY: mocks
mocks:  ## Regenerate gomock-based test mocks for domain ports
	cd $(SVC_DIR) && go generate ./internal/...

##@ Migrations
.PHONY: migrate-up
migrate-up: $(MIGRATE_BIN)  ## Apply all pending migrations
	$(MIGRATE_BIN) -dsn "$(DATABASE_URL)" -path $(SVC_DIR)/migrations up

.PHONY: migrate-down
migrate-down: $(MIGRATE_BIN)  ## Revert the last migration
	$(MIGRATE_BIN) -dsn "$(DATABASE_URL)" -path $(SVC_DIR)/migrations down 1

.PHONY: migrate-version
migrate-version: $(MIGRATE_BIN)  ## Print current schema version
	$(MIGRATE_BIN) -dsn "$(DATABASE_URL)" -path $(SVC_DIR)/migrations version

##@ Run
# CWD = ROOT so internal/config.LoadDotenv finds ./.env and
# apps/ledger-svc/.env on the relative paths it probes.
.PHONY: run-server
run-server: $(LEDGER_BIN)  ## Run the gRPC server (foreground; ctrl-c to stop)
	cd $(ROOT) && $(LEDGER_BIN)

.PHONY: run-reconciler
run-reconciler: $(RECON_BIN)  ## Run the reconciler daemon (foreground)
	cd $(ROOT) && $(RECON_BIN) --interval 1m

##@ Load test
# Hybrid topology: Go binary seeds + writes fixtures; k6 drives load.
# See apps/ledger-svc/load/README.md for the full flow.
LOADTEST_BIN  := $(DIST)/loadtest
FIXTURES_PATH := $(SVC_DIR)/load/fixtures.json
SEED_TENANTS  ?= 1000
SEED_ACCOUNTS ?= 100

.PHONY: loadtest-build
loadtest-build: $(LOADTEST_BIN)  ## Build the seed/fixtures binary

.PHONY: loadtest-seed
loadtest-seed: $(LOADTEST_BIN)  ## Seed accounts and emit fixtures.json (idempotent)
	$(LOADTEST_BIN) seed \
		-dsn "$(DATABASE_URL)" \
		-tenants $(SEED_TENANTS) \
		-accounts-per-tenant $(SEED_ACCOUNTS) \
		-fixtures $(FIXTURES_PATH)

.PHONY: loadtest-infra-up
loadtest-infra-up:  ## Bring up Prometheus + Grafana sidecar (ports 9091, 3001)
	docker compose -f $(SVC_DIR)/load/docker-compose.yml up -d

.PHONY: loadtest-infra-down
loadtest-infra-down:  ## Tear down the sidecar stack
	docker compose -f $(SVC_DIR)/load/docker-compose.yml down

.PHONY: loadtest-run
loadtest-run:  ## Run the k6 scenario (requires k6 + loadtest-seed first)
	cd $(ROOT) && K6_PROMETHEUS_RW_SERVER_URL=http://localhost:9091/api/v1/write \
		k6 run -o experimental-prometheus-rw \
		apps/ledger-svc/load/k6/transfer.js \
		-e FIXTURES=apps/ledger-svc/load/fixtures.json \
		-e ADDR=$${LOADTEST_ADDR:-localhost:9090} \
		-e PROTO_DIR=shared/proto

##@ Smoke
.PHONY: smoke
smoke:  ## Print example grpcurl invocations
	@printf '%s\n' \
		'# health check (public; no identity headers needed)' \
		'grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check' \
		'' \
		'# list services (reflection)' \
		'grpcurl -plaintext localhost:9090 list' \
		'' \
		'# transfer — direct callers (e.g. dev grpcurl) must mint the headers' \
		'# the BFF would forward in production: x-tenant-id, x-actor-subject.' \
		'grpcurl -plaintext \' \
		'  -H "x-tenant-id: tenant-dev" \' \
		'  -H "x-actor-subject: dev-user" \' \
		'  -d @ localhost:9090 fintech.ledger.v1.LedgerService/Transfer <<EOF' \
		'{' \
		'  "idempotency_key": "smoke-$$(date +%s)",' \
		'  "from_account_id": "11111111-1111-1111-1111-111111111111",' \
		'  "to_account_id":   "22222222-2222-2222-2222-222222222222",' \
		'  "amount":          "100.0000",' \
		'  "currency":        "USD"' \
		'}' \
		'EOF'

# Implicit binary builds — declared as PHONY so `make` always defers to nx,
# which has its own up-to-date logic and CGO flag plumbing.
.PHONY: $(LEDGER_BIN) $(RECON_BIN) $(MIGRATE_BIN) $(LOADTEST_BIN)
$(LEDGER_BIN) $(RECON_BIN) $(MIGRATE_BIN) $(LOADTEST_BIN):
	cd $(ROOT) && pnpm exec nx run ledger-svc:build
