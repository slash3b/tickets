HISTORY - TEARDOWNS, CLEANUPS AND KNOWN DRIFT
=============================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


TEARDOWN 2026-08-23 - CINEPLEX
------------------------------

  Deleted, in this order:
    application.argoproj.io/cineplex-prod   (argocd)
    deployment.apps/cineplex                (default)
    service/cineplex                        (default)

  The Application carried no resources-finalizer, so deleting it would not have cascaded;
  the Deployment and Service were removed explicitly. Nothing named cineplex remains in
  the cluster and the default namespace now holds only the kubernetes service.

  Backup of all three manifests before deletion:
    on the control plane   ~/cineplex-teardown-backup-2026-08-23.yaml

  Not touched, so this is reversible and cineplex can still come back:
    - the manifests in github.com/reeldex/cineplex.git under k8s/base
    - the published image slash3b/cineplex:v1.1.6 on Docker Hub
  To make the removal permanent at the source, delete k8s/base from that repo.

CLEANUP 2026-08-24 - CLEAN SLATE
--------------------------------

  Everything left behind by the old observability stack was removed. The cluster now
  runs only things that are actually in use. Backups of every deleted object are on the
  control plane in ~/cleanup-backup-2026-08-24/ (pvcs.yaml, pvs.yaml, namespaces.yaml,
  crds.yaml, crd-names.txt), and the script that did it is ~/cleanup-2026-08-24.sh.

  Deleted:
    PVCs        monitoring/storage-tempo-0        Bound 5Gi, Tempo long gone
                logging/storage-loki-stack-0      Pending for 377 days, Loki long gone
                Both PVs went with them - local-path reclaim policy is Delete.
    CRDs        12 x configuration.konghq.com     no Kong workload had existed for a year
                jaegers.jaegertracing.io          no Jaeger operator
                All 13 verified to hold ZERO instances before deletion. This matters:
                deleting a CRD CASCADES and destroys every object of that type, so the
                zero-instance check is not optional.
    Namespaces  kubernetes-dashboard, logging, monitoring, tracing - all empty
    Helm repos  jaegertracing, kubernetes-dashboard, opentelemetry - nothing installed
                from any of them. vm, grafana and jetstack were KEPT; the first two are
                needed by the planned observability stack.
    Images      crictl rmi --prune on all three nodes, plus apt-get clean on the workers.
                Freed the stale Argo CD upgrade hops (v3.1.16, v3.2.12, v3.4.7), old dex
                and redis tags, and a busybox left by the host-key verification pod.

  Result - disk, before this session and after:

    node             before   after    free
    k8s-ctrl-plane   83%      47%      7.5G
    k8s-node-1       50%      29%     10.4G
    k8s-node-2       43%      16%     12.4G

  30G free across the cluster, up from about 8G at the start of the day. Disk has gone
  from the binding constraint on this project to a non-issue for the foreseeable future,
  which changes the calculus on Kafka retention and on three-broker replication later.

  Verified after: all 23 pods Running, all three nodes Ready, zero PVCs, zero PVs,
  nine CRDs all backed by a live controller.

  NOT deleted, deliberately:
    - cert-manager, which is still inert with zero Issuers. It is about to get a job
      issuing certificates for the ingress layer, so removing it would be churn.
    - the argocd-upgrade-2026-08-23 and cineplex-teardown backups in ~ - those are
      records, not dirt.


REPO DRIFT
----------

  The control plane holds its own clone at ~/Projects/homelab-infrastructure and it is
  AHEAD of the workstation copy:

    workstation infra/    HEAD 52ab52b "fuck"
                          scripts: install.sh, jaeger-values, otel-values,
                                   prometheus-value, namespaces.sh
                          has cluster-config/namespaces/{monitoring,logging}.yaml
                          dirty: otel-values.yaml (one comment line)

    control plane         HEAD 5bb24ed "trying to install tempo+grafana"
                          extra commits: b33aafa otel-values in debug mode,
                                         5bb24ed tempo+grafana
                          extra files: grafana-values, loki-values, mimir-values,
                                       tempo-values
                          no cluster-config/ directory
                          dirty: grafana-values.yaml

  Pull origin/main into the workstation copy before touching anything under scripts/.

  scripts/install.sh is stale: it installs the old Jaeger + Prometheus stack, and it passes
  --values prometheus-values.yaml while the file on disk is named prometheus-value.yaml,
  so the script fails as written.
