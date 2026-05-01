# fintech-backend — root orchestration Makefile.
#
# Owns: docker-compose stack lifecycle, monorepo-wide nx, proto generation.
# Service workflows live in their own Makefiles, reachable via:
#
#   make -C apps/<svc> <target>
#
# Why split: services have different needs. Some own database migrations,
# some don't. Co-locating each service's verbs with its code keeps the
# root file an orchestration layer rather than a junk drawer of every
# service's targets prefixed by name.
#
# Env layering:
#   * Root .env: shared infra coords (DATABASE_URL, POSTGRES_*, REDIS_URL,
#     KAFKA_BROKERS). Loaded here for compose's sake and inherited by
#     any `make -C` child invocations.
#   * Per-service .env: app-internal knobs. Loaded by the service's
#     Makefile AND by the service binary at runtime via godotenv.
# CI can override either by exporting variables before `make`.

ifneq (,$(wildcard ./.env))
include .env
export
endif

COMPOSE      := docker compose
COMPOSE_LOAD := docker compose -f docker-compose.yml -f docker-compose.load.yml

# Auto-discover service directories that define a migrate-up target.
# Stateless services (no schema of their own) simply omit the target
# from their Makefile and are skipped automatically — adding a new
# stateful service requires no edit to this file.
SERVICES_WITH_MIGRATIONS := $(shell \
	for mf in apps/*/Makefile; do \
		grep -lE '^migrate-up:' "$$mf" 2>/dev/null | xargs -r dirname; \
	done)

.DEFAULT_GOAL := help

##@ General
.PHONY: help
help:  ## Show this help (root targets + list of service Makefiles)
	@awk 'BEGIN {FS = ":.*##"; printf "\nRoot targets:\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo ""
	@echo "Service Makefiles (run \`make -C <dir> help\` for details):"
	@for mf in apps/*/Makefile; do echo "  $$mf"; done
	@echo ""

##@ Build
.PHONY: build
build:  ## Build all Go binaries via nx (whole monorepo)
	pnpm exec nx run-many --target=build

.PHONY: test
test:  ## Run all Go tests via nx (whole monorepo)
	pnpm exec nx run-many --target=test

.PHONY: proto-gen
proto-gen:  ## Regenerate gRPC stubs from shared/proto
	cd shared/proto && buf lint && buf generate

##@ Stack (shared infra)
.PHONY: stack-up
stack-up: compose-up wait-postgres migrate-all  ## Bring up shared infra + apply all service migrations
	@echo ""
	@echo "Stack up. Run a service:  make -C apps/<svc> run-server"

.PHONY: stack-up-load
stack-up-load:  ## Bring up shared infra in LOAD-TEST profile (8GB PG, 8GB Redis, 4GB Redpanda)
	$(COMPOSE_LOAD) up -d postgres redis redpanda
	@$(MAKE) wait-postgres
	@$(MAKE) migrate-all
	@echo ""
	@echo "Load-test stack up. Run a service:  make -C apps/<svc> run-server"
	@echo "Or:  make -C apps/ledger-svc loadtest-seed && make -C apps/ledger-svc loadtest-run"

.PHONY: stack-down
stack-down:  ## Stop and DELETE the docker-compose stack (drops pgdata volume)
	$(COMPOSE) down -v

.PHONY: compose-up
compose-up:  ## docker-compose up postgres / redis / redpanda
	$(COMPOSE) up -d postgres redis redpanda

.PHONY: compose-logs
compose-logs:  ## Tail docker-compose logs
	$(COMPOSE) logs -f

# wait-postgres polls pg_isready until the container reports ready or we hit
# a hard 60s ceiling. Avoids the racy "compose up && immediately migrate"
# that fails on a slow first boot. POSTGRES_USER / POSTGRES_DB come from
# root .env — required, no defaults. Forces .env-as-source-of-truth so
# this file stays free of any service's specific naming.
.PHONY: wait-postgres
wait-postgres:  ## Block until Postgres accepts connections (60s ceiling)
	@if [ -z "$$POSTGRES_USER" ] || [ -z "$$POSTGRES_DB" ]; then \
		echo "POSTGRES_USER / POSTGRES_DB unset — cp .env.example .env and retry" >&2; \
		exit 1; \
	fi
	@echo "waiting for postgres..."
	@for i in $$(seq 1 60); do \
		if $(COMPOSE) exec -T postgres pg_isready \
			-U "$$POSTGRES_USER" \
			-d "$$POSTGRES_DB" >/dev/null 2>&1; then \
			echo "postgres ready"; exit 0; \
		fi; sleep 1; \
	done; \
	echo "postgres did not become ready within 60s" >&2; exit 1

# migrate-all delegates to every service Makefile that defines migrate-up.
# Discovery is grep-driven (see SERVICES_WITH_MIGRATIONS at top); no service
# names are hardcoded here.
.PHONY: migrate-all
migrate-all:  ## Apply migrations for every service that defines them
	@if [ -z "$(SERVICES_WITH_MIGRATIONS)" ]; then \
		echo "no services with migrate-up targets found"; \
	else \
		for d in $(SERVICES_WITH_MIGRATIONS); do \
			echo ">>> migrate-up: $$d"; \
			$(MAKE) -C "$$d" migrate-up || exit $$?; \
		done; \
	fi

##@ Cleanup
.PHONY: clean
clean:  ## Remove built binaries (root dist/)
	rm -rf dist
