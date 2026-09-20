OBSERVABILITY - SIGNOZ
======================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


OBSERVABILITY - SIGNOZ, INSTALLED 2026-08-24
--------------------------------------------

  ns signoz, managed by Argo CD. Chart signoz/signoz 0.138.0.
    signoz-0                            query service + UI, https://signoz.tickets.lan
    signoz-otel-collector               OTLP ingest, :4317 grpc / :4318 http
    chi-signoz-clickhouse-cluster-0-0-0 ClickHouse, the single store for all signals
    signoz-zookeeper-0                  ClickHouse coordination
    signoz-clickhouse-operator          manages the ClickHouseInstallation
  postgresql, redpanda and signoz-otel-gateway are DISABLED - nothing needs them and
  the gateway alone requests 2500m/2500Mi.

  ClickHouse and ZooKeeper carry node affinity keeping them OFF the control plane.
  Untainting it removed the only thing separating IO-heavy work from etcd, and
  local-path makes the placement permanent once the PVC binds.

  ENDPOINT FOR WORKLOADS - host:port, NO scheme, the OTLP HTTP exporter rejects one:
    signoz-otel-collector.signoz.svc.cluster.local:4318

  THE SECOND TRAP, and it looked exactly like the first. From 2026-08-24 to
  2026-08-27 SigNoz said "You are not sending traces yet" while logs and metrics
  arrived normally. Nothing was broken. pkg/obs built a TracerProvider, a
  MeterProvider and three OTLP exporters, installed them globally - and NO SERVICE
  EVER STARTED A SPAN OR CREATED AN INSTRUMENT. Only the hello canary did.
  Everything was wired and nothing was instrumented, and from the UI that is
  indistinguishable from a broken collector.

  The logs and metrics that WERE arriving came from the k8s-infra DaemonSet
  scraping /var/log/pods and the kubelet. That is infrastructure telemetry. It
  arrives whether or not a single service is instrumented, so "Logs ingestion is
  active" is not evidence that anything of yours is reporting.

  HOW TO TELL THE DIFFERENCE, without guessing, ask ClickHouse directly:
    kubectl -n signoz exec pod/chi-signoz-clickhouse-cluster-0-0-0 -c clickhouse -- \
      clickhouse-client -q "SELECT serviceName, name, count() \
        FROM signoz_traces.distributed_signoz_index_v3 \
        WHERE timestamp > now() - INTERVAL 15 MINUTE GROUP BY serviceName, name"
  App metrics live in signoz_metrics.distributed_samples_v4, metric_name LIKE
  'tickets%'. If serviceName only ever shows collector components, the services
  are not instrumented - do not go looking at the collector.

  VERIFIED 2026-08-27 in the cluster, a full purchase end to end:
    POST /api/holds -> inventory.Hold -> the conditional UPDATE that claims a seat
    POST /api/orders -> saga.created -> saga.awaiting_payment -> saga.paid ->
      saga.confirmed, with POST bank.bank.svc.cluster.local crossing into the bank
      service's own POST /authorize span - 114ms of a 119ms step was the fake bank
  Metrics flowing: tickets.holds, tickets.orders. tickets.hold.contention exists
  but has no points, which is correct - nothing has deadlocked yet, and an OTel
  counter reports nothing until its first Add.

  OBSERVABILITY AUDIT 2026-08-29, deliberate traffic across every route and error
  class. Two defects found that nothing else would have surfaced:

  1. THE SWEEP HAD GONE SILENT. Its span, its tickets.holds.swept counter and its
     hard-deadline warning were written into store.Sweeper when the loop ran
     inside the workers binary. The split moved the timer to workers and gave the
     work to a gRPC handler, and nothing called Sweeper again. SWEEPS KEPT
     HAPPENING; the telemetry about them stopped. Moved to the RPC that does the
     work now.

     That is the THIRD orphaned instrumentation in this repo - traces, then logs,
     now the sweep - and the shape is identical every time: the code still runs,
     nothing fails, and only missing telemetry says anything is wrong. When a loop
     or a handler MOVES, check what was measuring it moved too.

  2. A LOST RACE WAS COUNTED AS AN OUTAGE. Every layer this project controls is
     careful that losing a race is normal - logged at info, span not marked
     failed. otelgrpc marked it an error anyway, because it treats every non-OK
     gRPC code that way and offers no hook to change it. At 90% lost races during
     an on-sale the Services tab would have shown a 90% error rate on a system
     working perfectly, and an error rate that is always red is worthless on the
     day something is actually broken.

     The stats handler is wrapped on BOTH ENDS. Fixing only the server left half
     of them red, because one call makes a client span and a server span with the
     same name and each decides its own status.

  WHAT THE AUDIT CONFIRMED IS FINE: every request log carries a trace id; 409, 404
  and 400 all log at info; go runtime metrics on all nine services; pgxpool
  metrics on exactly the four that own a database and none on the gateway, which
  has no credentials for one.

  MEASURE AFTER THE ROLLOUT SETTLES. A check run immediately after a deploy caught
  the OLD pod still draining and reported the fix as not working.

  THE SAME TRAP AGAIN, ONE LAYER DOWN - LOGS. Fixed 2026-08-28. pkg/logger builds
  a careful correlation mechanism and the ONLY CALLER IN THE REPO WAS THE HELLO
  CANARY. The gateway served every request in the system and logged two lines,
  both at startup. SigNoz held, for the whole tickets namespace, 31 simulator
  lines and nginx access logs from web, and NOT ONE LOG CARRIED A TRACE ID.

  obs.Route now logs one line per request, inside otelhttp's handler so the
  context already has the span. After: gateway 143 of 147 lines correlated - the
  four without are startup, which is correct.

  CUSTOMER ID IN TRACES AND LOGS, 2026-08-29. X-Customer-Id is read at the gateway
  only, put into OTel baggage there, and picked up by every gRPC server it reaches.
  In SigNoz filter spans on customer.id and logs on customer_id.

    ui-<8 chars>              the browser, kept in localStorage, shown in the page
    sim-<profile>-<8 chars>   one per simulator session

  BAGGAGE IS A HEADER, so the value is sanitised to [A-Za-z0-9._-] and 64 bytes at
  the gateway. An unescaped comma or semicolon would not spoil one span, it would
  break the baggage header for every downstream hop.

  Verified: one purchase as ui-slash3b-demo tagged spans on gateway, catalog,
  inventory, orders and payments, and every log line for it. A 30-buyer burst
  produced 30 distinct customers in 30 distinct traces, each linked back to the
  on-sale burst span rather than nested under it.

  LOG COLLECTION, CORRECTED 2026-08-29. Volume was 22,540 lines/hour and most of
  it said nothing. Three things were wrong:

    Every service's logs were stored TWICE. pkg/logger tees to stdout and OTLP by
    design; the node agent also tails stdout. Measured 8,003 filelog records
    against 2,828 OTLP records in thirty minutes, with catalog, inventory, orders,
    payments, simulator and seeder matching their OTLP counts exactly. Only the
    OTLP copy carries trace_id - it was empty on all 8,003 filelog records.
    Fixed in deploy/platform/k8s-infra/values.yaml: those containers are on the
    logsCollection blacklist. web is NOT, because it is nginx and has no SDK.

    The OTLP half of the logger ignored the log level, so Debug lines shipped to
    the backend while stdout showed none of them. Fixed in pkg/logger.

    argocd-notifications-controller wrote 2,550 lines/hour with
    argocd-notifications-cm EMPTY - nothing configured to notify anything.
    Blacklisted. application-controller and repo-server stay: those are what you
    read when a sync fails.

  After: 10,080 lines/hour, and the only container in tickets still collected
  from stdout is web.

  WHAT THIS COSTS: a panic goes to stderr and the SDK never flushes it, so crash
  output no longer reaches SigNoz. Recover it with
    kubectl -n tickets logs <pod> --previous
  The restart itself is still visible in kubeletMetrics.

  APPLICATION METRICS, as of 2026-08-29. Infrastructure metrics were already
  complete - http.server/client.request.duration, rpc.server/client.call.duration,
  the full pgxpool set on the four services that own a database, go runtime on all
  nine. The business metrics were the gap:

    tickets.holds                by outcome: won, lost, error, exhausted
    tickets.hold.contention      retryable conflicts, the early warning
    tickets.holds.swept          by why: ttl or hard deadline
    tickets.orders               terminal state, plus failed_at naming the step
    tickets.orders.resumed       finished by the resumer, not their own request
    tickets.payments             succeeded, declined, UNKNOWN
    tickets.saga.duration        order lifetime, creation to terminal, by state
    tickets.saga.step.duration   by step and outcome
    tickets.payments.reconciled

  PROFILE-GUIDED OPTIMISATION, since 2026-08-30. Every service binary except
  workers is compiled against services/<name>/cmd/default.pgo, a CPU profile of
  this system under a real on-sale.

  REFRESH THEM with:
    ssh slash3b@192.168.1.116 'bash -s' < scripts/pgo.sh
  then copy /tmp/pgo/*.pgo over services/<name>/cmd/default.pgo and commit.

  CHECK THE SAMPLE TOTALS BEFORE TRUSTING A PROFILE. The first collection here
  ran against a sold-out event and produced 197-byte files — sixty seconds of
  sampling an idle process. That builds cleanly and optimises the idle path,
  which is the one thing PGO must not be pointed at. A good run looks like:

    catalog 20.46s of samples in 60s, gateway 14.50s, inventory 4.97s

  The script stages a 20,000-seat arena first for exactly this reason.

  workers ships no profile deliberately: it is a ticker and samples at ~10ms in a
  minute. An absent default.pgo is a clean no-op.

  PPROF IS ON LOOPBACK ONLY, 127.0.0.1:6060 inside each pod, which is why
  collection goes through port-forward:
    kubectl -n tickets port-forward deploy/gateway 6060:6060
  It is not on any Service and must not be. /debug/pprof/heap dumps live memory
  and /debug/pprof/profile pins a core; nothing here authenticates and the
  gateway's HTTP port is routed to a public hostname.

  VERIFY IT IS ACTUALLY APPLIED, because PGO that stops applying looks exactly
  like PGO that was never there:
    go version -m <binary> | grep pgo     ->  build -pgo=default.pgo

  NOTE ON THE DEPLOY TAG FOR THIS CHANGE: images were built from commit 994339b,
  which was then rewritten to d738b0f by an ill-advised amend after pushing. The
  trees are identical so the images are right, but that tag does not resolve to a
  commit on main. Do not amend a commit that CI has already started building.

  DASHBOARDS ARE IN GIT, infra/dashboards/. SigNoz keeps them in its own SQLite
  at /var/lib/signoz/signoz.db inside signoz-0, nothing backs that up, and there
  is no import API that works without a logged-in session — so the JSON lives in
  the repo and importing is a browser job:
    SigNoz UI -> Dashboards -> + New dashboard -> Import JSON.

  go-runtime-v6.json (try first) and go-runtime-v5.json (fallback) are the same
  eight panels in two schema versions, taken UNMODIFIED from
  github.com/SigNoz/dashboards. They work here because pkg/obs runs the current
  contrib/instrumentation/runtime, whose metric names are the current OTel
  semantic conventions and match what the dashboard expects.

  Verified 2026-08-30, all eight Go services reporting: goroutines from 11 (bank)
  to 60 (gateway), go.memory.used split stack/other by the go.memory.type label,
  around 10-18 MiB each.

  GOMEMLIMIT IS SET FROM THE CGROUP, since 2026-08-30 — pkg/memory/memlimit.go reads
  the container's own memory limit at startup and gives Go 90% of it. Before that
  the Go GC had no idea the container had a ceiling: GOGC=100 targets twice the
  live heap, a ratio with no absolute number in it, which is how the gateway and
  simulator were OOMKilled when limits sized for a cinema met an arena.

  VPA IS WHY IT READS THE CGROUP RATHER THAN AN ENV VAR. Measured on the day:

    service     manifest   ACTUAL pod limit   GOMEMLIMIT applied
    catalog     384Mi      400Mi              360 MiB
    gateway     384Mi      400Mi              360 MiB
    inventory   384Mi      400Mi              360 MiB
    orders      192Mi      300Mi              270 MiB
    payments    168Mi      300Mi              270 MiB
    workers     128Mi      266Mi              240 MiB
    simulator   256Mi      256Mi              230 MiB
    bank         64Mi       64Mi               58 MiB

  Six of eight had been moved by the autoscaler. A GOMEMLIMIT hardcoded beside the
  manifest limit would have been wrong for those six within a day, and wrong
  silently.

  KNOWN GAP: it is read ONCE at startup and this VPA runs InPlaceOrRecreate, so it
  can shrink a live container without restarting it. A lowered limit leaves the
  value stale and too high until the pod restarts. Re-reading on a timer closes it;
  not done yet.

  Verify with:  go.memory.limit in SigNoz, or the Memory Limit panel of the Go
  Runtime dashboard, which was empty until this existed.

  THE OLD NOTE, NOW HISTORY: Memory Limit (GOMEMLIMIT). That panel was empty until the
  change above, because the runtime only reports go.memory.limit when a limit
  actually exists. It has data now.

  MOST GO DASHBOARDS ON THE INTERNET WILL RENDER NOTHING, and it is not a fault
  in the instrumentation: they query runtime.go.mem.heap_alloc and
  process.runtime.go.*, the names the PREVIOUS generation of the runtime package
  emitted. Check a template's metric names before concluding anything is broken.

  KAFKA METRICS, ADDED 2026-08-30 to feed https://signoz.tickets.lan/messaging-queues/kafka
  which was empty because nothing produced them. Strimzi has no metricsConfig, so
  there is no JMX exporter, and no collector was scraping the brokers.

  The kafkametrics receiver lives in deploy/platform/k8s-infra/values.yaml, in the
  otelDeployment collector - one instance, not the DaemonSet, because these are
  cluster-wide facts and scraping them per node would multiply every series by the
  node count. It goes into the chart's `metrics/scraper` pipeline, which the chart
  declares empty for exactly this and which the presets do not touch.

  Flowing: kafka.brokers, kafka.topic.partitions, kafka.partition.current_offset,
  kafka.partition.oldest_offset, kafka.partition.replicas,
  kafka.partition.replicas_in_sync. About 80 series, and stable.

  THE `consumers` SCRAPER IS ON, AND THE REASON IT WAS BRIEFLY OFF IS FIXED AT THE
  SOURCE. pkg/events uses a unique group per process (broadcast, not work queue),
  so EVERY POD RESTART ABANDONS A GROUP. At Kafka's 7-day default offsets
  retention they piled up: 29 dead groups and 72 lag series after one day of
  rolling deploys, each scraped every minute, each reporting a lag that only grew
  because nobody was reading.

  deploy/data/kafka.yaml now sets offsets.retention.minutes: 60. A dead group is
  gone within the hour; a live one keeps committing and stays. MEASURED: 32 groups
  before, 1 after. Cardinality is bounded by RUNNING consumers, which is what it
  should always have been. Turning the scraper off was treating the symptom.

  Note it is a read-only broker config, so changing it ROLLS ALL THREE BROKERS.
  Confirm with:
    kubectl -n data exec tickets-kafka-dual-role-0 -c kafka -- \
      bin/kafka-configs.sh --bootstrap-server localhost:9092 \
      --entity-type brokers --entity-name 0 --describe --all | grep offsets.retention

  WHAT THE LAG MEANS HERE: every reader starts at kafka.LastOffset, so a healthy
  consumer sits at ~0 by construction rather than by merit. A RISING number is
  real; a zero proves less than it looks.

  kafka.consumer.fetch_latency_avg IS HAND-ROLLED, in pkg/events/metrics.go.
  Everywhere else it comes from a JVM consumer's JMX MBean, and every consumer
  here is Go on segmentio/kafka-go, which has no JMX and never will. It is the
  same measurement taken by the client: kafka-go's ReaderStats.WaitTime, the fetch
  round trip.

  NOT ReadTime, WHICH IS THE LOCAL DECODE OF A BATCH THAT ALREADY ARRIVED. That
  was the first choice and reported 0.01ms — ten microseconds for a network call
  is the number telling you it measures the wrong thing. An idle consumer shows
  ~250ms because a fetch with no data blocks until MaxWait; the JMX metric behaves
  the same way, so that is faithful rather than broken.

  MESSAGING SPANS, SAME DATE. Publishes and consumes now emit producer and
  consumer spans with the messaging semantic conventions, and W3C trace context
  rides in Kafka headers, so a trace crosses the broker instead of ending at the
  publish. Verified: one trace contains simulator -> gateway -> inventory ->
  `inventory.seat.held publish` -> `gateway:inventory.seat.held process`, with the
  saga and the bank in the same trace.

  USE THE PAGE'S OWN CONFIGURATION CHECK. The Kafka page has a
  "Missing Configuration" button that lists every attribute it wants, per
  producer and consumer, and marks the ones it cannot find. It named all four
  that were wrong here and is far faster than guessing from an empty dashboard.
  Its API is /api/v1/messaging-queues/kafka/onboarding/{producers,consumers,kafka}
  and it needs a logged-in session, so it is a browser job.

  THE FOUR IT FOUND, 2026-08-30:

    messaging.destination.partition.id  was set with attribute.Int, so it landed
      in the numeric attribute map. SigNoz reads the string map. The convention
      says string. The attribute was present and unfindable at once.
    messaging.destination.partition.id on PRODUCERS could not exist at all: the
      writer is async, so WriteMessages returns before the balancer has picked a
      partition. The span now travels in kafka.Message.WriterData and is ended by
      the writer's Completion callback, where kafka-go has filled in Partition
      and Offset.
    messaging.client_id  was never set. It is the hostname.
    service.instance.id  was missing from the RESOURCE, not the span, so it was
      missing everywhere in the system rather than only here.

  Verified after the rollout settled: 101 of 101 producer spans and 41 of 41
  consumer spans carry the partition. MEASURE AFTER THE OLD PODS ARE GONE — a
  first check sampled one span from a draining pod and reported the attributes
  missing when they were already fixed. That is the third time this trap has been
  worth writing down.

    VERIFY THEM BY FORCING THE FAILURE, NOT BY READING THE CODE. Two of these were
  wrong on the first deploy and looked fine in a green test suite. Set the bank to
  decline everything, buy a ticket, then check the labels:
    curl -sk --resolve app.tickets.lan:443:192.168.1.240 \
      -X PUT https://app.tickets.lan/admin/bank/config \
      -H 'Content-Type: application/json' -d '{"decline_rate":1.0}'
  PUT IT BACK TO 0.05 AFTERWARDS. It is a live setting on a running system and
  nothing resets it for you.

  SEEDER CRONJOB SUSPENDED 2026-08-29. `kubectl -n tickets get cronjob seeder`
  shows SUSPEND=true. Nothing creates showings on its own any more; they are
  staged from https://app.tickets.lan/admin. The CronJob is kept rather than
  deleted because suspend is one flag to reverse and scripts/wipe.sh --seed reads
  the seeder image out of its spec.

  WIPING THE CLUSTER. Use the make targets, from Q2 or Xtal - any machine with the repo
  and a working kubeconfig:

    make wipe-plan                     show what would happen, change nothing
    make wipe CONFIRM=WIPE             postgres, the redis projection, the bank
    make wipe-telemetry CONFIRM=WIPE   SigNoz traces, logs and metrics
    make wipe-all CONFIRM=WIPE         both

  What they run underneath, if you want it by hand:

    ssh slash3b@192.168.1.116 'bash -s -- --data --dry-run'   < scripts/wipe.sh
    ssh slash3b@192.168.1.116 'bash -s -- --data --yes'       < scripts/wipe.sh
    ssh slash3b@192.168.1.116 'bash -s -- --all --yes'        < scripts/wipe.sh
    ssh slash3b@192.168.1.116 'bash -s -- --telemetry --yes'  < scripts/wipe.sh

  THE FLAGS GO INSIDE THE QUOTES, AFTER `--`. Written the obvious way round,
  `ssh host 'bash -s' < scripts/wipe.sh --all`, the shell gives --all to ssh
  instead of the script and bash answers "invalid option" with no hint why. The
  header of the script documented the broken form until 2026-08-29.

  --yes IS NOT OPTIONAL HERE, AND THAT IS NOT LAZINESS. The script is piped in as
  stdin, so its "type WIPE" prompt reads that same stdin, gets EOF and aborts. It
  fails closed, but it can never be answered. CONFIRM=WIPE on the make target is
  the guard that actually works.

  --data empties Postgres (venues included), flushes the Redis seat-map
  projection, and restarts the bank, whose charges live in a map rather than a
  table so the restart IS the wipe. --telemetry empties SigNoz. Kafka is not
  purged and does not need to be: consumers start at kafka.LastOffset.

  Verified 2026-08-29: 4 venues / 6 events / 608 seats / 73 orders / 8 Redis keys
  -> all zero, then a cinema showing staged from the operator page opened 96 seats
  and sold one.

  RUN AGAIN 2026-09-10, --all, no backup taken and none wanted - this is simulated
  state, not records. 1 venue / 1 event / 20000 catalog seats / 20000 inventory
  seats / 1101 holds / 1067 orders / 1067 payments / 1 Redis key -> all zero, plus
  50 ClickHouse tables truncated, which threw away that evening's traces and logs
  along with the run they described. The residual counts printed straight after a
  --telemetry wipe are NOT a failure: the collector is already writing again by the
  time the summary query runs, so a few hundred rows is what success looks like.
  The simulator was restored to 1 replica by the trap and now idles - nothing is on
  sale until a showing is staged at https://app.tickets.lan/admin.

  The seeder was worse. It passed nil as the log provider and never called
  obs.Setup at all, and its manifest had no OTLP endpoint, so the one job that
  decides whether there is anything to sell tomorrow reported nothing at all. A
  CronJob is exactly the workload you cannot watch by eye; its pod is gone by the
  time you wonder whether it ran. It also flushes explicitly on exit now, because
  a job that lives two seconds loses nearly everything otherwise.

  VERIFIED 2026-08-28 by taking a bank log line's trace_id and looking it up in
  the traces table: it resolves to a trace rooted at simulator "session group",
  running through the gateway into the bank. Logs and traces are joined by id
  across three services, not by squinting at timestamps.

  THE SERVICES TAB IS BUILT FROM SPANS, which is why it was missing half the
  system until 2026-08-28. workers and seeder run background loops and never
  started a span, and a process that emits no spans is not a service as far as APM
  is concerned - it does not matter how much it is doing. web is nginx and will
  never appear; it has no OTel in it at all, by choice.

  Fixed by giving the three singletons a span per pass - sweep, reconcile, resume
  - and the seeder one for its run. BACKGROUND WORK IS THE LEAST OBSERVABLE THING
  IN THE SYSTEM: nobody waits on it, so nothing complains when it stops. Each span
  carries what it actually did, because a sweeper reclaiming zero holds forever is
  either idle or broken and those look identical from outside.

  PER-SERVICE CPU AND MEMORY, added the same day. The k8s-infra DaemonSet already
  reports k8s.pod.cpu.* and k8s.pod.memory.* - that is the CONTAINER view, keyed by
  pod name, so it does not survive a rollout and cannot separate a memory climb
  caused by a leak from one caused by more work. The Go runtime instrumentation now
  reports the PROCESS view keyed by service.name:
    go.goroutine.count  go.memory.used  go.memory.allocated  go.memory.gc.goal
    go.memory.allocations  go.config.gogc  go.processor.limit
  Baseline 2026-08-28: gateway 19 goroutines / 14.5MiB, workers 18 / 12.6MiB,
  simulator 18 / 11.8MiB, bank 15 / 11.2MiB, hello 12 / 10.2MiB.

  POOL METRICS answer the open pgbouncer question in DESIGN.md with data rather
  than opinion: pgxpool.acquire_duration, pgxpool.empty_acquire_wait_time and
  pgxpool.acquired_connections are the three that matter. At current load gateway
  holds 1 connection and workers 4, so the answer today is plainly "no pgbouncer".

  NOTE THE LABEL KEY IS DOTTED. Filtering these in ClickHouse is
  JSONExtractString(labels,'service.name'), not service_name. Half an hour went
  into that.

  RESOURCE ATTRIBUTES come from OTEL_RESOURCE_ATTRIBUTES in each Deployment, read
  by resource.WithFromEnv. Spans now carry deployment.environment.name=homelab and
  the emitting k8s.pod.name. The manifests build that string with downward-API
  $(VAR) expansion, which ONLY works when the referenced variables are declared
  EARLIER in the same env list - a later definition expands to the literal text.

  THE SETUP TRAP, and it cost real time. SigNoz will not configure its collector
  until an ORGANISATION EXISTS, and an org is only created by registering the first
  user in the UI. Until then:
    - the server logs "cannot create agent without orgId" every 30 seconds
    - the collector logs "Server returned an error response" from its OpAMP client
    - the collector has NO receivers, so nothing listens on 4317/4318
    - every client fails with "connection refused" to a Service that plainly exists
  Nothing about those symptoms points at "you have not signed up yet". Check first:
    curl -k https://signoz.tickets.lan/api/v1/version     -> {"setupCompleted":false}
  The collector's OTLP pipeline is configuration delivered over OpAMP, not static
  config, which is why an empty database disables data ingestion entirely.

  LOG COLLECTION FEEDBACK LOOP - fixed 2026-08-27. ClickHouse logs every query it
  runs. The k8s-infra agent collected those logs and INSERTed them into ClickHouse,
  which logged the insert, which was collected again. Measured at ~20,000 records per
  minute on a COMPLETELY IDLE cluster - 41,912 of 42,000 in a two-minute sample came
  from the clickhouse container - with ClickHouse pinned near its 4Gi limit and burning
  three CPU cores ingesting its own chatter.
  The chart's presets.logsCollection.blacklist.signozLogs is already true and does NOT
  catch it: the ClickHouse pods are created by the clickhouse-operator from a
  ClickHouseInstallation CR, not by the Helm release, so they never carry the labels
  that filter matches on. Excluding the container by name works:
    presets.logsCollection.blacklist.containers: [clickhouse]
  Rate afterwards: ~200-400/minute. Any log store that ingests its own logs will do
  this; check for it whenever one is installed.

  CLICKHOUSE ATE A WHOLE NODE WITH ITS OWN DIAGNOSTICS - fixed 2026-08-27.
  Symptom: node-2 at 97% CPU with ZERO queries running, disk 3% -> 48%.
  Cause: ClickHouse records very detailed telemetry ABOUT ITSELF into system.*_log.
  After three days:
    system.zookeeper_log   16.98 GiB   214,005,617 rows   (every ZooKeeper op)
    system.trace_log        9.83 GiB   470,600,466 rows   (sampling profiler)
    system.text_log         3.06 GiB   101,683,787 rows
    ~31 GiB of self-diagnostics against 5.25 GiB of real telemetry.
  All of it was being continuously merged, which is what consumed the CPU. The log
  feedback loop above inflated zookeeper_log especially, since every insert is
  ZooKeeper traffic.
  Fix: config.d/disable-system-logs.xml in the chart values, remove="1" on each.
  Result: node-2 97% -> 31% CPU, disk 54G -> 8.9G.

  TWO TRAPS WHILE FIXING IT, both worth remembering:

  1. DO NOT write <query_log><ttl>...</ttl></query_log> as a sibling element. The
     chart already declares an <engine> for those system tables, and ClickHouse then
     requires TTL INSIDE the engine definition. A sibling <ttl> is FATAL:
       Code: 36. If 'engine' is specified for system table, TTL parameters should be
       specified directly inside 'engine'
     The server exits 36 and crash-loops. This looked exactly like an OOM crash-loop
     because MEMORY_LIMIT_EXCEEDED errors were also in the log - the exit code is what
     distinguishes them. CHECK THE EXIT CODE BEFORE BELIEVING THE LOUDEST ERROR.

  2. A crash-looping ClickHouse cannot be fixed through Argo CD. The chart renders
     into a ClickHouseInstallation CR, the clickhouse-operator turns that into a
     StatefulSet, and the operator will not finish reconciling while the host is
     unhealthy - so a values change never reaches the pod. Deadlock. Break it by
     editing the live object directly:
       kubectl -n signoz patch sts chi-signoz-clickhouse-cluster-0-0 ...      (limits)
       kubectl -n signoz get cm chi-signoz-clickhouse-common-configd -o json  (config)
     then delete the pod. Push the same change through git afterwards so the two agree.

  MEMORY: raised to 6Gi. ClickHouse sets max_server_memory_usage to ~90% of the
  cgroup limit and a MERGE must fit inside it; at 4Gi it OOMed mid-merge on a large
  backlog and retried forever.

  ARGO CD PERMANENT OutOfSync, fixed with ignoreDifferences in
  deploy/argocd/apps/signoz.yaml. Two server-side defaults the chart never renders:
    volumeClaimTemplates[].apiVersion and .kind   on both StatefulSets
    resourceFieldRef.divisor                      on the operator Deployment
  Argo applied, re-detected the difference, burned five self-heal retries and parked
  at OutOfSync - which destroys OutOfSync as a signal, so real drift becomes
  invisible.
  HOW TO DIAGNOSE THIS CLASS PROPERLY: read Argo's own comparison,
    GET /api/v1/applications/<app>/managed-resources
  and diff normalizedLiveState against predictedLiveState. Diffing `helm template`
  output against the live object points at the WRONG fields, because it skips the
  normalizers Argo applies first. That mistake cost an hour here.
