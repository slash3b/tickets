REELDEX - TICKET SELLING SYSTEM - DESIGN
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

Seven. The seventh (gateway) exists only because the frontend is a React SPA; a
server-rendered frontend would not need it. Adding an eighth requires a reason written
down here first.

  gateway        Browser-facing BFF. One origin for the SPA, terminates WebSockets,
                 aggregates catalog reads, mints stub JWTs, applies per-user rate limits.
                 Stateless. Owns no data.
                 TEACHES: fan-out, connection handling, load shedding at the edge.

  catalog        Venues, seats, events, seat charts, prices. Read-heavy, almost never
                 written. Everything it serves is cacheable for minutes.
                 TEACHES: read scaling and cache invalidation, in the easy case.

  inventory      Owns EventSeat and Hold. THE contended service. The only writer of seat
                 status. Runs the expiry sweeper and the invariant checker.
                 TEACHES: concurrency control, isolation levels, deadlock, not
                 overselling. This is the heart of the project.

  orders         Saga orchestrator. Owns Order. Drives hold -> charge -> commit, persists
                 its position at every step so it can resume after a crash.
                 TEACHES: sagas, forward recovery, crash-consistent workflows.

  payments       Owns Payment. Idempotency keys, retry policy, webhook dedup. The only
                 service permitted to talk to the bank.
                 TEACHES: idempotency, exactly-once-ish semantics over an unreliable peer.

  bank           The fake bank. Deliberately adversarial - see its own section.
                 TEACHES: nothing by itself. It exists to make the others suffer.

  simulator      Spawns virtual buyers. Not a test script - a deployed service that keeps
                 the system permanently under load.
                 TEACHES: it is the source of every other lesson.

Plus web: the React SPA. Static build, served by nginx or by gateway.

Language is Go for every service, reusing cineplex/pkg (logger, otel, health, http, env,
option) as the shared foundation. That package set is already good and is the main thing
worth keeping from cineplex.


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

  NATS          JetStream. Inter-service events and the feed behind the live seat map.
    JetStream

Kafka was rejected. Three nodes with 15G disks already at 83 percent cannot host Kafka
plus this system, and nothing in the design needs partition-level ordering or log
replay at Kafka's scale. NATS JetStream gives durable at-least-once delivery in a
fraction of the footprint. Revisit only if a lesson genuinely requires Kafka semantics.

Hold expiry is driven by a sweeper polling Postgres for active holds past expires_at, not
by any TTL mechanism in Redis or NATS. Boring, correct, and observable.


EVENTS
------

At-least-once. Every consumer must be idempotent; assume every message arrives twice.

  inventory.seat.held        {event_id, seat_ids, hold_id, expires_at}
  inventory.seat.released    {event_id, seat_ids, hold_id, reason}
  inventory.seat.sold        {event_id, seat_ids, order_id}
  orders.created             {order_id, hold_id, user_id, amount}
  orders.confirmed           {order_id, event_id, seat_ids}
  orders.failed              {order_id, reason}
  payments.succeeded         {payment_id, order_id, amount}
  payments.failed            {payment_id, order_id, reason}

The inventory.seat.* stream is what gateway fans out to browsers. It is the only stream
the frontend cares about.


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
they talk over gRPC or NATS, and pkg/ is the only shared code.


DELIVERY
--------

  git push
    -> GitHub Actions, matrix over changed services only
    -> docker build, push to Docker Hub as slash3b/reeldex-<service>:<sha>
    -> the tag in deploy/overlays/homelab is bumped
    -> Argo CD syncs

Argo CD is already installed, upgraded to 3.5.1, and currently manages zero Applications,
so this is the first real use it will get. One Application per service, all pointing at
this repo under deploy/overlays/homelab.

Every cluster change made along the way gets written into infra/CLUSTER.md in the same
session, per CLAUDE.md.


OBSERVABILITY
-------------

Non-negotiable and built in from the first service, not retrofitted. cineplex/pkg/otel
already does the setup; the pattern is proven and gets lifted wholesale.

  traces    one trace from browser click to seat sold, across all seven services. The
            checkout path is meaningless without it.
  metrics   RED per service, plus domain metrics that matter more than the RED ones:
            holds created/expired/converted, oversell attempts blocked, bank decline
            rate, seat-map staleness, hold-to-confirm latency.
  logs      structured, zap, trace id on every line.

The cluster has empty monitoring, logging and tracing namespaces left over from a
previous stack. They get refilled - see LEFTOVERS in CLUSTER.md for what was torn out.

The single most important dashboard number is the oversell counter. It reads zero. If it
ever does not, everything else stops.


HOMELAB CONSTRAINTS
-------------------

From infra/CLUSTER.md, and these are real limits on the design, not footnotes:

  3 nodes, 15G disks at ~83 percent full. RUN sudo crictl rmi --prune ON EVERY NODE
  BEFORE the first multi-service rollout. Five Argo CD upgrade hops were enough to put
  every node under DiskPressure and stall scheduling for four minutes. Seven services
  with per-commit image tags will be far worse. Disk is the binding constraint on this
  whole project and should be solved properly before it bites.

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
  1  inventory alone. Schema, conditional-update hold, sweeper, invariant checker, and a
     concurrency test that fires 1000 goroutines at 10 seats and asserts exactly 10 wins.
     No HTTP, no k8s, no other service. Prove the core before building around it.
  2  bank + payments. Idempotency under the timeout-but-succeeded case.
  3  orders. The saga, crash recovery, the paid-to-confirmed gap.
  4  catalog + gateway. First real HTTP API.
  5  simulator, steady mode. First continuous load.
  6  containerise everything, deploy via Argo CD to the homelab.
  7  web. The live seat map.
  8  arenas and concerts. Second venue kind, sections, a 20,000-seat chart, on_sale_at
     in the future for the first time.
  9  on-sale mode. The Lady Gaga test. Break it, fix it, write down what broke, and only
     then decide whether it needs a waiting room.

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
