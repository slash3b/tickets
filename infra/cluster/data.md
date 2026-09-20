DATA PLANE - POSTGRES
=====================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


DATA PLANE - POSTGRES, INSTALLED 2026-08-27
-------------------------------------------

  CloudNativePG operator 0.29.0 (Postgres 1.30.0)   ns cnpg-system   Argo, wave 1
  Cluster tickets-pg                                 ns data          Argo, wave 4
    PostgreSQL 18.4, ONE instance, PINNED TO k8s-node-1, 20Gi local-path
    database "tickets" owned by role "tickets"

  SERVICES CNPG CREATES, and the distinction matters when services connect:
    tickets-pg-rw    the primary. Everything that writes uses this.
    tickets-pg-ro    replicas only. Empty here - there is one instance.
    tickets-pg-r     any instance, primary included.
  Use -rw unless you have a specific reason. With one instance -ro resolves to
  nothing, so a service pointed at it fails in a way that looks like a network
  problem.

  CREDENTIALS ARE GENERATED, NEVER IN GIT. secret data/tickets-pg-app holds the
  application user; tickets-pg-ca, -server and -replication hold TLS material.
    kubectl -n data get secret tickets-pg-app -o jsonpath='{.data.password}' | base64 -d

  ONE INSTANCE IS DELIBERATE. CNPG runs three with automatic failover, but
  local-path binds a volume to one node permanently, so a replica on another node
  could never take over the primary's data. Replicas here would be availability
  theatre. Postgres becomes genuinely HA on this cluster only when storage stops
  being node-local.

  NO BACKUPS. CNPG does continuous backup and PITR to object storage, and there is
  no object storage here. Same gap as etcd. Nothing in this system should be
  irreplaceable.

  REDIS                                              ns data          Argo, wave 4
    Plain Deployment, redis:8.2-alpine, NO OPERATOR AND NO CHART on purpose.
    Postgres earned an operator because backup, failover and version upgrades are
    genuinely hard; none of that applies to something whose recovery plan is
    "start again".
    NO PVC, --save "", --appendonly no, maxmemory 512mb, allkeys-lru.
    DESIGN.md's test: flushing Redis in production must cost latency and nothing
    else. It is configured to be losable so that stays true. Also the only
    data-plane component NOT welded to a node, precisely because it has no volume.
    Service: redis.data.svc.cluster.local:6379

  KAFKA 4.3.1 via Strimzi 1.2.0                     ns data          Argo, wave 4
    KRaft, NO ZOOKEEPER - removed from Kafka in 4.0, and Strimzi 1.x is KRaft-only.
    (The ZooKeeper on node-2 belongs to ClickHouse, not Kafka.)
    One dual-role node (controller + broker), RF=1 everywhere, 20Gi on node-1.
    Bootstrap: tickets-kafka-kafka-bootstrap.data.svc.cluster.local:9092

    API VERSION TRAP: Strimzi 1.x uses kafka.strimzi.io/v1. Essentially every
    example online still says v1beta2, and Argo rejects it with
      could not find version "v1beta2" of kafka.strimzi.io/Kafka
    The strimzi.io/node-pools and strimzi.io/kraft annotations are also gone - in
    1.x that is the only mode.

    RETENTION SET AT INSTALL: log.retention.hours 168, log.segment.bytes 128MB
    (not the 1GB default), log.retention.bytes 2GB per partition.
    WHY SEGMENT SIZE MATTERS MORE THAN IT LOOKS: retention only ever deletes
    CLOSED segments, so an oversized segment on a low-volume topic is never closed
    and therefore never deleted, whatever retention.ms says. Verified on the live
    broker - kafka-configs.sh shows our 134217728 overriding the 1073741824
    default.

    TOPICS ARE CRs. entityOperator gives KafkaTopic and KafkaUser, so topic
    definitions and their retention live in git rather than in a broker's memory.
    Verified: a KafkaTopic CR became a real topic in 10 seconds, produced and
    consumed successfully.

  PLACEMENT, chosen once because local-path makes it permanent:
    node-1   Postgres + Kafka   node-2   ClickHouse + ZooKeeper + Redis(no volume)
  The control plane takes no stateful workload - untainting it removed the only
  thing separating IO-heavy work from etcd.

  METRICS reach SigNoz through inheritedMetadata annotations on the Cluster
  (signoz.io/scrape, port 9187), not a PodMonitor - there is no Prometheus Operator
  on this cluster.

  RESOURCE POSTURE, measured 2026-08-27. The cluster is NOT constrained:
    k8s-ctrl-plane  37% memory,  2% cpu
    k8s-node-1      20% memory,  2% cpu
    k8s-node-2      28% memory, 35% cpu   (ClickHouse ingest and merges)
  ~11Gi in use of ~40Gi allocated. Requests were trimmed to measured reality
  (Postgres 512->256Mi against 66Mi actual, Redis 128->64Mi against 9Mi) because an
  inflated REQUEST reserves scheduling capacity nobody else can use. LIMITS were
  deliberately left alone - they are what stops something running away, and
  ClickHouse already demonstrated what happens when a limit is too small to
  complete a merge inside.
