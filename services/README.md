# services

Ten directories, **six binaries**. The other four are packages compiled *into*
those binaries — they are services in the design and libraries on disk, and
knowing which is which is the first thing anybody needs.

A directory is a binary if and only if it has a `cmd/`. CI discovers them that
way, so adding one needs no edit to the build workflow.

## Deployed

| Service | What it is | What it does |
|---|---|---|
| **gateway** | The public API. The only thing a browser talks to. | Serves `/api/*` — browse showings, load one section's seat map, hold seats, place orders. Owns no data itself: it calls catalog for what *exists* and inventory for what is *available*, which is the one place those two are assembled. Reachable at `api.tickets.lan`, and at `app.tickets.lan/api` for the SPA. |
| **workers** | Every singleton in the system, in one process. | Runs the three background loops that keep data consistent: the inventory sweepers, the payment reconciler and the order resumer. **Must stay at one replica** — nothing there is unsafe concurrently, but N replicas do N times the work on the same rows and multiply traffic to the bank. `strategy: Recreate`, so a rolling update cannot briefly run two. |
| **simulator** | The load. A deployed service, not a test script. | Virtual buyers as state machines across five profiles, arriving as a Poisson process. It is a **client, not a peer**: constructed with nothing but a base URL, no database handle, no privileged path. If it ever needs an internal API, the API is wrong. Dial at `sim.tickets.lan` (`/stats`, `PUT /config`). |
| **bank** | A deliberately adversarial fake payment processor. | Declines, times out, and sometimes takes the money and says nothing — which is the case the whole payment design exists to survive. Idempotency keys make a retried timeout return the *original* charge rather than double-charging. Chaos dial at `bank.tickets.lan` (`PUT /config`). |
| **seeder** | A CronJob, not a server. | Creates one showing a day at 03:00, idempotently, so the system permanently has something real to do without anyone deciding to give it work. `SEED_DAYS_AHEAD=N` makes one on demand. |
| **hello** | The canary. Does nothing useful, on purpose. | Emits one metric, one log line and one span per request, so the whole telemetry path can be proven end to end without involving anything that matters. It is the leftover this project keeps *not* deleting, deliberately. |

## Compiled into gateway and workers

These are the four services DESIGN.md describes as separate processes speaking
gRPC. Today they are packages in the two binaries above. Every boundary is already
a consumer-declared interface, so splitting them out later is wiring rather than
rework — and running two processes until there is a reason to run six avoids
paying for network hops that buy nothing yet.

| Package | What it is | What it does |
|---|---|---|
| **catalog** | What *exists*. The easy one, on purpose. | Owns venues, sections, seats, events and prices. Read-heavy and almost never written. It reports which seats exist; it **does not write `inventory.event_seats`**, not even on the initial load, where letting it would have been the obvious shortcut. |
| **inventory** | What is *available*. The contended core. | The only writer of seat status anywhere in the system. A seat is claimed by one conditional `UPDATE ... WHERE status = 'available'`, so the check and the write are atomic and uncontended seats still proceed fully in parallel. Also owns hold expiry — and the rule that `converting` stops the short TTL, without which a slow bank costs a customer their seats. |
| **orders** | The saga. | Drives an order through created → awaiting payment → paid → confirmed, writing the saga log *before* each attempt. Recovery is **forward, never backward**: a paid order whose confirmation was lost gets finished, not refunded. |
| **payments** | Whether money moved. | Written on the assumption that the bank may take money and then say nothing, so `unknown` is a first-class state distinct from failed. An unknown payment must **not** release the seats — that is precisely how a paying customer loses them to someone else. |

## Not here

`web/` is the React seat map (`app.tickets.lan`) and is not a Go service; it is a
static nginx image built from its own Dockerfile.
