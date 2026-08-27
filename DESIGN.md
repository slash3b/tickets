TICKETS - TICKET SELLING SYSTEM - DESIGN
Draft 1, 2026-08-24. Nothing here is built yet.

This is a deliberately fake system built to be operated for real. Modelled on Juraj
Majerik's Rides project: the value is not the product, it is that a simulated population
of users keeps the thing under continuous realistic load on actual hardware, so that
distributed-systems failures happen to you instead of being read about.

Target: the homelab Kubernetes cluster described in infra/CLUSTER.md.


THE ONE HARD PROBLEM
--------------------

  N buyers, M seats, N >> M.
  Never sell the same seat twice. Never lose a seat to a checkout that died.

Everything else in this document is scaffolding around that sentence. When a design
decision is unclear, the tiebreaker is whichever option makes seat contention more
visible and more honest.

Assigned seating was chosen over general admission on purpose. GA collapses the whole
problem into one hot integer per event. Assigned seating gives per-seat contention,
all-or-nothing multi-seat purchases ("three together"), adjacency search, and a seat map
that is genuinely interesting to watch fill up in real time.


TWO CONTENTION REGIMES - MOVIES NOW, CONCERTS LATER
---------------------------------------------------

The system sells movie showtimes first and concerts later. This is not a content
decision, it is the load plan. The two stress opposite axes and the design has to survive
both without being rewritten in between.

  MOVIES - breadth.    A cinema holds 100-300 seats. There are hundreds of showtimes
                       live at once, each with mild contention, spread across the day.
                       Load is broad, steady, never especially hot on any one row.
                       Stresses: catalog read volume, cache hit rate, connection counts,
                       many parallel low-contention writers, seat-map fan-out breadth.

  CONCERTS - depth.    An arena holds 20,000 seats. One event. Everybody arrives inside
                       the same 60 seconds because on_sale_at said so. A Lady Gaga
                       on-sale is 100,000 people fighting over 20,000 seats, most of them
                       wanting 2-6 adjacent ones.
                       Stresses: single-event write contention, deadlock rate, adjacency
                       search on a large seat chart, queueing and load shedding, hold
                       churn from everyone losing races, WebSocket fan-out to a huge
                       audience all watching one seat map.

Movies alone would let a lazy design pass, because nothing is ever hot enough to hurt.
Concerts alone would hide the read-scaling work. Building movies first and concerts
second makes the concert on-sale a genuine exam of work already done, which is exactly
the shape a learning project wants.

WHAT THIS FORBIDS, starting now, while only movies exist:

  - No assumption anywhere that a seat chart is small. No unpaginated seat lists, no
    O(seats^2) adjacency search, no "just load the whole event into memory".
  - No assumption that an event's seats fit in one cache entry or one WebSocket frame.
    Seat maps are fetched and pushed by section, never whole.
  - on_sale_at is a first-class field from day one, even though for movies it is always
    in the past. The concert case is entirely about that one timestamp.
  - Venue carries a kind (cinema | arena) and a section model rich enough for floor,
    tiers and boxes. A cinema simply has one section.

Nothing else in this document changes between the two. Venue / Seat / Event / EventSeat
is deliberately venue-agnostic: a 20,000-seat arena is the same model with a bigger
number, and if that ever stops being true then the model was wrong.


WHAT IS BEING LEARNED
---------------------

In rough order of how much of the design they drive:

  concurrency control      not overselling under real parallel load
  sagas and compensation   multi-step money flows that fail halfway
  idempotency              retries and duplicate webhooks that must not double-charge
  backpressure             on-sale thundering herd, shedding load without falling over
  eventual consistency     read models that lag the write model, and users noticing
  operability              tracing a slow buy across seven services at 3am
  gitops delivery          monorepo to image to Argo CD, without hand-applied YAML

NON-GOALS. Real money. Real identity (JWTs are minted by a stub, unverified). Real
fraud, tax, or accounting. GDPR. Multi-region. Mobile apps. Anything that adds work
without adding one of the lessons above.


DOMAIN MODEL
------------

  Venue        a physical building. Has a kind (cinema | arena), one or more Sections,
               and a fixed seating chart. Scale differs by two orders of magnitude
               between kinds; nothing in the model may assume the small one.
  Section      floor, tier, box, or for a cinema just "the room". The unit that seat
               maps are fetched and pushed by, so that a 20,000-seat chart never has to
               move as a single blob.
  Seat         a physical seat in a venue: section, row, number, and x/y for the map.
               Belongs to the venue, not to the event. Never created or destroyed by
               ticket sales.
  Event        a showing: venue + title + starts_at + on_sale_at. The unit people buy
               into.
  EventSeat    the sellable instance of a Seat for one Event, carrying price and status.
               This is the contended row. M of these per event.
  Hold         a temporary claim on one or more EventSeats by one user, with an expiry.
  Order        the purchase built on top of a Hold.
  Payment      one attempt to move money for an Order, via the fake bank.
  User         a simulated buyer. No password, no email that goes anywhere.

The split between Seat and EventSeat matters. Seat is catalog data, written once, read
constantly, trivially cacheable. EventSeat is transactional data, written under
contention, cached only with great care. Different services own them for that reason.


SEAT STATE MACHINE
------------------

EventSeat.status, and nothing else, decides whether a seat can be sold.

    available ──hold──> held ──commit──> sold
        ^                 │
        └────release──────┘

  available   nobody has a claim. The only state a hold can be taken from.
  held        claimed by exactly one active Hold. Not purchasable by anyone else.
  sold        terminal for the life of the event. Never goes back.

There is no "reserved" or "pending" seat state. Anything more granular than these three
belongs on the Hold, not on the seat. Keeping the seat machine this small is what makes
the oversell invariant checkable by eye.

INVARIANT: a seat is held if and only if exactly one Hold in state active or converting
references it. A seat is sold if and only if exactly one Order in state confirmed
references it. Both are assertable in SQL, and a background checker should assert them
continuously and loudly. If this system ever oversells, that checker is how you find out.


HOLD STATE MACHINE
------------------

The Hold, not the seat, carries the lifecycle nuance.

    active ──checkout──> converting ──commit──> consumed
       │                     │
       │                     └──abandon/hard-timeout──> released
       └──expire (ttl)/cancel──> released

  active       user is browsing checkout. Expires on a short TTL, default 5 minutes.
               This is the state the sweeper is allowed to expire.
  converting   payment is in flight. The short TTL NO LONGER APPLIES. Instead a long
               hard deadline applies, default 15 minutes.
  consumed     order confirmed, seats moved to sold. Terminal.
  released     seats went back to available. Terminal.

Why converting exists, and it is the single most important decision in this document:

  If the short TTL kept running while the bank was thinking, a slow bank would expire the
  hold, the sweeper would return the seats to the pool, someone else would buy them, and
  then the payment would succeed. You would have taken money for a seat you cannot
  deliver. That is the worst outcome the system can produce and it is entirely
  self-inflicted.

  Entering converting stops the clock. The 15-minute hard deadline still exists because a
  hold cannot be immortal - if orders dies permanently mid-checkout, those seats must
  eventually return. Crossing the hard deadline forces a release AND flags the order for
  reconciliation, because a payment may have succeeded against seats that no longer
  belong to it. That path ends in a refund, and it should be rare enough to alert on.


ORDER STATE MACHINE
-------------------

    created ──> awaiting_payment ──> paid ──> confirmed
                     │                 │
                     │                 └──> reconciling ──> refunded
                     └──> failed

  created            row exists, hold is still active.
  awaiting_payment   hold moved to converting, payment intent created at the bank.
  paid               bank authorised and captured. Money has moved. Seats NOT yet sold.
  confirmed          seats are sold, hold consumed. Terminal, happy.
  failed             bank declined, or user abandoned. Hold released. Terminal.
  reconciling        we hold money but could not deliver seats. Needs a refund.
  refunded           money returned. Terminal, unhappy but correct.

The gap between paid and confirmed is where all the interesting bugs live. It is crossed
by FORWARD RECOVERY, not rollback: once money has moved, the orchestrator retries the
commit rather than trying to un-charge, because the seats are still held in converting
and nobody else can take them. Only the hard deadline sends an order to reconciling.


SERVICES
--------

Seven, plus the SPA. The seventh (gateway) exists only because the frontend is a React
SPA; a server-rendered frontend would not need it. Adding an eighth requires a reason
written down here first.

  hello is that reason, and it is NOT a domain service. It is the platform canary: a
  service with no business logic that serves /livez and /readyz and emits exactly one
  metric, one log line and one trace span. It stays permanently.

  WHY IT EARNS ITS PLACE: it is the control variable. When inventory stops producing
  traces, the question is whether the code broke or the platform broke, and without a
  known-good service that question costs an hour of debugging the wrong thing. With
  hello it costs five seconds. It is also the smoke test after every platform change -
  upgrade Argo CD, ingress-nginx or SigNoz, and the question "does anything still
  build, deploy and trace" has a cheap answer. And it is the reference implementation:
  every service here is hello with logic added, so a new service that misbehaves gets
  diffed against it.

  It costs 32Mi and 10m of CPU. If it ever stops being maintained it becomes exactly
  the kind of leftover this project keeps deleting, so it is maintained deliberately.

Language is Go for every service, reusing cineplex/pkg (logger, otel, health, http, env,
option) as the shared foundation. That package set is already good and is the main thing
worth keeping from cineplex. Every service exposes /livez and /readyz and emits OTLP to
the node's Alloy agent; none of them know what is behind it.

DEPENDENCY DIRECTION. Arrows point from caller to callee. There are no cycles, and any
change that would introduce one is wrong.

    browser
       |
       v
    gateway ------+-----------+
       |          |           |
       v          v           v
    catalog   inventory <-- orders --> payments --> bank
       |          |           |            |          |
       +----------+-----------+------------+----------+
                        |
                     Kafka  (everything produces; gateway and catalog consume)

  simulator drives gateway exactly as a browser would - it is a client, not a peer, and
  it gets no privileged path into the system. If the simulator needs an internal API to
  work, the API is wrong.


  gateway
  .......
    ROLE       Browser-facing BFF. The only service reachable from outside the cluster.
    OWNS       No data. Fully stateless, horizontally scalable, no PVC.
    SERVES     the entire public API in DESIGN.md > PUBLIC API
    CALLS      catalog (gRPC), inventory (gRPC), orders (gRPC)
    CONSUMES   inventory.seat.held / released / sold -> fans out to WebSocket clients
    PRODUCES   nothing
    STATE      in-memory WebSocket registry, keyed by (event_id, section_id). Lost on
               restart, and that is fine - clients reconnect and re-fetch the section.
    SCALING    the connection-bound service. During a concert on-sale this is what runs
               out of file descriptors first, not CPU. Every replica consumes the same
               Kafka topics with its own consumer group, because every replica needs
               every seat event for the clients it holds.
    TEACHES    fan-out, connection handling, load shedding at the edge, and why a BFF's
               consumer group semantics differ from a worker's.

  catalog
  .......
    ROLE       Venues, sections, seats, events, prices. Read-heavy, almost never written.
    OWNS       catalog.venues, catalog.sections, catalog.seats, catalog.events
    SERVES     gRPC: ListEvents, GetEvent, ListSections, GetSectionSeats
    CALLS      nothing
    CONSUMES   inventory.seat.status (compacted) -> maintains the seat-map read model
    PRODUCES   nothing
    STATE      Postgres for truth, Redis for the seat-map projection and event lists
    SCALING    trivially horizontal, stateless replicas over a shared cache. This is the
               easy service, on purpose - it is where the caching lessons live without
               correctness at stake.
    TEACHES    read scaling, cache invalidation, and read models that are ALLOWED to lag.

  inventory
  .........
    ROLE       The contended core. The ONLY writer of seat status anywhere in the system.
    OWNS       inventory.event_seats, inventory.holds, inventory.hold_seats
    SERVES     gRPC: Hold, Release, Convert, Commit, GetHold
    CALLS      nothing. It is a leaf, deliberately - the most contended service has the
               fewest dependencies so its latency is its own.
    CONSUMES   nothing
    PRODUCES   inventory.seat.held, .released, .sold, .status
    STATE      Postgres only. NEVER serves a read from Redis - the cache is downstream in
               catalog, where staleness is harmless.
    BACKGROUND expiry sweeper (active holds past expires_at -> released)
               hard-deadline sweeper (converting holds past 15m -> released + flag order)
               invariant checker (continuous, asserts the two invariants in SEAT STATE
               MACHINE and fires an alert on any violation)
    SCALING    horizontal replicas are fine - all coordination is in Postgres, none in
               the process. Postgres row locks are the real limit, and that limit is
               per-seat, so throughput scales with seat spread rather than replica count.
               The sweepers must be singletons; run them as a separate Deployment with
               one replica, or lease them, but do not run one per API replica.
    TEACHES    concurrency control, isolation levels, deadlock and its retry, and not
               overselling. This is the heart of the project.

  orders
  ......
    ROLE       Saga orchestrator. Owns the multi-step purchase and its failure paths.
    OWNS       orders.orders, orders.order_seats, orders.saga_log
    SERVES     gRPC: CreateOrder, GetOrder
    CALLS      inventory (Convert, Commit, Release), payments (CreateIntent, GetIntent)
    CONSUMES   payments.succeeded, payments.failed
    PRODUCES   orders.created, orders.confirmed, orders.failed
    STATE      Postgres. Every saga step is persisted BEFORE it is attempted, so a crash
               resumes rather than restarts. saga_log is the crash-recovery record and
               the audit trail both.
    BACKGROUND resumer - finds orders stuck mid-saga after a crash and drives them
               forward. Singleton, same as inventory's sweepers.
    SCALING    horizontal for the API, singleton for the resumer.
    TEACHES    sagas, forward recovery, crash-consistent workflows, and the paid-to-
               confirmed gap where all the interesting bugs live.

  payments
  ........
    ROLE       The only service permitted to talk to the bank. All money moves through it.
    OWNS       payments.payments, payments.idempotency_keys, payments.webhook_events
    SERVES     gRPC: CreateIntent, GetIntent
               HTTP: POST /webhooks/bank  (the bank calls in here)
    CALLS      bank (HTTP)
    CONSUMES   nothing
    PRODUCES   payments.succeeded, payments.failed
    STATE      Postgres. idempotency_keys maps key -> result so a retry returns the
               original answer instead of charging twice. webhook_events dedups by
               (payment_id, bank_event_id) because the bank WILL deliver duplicates.
    BACKGROUND reconciler - for every payment in an unknown state past a timeout, asks
               the bank what actually happened. This is what catches the
               timed-out-but-succeeded case, and it is the reason that case is survivable
               rather than fatal.
    SCALING    horizontal. All dedup state is in Postgres, keyed, unique-constrained.
    TEACHES    idempotency, exactly-once-ish over an unreliable peer, reconciliation.

  bank
  ....
    ROLE       The antagonist. Fake, adversarial, configurable. See THE FAKE BANK.
    OWNS       bank.accounts, bank.charges  (entirely fictional money)
    SERVES     HTTP: POST /authorize, /capture, /refund, and PUT /config for chaos knobs
    CALLS      payments, via webhook callback
    STATE      Postgres, or in-memory - it does not matter, because nothing downstream
               is allowed to trust it anyway.
    SCALING    single replica. Making the antagonist highly available would be absurd.
    TEACHES    nothing by itself. It exists so the others have something to survive.

  simulator
  .........
    ROLE       The load. Virtual buyers as state machines. A deployed service, not a
               test script, and not a peer - it drives the public API like a browser.
    OWNS       no shared data. Local state per virtual user only.
    SERVES     HTTP: POST /config (profile mix, arrival rate, target event, mode)
    CALLS      gateway, over the public HTTP and WebSocket API, exactly as a browser does
    PRODUCES   its own metrics only - attempted, succeeded, 409'd, latency histograms
    SCALING    THIS IS THE LOAD DIAL. Raising load is raising replica count.
    TEACHES    everything else. It is the reason any of the other lessons ever occur.
    KEY RULE   the simulator's own count of successful purchases is compared against the
               backend's count of confirmed orders. Divergence between those two numbers
               is the alarm that matters most in the entire system, because it is how an
               oversell or a lost order announces itself.

  web
  ...
    ROLE       React SPA. Static build.
    SERVES     the seat map and the ops board
    CALLS      gateway only
    DEPLOY     built in CI, served by an nginx container behind the same ingress host as
               gateway so there is no CORS and no second origin.

SCHEMA OWNERSHIP IS ABSOLUTE. Every service owns its schema and no other service reads
it - not with a join, not with a foreign key, not "just this once for a report". They
share one Postgres instance purely to save memory on a small cluster, and that restraint
is the only thing that keeps splitting them into separate databases later a config change
rather than a rewrite.


HOW A SEAT IS ACTUALLY CLAIMED
------------------------------

Conditional update, rows-affected check, no explicit transaction juggling:

    UPDATE event_seats
       SET status = 'held', hold_id = $1, updated_at = now()
     WHERE event_id = $2
       AND seat_id = ANY($3)
       AND status = 'available';

Then compare rows affected against len($3). If they differ, roll back the whole thing and
return 409 - multi-seat purchases are all-or-nothing, because handing someone two of the
three seats they asked for is worse than handing them none.

Two properties make this the right primitive. It is a single statement, so there is no
window between check and write for anyone to slip through. And it needs no advisory locks
or SERIALIZABLE, so different seats in the same event still proceed fully in parallel -
only genuine contention on the same seat serializes.

GOTCHA, and it will happen: two concurrent multi-seat updates over overlapping seat sets
can deadlock, because Postgres locks rows in scan order and the two scans may disagree.
Symptom is SQLSTATE 40P01. Two mitigations, use both: sort seat ids before sending them,
and retry 40P01 with jitter at the repository layer. Do not "fix" this by taking a table
lock; that throws away the parallelism the design exists to get.

BUILT AND MEASURED 2026-08-27 in services/inventory/store. 1000 goroutines against 10
seats: exactly 10 wins, 990 clean losses, zero errors, 7428 attempts/sec under -race.
Note what that last number does NOT tell you - whether deadlocks occurred and were
retried, or whether sorting prevented them entirely. Both look like errored=0. If that
distinction ever matters, count the retries.

READ MODELS ARE NOT AUTHORITATIVE. The seat map the browser renders is a cached,
event-driven projection and it is allowed to be stale. A user clicking a seat that the
map showed as free and getting a 409 is CORRECT BEHAVIOUR, not a bug, and the SPA must
handle it gracefully. Postgres is the only truth for seat status.


DATA STORES
-----------

  Postgres      Single instance, schema per service, no cross-schema joins and no
                cross-schema foreign keys. That restraint is what makes splitting into
                separate databases later a config change instead of a rewrite. Source of
                truth for absolutely everything that matters.

  Redis         Catalog reads and seat-map projections only. NEVER the truth for
                inventory, and specifically NOT the holder of hold TTLs - Redis key
                expiry is not a reliable event source, and a hold that exists in Redis
                but not Postgres is an oversell waiting to happen.
                TEST OF THE DESIGN: flushing Redis in production must cost latency and
                nothing else. If it can cost correctness, the design is wrong.

  Kafka         Inter-service events and the feed behind the live seat map. See its own
                section - on this hardware it is the component that needs the most care.

Hold expiry is driven by a sweeper polling Postgres for active holds past expires_at, not
by any TTL mechanism in Redis or Kafka. Boring, correct, and observable.


KAFKA
-----

Chosen over NATS deliberately, and the reason is not throughput - it is that Kafka's
model contains a lesson this system cannot avoid learning.

  Seat events for one event MUST stay ordered. held-then-sold arriving as sold-then-held
  corrupts every read model downstream. Ordering in Kafka means keying by event_id so a
  given event's events always land on one partition.

  But a partition is also the unit of parallelism. So keying by event_id means ONE
  PARTITION TAKES THE ENTIRE LADY GAGA ON-SALE while the others sit idle. You cannot fix
  this by adding partitions, because the key decides placement, not the count.

  That is the hot partition problem, it is unavoidable once you demand per-key ordering,
  and it is exactly the shape of this system's worst case. Wrestling with it - key by
  (event_id, section) and give up cross-section ordering? isolate hot events onto their
  own topic? relax ordering and make consumers commutative? - is a large part of why
  Kafka is here at all. NATS would have hidden the problem behind subject fan-out.

Secondary lessons that come free: consumer group rebalancing, offset management and what
"at-least-once" really costs, log compaction for the seat-status projection, and replay
of a topic from offset zero to rebuild a read model from scratch.

HOW IT IS RUN HERE

  Kafka 4.x in KRaft mode. NO ZOOKEEPER - it was removed in Kafka 4.0. Most of the
  received wisdom about Kafka being too heavy for small clusters predates this and is
  now wrong; dropping ZK removes an entire stateful ensemble.

  Strimzi operator. Kafka becomes CRDs - Kafka, KafkaTopic, KafkaUser - which means
  topics live in deploy/ under git like everything else, and Argo CD manages them.
  Costs one operator pod. Worth it, and operating a stateful system through an operator
  is itself part of the point.

  ONE broker, replication factor 1, to begin with. This buys nothing in reliability and
  is honest about that: a broker loss loses the log. Move to three brokers with RF=3 and
  min.insync.replicas=2 at the milestone where ISR shrink, leader election and unclean
  leader election become the thing being learned - not before, because until then it is
  three times the disk for no lesson.

  local-path storage pins the broker to one node permanently, same as Postgres. Node-2
  has noticeably less RAM than the other two (5.9Gi vs 8Gi); Kafka should not land there.

RETENTION IS THE WHOLE GAME ON THIS HARDWARE

  Kafka's defaults assume a real cluster and will eat a 15G node alive. The simulator
  produces continuously, forever, by design. Defaults of 7-day retention and 1GB
  segments are simply not survivable here. Set, explicitly, on every topic:

    retention.ms       1 to 6 hours, not days. Nothing here needs yesterday's events.
    segment.bytes      64-128MB, not 1GB. Retention only deletes CLOSED segments, so a
                       1GB segment on a low-volume topic is never closed and therefore
                       never deleted, no matter what retention.ms says. This is the
                       single most common way a small Kafka fills a disk.
    retention.bytes    a hard per-partition cap. Belt and braces - it is the only
                       setting that bounds worst case when a burst outruns time-based
                       retention.

  The seat-status projection topic is the exception: log compaction rather than deletion,
  keyed by seat, so it keeps exactly the current status of every seat and can rebuild the
  read model from scratch without unbounded growth.

  Broker heap gets set explicitly, around 1G. Do not let it default and do not let it
  size itself from a node whose memory it is sharing with everything else.


EVENTS
------

At-least-once. Every consumer must be idempotent; assume every message arrives twice.
Every topic below names its partition key, because on Kafka the key is a design decision
and not a detail - it fixes both ordering and where load lands.

  topic                  key           payload
  ---------------------  ------------  --------------------------------------------
  inventory.seat.held    event_id      {event_id, section_id, seat_ids, hold_id,
                                        expires_at}
  inventory.seat.released event_id     {event_id, section_id, seat_ids, hold_id,
                                        reason}
  inventory.seat.sold    event_id      {event_id, section_id, seat_ids, order_id}
  orders.created         order_id      {order_id, hold_id, user_id, amount}
  orders.confirmed       order_id      {order_id, event_id, seat_ids}
  orders.failed          order_id      {order_id, reason}
  payments.succeeded     order_id      {payment_id, order_id, amount}
  payments.failed        order_id      {payment_id, order_id, reason}

  inventory.seat.status  seat_id       COMPACTED. Current status of every seat, the
                                       rebuildable source for the seat-map read model.

The inventory.seat.* topics are what gateway fans out to browsers, and they are the only
ones the frontend cares about.

The orders.* and payments.* topics key by order_id, which spreads perfectly - orders are
independent of each other and nothing needs cross-order ordering. The inventory topics
key by event_id and therefore do not spread at all during an on-sale. That asymmetry is
deliberate and is the thing to go and measure at milestone 9: same cluster, same load,
one set of topics idle and one partition on fire.


PUBLIC API
----------

REST over HTTP, JSON, served by gateway. Every mutating call takes an Idempotency-Key
header and is safe to retry.

  GET    /api/events                        list on-sale events
  GET    /api/events/{id}                   event detail
  GET    /api/events/{id}/sections          section list + availability counts
  GET    /api/events/{id}/sections/{sid}    seats + status for ONE section. Cached and
                                            explicitly stale. Never a whole-event dump -
                                            that endpoint does not exist, on purpose.
  POST   /api/holds                         {event_id, seat_ids[]} -> {hold_id, expires_at}
                                            409 if any seat is gone. All or nothing.
  DELETE /api/holds/{id}                    release early
  POST   /api/orders                        {hold_id} -> {order_id, status}
  GET    /api/orders/{id}                   poll order status
  WS     /api/events/{id}/live?section=   seat status deltas, server -> client only.
                                            Subscription is per section for the same
                                            reason the REST read is: an arena's whole-
                                            event delta stream fans out to too many
                                            browsers to be worth sending.

  POST   /api/sim/config                    simulator knobs, see below

Internal calls are gRPC between services. HTTP only at the edge.


THE FAKE BANK
-------------

The bank is the antagonist. If it is easy to talk to, the project teaches nothing. It
implements authorize / capture / refund with a webhook callback, and its misbehaviour is
configurable at runtime so failures can be provoked on demand:

  latency          200ms to 3s, long tail to 30s
  decline rate     5 percent, with realistic decline codes
  timeout rate     1 percent - request times out but the charge SUCCEEDS server-side.
                   This is the single most valuable behaviour it has. It is what forces
                   payments to be genuinely idempotent rather than accidentally so.
  duplicate hooks  occasionally fires the same webhook twice
  out-of-order     occasionally fires capture-succeeded before authorize-succeeded
  hard outage      a switch that makes it refuse everything, to watch backpressure

Defaults are tame enough for normal running and can be turned savage for an afternoon of
chaos testing.


THE SIMULATOR
-------------

Virtual buyers as state machines, arriving as a Poisson process, each picking a profile:

  browser     opens a seat map, never buys. Pure read load.
  decisive    picks the first acceptable seat and buys immediately.
  picky       holds seats, releases, re-holds elsewhere. Generates hold churn.
  group       demands 3-6 adjacent seats. Generates the interesting contention.
  abandoner   reaches checkout and vanishes. Exercises expiry.
  retrier     hammers a failed request. Exercises idempotency.

Two load modes, and the system must survive both:

  steady      constant arrival rate, hours or days. Finds leaks, drift, slow degradation.
              THE DEFAULT IS ONE SHOWING PER DAY with a handful of buyers, and it stays
              there until someone deliberately raises it. A system that is quietly busy
              is observable and debuggable; a system under permanent stress is neither,
              and it outruns anyone's ability to read the code that produced it. The
              on-sale burst below is a thing you trigger, never a default.
  on-sale     the concert case. 100,000 buyers arriving inside 60 seconds for one
              20,000-seat arena event, most of them wanting 2-6 adjacent seats. Finds
              everything else. This is where "high load" means something here, and it is
              the reason the whole design refuses to assume small seat charts.

Scaling load is scaling the simulator Deployment's replica count. The simulator reports
its own view of truth - attempted buys, successes, 409s, latency - which is compared
against the backend's numbers. Divergence between the two is the alarm that matters.


FRONTEND
--------

React SPA. Two screens are enough:

  seat map    the live picture. Seats change colour as other people take them, over the
              WebSocket. This is the payoff of the entire project and should be built
              early enough to be motivating.
  ops board   orders per second, hold churn, expiry rate, decline rate, p99 checkout,
              oversell counter (which must read zero, forever).

Deliberately not built: user accounts, real login, ticket delivery, seat pricing tiers in
the UI, anything that is product work rather than systems work.


REPO LAYOUT
-----------

Monorepo. Go workspace at the root.

  services/
    gateway/  catalog/  inventory/  orders/  payments/  bank/  simulator/
  web/                  React SPA
  pkg/                  lifted from cineplex/pkg - logger, otel, health, http, env
  deploy/
    base/               kustomize per service
    overlays/homelab/   what Argo CD actually syncs
  infra/                CLUSTER.md and cluster scripts. Stays as is.
  cineplex/             kept for reference until pkg/ is extracted, then deleted.
  tpl/                  becomes the new-service generator, or is deleted.

Each service owns its Dockerfile. Nothing imports another service's internal package -
they talk over gRPC or Kafka, and pkg/ is the only shared code.


DELIVERY
--------

  git push
    -> GitHub Actions, matrix over changed services only
    -> docker build, push to Docker Hub as slash3b/tickets-<service>:<sha>
    -> the tag in deploy/overlays/homelab is bumped
    -> Argo CD syncs

Argo CD is already installed, upgraded to 3.5.1, and currently manages zero Applications,
so this is the first real use it will get. One Application per service, all pointing at
this repo under deploy/overlays/homelab.

Every cluster change made along the way gets written into infra/CLUSTER.md in the same
session, per CLAUDE.md.


OBSERVABILITY
-------------

Non-negotiable and built in from the first service, not retrofitted. The target is
Datadog parity with open source, and the platform is SigNoz - see PLAN.md for what gets
installed and why that was chosen over assembling the Grafana stack.

WHAT THE SERVICES ACTUALLY DO is the important part here, and it is almost nothing:

  Every service emits plain OTLP. cineplex/pkg/otel already does this and gets lifted
  wholesale. No service imports a SigNoz package, knows a SigNoz hostname, or contains
  the string "signoz" anywhere. They speak OTLP to a collector endpoint given by an
  environment variable.

  That discipline is worth stating because it is what keeps the backend replaceable. If
  SigNoz turns out to be the wrong call at milestone 6, moving to Tempo and Loki and
  VictoriaMetrics is a collector reconfiguration - not one line changed across eight
  services. Vendor neutrality is the entire point of OTLP and it is free as long as
  nobody breaks it for convenience.

  traces    one trace from browser click to seat sold, across all seven services. The
            checkout path is meaningless without it. Browser spans included, so a slow
            purchase is traceable from the user's click rather than from the gateway in.
  metrics   RED per service, plus the domain metrics that matter more than RED here:
            holds created / expired / converted, oversell attempts blocked, bank decline
            rate, seat-map staleness, hold-to-confirm latency, and Kafka consumer lag
            per partition - which is how the hot partition will announce itself.
  logs      structured, zap, trace id on EVERY line. A log line without a trace id is a
            log line that cannot be correlated, which on this stack makes it nearly
            worthless.
  profiles  continuous CPU and memory profiling, added at milestone 9 via Pyroscope.
            For a system whose hot path is lock contention on seat rows, always-on
            profiling is how you answer WHY p99 moved rather than just noticing it did.

THE SINGLE MOST IMPORTANT NUMBER on any dashboard is the oversell counter. It reads zero.
If it ever does not, everything else stops.

THE SECOND MOST IMPORTANT is the divergence between the simulator's count of successful
purchases and the backend's count of confirmed orders. Those two numbers are produced by
independent systems and must agree. When they do not, either an oversell or a lost order
has happened, and the gap is the first place it becomes visible.


HOMELAB CONSTRAINTS
-------------------

From infra/CLUSTER.md, and these are real limits on the design, not footnotes:

  3 nodes, 15G disks. AS OF 2026-08-24 THIS IS NO LONGER THE BINDING CONSTRAINT: the
  cluster was cleaned out and now sits at 47/29/16 percent with ~30G free. Memory is the
  constraint now, and node-2's 5.7Gi against the others' 7.6Gi is what drives placement.
  The history below is kept because the diagnosis is the useful part. Measured on the control plane 2026-08-24, the 12G is NOT container images -
  crictl reports 9 images totalling 710M and every one is in use, so the crictl prune
  advice previously recorded here would have freed roughly nothing. The actual
  occupants, and roughly 5G is reclaimable in three commands:

    2.7G  /home/slash3b/go-pkgs        Go module cache. A Go toolchain is building on
                                       the control plane; go clean -modcache.
    1.2G  /etc/kubernetes/tmp          three kubeadm etcd backups from the 2026-08-23
                                       upgrade session. kubeadm leaves these behind and
                                       never cleans them up. Pure garbage once the
                                       upgrade is verified, which it is.
    1.1G  /var/cache/apt               apt-get clean.
    488M  /home/slash3b/go             GOPATH.

  Do that before Kafka goes anywhere near the cluster. crictl prune stays worth running
  LATER, once per-commit image tags from seven services have actually accumulated - it
  is the right tool for a problem this cluster does not have yet.

  Memory is the second constraint and it is not uniform: ctrl-plane and node-1 have 8Gi,
  node-2 has 5.9Gi. Kafka and Postgres both want page cache and both pin to a node via
  local-path. Neither belongs on node-2.

  Single-member etcd, no backup job. The cluster is one bad afternoon from total loss.
  Nothing here should be irreplaceable.

  local-path storage only. A PVC pins its pod to one node forever, and the data dies with
  the node. Postgres therefore pins to a node. Accept it, and know that "the database
  moved nodes" is not a scenario this cluster can offer.

  No Ingress controller, no LoadBalancer. Everything external is a NodePort.

  "High load" here means a few hundred RPS against contended seats, not tens of
  thousands. That is entirely sufficient: oversell, saga compensation, deadlock and
  backpressure all appear at 200 RPS on a hot seat exactly as they do at 20k. Chasing a
  bigger number on this hardware would cost real time and teach nothing new.

  The concert on-sale therefore gets simulated at homelab scale - a 20,000-seat chart is
  cheap to store, and the arrival burst is compressed to whatever the cluster can take.
  The interesting part of a Lady Gaga on-sale is the contention shape, not the absolute
  request count, and the shape survives being scaled down.


MILESTONES
----------

  0  this document, reviewed and agreed
  1  inventory alone.                                          IN PROGRESS
     DONE 2026-08-27: schema with the oversell invariant as a CHECK constraint, the
     conditional-update claim, seat-id sorting plus 40P01 retry, the invariant
     checker, and the concurrency test. Measured: 1000 goroutines on 10 seats,
     exactly 10 wins, 7428 attempts/sec, zero errors, under -race. Overlapping
     three-seat groups hold too.
     THE OVERSELL TEST IS MANUAL - build tag `oversell`, so `go test ./...` does
     not compile it and it cannot run by accident. `make oversell` runs it.
     SWEEPERS DONE 2026-08-27: expiry sweeper, hard-deadline sweeper, Convert, and
     a durable released_reason so "died of a slow bank" and "died of an abandoned
     checkout" stay distinguishable. The test that matters most passes - a hold in
     `converting` SURVIVES its short TTL, so a slow bank cannot cost a customer
     money for seats already sold to someone else.
     MILESTONE 1 COMPLETE. Still no HTTP, no k8s, no other service - the core was
     proven first, which was the entire point of doing it this way round.
  2  bank + payments.                                          IN PROGRESS
     DONE 2026-08-27: the adversarial bank (configurable latency, declines,
     outage, and the timeout-with-side-effect case) plus the client that survives
     it. Five tests pass under -race, and they encode the semantics:
       a timeout does NOT double-charge on retry
       a lost outcome is recoverable by lookup, not by guessing
       a repeated key does NOT re-roll the verdict - an authorization cannot
         become a decline because the caller retried
       an outage reads as UNKNOWN, never as a decline
       when the bank genuinely never saw it, the caller is told the retry is safe
     The bank runs as a service using the same pkg/ as hello, which is the first
     evidence that "every service is hello with logic added" is true rather than
     aspirational.
     REMAINING: the payments store (payments, idempotency_keys, webhook_events),
     webhook delivery and its dedup, and the background reconciler.
  3  orders. The saga, crash recovery, the paid-to-confirmed gap.
  4  catalog + gateway. First real HTTP API.
  5  simulator, steady mode. First continuous load.
  6  containerise everything, deploy via Argo CD to the homelab. Reclaim disk FIRST and
     stand up Kafka via Strimzi with retention set correctly from the very first topic -
     a Kafka installed with defaults on this hardware will fill a node before you have
     finished reading about it.
  7  web. The live seat map.
  8  arenas and concerts. Second venue kind, sections, a 20,000-seat chart, on_sale_at
     in the future for the first time.
  9  on-sale mode. The Lady Gaga test. Break it, fix it, write down what broke, and only
     then decide whether it needs a waiting room. This is where the hot partition stops
     being a paragraph in this document and starts being a graph.
  10 three brokers, RF=3, min.insync.replicas=2. ISR shrink, leader election, and what
     happens to producers when a broker dies mid-on-sale. Only worth doing once there is
     disk headroom for it.

Milestone 1 is deliberately unglamorous. If the seat-claim primitive is wrong, every
milestone after it is built on sand, and it is far cheaper to find that out with a
goroutine test than with a simulator and seven deployments in the way.


OPEN QUESTIONS
--------------

  - Adjacency search for group buys: computed in inventory at hold time, or precomputed
    per event and maintained incrementally? At 300 cinema seats either works; at 20,000
    arena seats under an on-sale it is the difference between working and not. Decide
    before milestone 8, not during it.
  - Does a concert on-sale need a real virtual waiting room, or is edge rate limiting
    at the gateway enough? Decide with data from milestone 9, not by guessing now.
  - Do concert seats need price tiers that differ by section? Real arenas do. It is
    product work, so it only earns a place if it makes contention more interesting.
  - Postgres connection pooling: pgbouncer, or is Go's pool enough at this scale?
  - Do orders and payments stay separate services, or is that a split made for the sake
    of having services? Revisit after milestone 3 and be willing to merge them.
  - Where does the React build get served from - nginx sidecar, or embedded in gateway?
  - The hot partition, and there is no obviously right answer: key inventory topics by
    (event_id, section_id) and accept that cross-section ordering is lost, put hot events
    on a dedicated topic, or make the seat-map consumers commutative so ordering stops
    mattering. Do not decide this now. Decide it at milestone 9 holding a graph.
  - Do the 15G disks get grown? Everything above is working around a limit that may
    simply be fixable at the hypervisor, and if it is, that is a far better use of an
    afternoon than tuning retention.
