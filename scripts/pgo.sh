#!/usr/bin/env bash
#
# Refresh the profile-guided-optimisation profiles.
#
# PGO IS ONLY AS GOOD AS THE PROFILE. A profile taken on an idle system optimises
# the idle path, which is the one thing it must not be pointed at — so this drives
# a real on-sale and profiles the services WHILE the contention is happening.
#
# RUN IT FROM THE CONTROL PLANE:
#   ssh slash3b@192.168.1.116 'bash -s' < scripts/pgo.sh
# then copy /tmp/pgo/*.pgo into services/<name>/cmd/default.pgo and commit.
#
# WHY port-forward AND NOT A SERVICE: pkg/profiling binds pprof to 127.0.0.1
# inside the pod, so it is unreachable from other pods and from the Gateway API.
# port-forward attaches to the pod's own network namespace, which is why loopback
# is still reachable this way and only this way.
set -u

H="app.tickets.lan:443:192.168.1.240"; B="https://app.tickets.lan"
c(){ curl -sk --resolve "$H" -H 'Content-Type: application/json' "$@"; }

BUYERS=${BUYERS:-3000}
OVER=${OVER:-40}
SECONDS_PROFILE=${SECONDS_PROFILE:-60}
OUT=${OUT:-/tmp/pgo}

# A 20,000-seat arena, because a sold-out cinema produces a profile of failed
# holds rather than of the system doing its job.
echo "staging an arena"
EV=$(c -X POST $B/api/admin/showings \
      -d '{"venue":"arena","title":"PGO Profile Load","on_sale_in_seconds":20}' | jq -r .event_id)
echo "  event ${EV:0:8}"

for i in $(seq 1 40); do
  SEC=$(c $B/api/events/$EV/sections | jq -r '.sections[0].id // empty')
  [ -n "$SEC" ] && N=$(c $B/api/events/$EV/sections/$SEC | jq '[.seats[]?|select(.status=="available")]|length') || N=0
  [ "${N:-0}" -gt 100 ] && { echo "  on sale, $N seats in section 0"; break; }
  sleep 5
done
[ "${N:-0}" -gt 100 ] || { echo "event never went on sale"; exit 1; }

rm -rf "$OUT"; mkdir -p "$OUT"
declare -A NS=( [gateway]=tickets [catalog]=tickets [inventory]=tickets [orders]=tickets
                [payments]=tickets [simulator]=tickets [bank]=bank )
# workers is deliberately absent: it is a ticker and profiles at ~10ms of samples
# in a minute. A profile that thin is noise, and PGO fed noise optimises noise.

port=16060; declare -A PORT; declare -a FWD
for svc in "${!NS[@]}"; do
  kubectl -n "${NS[$svc]}" port-forward deploy/"$svc" $port:6060 >/dev/null 2>&1 &
  FWD+=($!); PORT[$svc]=$port; port=$((port+1))
done
# Kill the forwards on ANY exit. Without this they outlive the script and the
# next run cannot bind its ports.
trap 'kill "${FWD[@]}" 2>/dev/null' EXIT
sleep 8

# Profiles first, load second, so the burst lands inside the window.
declare -a JOBS
for svc in "${!NS[@]}"; do
  curl -s --max-time $((SECONDS_PROFILE + 60)) -o "$OUT/$svc.pgo" \
    "http://127.0.0.1:${PORT[$svc]}/debug/pprof/profile?seconds=${SECONDS_PROFILE}" &
  JOBS+=($!)
done
sleep 3

echo "firing $BUYERS buyers over ${OVER}s"
c -X POST $B/admin/sim/onsale \
  -d "{\"event_id\":\"$EV\",\"buyers\":$BUYERS,\"over_seconds\":$OVER,\"group_share\":0.3}" \
  | jq -c '{sessions,bought,lost_race_409,errors,took}'

# Wait for the PROFILES ONLY. A bare `wait` also waits on the port-forwards,
# which never exit, so the script hangs forever — it did exactly that the first
# time this was written.
wait "${JOBS[@]}"

echo ""
echo "profiles in $OUT — check the sample totals before trusting them:"
for f in "$OUT"/*.pgo; do
  printf '  %-12s %s\n' "$(basename "$f")" \
    "$(go tool pprof -top -nodecount=1 "$f" 2>/dev/null | grep -i 'Duration' | head -1)"
done
