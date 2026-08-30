SIGNOZ DASHBOARDS
=================

Dashboard JSON kept in git so a rebuilt SigNoz can be put back the way it was.
SigNoz stores dashboards in its own SQLite at /var/lib/signoz/signoz.db inside the
signoz-0 pod; nothing else backs that up, and the PVC is 2Gi of homelab disk.

HOW TO IMPORT
  SigNoz UI -> Dashboards -> + New dashboard -> Import JSON -> paste the file.

There is no API route for this that does not need a logged-in session, so it is a
browser job. The endpoints exist (/api/v1/dashboards) but answer 401 to anything
without a cookie.


go-runtime-v6.json / go-runtime-v5.json
---------------------------------------

The same eight panels in two schema versions, taken unmodified from
github.com/SigNoz/dashboards (go-runtime/). TRY v6 FIRST; if the import is
rejected, use v5. They differ only in envelope — v6 nests everything under
`spec` with `panels` keyed by id, v5 is a flat `widgets` array — and both query
identical metric names.

They are kept unmodified ON PURPOSE. An edited copy silently drifts from
upstream, and the reason these work at all is that our metric names already match
what upstream expects.

WHY THEY WORK HERE. pkg/obs starts
go.opentelemetry.io/contrib/instrumentation/runtime, which emits the CURRENT
OpenTelemetry semantic-convention names:

  go.memory.used          go.goroutine.count
  go.memory.allocated     go.processor.limit
  go.memory.allocations   go.config.gogc
  go.memory.gc.goal

Seven of the eight panels are fed by those and work as soon as the dashboard is
imported, for all nine Go services, selected with the service.name variable at
the top.

THE EIGHTH PANEL, "Memory Limit (GOMEMLIMIT)", WILL BE EMPTY, and that is a fact
about the services rather than the dashboard. The runtime package only reports
go.memory.limit when a memory limit has actually been set, and nothing here sets
one: GOMEMLIMIT is unset, so Go's GC does not know the container has a ceiling.

That is worth more than an empty panel suggests. The Go GC targets a heap that
doubles (GOGC=100) and is blind to the cgroup limit, which is how a Go process in
a 256Mi container walks past it and gets OOMKilled — which has happened here
before, to the gateway and the simulator. Setting GOMEMLIMIT to roughly 90% of
the container limit makes the GC work harder as it approaches the ceiling and
trade CPU for staying alive.

Not done yet, deliberately: it changes GC behaviour in every service and is a
bigger decision than importing a dashboard.


BEWARE THE OLD METRIC NAMES
---------------------------

Most Go dashboards on the internet, and older SigNoz templates, query
runtime.go.mem.heap_alloc / process.runtime.go.* — the names the PREVIOUS
generation of the runtime package emitted. Those will import cleanly and render
nothing at all, which looks like broken instrumentation and is not. Check the
metric names in a template against the list above before concluding anything is
wrong.
