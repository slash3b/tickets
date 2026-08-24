TICKETS - BUILD AND PLATFORM PLAN
Draft 1, 2026-08-24. Companion to DESIGN.md.

DESIGN.md says what the system is. This file says what has to exist on the cluster before
it can run, in what order things get stood up, and what each piece costs. Read DESIGN.md
first; nothing here re-argues a decision made there.

DECISION: INFRASTRUCTURE FIRST. The platform gets built, proven and observable before the
first real service is written. Rationale and the one exception are in PHASING at the end.


PART 1 - WHAT ALREADY EXISTS
----------------------------

From infra/CLUSTER.md, verified 2026-08-24. Nothing here needs installing.

  kubeadm 1.36.4        3 nodes, single-member etcd, no backup job
  flannel 0.27.0        CNI, 10.244.0.0/16 vxlan overlay
  local-path 0.0.28     the only StorageClass, default, node-pinned volumes
  Argo CD 3.5.1         installed from raw manifests, managing ZERO Applications
  cert-manager 1.19.1   healthy, zero Issuers, zero Certificates - completely inert

cert-manager is the interesting one: it is already running and doing nothing. It stops
being dead weight the moment the ingress layer below needs certificates, which is the
first thing this plan does. Nothing new to install for it.

RESOURCE BASELINE, measured 2026-08-24.

The three nodes are KVM guests on a single physical host, and they are badly
under-provisioned against it:

  node             vCPU   RAM     disk   used
  k8s-ctrl-plane      6   7.7Gi    15G    47%
  k8s-node-1          4   7.6Gi    15G    29%
  k8s-node-2          4   5.7Gi    15G    16%
  ------------------------------------------
  allocated          14  ~21Gi     45G
  PHYSICAL HOST       ?   48G     500G

  ~27G of RAM and ~455G of disk are sitting unallocated. The guests are using nine
  percent of the host's storage.

  This single fact invalidates most of the resource anxiety elsewhere in this file and in
  DESIGN.md. Aggressive Kafka retention, dropping a signal to save memory, deferring
  replication - all of that was solving a problem created by VM sizing rather than by the
  hardware. The answer is to resize the guests, which is phase 0.0, not to tune around it.

  TARGET AFTER RESIZE - 40G RAM and 400G disk allocated, leaving the host ~8G and ~100G:

  node             RAM    disk
  k8s-ctrl-plane   12G    100G
  k8s-node-1       14G    150G
  k8s-node-2       14G    150G

  ~35Gi schedulable after per-node system overhead. Everything in TOTAL FOOTPRINT fits
  inside that, including the additions deferred to milestones 9 and 10.

  STILL TRUE AFTER THE RESIZE: local-path storage pins a PVC to one node permanently, so
  ClickHouse, Postgres and Kafka each choose a node once and live there. Spread them
  across the three deliberately rather than letting the scheduler stack them.


PART 2 - WHAT MUST BE INSTALLED
-------------------------------

Grouped by why, with what it costs. Nothing goes on this list without a service in
DESIGN.md that needs it.

ACCESS LAYER - because NodePort does not scale to eight services
................................................................

  MetalLB               Gives the cluster real LoadBalancer IPs from a LAN pool, in L2
                        mode. Today every exposed thing is a NodePort on a random
                        30000-32767 port, which is already awkward at two services and
                        becomes unusable at eight.
                        Install: helm, metallb/metallb. One controller + a speaker
                        DaemonSet. Needs a spare LAN range reserved outside DHCP.
                        Cost: ~200Mi total.

  ingress-nginx         One entry point, host-based routing, TLS termination. Turns
                        eight NodePorts into one LoadBalancer IP and eight hostnames.
                        Install: helm, ingress-nginx/ingress-nginx. Service type
                        LoadBalancer, which is what MetalLB is there to satisfy.
                        Cost: ~256Mi.

  cert-manager          ALREADY INSTALLED, currently inert. Gets a self-signed
                        ClusterIssuer plus an internal CA so every ingress host gets a
                        real certificate. Browser will warn unless the CA is trusted on
                        the workstation, which is a one-off import.
                        Cost: zero new. It is already running.

DATA PLANE - the stores DESIGN.md names
.......................................

  CloudNativePG         Postgres as a CRD. Chosen over a bare Deployment or the Bitnami
  (CNPG operator)       chart because it handles backup, PITR and failover as first-class
                        objects, and because operating a database through an operator is
                        itself one of the things worth learning here.
                        Single instance to start - local-path pins it to a node anyway,
                        so replicas would be theatre until storage changes.
                        Install: helm, cnpg/cloudnative-pg, then a Cluster CR.
                        Cost: operator ~200Mi, Postgres instance ~1.5Gi + PVC.
                        PLACEMENT: not node-2.

  Redis                 Cache only, per DESIGN.md - never the truth for anything. A plain
                        Deployment with no persistence is correct here, because a Redis
                        that cannot lose data is a Redis whose loss can hurt, and the
                        design explicitly forbids that.
                        Install: a Deployment and a Service. No operator, no chart.
                        Cost: ~256Mi, no PVC.

  Strimzi               Kafka as CRDs - Kafka, KafkaTopic, KafkaUser. Topics live in git
  + Kafka 4.x KRaft     under deploy/ and Argo CD manages them, which is the whole point.
                        KRaft means no ZooKeeper; that ensemble no longer exists in 4.x.
                        One broker, RF=1, until milestone 10.
                        Install: helm, strimzi/strimzi-kafka-operator, then a Kafka CR.
                        Cost: operator ~300Mi, broker ~1.5Gi + PVC.
                        PLACEMENT: not node-2.
                        RETENTION IS MANDATORY FROM THE FIRST TOPIC - see DESIGN.md.

OBSERVABILITY - SIGNOZ. THE TARGET IS DATADOG PARITY
....................................................

The brief is Datadog-level observability with open source, and SigNoz was chosen over
assembling the Grafana stack because it delivers the Datadog EXPERIENCE rather than the
Datadog ingredients. Service map, per-endpoint RED metrics, flame graphs, exception
tracking and trace-to-log correlation are prebuilt screens, not dashboards you construct.

The cluster's empty monitoring, logging and tracing namespaces from the old torn-out
stack were deleted 2026-08-24; this installs onto a clean cluster.

  ClickHouse            The single store. All three signals - metrics, traces, logs -
                        live in one columnar database rather than in three purpose-built
                        ones. That is the whole architectural bet: one store to operate,
                        one query engine, and joins across signals are cheap because they
                        are literally the same database.
                        Needs a coordination service (ClickHouse Keeper, or ZooKeeper on
                        older charts) - the SigNoz chart brings it.
                        Cost: ~6Gi and it is by far the largest single consumer. Give it
                        real disk; with 150G per node this is finally not a problem.
                        PLACEMENT: pin it. local-path means wherever it lands is where it
                        lives forever.

  SigNoz                Query service plus the UI. The APM views, service map, alerting
                        rules and dashboards. One ingress host, one login.
                        Cost: ~1.5Gi.

  OTel Collector        The ingest point. SigNoz ships its own, and it speaks plain OTLP -
                        there is no proprietary agent anywhere in this stack.
                        THIS IS THE PART THAT MATTERS MOST STRATEGICALLY: our services
                        emit vanilla OTLP and know nothing about SigNoz. cineplex/pkg/otel
                        already does this and needs no change. If SigNoz disappoints, the
                        migration to the Grafana stack is a collector config change and a
                        new backend - not a single line touched in eight services.
                        Cost: ~1Gi.

  k8s-infra             SigNoz's DaemonSet for cluster telemetry - node metrics, pod
                        metrics, container log tailing. This is what makes the
                        infrastructure half of the Datadog picture appear.
                        Cost: ~256Mi per node, ~768Mi total.

  metrics-server        NOT part of SigNoz and currently MISSING from this cluster -
                        kubectl top and the Metrics API return "Metrics API not
                        available" today. Required for kubectl top, for HorizontalPod-
                        Autoscaler, and for any autoscaling the simulator load might
                        justify later. Tiny.
                        Cost: ~128Mi.

WHERE SIGNOZ FALLS SHORT OF DATADOG, AND HOW EACH GAP GETS FILLED

  Being honest about this now is cheaper than discovering it at milestone 9. Three
  Datadog features have no good SigNoz equivalent:

  continuous profiling   Datadog's Continuous Profiler is always-on CPU and memory
                         profiling in production. SigNoz has no equivalent.
                         FILL: Pyroscope, standalone, ~2Gi. Go has first-class pprof
                         support so instrumenting is nearly free, and for a system whose
                         hot path is lock contention on seat rows, always-on profiling is
                         not a luxury - it is how you find out WHY p99 moved.
                         Worth installing. Not in phase 0; add it at milestone 9 when
                         there is contention worth profiling.

  browser RUM            Datadog RUM captures real user page performance and JS errors.
                         SigNoz's frontend story is thin.
                         FILL: OTel browser instrumentation in the React SPA, emitting
                         OTLP to the same collector. Weaker than Datadog RUM, but it puts
                         browser spans in the same trace as the backend, which is the
                         part that actually matters here.

  synthetics             Datadog runs scripted checks from outside.
                         FILL: k6 on a CronJob hitting the public API, exporting to the
                         collector. Small, and k6 is worth knowing anyway.

  Everything else - infra metrics, APM, distributed tracing, log search, alerting,
  dashboards, service map - SigNoz covers natively.


DELIVERY
........

  Argo CD               ALREADY INSTALLED. Gets an app-of-apps: one root Application
                        pointing at deploy/argocd/, which contains one Application per
                        component and per service. Adding a service becomes adding a file.
                        Cost: zero new.

  GitHub Actions        Runs outside the cluster, costs it nothing. Matrix build over
                        changed services only, push to Docker Hub, then commit the new
                        image tag back to deploy/overlays/homelab. Argo sees the commit
                        and syncs.
                        Deliberately NOT Argo CD Image Updater: having CI commit the tag
                        makes every deploy a visible commit in git history, which is
                        worth more here than the automation saves.

TOTAL FOOTPRINT
...............

Sized for the POST-RESIZE cluster - 40Gi allocated across three VMs, see 0.0 in PHASING.

  access        MetalLB 200Mi + ingress-nginx 256Mi                        ~0.5Gi
  data          CNPG 200Mi + Postgres 4Gi + Redis 1Gi + Strimzi 300Mi
                + Kafka 4Gi                                                ~9.5Gi
  observability ClickHouse 6Gi + SigNoz 1.5Gi + collector 1Gi
                + k8s-infra 768Mi + metrics-server 128Mi                   ~9.4Gi
  existing      Argo CD ~1.5Gi + cert-manager 256Mi                        ~1.8Gi
  services      8 x ~256Mi                                                 ~2.0Gi
  simulator     variable, this is the load dial                            ~2.0Gi
                                                                          --------
                                                                          ~25.2Gi

  later, not phase 0:
  Pyroscope     continuous profiling, added at milestone 9                  ~2.0Gi
  Kafka RF=3    two more brokers at milestone 10                            ~8.0Gi
                                                                          --------
                                                                          ~35.2Gi

Against ~35Gi schedulable after the resize (40Gi allocated, minus ~1.5Gi/node system
overhead). It fits, including the milestone 9 and 10 additions, which is the entire
justification for resizing before installing anything.

ON THE CURRENT 21Gi IT WOULD NOT FIT. ClickHouse alone wants 6Gi and Kafka plus Postgres
another 8Gi; that is two thirds of the present cluster before a single service runs.
This is why 0.0 comes before 0.3.

RETENTION. With 400G of disk instead of 45G, retention stops being a survival tactic and
becomes a real choice. Targets, all generous by homelab standards and all still bounded,
because "unbounded" is how you end up back where this project started:

    metrics    90 days     the cheapest signal per byte, and the one you want history for
    logs       30 days
    traces     7 days      highest volume, shortest useful life
    Kafka      7 days      long enough to replay a topic and rebuild a read model

Every one of these is set at install time. ClickHouse TTLs and Kafka retention are
configured with the first install, never bolted on after the first disk alert.


PART 3 - PHASING
----------------

PHASE 0 - PLATFORM. No business logic. Ends with a cluster that is fully observable and
delivers code from git commit to running pod with no human step.

  0.0  VMs resized                                              DONE 2026-08-24
       RAM 7.7/7.6/5.7Gi -> 12.5/14.5/14.5G, disk 15/15/15G -> 40/120/120G. Done online
       via Proxmox with no downtime and no reboot - qm resize, then growpart and
       resize2fs inside each guest. All three nodes Ready afterwards, zero pods
       disturbed. See VIRTUALIZATION in CLUSTER.md.
       Total VM disk allocation is 310.00G against a 337.86 GiB LVM-thin pool, chosen
       so the pool can NEVER be over-committed. Deleting the minikube VM and the pihole
       LXC freed ~74G, ~50G of which went to the two workers. ~28G of headroom is left
       deliberately: cloning k8s-node-tpl for a fourth node would consume 15G of it.
       Control plane untainted 2026-08-24, so all three nodes are schedulable: ~39.2Gi
       allocatable and 14 cpu, up from ~27Gi on two nodes. This makes node affinity
       MANDATORY on every stateful workload - ClickHouse, Kafka and Postgres must be
       pinned to node-1 or node-2, because nothing else now keeps them off etcd's disk.

  0.1  disk reclaimed, all three nodes                           DONE 2026-08-24
       83/50/43% -> 47/29/16%, ~30G free cluster-wide. See DISK in CLUSTER.md.
  0.2  worker SSH fixed                                          DONE 2026-08-24
       Host keys verified out-of-band via the Kubernetes API, known_hosts repaired, key
       auth installed on both workers. Control plane is a working jump host.
       REMAINING: password auth is still enabled on both workers and should be disabled.
  0.2b clean slate                                               DONE 2026-08-24
       All observability leftovers deleted - 2 PVCs, 13 orphan CRDs, 4 empty namespaces,
       3 unused helm repos, stale images on every node. See CLEANUP in CLUSTER.md.
  0.3  MetalLB + ingress-nginx + cert-manager ClusterIssuer.
       EXIT TEST: a hostname on the LAN resolves to a cluster service over HTTPS.
  0.4  Argo CD app-of-apps, deploy/ tree laid out, everything from 0.3 moved INTO git and
       re-synced from there rather than left as a hand-applied install.
       EXIT TEST: kubectl delete the ingress controller, Argo puts it back.
  0.5  hello-service. A Go service that does nothing but serve /healthz and emit one
       metric, one log line and one trace span. Full CI: commit -> image -> Docker Hub ->
       tag bump -> Argo sync -> reachable via ingress.
       EXIT TEST: change a string, push, and watch it reach the cluster untouched by hand.
       This is the single most valuable step in phase 0 and it is worth more than it
       looks - every later service is this service with logic added.
  0.6  observability. SigNoz, plus metrics-server which the cluster is missing entirely.
         a) metrics-server              EXIT TEST: kubectl top nodes returns numbers
         b) SigNoz - ClickHouse, query service, OTel collector, behind an ingress host
         c) k8s-infra DaemonSet         EXIT TEST: every node and pod visible in SigNoz
         d) point hello-service at the collector over OTLP
            EXIT TEST: one hello-service request shows up as a trace, its log lines
            correlated to it, and its RED metrics on the service map - all three signals
            from one request, which is the whole reason for choosing a single store.
       The old stack's leftovers are already gone as of 0.2b, so this installs onto an
       empty cluster rather than around a corpse.
       Set ClickHouse TTLs HERE, at install, not later.
  0.7  data plane. CNPG + Postgres, Redis, Strimzi + Kafka with retention set.
       EXIT TEST: hello-service writes a row, caches it, produces and consumes a Kafka
       message, and all of it is visible in one Grafana trace.

  At the end of phase 0 there is no product at all, and that is correct. Everything after
  this lands on a substrate that is already proven, so a failure in phase 1 is a failure
  in phase 1's code rather than an argument with the platform.

PHASE 1 - THE SYSTEM. As the milestones in DESIGN.md, now with somewhere to run.

THE ONE EXCEPTION, and it matters:

  DESIGN.md milestone 1 - the inventory seat-claim primitive and its 1000-goroutine
  oversell test - does NOT wait for phase 0. It needs Postgres in a local container and
  nothing else. It runs in parallel, starting now.

  The reason is that it is the one piece where being wrong invalidates everything built
  on top, and phase 0 is measured in evenings. Blocking the highest-risk question behind
  the platform buildout would be exactly the wrong order. Infrastructure-first is right
  for everything that needs infrastructure; the core concurrency primitive does not.


PART 4 - RUNNING RULES
----------------------

  - Every cluster change is written into infra/CLUSTER.md in the same session, per
    CLAUDE.md. Phase 0 is nothing but cluster changes, so this will be most of the work.
  - Nothing is installed by hand twice. First install may be helm by hand to learn the
    shape; it is then moved into deploy/ and Argo CD owns it. If Argo does not own it, it
    does not exist.
  - Retention is set at install time for every store that grows. No exceptions.
  - Nothing stateful lands on node-2.
  - No credentials in this repo or in CLUSTER.md. Record where a secret lives and the
    command to reset it.
