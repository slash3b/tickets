#!/usr/bin/env bash
#
# Wipe accumulated state without wrecking the system that produced it.
#
# FOUR places accumulate state, not two: the application's transactional rows in
# Postgres, the seat-map projection in Redis, the fake bank's charges (in memory,
# so a restart is the wipe), and the telemetry in ClickHouse behind SigNoz.
#
# --data covers the first three, because they are one system: seat rows, the
# projection of those rows, and the charges against them. Leaving Redis behind
# after truncating Postgres is how you get a seat map for a showing that no longer
# exists. Telemetry is separate because you usually want one and not the other —
# resetting the simulation while keeping the traces that explain the last run is
# the common case.
#
# RUN IT ON THE CONTROL PLANE. It drives everything through kubectl exec, so it
# needs a shell that can reach the cluster, not a copy of the repo:
#
#   ssh slash3b@192.168.1.116 'bash -s -- --all --seed' < scripts/wipe.sh
#
# OR JUST USE THE MAKE TARGETS, which is what these notes recommend:
#   make wipe-plan                     show what would happen, change nothing
#   make wipe CONFIRM=WIPE             postgres, the redis projection, the bank
#   make wipe-telemetry CONFIRM=WIPE   SigNoz only
#   make wipe-all CONFIRM=WIPE         both
#
# --yes IS REQUIRED over ssh. The script arrives ON stdin, so the "type WIPE"
# prompt below reads that same stdin, gets EOF and aborts. It fails closed, but it
# cannot be answered — the guard lives in the makefile instead.
#
# THE FLAGS GO INSIDE THE QUOTES, after `--`. Written the obvious way round —
#   ssh host 'bash -s' < scripts/wipe.sh --all --seed
# — the shell hands --all and --seed to ssh instead of to the script, ssh passes
# them to bash as its own options, and bash answers "invalid option" with no hint
# that the flags were ever meant for the script.
#
# Usage:
#   wipe.sh --data          showings, seats, holds, orders, payments, venues,
#                           the Redis projection and the bank's charges
#   wipe.sh --telemetry     SigNoz traces, logs and metrics only
#   wipe.sh --all           both
#   wipe.sh --seed          seed one fresh showing afterwards
#   wipe.sh --yes           do not ask
#   wipe.sh --dry-run       print what would happen and stop
set -euo pipefail

NS_APP=tickets
NS_DATA=data
NS_SIGNOZ=signoz
NS_BANK=bank
PG_CLUSTER=tickets-pg
DB=tickets

do_data=false do_telemetry=false do_seed=false assume_yes=false dry_run=false

for arg in "$@"; do
  case "$arg" in
    --data)      do_data=true ;;
    --telemetry) do_telemetry=true ;;
    --all)       do_data=true; do_telemetry=true ;;
    --seed)      do_seed=true ;;
    --yes|-y)    assume_yes=true ;;
    --dry-run)   dry_run=true ;;
    # Print the whole header comment rather than a hardcoded line range — the
    # range was 2,25 and the header has since grown past it, which silently cut
    # the usage block off the bottom of --help.
    -h|--help)   awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "$0"; exit 0 ;;
    *) echo "unknown flag: $arg (try --help)" >&2; exit 2 ;;
  esac
done

if ! $do_data && ! $do_telemetry && ! $do_seed; then
  echo "nothing selected. --data, --telemetry, --all or --seed (see --help)" >&2
  exit 2
fi

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# THE TABLES THAT GET EMPTIED — now including the venues.
#
# VENUES USED TO BE PRESERVED and no longer are. The old reason was that the
# seeder looked its venue up by name and failed if it was missing, so truncating
# venues broke the 03:00 CronJob somewhere nobody was watching. That reason was
# WRONG about the seeder — it creates the venue when it is absent (see
# services/seeder, the ErrNoVenue branch) — and is now moot anyway: the CronJob is
# suspended and the operator page builds whatever room it is asked for. Keeping
# them only accumulated half-used seating charts from custom layouts.
#
# One statement, so Postgres resolves the foreign keys itself rather than making
# the order of this list load-bearing.
#
# These are four services' schemas in one database. That is not a boundary
# violation — each service is the only one with credentials for its own schema —
# but it does mean the wipe reaches across all four, which is exactly what makes
# it a wipe and not four of them.
read -r -d '' TRUNCATE_SQL <<'SQL' || true
TRUNCATE
  catalog.venues,
  catalog.sections,
  catalog.seats,
  catalog.events,
  catalog.event_prices,
  inventory.event_seats,
  inventory.holds,
  inventory.hold_seats,
  orders.orders,
  orders.saga_log,
  payments.payments
CASCADE;
SQL

psql_do() {
  kubectl -n "$NS_DATA" exec "$PG_CLUSTER-1" -c postgres -- \
    psql -qAt -U postgres -d "$DB" -c "$1"
}

ch_do() {
  kubectl -n "$NS_SIGNOZ" exec pod/chi-signoz-clickhouse-cluster-0-0-0 -c clickhouse -- \
    clickhouse-client -q "$1"
}

redis_do() {
  kubectl -n "$NS_DATA" exec deploy/redis -- redis-cli "$@"
}

say "PLAN"
$do_data      && echo "  postgres  $DB: empty every table the system writes, venues included"
$do_data      && echo "  redis     : flush the seat-map projection"
$do_data      && echo "  bank      : restart, which is how its in-memory charges are cleared"
$do_telemetry && echo "  clickhouse: empty SigNoz traces, logs and metrics"
$do_seed      && echo "  seeder    : create one fresh showing"
echo "  simulator : paused for the duration either way, so it cannot write mid-wipe"
echo ""
echo "  KAFKA IS NOT PURGED, and does not need to be. Consumers use a unique group"
echo "  per process starting at kafka.LastOffset, so nothing replays old messages"
echo "  into a fresh projection. The topics age out on their own retention."

if $dry_run; then
  say "DRY RUN — stopping here"
  $do_data && { echo "$TRUNCATE_SQL"; }
  exit 0
fi

if ! $assume_yes; then
  printf '\ntype WIPE to continue: '
  read -r reply
  [ "$reply" = "WIPE" ] || { echo "aborted"; exit 1; }
fi

# PAUSE THE LOAD FIRST. The simulator buys continuously; truncating underneath it
# leaves rows written between the TRUNCATE and the seed, which is exactly the kind
# of half-state that makes the next hour of debugging pointless.
say "pausing the simulator"
replicas=$(kubectl -n "$NS_APP" get deploy simulator -o jsonpath='{.spec.replicas}')
kubectl -n "$NS_APP" scale deploy/simulator --replicas=0 >/dev/null
kubectl -n "$NS_APP" rollout status deploy/simulator --timeout=90s >/dev/null 2>&1 || true
# Bring it back whatever happens below, including on a failure or a Ctrl-C.
restore() {
  say "restoring the simulator to $replicas replica(s)"
  kubectl -n "$NS_APP" scale deploy/simulator --replicas="$replicas" >/dev/null || true
}
trap restore EXIT

if $do_data; then
  say "emptying the application tables"
  before=$(psql_do "SELECT
      (SELECT count(*) FROM catalog.events)      AS showings,
      (SELECT count(*) FROM inventory.event_seats) AS seats,
      (SELECT count(*) FROM orders.orders)       AS orders,
      (SELECT count(*) FROM payments.payments)   AS payments" | tr '|' ' ')
  echo "  before: showings/seats/orders/payments = $before"
  psql_do "$TRUNCATE_SQL" >/dev/null
  after=$(psql_do "SELECT
      (SELECT count(*) FROM catalog.events),
      (SELECT count(*) FROM inventory.event_seats),
      (SELECT count(*) FROM orders.orders),
      (SELECT count(*) FROM payments.payments)" | tr '|' ' ')
  echo "  after:  showings/seats/orders/payments = $after"
  kept=$(psql_do "SELECT
      (SELECT count(*) FROM catalog.venues),
      (SELECT count(*) FROM catalog.seats)" | tr '|' ' ')
  echo "  kept:   venues/seats in the catalog = $kept  <- both should be 0"

  # THE PROJECTION HAS TO GO WITH THE ROWS IT PROJECTS. Redis holds seat maps
  # keyed by event id; the events are gone, so every key in here is now a map of a
  # showing that does not exist. FLUSHDB rather than deleting seatstatus:* because
  # this Redis holds nothing else, and a pattern list is one more thing to forget
  # to update the next time something is cached.
  say "flushing the seat-map projection"
  keys=$(redis_do DBSIZE | tr -d '\r')
  redis_do FLUSHDB >/dev/null
  echo "  redis: $keys keys -> $(redis_do DBSIZE | tr -d '\r')"

  # THE BANK KEEPS ITS CHARGES IN A MAP, not a database, so there is nothing to
  # truncate — the restart IS the wipe. Leaving them behind means the idempotency
  # keys from the last run are still live, and a replayed key returns the OLD
  # charge instead of making a new one.
  say "restarting the bank to clear its charges"
  kubectl -n "$NS_BANK" rollout restart deploy/bank >/dev/null
  kubectl -n "$NS_BANK" rollout status deploy/bank --timeout=120s >/dev/null
  echo "  bank: restarted, charge ledger empty"
fi

if $do_telemetry; then
  say "emptying SigNoz"
  # Every data table in SigNoz's databases EXCEPT schema_migrations_v2, which is
  # SigNoz's own bookkeeping of which migrations it has applied. Truncating that
  # does not clear data, it makes SigNoz think it is a fresh install and try to
  # rebuild schemas that already exist.
  #
  # Distributed tables and views are skipped: they hold no data of their own, and
  # truncating the local table underneath is what actually frees the space.
  tables=$(ch_do "SELECT database || '.' || name
                  FROM system.tables
                  WHERE database LIKE 'signoz%'
                    AND engine NOT LIKE '%View%'
                    AND engine != 'Distributed'
                    AND name != 'schema_migrations_v2'
                  ORDER BY database, name")
  n=0
  while read -r t; do
    [ -n "$t" ] || continue
    ch_do "TRUNCATE TABLE IF EXISTS $t" >/dev/null
    n=$((n+1))
  done <<< "$tables"
  echo "  truncated $n tables"
  ch_do "SELECT database, formatReadableQuantity(sum(total_rows)) AS rows
         FROM system.tables WHERE database LIKE 'signoz%'
           AND engine NOT LIKE '%View%' AND engine != 'Distributed'
         GROUP BY database ORDER BY database FORMAT PrettyCompactMonoBlock"
fi

if $do_seed; then
  say "seeding one showing"
  kubectl -n "$NS_APP" delete job wipe-seed --ignore-not-found >/dev/null 2>&1
  img=$(kubectl -n "$NS_APP" get cronjob seeder \
        -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')
  kubectl apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata: {name: wipe-seed, namespace: $NS_APP}
spec:
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: OnFailure
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: seeder
          image: $img
          env:
            # The seeder has no database of its own since the split: it creates
            # the showing through catalog and opens the seats through inventory,
            # so both must be up for this step to work.
            - {name: CATALOG_ADDR,   value: "catalog.$NS_APP.svc.cluster.local:9090"}
            - {name: INVENTORY_ADDR, value: "inventory.$NS_APP.svc.cluster.local:9090"}
            - {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "signoz-otel-collector.$NS_SIGNOZ.svc.cluster.local:4318"}
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits:   {memory: 96Mi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
YAML
  kubectl -n "$NS_APP" wait --for=condition=complete job/wipe-seed --timeout=150s >/dev/null
  kubectl -n "$NS_APP" logs job/wipe-seed --tail=1 | sed 's/^/  /'
fi

say "done"
