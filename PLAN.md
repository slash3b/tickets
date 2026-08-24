REELDEX - BUILD AND PLATFORM PLAN
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

RESOURCE BASELINE, measured 2026-08-24 after disk reclamation:

  node             disk free    memory
  k8s-ctrl-plane   7.5G         7.7Gi
  k8s-node-1       7.5G         7.6Gi
  k8s-node-2       8.6G         5.7Gi   <- smaller, keep stateful workloads off it

  total usable     ~23G disk    ~21Gi memory, minus ~1.5Gi/node system overhead
                                so call it ~17Gi schedulable


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

OBSERVABILITY - the part that was specifically asked about
..........................................................

The cluster already has empty monitoring, logging and tracing namespaces left from a
previous stack that was torn out (see LEFTOVERS in CLUSTER.md). This refills them, and
also cleans up the two orphaned PVCs still bound there from the old Tempo and Loki.

The shape is the Grafana stack, with one substitution:

  Grafana Alloy         THE AGENT. One DaemonSet that does all three signals: scrapes
                        Prometheus metrics, tails container logs and ships them to Loki,
                        and receives OTLP traces from services and forwards them to
                        Tempo.
                        This replaces what would otherwise be THREE separate agents -
                        the OpenTelemetry Collector, Promtail, and a metrics scraper.
                        Promtail is deprecated in favour of Alloy, so this is also the
                        current path rather than the legacy one.
                        Services speak plain OTLP to Alloy and know nothing about what
                        is behind it. cineplex/pkg/otel already emits OTLP and needs no
                        change.
                        Install: helm, grafana/alloy, as a DaemonSet.
                        Cost: ~256Mi PER NODE, so ~768Mi total.

  VictoriaMetrics       METRICS STORAGE. Chosen over Prometheus deliberately: same
                        PromQL, same scrape model, roughly a third of the memory and
                        materially less disk for the same retention. On 15G nodes that
                        difference decides whether the stack fits.
                        Note the vm helm repo is ALREADY configured on the control plane
                        from a previous attempt - past-you was already thinking this.
                        Install: helm, vm/victoria-metrics-single. Retention 7d.
                        Cost: ~1Gi + PVC. PLACEMENT: not node-2.

  Loki                  LOG STORAGE. Single-binary mode, filesystem backend. Not the
                        microservices deployment mode - that is for object storage and
                        real scale, and would be several pods for no benefit here.
                        Retention MUST be set: 72h. The previous Loki left a PVC that sat
                        Pending for 377 days; that PVC gets deleted, not reused.
                        Install: helm, grafana/loki, singleBinary mode.
                        Cost: ~1Gi + PVC.

  Tempo                 TRACE STORAGE. Single-binary mode, filesystem backend, same
                        reasoning as Loki. Retention 24h - traces are the highest-volume
                        and shortest-useful-life signal here.
                        The old Tempo left a bound 5Gi PVC in monitoring; delete it.
                        Install: helm, grafana/tempo.
                        Cost: ~1Gi + PVC.

  Grafana               THE UI. One place for metrics, logs and traces, with the three
                        wired as datasources and correlated by trace_id so a slow request
                        goes trace -> logs -> metrics in three clicks. That correlation is
                        the entire reason to run all three rather than just metrics.
                        cineplex/grafana-datasources.yaml is a starting point.
                        Install: helm, grafana/grafana. Ingress host, cert from
                        cert-manager.
                        Cost: ~512Mi + small PVC for dashboards.

  WHAT IS DELIBERATELY NOT INSTALLED:
    Prometheus / kube-prometheus-stack   VictoriaMetrics replaces it at a third the cost.
                                         kube-state-metrics and node-exporter are still
                                         wanted; install those two standalone, Alloy
                                         scrapes them.
    OpenTelemetry Collector              Alloy does OTLP receive. Two agents doing one
                                         job is how the previous stack got confusing.
    Jaeger                               Tempo does this and lives in Grafana already.
                                         The orphaned jaegertracing.io CRD gets deleted.
    Mimir                                VictoriaMetrics is the substitution. Not needed.

  Standalone metrics exporters, both tiny, both required for any useful cluster dashboard:
    kube-state-metrics    object-level metrics - deployments, pods, PVCs   ~128Mi
    node-exporter         host-level metrics - cpu, memory, disk, network  ~64Mi/node

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

  access        MetalLB 200Mi + ingress-nginx 256Mi                        ~0.5Gi
  data          CNPG 200Mi + PG 1.5Gi + Redis 256Mi + Strimzi 300Mi
                + Kafka 1.5Gi                                              ~3.8Gi
  observability Alloy 768Mi + VM 1Gi + Loki 1Gi + Tempo 1Gi
                + Grafana 512Mi + ksm/node-exporter 320Mi                  ~4.6Gi
  existing      Argo CD ~1.5Gi + cert-manager 256Mi                        ~1.8Gi
  services      8 x ~200Mi                                                 ~1.6Gi
  simulator     variable, this is the dial                                 ~0.5Gi+
                                                                          --------
                                                                          ~12.8Gi

Against ~17Gi schedulable. It fits, with roughly 4Gi of headroom, and observability is
the single largest consumer at over a third of the total. That is why PHASING below
brings it up in three steps rather than all at once - if memory gets tight, the honest
lever is dropping Loki first, because logs are the signal you can most afford to lose
when traces and metrics are both present.

Disk is the tighter constraint and it is entirely a retention question. Every one of
Kafka, VictoriaMetrics, Loki and Tempo will grow without bound on defaults, and the
simulator produces continuously by design. Retention caps are set at install time, not
after the first disk alert.


PART 3 - PHASING
----------------

PHASE 0 - PLATFORM. No business logic. Ends with a cluster that is fully observable and
delivers code from git commit to running pod with no human step.

  0.1  disk reclaimed on the control plane                       DONE 2026-08-24
       83% -> 50%, 7.5G free. See DISK in CLUSTER.md.
  0.2  worker SSH fixed. node-1 refuses key auth from the control plane; node-2's host
       key has changed and needs verifying before it is accepted. Not urgent for capacity
       - both workers sit under 48% - but needed before any node-level work.
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
  0.6  observability, in three steps, each proven with hello-service before the next:
         a) VictoriaMetrics + Grafana + kube-state-metrics + node-exporter + Alloy metrics
         b) Tempo + Alloy OTLP receive          EXIT TEST: hello-service span in Grafana
         c) Loki + Alloy log tailing            EXIT TEST: log line correlated to that
                                                span by trace_id
       Also in this step: delete the two orphaned PVCs and the Jaeger and Kong CRDs left
       from the old stack, and record it in CLUSTER.md.
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
