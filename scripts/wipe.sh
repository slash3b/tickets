#!/usr/bin/env bash
#
# Wipe accumulated state without wrecking the system that produced it.
#
# Two independent stores accumulate: the application's transactional rows in
# Postgres, and the telemetry in ClickHouse behind SigNoz. They are wiped
# separately because you usually want one and not the other — resetting the
# simulation while keeping the traces that explain the last run is the common case.
#
# RUN IT ON THE CONTROL PLANE. It drives everything through kubectl exec:
#   ssh slash3b@192.168.1.116 'bash -s' < scripts/wipe.sh --all --seed
#
# Usage:
#   wipe.sh --data          transactional rows only (showings, holds, orders, payments)
#   wipe.sh --telemetry     SigNoz traces, logs and metrics only
#   wipe.sh --all           both
#   wipe.sh --seed          seed one fresh showing afterwards
#   wipe.sh --yes           do not ask
#   wipe.sh --dry-run       print what would happen and stop
set -euo pipefail

NS_APP=tickets
NS_DATA=data
NS_SIGNOZ=signoz
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
    -h|--help)   sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $arg (try --help)" >&2; exit 2 ;;
  esac
done

if ! $do_data && ! $do_telemetry && ! $do_seed; then
  echo "nothing selected. --data, --telemetry, --all or --seed (see --help)" >&2
  exit 2
fi

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# THE TABLES THAT GET EMPTIED. Everything here is produced by the system running;
# none of it is configuration.
#
# catalog.venues, catalog.sections and catalog.seats are DELIBERATELY ABSENT. They
# describe the cinema itself, not anything that accumulated — Screen 1 has the same
# 96 seats tomorrow. More importantly the seeder looks the venue up by name and
# RETURNS AN ERROR IF IT IS MISSING; it does not create one. Truncating venues
# therefore breaks the 03:00 CronJob permanently, and it fails somewhere nobody is
# watching, which is the worst possible place for it.
#
# One statement, so Postgres resolves the foreign keys itself rather than making
# the order of this list load-bearing.
read -r -d '' TRUNCATE_SQL <<'SQL' || true
TRUNCATE
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

say "PLAN"
$do_data      && echo "  postgres  $DB: empty the transactional tables, KEEP the venue and its seats"
$do_telemetry && echo "  clickhouse: empty SigNoz traces, logs and metrics"
$do_seed      && echo "  seeder    : create one fresh showing"
echo "  simulator : paused for the duration either way, so it cannot write mid-wipe"

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
  echo "  kept:   venues/seats in the catalog = $kept  <- the seeder needs these"
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
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef: {name: tickets-pg-app, key: uri}
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
