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

##@ Profile (pprof)
# Capture targets dump profiles to ./profiles/ with a timestamp so
# multiple runs don't clobber each other. Use the matching analyze-*
# target to open the latest in `go tool pprof`'s web UI (Graphviz
# required: pacman -S graphviz).
#
# All targets assume PPROF_ENABLED=true and METRICS_ADDR points at
# localhost:9100 (the .env.example defaults). For block / mutex
# profiles to be useful, also set PPROF_BLOCK_PROFILE_RATE=1 and/or
# PPROF_MUTEX_PROFILE_FRACTION=1 in the env BEFORE the server boots —
# overhead is real (~3–5% CPU) so flip back to 0 when done.
PPROF_ADDR    ?= localhost:9100
PPROF_DIR     := $(SVC_DIR)/profiles
PPROF_SECONDS ?= 30
PPROF_STAMP   := $(shell date +%Y%m%d-%H%M%S)

$(PPROF_DIR):
	mkdir -p $(PPROF_DIR)

.PHONY: profile-cpu
profile-cpu: $(PPROF_DIR)  ## Capture a CPU profile (default 30s); writes profiles/cpu-<ts>.pb.gz
	@echo "capturing CPU profile for $(PPROF_SECONDS)s..."
	curl -fsS -o $(PPROF_DIR)/cpu-$(PPROF_STAMP).pb.gz \
		"http://$(PPROF_ADDR)/debug/pprof/profile?seconds=$(PPROF_SECONDS)"
	@echo "wrote $(PPROF_DIR)/cpu-$(PPROF_STAMP).pb.gz"

.PHONY: profile-heap
profile-heap: $(PPROF_DIR)  ## Capture an in-use heap profile; writes profiles/heap-<ts>.pb.gz
	curl -fsS -o $(PPROF_DIR)/heap-$(PPROF_STAMP).pb.gz \
		"http://$(PPROF_ADDR)/debug/pprof/heap"
	@echo "wrote $(PPROF_DIR)/heap-$(PPROF_STAMP).pb.gz"

.PHONY: profile-allocs
profile-allocs: $(PPROF_DIR)  ## Capture an allocations profile (since process start)
	curl -fsS -o $(PPROF_DIR)/allocs-$(PPROF_STAMP).pb.gz \
		"http://$(PPROF_ADDR)/debug/pprof/allocs"
	@echo "wrote $(PPROF_DIR)/allocs-$(PPROF_STAMP).pb.gz"

.PHONY: profile-block
profile-block: $(PPROF_DIR)  ## Capture a block profile (needs PPROF_BLOCK_PROFILE_RATE>0)
	curl -fsS -o $(PPROF_DIR)/block-$(PPROF_STAMP).pb.gz \
		"http://$(PPROF_ADDR)/debug/pprof/block"
	@echo "wrote $(PPROF_DIR)/block-$(PPROF_STAMP).pb.gz"
	@echo "  (empty profile = PPROF_BLOCK_PROFILE_RATE was 0 at boot)"

.PHONY: profile-mutex
profile-mutex: $(PPROF_DIR)  ## Capture a mutex contention profile (needs PPROF_MUTEX_PROFILE_FRACTION>0)
	curl -fsS -o $(PPROF_DIR)/mutex-$(PPROF_STAMP).pb.gz \
		"http://$(PPROF_ADDR)/debug/pprof/mutex"
	@echo "wrote $(PPROF_DIR)/mutex-$(PPROF_STAMP).pb.gz"
	@echo "  (empty profile = PPROF_MUTEX_PROFILE_FRACTION was 0 at boot)"

.PHONY: profile-goroutines
profile-goroutines: $(PPROF_DIR)  ## Dump full goroutine stacks (text)
	curl -fsS "http://$(PPROF_ADDR)/debug/pprof/goroutine?debug=2" \
		> $(PPROF_DIR)/goroutines-$(PPROF_STAMP).txt
	@echo "wrote $(PPROF_DIR)/goroutines-$(PPROF_STAMP).txt"

.PHONY: profile-trace
profile-trace: $(PPROF_DIR)  ## Capture an execution trace (default 5s; file is large)
	@echo "capturing execution trace for $${PPROF_TRACE_SECONDS:-5}s..."
	curl -fsS -o $(PPROF_DIR)/trace-$(PPROF_STAMP).out \
		"http://$(PPROF_ADDR)/debug/pprof/trace?seconds=$${PPROF_TRACE_SECONDS:-5}"
	@echo "wrote $(PPROF_DIR)/trace-$(PPROF_STAMP).out"
	@echo "open with:  go tool trace $(PPROF_DIR)/trace-$(PPROF_STAMP).out"

.PHONY: profile-analyze-cpu
profile-analyze-cpu:  ## Open the latest CPU profile in pprof's web UI (port 8888)
	@latest=$$(ls -t $(PPROF_DIR)/cpu-*.pb.gz 2>/dev/null | head -1); \
	if [ -z "$$latest" ]; then echo "no CPU profile in $(PPROF_DIR); run profile-cpu first" >&2; exit 1; fi; \
	echo "opening $$latest"; \
	go tool pprof -http=:8888 "$$latest"

.PHONY: profile-analyze-heap
profile-analyze-heap:  ## Open the latest heap profile in pprof's web UI (port 8889)
	@latest=$$(ls -t $(PPROF_DIR)/heap-*.pb.gz 2>/dev/null | head -1); \
	if [ -z "$$latest" ]; then echo "no heap profile; run profile-heap first" >&2; exit 1; fi; \
	echo "opening $$latest"; \
	go tool pprof -http=:8889 "$$latest"

.PHONY: profile-analyze-allocs
profile-analyze-allocs:  ## Open the latest allocs profile in pprof's web UI (port 8890)
	@latest=$$(ls -t $(PPROF_DIR)/allocs-*.pb.gz 2>/dev/null | head -1); \
	if [ -z "$$latest" ]; then echo "no allocs profile; run profile-allocs first" >&2; exit 1; fi; \
	echo "opening $$latest"; \
	go tool pprof -http=:8890 "$$latest"

# Goroutine dumps are text (?debug=2 from /debug/pprof/goroutine), not
# pprof binary. Rather than open in a web UI, this prints a quick
# state-distribution summary then pages the full dump. The summary is
# the "are we leaking / what's stuck" view; the pager is the "give me
# the actual stack traces" view.
.PHONY: profile-analyze-goroutines
profile-analyze-goroutines:  ## Summary + pager view of the latest goroutine dump
	@latest=$$(ls -t $(PPROF_DIR)/goroutines-*.txt 2>/dev/null | head -1); \
	if [ -z "$$latest" ]; then echo "no goroutine dump; run profile-goroutines first" >&2; exit 1; fi; \
	echo "==== $$latest ===="; \
	echo "total goroutines: $$(grep -c '^goroutine ' "$$latest")"; \
	echo ""; \
	echo "by state (top 10):"; \
	awk -F'[][]' '/^goroutine [0-9]+ \[/ {split($$2, a, ","); print a[1]}' "$$latest" \
		| sort | uniq -c | sort -rn | head -10; \
	echo ""; \
	echo "opening in $${PAGER:-less}..."; \
	$${PAGER:-less} "$$latest"

.PHONY: profile-diff-cpu
profile-diff-cpu:  ## Diff two CPU profiles (PPROF_BASE=path PPROF_NEW=path; absolute, repo-relative, or svc-relative all work)
	@if [ -z "$(PPROF_BASE)" ] || [ -z "$(PPROF_NEW)" ]; then \
		echo "usage: make profile-diff-cpu PPROF_BASE=<path> PPROF_NEW=<path>" >&2; \
		echo "  paths may be absolute, relative to the repo root, or relative to apps/ledger-svc/" >&2; \
		exit 1; fi
	@base="$(PPROF_BASE)"; [ -f "$$base" ] || base="$(ROOT)/$(PPROF_BASE)"; \
	new="$(PPROF_NEW)";  [ -f "$$new" ]  || new="$(ROOT)/$(PPROF_NEW)"; \
	if [ ! -f "$$base" ]; then echo "PPROF_BASE not found: $(PPROF_BASE)" >&2; exit 1; fi; \
	if [ ! -f "$$new"  ]; then echo "PPROF_NEW not found:  $(PPROF_NEW)"  >&2; exit 1; fi; \
	go tool pprof -http=:8888 -base "$$base" "$$new"

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
