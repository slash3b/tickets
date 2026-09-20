THE APPLICATION - THE TICKETS PLATFORM ITSELF
=============================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


THE APPLICATION - DEPLOYED 2026-08-27
-------------------------------------

  Redis has its OWN Argo application since 2026-08-29 rather than living inside
  `data` with Postgres and Kafka. It has a different lifecycle from both - no PVC,
  losing it costs latency and no data - and sharing a sync boundary meant the
  safest thing in the data plane could only be touched at the pace of the
  riskiest. Argo adopted the running pod rather than recreating it, so the split
  cost no downtime at all.

  ns tickets, Argo app `tickets`, wave 5. SPLIT INTO NINE SERVICES 2026-08-28;
  before that, catalog/inventory/orders/payments were packages inside gateway and
  workers. They speak gRPC on :9090, generated from proto/tickets/v1, and each
  serves /livez and /readyz over HTTP on :8080 because kubelet probes speak HTTP.
    catalog     what EXISTS             grpc only, no HTTPRoute
    inventory   what is AVAILABLE       grpc only. THE CONTENDED CORE
    orders      the saga                grpc only. calls inventory + payments
    payments    whether money moved     grpc only. the only link to the bank
    gateway     the public API          https://api.tickets.lan
    workers     every singleton         replicas 1, strategy Recreate
    simulator   the load                https://sim.tickets.lan  (/stats, /config)
    seeder      CronJob 03:00 daily     one showing, idempotent
                SEED_DAYS_AHEAD=N to create one on demand, N days out. The daily
                run leaves it unset.

  THE SHOWING USED TO SELL OUT IN NINETY MINUTES, and that was twice mistaken for
  broken instrumentation: with nothing left to sell, holds, orders, payments and
  the bank emit nothing, and an idle half of a system looks exactly like an
  unreported one. Repaced 2026-08-28 to last a full day - by changing the buyer
  MIX to 93% browsers, not by lowering the arrival rate, which would have hit the
  same target by making the system silent between arrivals. See milestone 5 in
  DESIGN.md for the arithmetic.

  If the interesting half of the system looks idle, check whether there is
  anything left to sell before assuming instrumentation broke.

  WIPING ACCUMULATED STATE: scripts/wipe.sh, run ON THE CONTROL PLANE.
    ssh slash3b@192.168.1.116 'bash -s' < scripts/wipe.sh --all --seed
  --data is Postgres, --telemetry is ClickHouse, --all is both, --dry-run prints
  the plan and stops. It pauses the simulator and restores it from a trap, so a
  failure or a Ctrl-C halfway cannot leave the load generator writing into a
  half-truncated database.

  IT KEEPS TWO THINGS THAT LOOK LIKE OMISSIONS. catalog.venues, sections and seats
  describe the cinema, not anything that accumulated - and the seeder looks the
  venue up BY NAME and returns an error if it is gone rather than creating one, so
  truncating venues breaks the 03:00 CronJob permanently and fails at 3am where
  nobody is watching. signoz schema_migrations_v2 is SigNoz's record of which
  migrations it has applied; truncating it clears no data and makes SigNoz believe
  it is a fresh install.

  RUN 2026-08-28: 4 showings, 184 orders, 67k spans and 41M metric rows to zero;
  venue and its 96 seats intact; one fresh showing seeded; simulator restored.
    web         the seat map, React     https://app.tickets.lan

  THE SEAT MAP IS SERVED AS STATIC FILES AND NOTHING ELSE. nginx holds the built
  bundle; it does not proxy. The HTTPRoute for app.tickets.lan has two rules -
  /api goes to the gateway Service, / goes to the bundle - so the browser sees one
  origin and there is no CORS anywhere. Gateway API picks rules by specificity, so
  the longer /api prefix wins over /.

  Do not put a proxy_pass back into that nginx. nginx resolves a literal upstream
  hostname once, when it reads the config, so the web pod would then crash-loop
  any time the gateway Service was missing and would hold a dead ClusterIP if the
  Service were recreated. Routing at the Gateway has neither problem.

  api.tickets.lan still exists and still points at the gateway directly. That is
  what curl and the simulator use, and it means debugging the API never depends on
  the frontend being deployed.
  ns bank, Argo app `bank`
    bank        the adversarial fake    https://bank.tickets.lan  (PUT /config)

  WORKERS MUST STAY AT ONE REPLICA. It runs both inventory sweepers, the payment
  reconciler and the order resumer. Nothing there is unsafe concurrently, but N
  replicas do N times the work on the same rows and multiply traffic to the bank.
  strategy: Recreate is deliberate - a rolling update would briefly run two.

  NODE-2 SAT AT 80%+ CPU FOR DAYS AND IT WAS NOT A SCHEDULING PROBLEM.
  Fixed 2026-08-29. ClickHouse alone was 3106m of node-2's 3335m; everything else
  on that node came to about 230m. Moving pods around would have achieved nothing,
  and ClickHouse cannot move anyway - pinned off the control plane for etcd's
  sake, and bound to node-2 by a local-path PVC.

  IT WAS MERGING ITS OWN DIAGNOSTICS. 2.3 GiB of system log tables against 81 MiB
  of real telemetry, 28 to 1, with every merge in flight belonging to a system
  table. system.text_log alone was 1.54 GiB and the largest table in the database.

  THE FIX THAT WAS ALREADY IN PLACE HAD NEVER WORKED, and the reason is the
  lesson: config.d files load in ALPHABETICAL order and later files override
  earlier ones. Ours was disable-system-logs.xml; the chart ships system_log.xml;
  d sorts before s. For every table that file declares - text_log, error_log,
  latency_log, query_metric_log - the chart silently won. trace_log, which it does
  not declare, had stopped correctly months earlier, which is what made the
  failure so hard to see: the fix half worked.

  Renamed zz-disable-system-logs.xml so it sorts after anything the chart ships.

  REMOVE=1 ONLY STOPS WRITES. Existing parts stay on disk and get merged forever,
  so the tables were also truncated. That is a second, separate step and skipping
  it leaves most of the CPU where it was.

  metric_log went too. It had been kept on the grounds that it and query_log were
  "both small"; once everything louder was off, it was 206 MiB and the only thing
  still merging, seven at a time. query_log stays - it is how you find a slow
  query and the one system table that has ever been useful here.

  RESULT: node-2 83% -> 5% CPU. ClickHouse 3106m -> 140m, 5308Mi -> 3036Mi, and
  547 MiB on disk. Ingestion unaffected: spans and logs still arriving. The three
  nodes now sit at 2%, 2% and 5%.

  KAFKA WENT TO THREE BROKERS 2026-08-29, RF=3, min.insync.replicas=2. Four
  things bit, and none of them are obvious:

  1. THE SYNC DEADLOCKED ON ITS OWN WAVES. KafkaTopic resources had no sync-wave
     annotation, so they defaulted to wave 0 - applied first and then waited on
     for health, while the brokers they need sat in wave 4 behind them. Topics
     could not go healthy because RF=3 wants three brokers; the brokers were never
     applied because Argo was waiting for the topics. Topics are wave 5 now. The
     dependency was always that way round; one broker at RF=1 never exposed it.

  2. STRIMZI CANNOT CHANGE A TOPIC'S REPLICATION FACTOR. The operator says so:
     "Replication factor change not supported". Topics created at RF=1 must be
     deleted and recreated. There is no in-place path without Cruise Control.

  3. DELETING THE KafkaTopic CR DOES NOT DELETE THE KAFKA TOPIC here. The topic
     survived, and the recreated CR simply re-adopted it - still at RF=1, still
     reporting NotSupported. The topic itself has to be deleted with
     kafka-topics.sh --delete before the CR is recreated.

  4. __consumer_offsets WAS LEFT AT RF=1 because Strimzi does not manage internal
     topics. That is the one that matters most and is easiest to miss: losing a
     broker would lose every consumer position, and the gateway would replay or
     skip seat changes on restart. A cluster that looks replicated and is not.
     Fixed by hand with kafka-reassign-partitions across all three brokers.

  TOOL NAMES MOVED IN KAFKA 4.x. kafka.tools.GetOffsetShell is gone; the script is
  bin/kafka-get-offsets.sh. The old invocation fails SILENTLY and reports zero
  messages on topics that are working perfectly, which wastes an hour.

  AND kubectl exec NEEDS -i TO PIPE A FILE IN. Without it the file lands empty and
  the error you get back is about JSON parsing, a long way from the cause.

  THE HOT PARTITION, MEASURED 2026-08-29 during a 3,000-buyer on-sale:
    inventory.seat.held  781 msgs, ALL on partition 0
    orders.created       781 msgs, 261/263/257 across three partitions
  Same cluster, same burst, near-identical volume, opposite distribution - the
  partition key is the whole of the difference. inventory keys by event_id and an
  on-sale is one event; orders keys by order_id and orders are independent.

  BROKER FAILURE, TESTED FOR REAL 2026-08-29. Broker 1 deleted 25 seconds into a
  3,000-buyer on-sale. ISR went 1,2,0 -> 2,0 -> 0,1,2 within about twenty seconds,
  writes never stopped because two in-sync replicas still satisfy
  min.insync.replicas=2, and the burst finished with 292 bought, 2,695 lost races
  and ZERO errors. No oversell. Six seat-change messages were dropped while the
  broker was gone - the async publish trading a stale read model for never
  blocking a sale, working exactly as intended and visible for the first time.

  STAGING A SALE FROM THE BROWSER: POST /api/admin/showings on the gateway, or the
  button on https://app.tickets.lan/admin. It creates the event with an on_sale_at
  a minute or two out and does NOT open the seats - the workers on-sale loop does
  that when the moment arrives, the same path the 03:00 CronJob's showing takes.
  One mechanism starts a sale, and the operator page did not become a second one.

  THE DAILY MOVIE IS UNAFFECTED by any of it. The seeder CronJob still creates
  exactly one cinema showing at 03:00 and knows nothing about the arena.

  REDIS IS NO LONGER INERT EITHER, as of 2026-08-29. It holds the seat-map
  projection, which is exactly the job DESIGN.md gave it and nothing more.

    key      seatstatus:<event_id>, a hash of seat_id -> available|held|sold
    writer   gateway, from the same inventory.seat.* stream that feeds browsers
    reader   gateway, on GET /api/events/{id}/sections/{sid}

  MEASURED, WHICH IS WHY IT WAS WORTH DOING. Before: a seat-map read averaged
  586ms and hit 2918ms at p95, because it read 2,000 rows from
  inventory.event_seats - THE SAME TABLE EVERY SEAT CLAIM CONTENDS ON. Under a
  2,000-buyer on-sale afterwards: median 24ms, p95 44ms, max 59ms. Roughly 25x at
  the median and 66x at p95, and browse traffic no longer touches the contended
  writer's rows at all.

  THE DESIGN'S OWN TEST, RUN FOR REAL: redis-cli FLUSHALL during traffic. The next
  read went from 16ms to 31ms and returned the correct 2,000 seats; the one after
  was warm again. Flushing costs latency and nothing else, which is the property
  the whole arrangement depends on.

  IT IS A CACHE AND NEVER THE TRUTH. A partial hit counts as a miss, every miss
  falls through to inventory, client timeouts are short so a slow Redis degrades
  into a miss rather than a slow request, and an unreachable Redis is a start-up
  warning rather than a refusal to boot. It is deliberately NOT in readiness.

  NEVER PUT HOLD TTLs HERE. Redis key expiry is not a reliable event source, and a
  hold that exists in Redis but not Postgres is a seat sold twice. The sweeper in
  Postgres owns expiry and always will.

  KAFKA IS NO LONGER INERT, as of 2026-08-29. From phase 0.7 until then a broker
  ran doing nothing while DESIGN.md described an event-driven system that was
  entirely synchronous - which is exactly the "installed but unused" state this
  file exists to call out, and did not.

    topics   inventory.seat.held / .released / .sold, as KafkaTopic CRs in
             deploy/data/topics.yaml. One partition each: there is one broker,
             so more partitions buy no parallelism.
    producer inventory, async, after the database commit
    consumer every gateway replica, EACH WITH ITS OWN GROUP ID
    exposed  GET /api/events/{id}/stream, server-sent events

  THE SEAT MAP IS PUSHED NOW. Browsers used to poll a whole section every two
  seconds - a gateway request, a catalog call and an inventory call each time,
  almost always to be told nothing had changed. That cost grew with the SIZE OF
  THE VENUE rather than with how much was happening, which is backwards for an
  on-sale.

  THE SSE ROUTE NEEDS A LONG TIMEOUT. Envoy's default is 15 seconds and would
  sever the stream every fifteen seconds forever - the same default that killed
  the first on-sale burst. deploy/apps/tickets/web.yaml gives /api/events 3600s.

  IT IS ALL OPTIONAL. No KAFKA_BROKERS means the publisher is nil, every publish
  is a no-op, and the frontend falls back to polling. The broker is not something
  these services refuse to start without.

  AUTOSCALING, BOTH KINDS, ADDED 2026-08-29 after the first on-sale OOMKilled the
  gateway and the simulator.

    VPA 1.7.1  ns vpa, Argo app `vpa`, wave 1. Chart fairwinds/vpa 5.0.0.
               Owns MEMORY only. updateMode InPlaceOrRecreate.
    HPA        autoscaling/v2 objects in deploy/apps/tickets/hpa.yaml.
               Owns CPU only.

  THEY MUST OWN DIFFERENT RESOURCES OR THEY OSCILLATE. VPA changes a pod's
  request; HPA scales on utilisation as a percentage OF that request, so pointing
  both at the same resource makes each one's action change the other's input. The
  split is also right for this system: memory is driven by the size of a seat map,
  which is a property of the venue, and CPU by how many people are asking.

  VPA IS USABLE HERE ONLY BECAUSE OF THE KUBERNETES VERSION. 1.36 exposes the
  pods/resize subresource, so VPA 1.7 changes a running pod's resources IN PLACE
  rather than evicting it. Check before assuming on another cluster:
    kubectl get --raw /api/v1 | grep pods/resize

  NEITHER TOUCHES workers, AND THAT IS NOT AN OVERSIGHT. It runs the singletons -
  both sweepers, the reconciler, the resumer. N replicas do N times the work on
  the same rows and multiply traffic to the bank by N. Argo also still enforces
  its replica count, because ignoreDifferences for /spec/replicas is listed BY
  NAME for the six scalable services rather than for all Deployments.

  Without that ignoreDifferences, selfHeal reverts every scale-up within seconds,
  during exactly the burst that needed the capacity.

  VERIFIED 2026-08-29 under 4,000 buyers: gateway 1->6, inventory 1->6, catalog,
  orders and payments 1->4, workers stayed at 1, nothing restarted, no oversell.

  THE GATEWAY HAS NO DATABASE CREDENTIALS. That is deliberate and worth not
  undoing: it cannot reach Postgres at all, so the front door cannot read or write
  a table even by mistake. Only catalog, inventory, orders and payments hold the
  secret, one schema each.

  THE ONE MANUAL STEP: database credentials. CloudNativePG writes secret
  tickets-pg-app into ns data, and Kubernetes secrets do not cross namespaces, so
  it has to be copied into ns tickets:

    kubectl -n data get secret tickets-pg-app -o json \
      | jq '.metadata = {name:"tickets-pg-app", namespace:"tickets"}' \
      | kubectl apply -f -

  IT WILL GO STALE IF THE PASSWORD IS EVER ROTATED. A secret-replicating operator
  would fix this properly; until then, rotating the Postgres password means
  redoing this copy, and the symptom will be gateway and workers failing readiness
  with an auth error.

  SCHEMAS are applied at startup by whichever of gateway/workers/seeder starts
  first, under a Postgres advisory lock so concurrent starts cannot collide. See
  pkg/migrate, which is explicit that it is NOT a migration tool: no versions, no
  ordering, no ALTER. Replace it before a column ever changes under live data.

  THE HEALTH CHECK THAT MATTERS is not a probe. The simulator counts its own
  purchases and the backend counts confirmed orders and sold seats; those come
  from independent systems and must agree:
    curl -k https://sim.tickets.lan/stats
    kubectl -n data exec tickets-pg-1 -c postgres -- psql -U postgres -d tickets \
      -tAc "SELECT count(*) FROM orders.orders WHERE state='confirmed'"
  Divergence means an oversell or a lost order. Verified equal on 2026-08-27.
