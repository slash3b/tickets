# services

Nine directories, **nine binaries**. Every one of them is its own deployment.

Until 2026-08-28 four of these — catalog, inventory, orders, payments — were
packages compiled into gateway and workers. They are separate processes now,
speaking gRPC generated from `proto/tickets/v1`.

A directory is a binary if and only if it has a `cmd/`. CI discovers them that
way, so adding one needs no edit to the build workflow.

## Deployed

| Service | What it is | What it does |
|---|---|---|
| **gateway** | The public API. The only thing a browser talks to. | Serves `/api/*` — browse showings, load one section's seat map, hold seats, place orders. **Owns no data and has no database credentials at all**: it calls catalog for what *exists*, inventory for what is *available*, and orders to buy. Reachable at `api.tickets.lan`, and at `app.tickets.lan/api` for the SPA. |
| **catalog** | What *exists*. The easy one, on purpose. | Owns venues, sections, seats, events and prices. Read-heavy and almost never written. It reports which seats exist; it **does not write `inventory.event_seats`**, not even on the initial load, where letting it would have been the obvious shortcut. |
| **inventory** | What is *available*. The contended core. | The only writer of seat status anywhere — since the split that is enforced by topology, because nothing else has credentials for the schema. A seat is claimed by one conditional `UPDATE ... WHERE status = 'available'`, so the check and the write are atomic and uncontended seats still proceed fully in parallel. Also owns hold expiry, and the rule that `converting` stops the short TTL, without which a slow bank costs a customer their seats. |
| **orders** | The saga. The only service that calls two others. | Drives an order through created → awaiting payment → paid → confirmed, writing the saga log *before* each attempt. Recovery is **forward, never backward**: a paid order whose confirmation was lost gets finished, not refunded. |
| **payments** | Whether money moved. | Holds the only connection to the bank. Written on the assumption that the bank may take money and then say nothing, so `unknown` is a first-class state distinct from failed. An unknown payment must **not** release the seats — that is precisely how a paying customer loses them to someone else. |
| **workers** | Every singleton in the system, in one process. | Runs the three background loops that keep data consistent: the inventory sweepers, the payment reconciler and the order resumer. **Must stay at one replica** — nothing there is unsafe concurrently, but N replicas do N times the work on the same rows and multiply traffic to the bank. `strategy: Recreate`, so a rolling update cannot briefly run two. |
| **simulator** | The load. A deployed service, not a test script. | Virtual buyers as state machines across five profiles, arriving as a Poisson process. It is a **client, not a peer**: constructed with nothing but a base URL, no database handle, no privileged path. If it ever needs an internal API, the API is wrong. Dial at `sim.tickets.lan` (`/stats`, `PUT /config`). |
| **bank** | A deliberately adversarial fake payment processor. | Declines, times out, and sometimes takes the money and says nothing — which is the case the whole payment design exists to survive. Idempotency keys make a retried timeout return the *original* charge rather than double-charging. Chaos dial at `bank.tickets.lan` (`PUT /config`). |
| **seeder** | A CronJob, not a server. | Creates one showing a day at 03:00, idempotently, so the system permanently has something real to do without anyone deciding to give it work. `SEED_DAYS_AHEAD=N` makes one on demand. |

## How they talk

gRPC, generated from `proto/tickets/v1`. The `.proto` files are the only
definition of what crosses a boundary — one artefact both sides are generated
from, rather than two hand-written structs that agree until somebody renames a
field.

**Status codes are part of the contract.** `ABORTED` means the seats were taken
by someone else — a normal outcome, the most common non-OK in the system, which
the gateway turns into a 409. `FAILED_PRECONDITION` means the hold is already
gone and retrying can never succeed. Everything else is a genuine fault.

Each service serves gRPC on 9090 and a tiny HTTP server on 8080 for `/livez` and
`/readyz`, because kubelet probes speak HTTP.

## Not here

`web/` is the React seat map (`app.tickets.lan`) and is not a Go service; it is a
static nginx image built from its own Dockerfile.
