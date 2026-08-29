.PHONY: help build test vet oversell pg-up pg-down pg-url \
        wipe-plan wipe wipe-all wipe-telemetry

PG_CONTAINER := tickets-pg-dev
PG_URL       := postgres://tickets:tickets@127.0.0.1:55432/tickets?sslmode=disable

# The homelab control plane. The wipe targets run THERE, because the script drives
# everything through kubectl and needs a shell that can reach the cluster.
CONTROL_PLANE := slash3b@192.168.1.116

help:
	@grep -E '^[a-z-]+:.*?##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/' | column -t -s "$$(printf '\t')"

build: ## compile everything
	go build ./...

vet: ## go vet
	go vet ./...

## -p 1 IS GONE, because the thing it was working around is fixed.
## It used to say: packages share one database and truncate it on setup, so in
## parallel one package's TRUNCATE deletes rows another just created. It also said
## the alternative was a database per package. That alternative now exists —
## pkg/pgtest hands each package its own — so the suite runs parallel again.
## Verified over nine consecutive full runs before this flag came off.
test: ## unit tests. Needs DATABASE_URL for the store tests; skips them without it.
	DATABASE_URL="$(PG_URL)" go test -race -shuffle=on -timeout=3m ./...

## The oversell test is NOT part of `make test` and not part of CI.
## It fires 1000 concurrent goroutines at a database — a load test wearing a unit
## test's clothes. Run it deliberately, after changing the claim primitive, and
## read the throughput line rather than just the pass/fail.
oversell: ## MANUAL: 1000 goroutines vs 10 seats, at the store AND over gRPC
	DATABASE_URL="$(PG_URL)" go test -tags oversell -race -count=1 -v -timeout=5m \
		./services/inventory/store/ ./services/inventory/oversell/

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


## ---------------------------------------------------------------------------
## WIPING THE RUNNING SYSTEM
##
## scripts/wipe.sh is piped to the control plane over ssh, so it needs no copy of
## the repo on the node — but that also means the script IS stdin. Its interactive
## "type WIPE" prompt reads the same stdin and gets EOF immediately, so it can
## never be answered that way; it fails closed with "aborted". These targets pass
## --yes and put the guard here instead, where it can actually work.
##
##   make wipe-plan                 show what would happen, change nothing
##   make wipe CONFIRM=WIPE         app state: postgres, redis, bank charges
##   make wipe-telemetry CONFIRM=WIPE   SigNoz traces, logs and metrics
##   make wipe-all CONFIRM=WIPE     both
##
## After a wipe there are no showings and nothing creates them — the seeder
## CronJob is suspended. Stage one at https://app.tickets.lan/admin.

define require_confirm
	@if [ "$(CONFIRM)" != "WIPE" ]; then \
		echo "This deletes data on $(CONTROL_PLANE) and cannot be undone."; \
		echo "Re-run it as:  make $@ CONFIRM=WIPE"; \
		echo "Or preview it: make wipe-plan"; \
		exit 1; \
	fi
endef

wipe-plan: ## show what a data wipe would do, changing nothing
	ssh $(CONTROL_PLANE) 'bash -s -- --all --dry-run' < scripts/wipe.sh

wipe: ## DESTRUCTIVE: postgres rows, the redis projection, the bank's charges
	$(require_confirm)
	ssh $(CONTROL_PLANE) 'bash -s -- --data --yes' < scripts/wipe.sh

wipe-telemetry: ## DESTRUCTIVE: SigNoz traces, logs and metrics
	$(require_confirm)
	ssh $(CONTROL_PLANE) 'bash -s -- --telemetry --yes' < scripts/wipe.sh

wipe-all: ## DESTRUCTIVE: everything above, application state and telemetry
	$(require_confirm)
	ssh $(CONTROL_PLANE) 'bash -s -- --all --yes' < scripts/wipe.sh
