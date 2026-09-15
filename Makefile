.DEFAULT_GOAL := help
SHELL := /bin/bash

# Local (non-docker) runs talk to the compose Postgres/Redis over their published
# ports, so host names differ from the in-network ones in .env.
LOCAL_DATABASE_URL ?= postgres://recipes:recipes@localhost:5432/recipes?sslmode=disable
LOCAL_REDIS_URL ?= redis://localhost:6379/0

# Integration tests reset the schema they run against, so they get their own
# database on the same instance. Pointing them at the dev database would drop an
# ingested 64k corpus.
TEST_DB_NAME ?= recipes_test
TEST_DATABASE_URL ?= postgres://recipes:recipes@localhost:5432/$(TEST_DB_NAME)?sslmode=disable

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: network .env ## Create the shared network and a local .env
	@echo "ready: edit .env, then run make up"

.PHONY: network
network: ## Create the shared recipe-net network (idempotent)
	@docker network inspect recipe-net >/dev/null 2>&1 \
		|| docker network create recipe-net

.env:
	@cp .env.example .env
	@echo "created .env from .env.example — set JWT_SECRET before exposing this service"

.PHONY: up
up: network ## Start postgres, redis, migrations and core-api
	docker compose up --build -d

.PHONY: down
down: ## Stop the stack, keep data
	docker compose down

.PHONY: reset
reset: ## Stop the stack and delete the database volume
	docker compose down -v

.PHONY: logs
logs: ## Tail core-api logs
	docker compose logs -f core-api

.PHONY: migrate
migrate: ## Apply migrations
	docker compose run --rm migrate -command up

.PHONY: migrate-status
migrate-status: ## Show migration status
	docker compose run --rm migrate -command status

.PHONY: migrate-down
migrate-down: ## Roll back the last migration
	docker compose run --rm migrate -command down

.PHONY: psql
psql: ## Open a psql shell
	docker compose exec postgres psql -U recipes -d recipes

.PHONY: ingest
ingest: ## Load the 64k corpus (expects data/recipes.csv)
	docker compose --profile ingest run --rm ingest

.PHONY: build
build: ## Build the binaries locally
	go build ./...

.PHONY: test
test: ## Run unit tests
	go test ./...

.PHONY: test-db
test-db: ## Create the throwaway integration-test database (idempotent)
	@docker compose exec -T postgres \
		psql -U recipes -d postgres -tc \
		"SELECT 1 FROM pg_database WHERE datname = '$(TEST_DB_NAME)'" \
		| grep -q 1 \
		|| docker compose exec -T postgres createdb -U recipes $(TEST_DB_NAME)

.PHONY: test-integration
test-integration: test-db ## Run integration tests against a throwaway database
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -tags=integration -count=1 ./...

.PHONY: lint
lint: ## Vet and gofmt check
	go vet ./...
	@out=$$(gofmt -l . | grep -v '^tmp/' || true); \
		if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: run
run: ## Run core-api on the host against the compose dependencies
	DATABASE_URL="$(LOCAL_DATABASE_URL)" REDIS_URL="$(LOCAL_REDIS_URL)" \
		go run ./cmd/api
