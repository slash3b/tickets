WHAT RUNS ON THE CLUSTER, AND HOW IT WAS BUILT
==============================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


RUNNING SERVICES AND HOW TO REACH THEM
--------------------------------------

Reachable from your LAN:

  Argo CD web UI      https://argocd.tickets.lan      preferred, needs /etc/hosts ->
                                                      192.168.1.240 and the internal CA
                      http://192.168.1.116:31439      (also :31439 on 192.168.1.88 / .24)
                      https://192.168.1.116:32640     self-signed cert, browser will warn
                      service argocd/argocd-server, NodePort, cluster port 80/443 -> pod 8080
                      login  slash3b    local account, role:admin, use this one
                             password set 2026-08-23, 24 random chars - it is a bcrypt
                             hash in the cluster and cannot be read back, so keep it in
                             your password manager. Reset command under ARGOCD ACCOUNTS.
                      login  admin      built-in, still enabled, password never rotated:
                             kubectl -n argocd get secret argocd-initial-admin-secret \
                               -o jsonpath='{.data.password}' | base64 -d

                      CLI: argocd login 192.168.1.116:31439 --insecure --grpc-web \
                             --username slash3b

                      See ARGOCD ACCOUNTS below for how this is wired.

  Kubernetes API      https://192.168.1.116:6443

Cluster-internal only (ClusterIP - reach with kubectl port-forward):

  argocd/argocd-repo-server                 8081, 8084
  argocd/argocd-redis                       6379
  argocd/argocd-dex-server                  5556, 5557, 5558
  argocd/argocd-applicationset-controller   7000 webhook, 8080 metrics
  argocd/argocd-metrics                     8082
  argocd/argocd-server-metrics              8083
  argocd/argocd-notifications-controller-metrics  9001
  cert-manager/cert-manager                 9402 metrics
  cert-manager/cert-manager-cainjector      9402
  cert-manager/cert-manager-webhook         443 https, 9402 metrics
  kube-system/kube-dns                      53/UDP, 53/TCP, 9153 metrics
  default/kubernetes                        443 -> apiserver 6443

  Example: kubectl -n argocd port-forward svc/argocd-server 8080:80
           then http://localhost:8080

The tickets application IS running - namespace tickets, nine workloads, all Synced and
Healthy under Argo CD. cineplex, the earlier project, was removed on 2026-08-23 (see
TEARDOWN below); the line here used to say nothing was running and was left behind when
the app landed.


NODES - 3 KVM VMs (QEMU i440FX), Debian 13 trixie, kernel 6.12.101
------------------------------------------------------------------

Re-read off the live cluster 2026-08-30. Everything below had been stale since the
2026-08-24 resize: this section still carried the pre-resize RAM and the 15G disks, still
said the control plane was tainted, and still said node-2 was idle. None of those were
true. Kubelet v1.36.4 on all three.

  k8s-ctrl-plane   192.168.1.116   control-plane   6 CPU   11.9Gi RAM   40G disk, 58% used
                   NOT TAINTED. The node-role.kubernetes.io/control-plane taint is gone,
                   and this node now carries MORE pods than either worker - 32 against 19
                   and 15. That is the single most surprising line in this file: the
                   control plane is a general-purpose worker that also happens to run
                   etcd and the apiserver. Anything scheduled without a nodeSelector can
                   land on top of the thing the cluster cannot survive losing.
  k8s-node-1       192.168.1.88    worker          4 CPU   13.8Gi RAM   118G disk, 9% used
  k8s-node-2       192.168.1.24    worker          4 CPU   13.8Gi RAM   118G disk, 10% used
                   No longer idle - it carries 15 pods.

Pod placement 2026-08-30: 32 ctrl-plane / 19 node-1 / 15 node-2.

RAM figures are kubelet capacity, which is a little under what Proxmox allocates the
guest (12500M / 14500M / 14500M - see VIRTUALIZATION). The disks are the post-resize
ones; the workers went 15G -> 120G on 2026-08-24 and have room to spare, while the
control plane at 40G is the tight one at 58%.

Cluster age 433 days (built 2025-06-23). All three Ready, no disk or memory pressure.


CLUSTER BUILD
-------------

  Installer      kubeadm, v1.36.4 across kubelet / kubeadm / kubectl / control-plane images
                 apt source pkgs.k8s.io/core:/stable:/v1.36, packages NOT held -
                 an apt upgrade will move the version out from under you
  Runtime        containerd 1.7.24 (Debian package), cgroup v2, cri-tools 1.33, CNI plugins 1.6.0
  etcd           3.6.8, single member, local /var/lib/etcd, no backup job configured
  DNS            CoreDNS v1.14.2, 2 replicas
  kube-proxy     default iptables mode
  Networking     podSubnet 10.244.0.0/16, serviceSubnet 10.96.0.0/12, dnsDomain cluster.local
  CNI            flannel v0.27.0, vxlan backend, nftables disabled
                 (DaemonSet in kube-flannel, plain manifests, not Helm)
  Certificates   leaf certs expire 2027-08-23 (auto-renewed 2026-08-23 on restart)
                 CAs valid to 2035-06-21


NAMESPACES
----------

  Live list 2026-08-30, 17 namespaces:

  argocd, bank, cert-manager, cnpg-system, data, default, envoy-gateway-system,
  kube-flannel, kube-node-lease, kube-public, kube-system, local-path-storage,
  metallb-system, signoz, strimzi-system, tickets, vpa

  ingress-nginx IS GONE. The list here used to name it; the edge is Envoy Gateway
  (envoy-gateway-system) now, not ingress-nginx.

  Every namespace here holds something. kubernetes-dashboard, logging, monitoring and
  tracing were deleted 2026-08-24 - see CLEANUP. The observability namespaces came back
  as signoz, created by Argo CD rather than by hand, which is what was intended.


WHAT IS ACTUALLY RUNNING - AND WHY EACH PIECE IS THERE
------------------------------------------------------

Anything marked REQUIRED cannot be removed without breaking the cluster. Anything marked
OPTIONAL is a choice that was made and could be undone.

  kubeadm control plane            ns kube-system                  REQUIRED
    kube-apiserver v1.36.4         The API. Every kubectl call, every controller, every
                                   kubelet talks to it. Nothing works without it.
    etcd 3.6.8                     The only database. Holds every object in the cluster.
                                   Single member here, so it is also the single point of
                                   failure, and there is no backup job.
    kube-scheduler v1.36.4         Decides which node a new pod lands on.
    kube-controller-manager        Runs the built-in control loops: keeps ReplicaSets at
                                   the right count, manages node lifecycle, issues certs.
    kube-proxy v1.36.4             Programs iptables on every node so ClusterIP services
                                   resolve to a real pod. Without it Service IPs are dead.
    CoreDNS v1.14.2                In-cluster DNS. Turns "argocd-server.argocd.svc" into
                                   an IP. Every service-to-service call depends on it.

  flannel v0.27.0                  ns kube-flannel                 REQUIRED
                                   The CNI plugin - gives each pod an IP from 10.244.0.0/16
                                   and builds a vxlan overlay so a pod on node-1 can reach
                                   a pod on node-2. Kubernetes ships no networking of its
                                   own; without a CNI every pod stays Pending.
                                   Chosen because it is the simplest overlay that works.
                                   Swappable for Calico/Cilium, but that means rebuilding
                                   pod networking cluster-wide.

  local-path-provisioner v0.0.28   ns local-path-storage           OPTIONAL but load-bearing
                                   Watches for PVCs and satisfies them with a directory on
                                   the node's own disk. It is the default StorageClass, so
                                   without it every PVC hangs Pending forever. The tradeoff
                                   is that a volume lives on one node - the pod using it can
                                   never move, and if that node dies the data is gone.
                                   Correct choice for a homelab, wrong for anything real.

  Argo CD v3.5.1                   ns argocd                       OPTIONAL
    argocd-server                  Serves the web UI and the gRPC/REST API. This is the
                                   NodePort you log into.
    argocd-application-controller  The actual engine. Compares what git says should be in
                                   the cluster against what is really there, and syncs.
    argocd-repo-server             Clones git repos and renders manifests (helm template,
                                   kustomize build) into plain YAML for the controller.
    argocd-redis 8.2.3-alpine      Cache for rendered manifests and cluster state. Losing
                                   it costs performance, not data.
    argocd-dex-server v2.45.0      SSO/OIDC broker for external identity providers.
                                   NOT USED HERE - no OIDC is configured, local accounts
                                   only. Could be disabled.
    argocd-applicationset-ctrl     Generates Applications in bulk from templates.
                                   NOT USED HERE - no ApplicationSets exist.
    argocd-notifications-ctrl      Sends sync/health notifications to Slack, email, etc.
                                   NOT USED HERE - no triggers configured.
                                   Installed from raw manifests, NOT a Helm release.
                                   Currently manages zero Applications - see GITOPS.

  cert-manager v1.19.1             ns cert-manager                 OPTIONAL, currently inert
    cert-manager                   Watches Certificate objects and obtains/renews TLS certs
                                   from an issuer (Let's Encrypt, an internal CA, etc).
    cert-manager-webhook           Admission webhook that validates cert-manager resources.
    cert-manager-cainjector        Injects CA bundles into webhook configs and CRDs.
                                   Originally installed because the OpenTelemetry Operator
                                   required it. That operator is gone, and there are zero
                                   Issuers and zero Certificates, so cert-manager is
                                   running and healthy while doing nothing at all.
                                   Note it does NOT manage the kubeadm control-plane certs
                                   - kubeadm does that itself.

Helm
  Exactly one Helm release exists cluster-wide (helm list -A):
    cert-manager   ns cert-manager   rev 8   cert-manager-v1.19.1   deployed
  Values are just: installCRDs: true
  History: v1.18.2 installed 2025-09-03, upgraded to v1.19.1 on 2025-10-15 over 7 revisions.
  Revisions 3 and 5 failed with "context canceled"; revision 8 is the good one.

  Helm repos configured on the control plane (helm repo list):
    vm (VictoriaMetrics), grafana, jaegertracing, jetstack, opentelemetry, kubernetes-dashboard
  Only jetstack is actually used - nothing from the others is installed.

CRDs
  argoproj.io                  applications, applicationsets, appprojects - in use
  cert-manager.io + acme       certificates, issuers, orders, challenges - installed but
                               ZERO objects exist, so cert-manager issues nothing today
  Nine CRDs remain and all nine are backed by a running controller:
    argoproj.io                applications, applicationsets, appprojects - Argo CD
    cert-manager.io + acme     certificates, certificaterequests, issuers,
                               clusterissuers, orders, challenges - cert-manager
  The 12 Kong CRDs and jaegertracing.io/jaegers were deleted 2026-08-24 - see CLEANUP.
  cert-manager's CRDs still have ZERO objects, so it continues to issue nothing until
  the ingress work gives it a ClusterIssuer.

Storage
  local-path (rancher.io/local-path) is the only StorageClass and is the default.
  Delete reclaim policy, WaitForFirstConsumer, no volume expansion.
  Node-local disk, so a PVC pins its pod to one node.

  SEVEN PVCs, all Bound, all local-path. Re-read 2026-08-30; this section used to say
  zero, which was true only in the gap between the 2026-08-24 cleanup and the data plane
  landing.

    data     tickets-pg-1                       20Gi   node-1
    data     data-0-tickets-kafka-dual-role-0   20Gi   node-1
    data     data-0-tickets-kafka-dual-role-1   20Gi   node-1
    data     data-0-tickets-kafka-dual-role-2   20Gi   node-1
    signoz   clickhouse-cluster-0-0-0           40Gi   node-2
    signoz   data-signoz-zookeeper-0             8Gi   node-2
    signoz   signoz-db-signoz-0                  2Gi   ctrl-plane

  EVERY PIECE OF THE DATA PLANE IS ON ONE NODE. Postgres and all three Kafka brokers
  hold local-path volumes on k8s-node-1, so they are pinned there permanently and
  k8s-node-1 is a single point of failure for the entire application - losing it loses
  the database and the whole Kafka quorum at once. Three Kafka replicas on one node is
  replication that buys nothing; it survives a broker crash and not a node one. This is
  a consequence of local-path being the only StorageClass, not a scheduling mistake, and
  it does not get fixed by moving pods around. It gets fixed by storage that is not
  node-local.

  The two 2026-08-24 orphans (monitoring/storage-tempo-0 bound 5Gi,
  logging/storage-loki-stack-0 pending 377 days) were deleted - see CLEANUP.
