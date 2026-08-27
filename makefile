.PHONY: help build test vet oversell pg-up pg-down pg-url

PG_CONTAINER := tickets-pg-dev
PG_URL       := postgres://tickets:tickets@127.0.0.1:55432/tickets?sslmode=disable

help:
	@grep -E '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | column -t -s "$$(printf '\t')"

build: ## compile everything
	go build ./...

vet: ## go vet
	go vet ./...

test: ## unit tests. Needs DATABASE_URL for the store tests; skips them without it.
	DATABASE_URL="$(PG_URL)" go test -race -shuffle=on -timeout=2m ./...

## The oversell test is NOT part of `make test` and not part of CI.
## It fires 1000 concurrent goroutines at a database — a load test wearing a unit
## test's clothes. Run it deliberately, after changing the claim primitive, and
## read the throughput line rather than just the pass/fail.
oversell: ## MANUAL: 1000 goroutines vs 10 seats, proves no oversell
	DATABASE_URL="$(PG_URL)" go test -tags oversell -race -count=1 -v -timeout=5m \
		./services/inventory/store/

pg-up: ## start a throwaway Postgres for tests
	@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	docker run -d --name $(PG_CONTAINER) \
		-e POSTGRES_USER=tickets -e POSTGRES_PASSWORD=tickets -e POSTGRES_DB=tickets \
		-p 55432:5432 postgres:18-alpine >/dev/null
	@printf 'waiting for postgres'; \
	for i in $$(seq 1 40); do \
		docker exec $(PG_CONTAINER) pg_isready -U tickets -q 2>/dev/null && { echo " ready"; exit 0; }; \
		printf '.'; sleep 0.5; \
	done; echo " TIMED OUT"; exit 1

pg-down: ## remove it
	docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true

pg-url: ## print the connection string
	@echo $(PG_URL)
