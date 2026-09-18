# Ọja — card scheme, merchant acceptance, private exchange
#
# The database is the only hard dependency. `make db` will use a local
# Postgres if one is listening, and fall back to docker compose otherwise.

API        := apps/api
DB_HOST    ?= localhost
DB_PORT    ?= 5432
SCHEME_URL ?= postgres://freedom_scheme:freedom@$(DB_HOST):$(DB_PORT)/freedom
PART_URL   ?= postgres://freedom_participant:freedom@$(DB_HOST):$(DB_PORT)/freedom

export DATABASE_URL = $(SCHEME_URL)

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: db
db: ## Create the database and the two Postgres roles
	@pg_isready -h $(DB_HOST) -p $(DB_PORT) >/dev/null 2>&1 || docker compose up -d
	@psql -h $(DB_HOST) -p $(DB_PORT) -d postgres -tAc \
	  "SELECT 1 FROM pg_database WHERE datname='freedom'" | grep -q 1 \
	  || createdb -h $(DB_HOST) -p $(DB_PORT) freedom
	@psql -h $(DB_HOST) -p $(DB_PORT) -d freedom -q -v ON_ERROR_STOP=1 -f db/bootstrap.sql
	@echo "database ready at $(SCHEME_URL)"

.PHONY: test
test: db ## Run every test, including the end-to-end rail
	# -p 1 because the database-backed packages share one database: the
	# end-to-end suite truncates the network between runs, which deadlocks
	# against another package inserting into it at the same moment.
	cd $(API) && go test ./... -count=1 -p 1

.PHONY: e2e
e2e: db ## Run only the end-to-end golden path
	cd $(API) && go test ./internal/e2e/ -count=1 -v

.PHONY: unit
unit: ## Run only the tests that need no database
	cd $(API) && go test ./internal/money/ ./internal/share/ ./internal/alloc/ \
	  ./internal/fee/ ./internal/tapcrypto/ ./internal/scheme/ -count=1

.PHONY: race
race: db ## Run the concurrency tests under the race detector
	cd $(API) && go test ./internal/e2e/ -count=1 -race -run Concurrent

.PHONY: vet
vet: ## go vet and gofmt check
	cd $(API) && go vet ./...
	@test -z "$$(cd $(API) && gofmt -l .)" || { \
	  echo "unformatted files:"; cd $(API) && gofmt -l .; exit 1; }

.PHONY: run
run: db ## Run exchanged locally: rail (RAIL_TOKEN=dev-rail-token) and console (CONSOLE_TOKEN=dev-console-token) mounted
	cd $(API) && RAIL_TOKEN=$${RAIL_TOKEN:-dev-rail-token} CONSOLE_TOKEN=$${CONSOLE_TOKEN:-dev-console-token} \
	  MARKET_CLOSE_AT=$${MARKET_CLOSE_AT:-off} MM_QUOTE_AT=$${MM_QUOTE_AT:-off} PORT=$${PORT:-8081} go run ./cmd/exchanged

.PHONY: reset
reset: ## Drop and recreate the schema
	psql -h $(DB_HOST) -p $(DB_PORT) -d freedom -q -c \
	  "DROP SCHEMA public CASCADE; CREATE SCHEMA public; \
	   CREATE EXTENSION IF NOT EXISTS btree_gist; \
	   GRANT ALL ON SCHEMA public TO freedom_scheme; \
	   GRANT USAGE ON SCHEMA public TO freedom_participant;"
	@echo "schema dropped; the next test run will migrate it"

.PHONY: check
check: vet test ## Everything CI would run
