HOMELAB KUBERNETES - STATE OF THE CLUSTER
Snapshot 2026-08-24, taken from k8s-ctrl-plane (192.168.1.116).
Changes made 2026-08-23/24: cineplex removed, slash3b account added, Argo CD 3.0.6 -> 3.5.1.
2026-08-24: CLEAN SLATE. All observability leftovers deleted - 2 PVCs, 13 CRDs, 4 empty
namespaces, 3 helm repos, stale images on every node. Disk 83/50/43% -> 47/29/16%, ~30G
free cluster-wide. Worker host keys verified out-of-band and key auth fixed. See CLEANUP,
DISK and ACCESS. No change to anything that was actually running.


ACCESS
------

  SSH control plane   ssh slash3b@192.168.1.116        (user slash3b, sudo NOPASSWD)
  API server          https://192.168.1.116:6443
  kubeconfig          ~/.kube/config on the control plane only, not on the workstation.
                      To drive the cluster from the laptop:
                        scp slash3b@192.168.1.116:~/.kube/config ~/.kube/homelab
                        export KUBECONFIG=~/.kube/homelab
  NodePort range      30000-32767 (default)

  WORKER HOST KEYS - verified and repaired 2026-08-24.

    k8s-node-1  192.168.1.88   ED25519 SHA256:XB3FdqqQrZLoBOQPyhBDLx3l/BJC9XhRb533hFQ2194
    k8s-node-2  192.168.1.24   ED25519 SHA256:vnL6/8T/p5tSlHEaBg40OITWDzp28yiO4fobXnM5kpM

  The control plane's known_hosts had a stale ECDSA entry for .24 and SSH was reporting
  REMOTE HOST IDENTIFICATION HAS CHANGED. It was not an attack - node-2 had been rebuilt
  at some point and the entry was never updated. Both stale entries were removed and the
  verified ED25519 keys added.

  HOW THEY WERE VERIFIED, because this is the useful trick. Do not confirm a host key
  over the same SSH connection you are trying to trust - that proves nothing. Read the
  key off the node's own filesystem through a DIFFERENT channel, here the Kubernetes API:

    kubectl run/apply a busybox Pod with nodeName: <node>, tolerations [{operator:
    Exists}], and hostPath / mounted read-only at /host, whose command is
      cat /host/etc/ssh/ssh_host_ed25519_key.pub
    then kubectl logs it and pipe through ssh-keygen -lf -

  Both nodes' on-disk keys matched what ssh-keyscan saw on the network, which is what
  rules out a man in the middle. The pod is disposable; delete it afterwards.

  KEY AUTH WORKS ON BOTH WORKERS as of 2026-08-24. The control-plane key
    ssh-ed25519 ... slash3b@gmail.com   SHA256:gcgPtrJ4MMYnxpD7pMDxkof6EGOInW/XMJVO+xYPxEQ
  is installed in ~slash3b/.ssh/authorized_keys on .88 and .24, so the control plane is a
  working jump host to every node:
    ssh slash3b@192.168.1.116                       control plane
    ssh -J slash3b@192.168.1.116 slash3b@192.168.1.88   node-1
    ssh -J slash3b@192.168.1.116 slash3b@192.168.1.24   node-2

  The workstation itself still has no host keys or authorized key for the workers; go
  through the control plane, or repeat ssh-copy-id from the workstation if direct access
  is wanted.

  PASSWORD AUTH IS STILL ENABLED on both workers. Now that key auth works it should be
  turned off - set PasswordAuthentication no in /etc/ssh/sshd_config on each worker and
  reload sshd. The account password is in your password manager, not in this file.

There is no Ingress controller and no LoadBalancer. Anything reachable from outside the
cluster is a NodePort; everything else needs kubectl port-forward.


RUNNING SERVICES AND HOW TO REACH THEM
--------------------------------------

Reachable from your LAN:

  Argo CD web UI      http://192.168.1.116:31439      (also :31439 on 192.168.1.88 / .24)
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

No application workloads are running. cineplex was removed on 2026-08-23 (see TEARDOWN below).


NODES - 3 KVM VMs (QEMU i440FX), Debian 13 trixie, kernel 6.12.101
------------------------------------------------------------------

  k8s-ctrl-plane   192.168.1.116   control-plane   6 CPU   7.7Gi RAM
                   tainted node-role.kubernetes.io/control-plane
                   WARNING: / is 84% full (12G of 15G)
  k8s-node-1       192.168.1.88    worker          4 CPU   7.6Gi RAM
                   carries every non-kubeadm pod today
  k8s-node-2       192.168.1.24    worker          4 CPU   5.7Gi RAM
                   only flannel + kube-proxy, effectively idle

Cluster age 426 days (built ~2025-06-23). All three Ready, no disk or memory pressure.


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

  argocd, cert-manager, default, kube-flannel, kube-node-lease, kube-public,
  kube-system, local-path-storage

  Every namespace here holds something. kubernetes-dashboard, logging, monitoring and
  tracing were deleted 2026-08-24 - see CLEANUP. The observability namespaces will come
  back when the new stack is installed, created by Argo CD rather than by hand.


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

  There are now ZERO PersistentVolumeClaims and ZERO PersistentVolumes in the cluster.
  The two orphans (monitoring/storage-tempo-0 bound 5Gi, logging/storage-loki-stack-0
  pending 377 days) were deleted 2026-08-24 - see CLEANUP. The next PVC created will be
  the first real one.


GITOPS
------

  Argo CD has NO Applications left. The only one, cineplex-prod, was deleted 2026-08-23.
  It pointed at github.com/reeldex/cineplex.git, path k8s/base, revision HEAD, into the
  default namespace. Its syncPolicy was empty (manual sync) at the time of deletion.
  Everything else in the cluster is applied by hand.


ARGOCD UPGRADE - v3.0.6 to v3.5.1, done 2026-08-24
--------------------------------------------------

  Argo CD here is installed from raw manifests (non-HA), so an upgrade is just re-applying
  a newer install.yaml. Two rules matter:

  1. DO NOT SKIP MINOR VERSIONS. Argo CD only supports one minor at a time. Going 3.0 -> 3.5
     in a single apply is unsupported. The path taken was:
       v3.0.6 -> v3.1.16 -> v3.2.12 -> v3.3.14 -> v3.4.7 -> v3.5.1

  2. USE SERVER-SIDE APPLY. From 3.3 onward the ApplicationSet CRD is larger than the 262144
     byte limit on the kubectl.kubernetes.io/last-applied-configuration annotation, so plain
     client-side "kubectl apply" fails with:
       metadata.annotations: Too long: may not be more than 262144 bytes
     Server-side apply does not write that annotation, so it sidesteps the limit.

  The command, once per version in order:

    kubectl apply --server-side --force-conflicts -n argocd \
      -f https://raw.githubusercontent.com/argoproj/argo-cd/vX.Y.Z/manifests/install.yaml

    kubectl -n argocd rollout status deploy/argocd-server --timeout=300s
    kubectl -n argocd rollout status sts/argocd-application-controller --timeout=300s
    # then repeat for the next minor

  --force-conflicts takes field ownership away from the previous client-side apply. It
  overwrites fields the manifest defines (affinity, env, probes); fields the manifest does
  NOT define (our accounts.slash3b keys, policy.csv, resource limits, tolerations) are left
  alone. Verified after every step - the slash3b account and RBAC grant survived all five.

  Read the upgrade notes for each hop before doing this again:
    https://github.com/argoproj/argo-cd/tree/master/docs/operator-manual/upgrading
  Nothing in 3.0->3.5 affected this cluster: no Applications existed, no OIDC, no
  ApplicationSets, no UI extensions, no GnuPG signing, no source hydrator.

  Backups taken before the upgrade, on the control plane:
    ~/argocd-upgrade-2026-08-23/argocd-all.yaml         all workloads in the namespace
    ~/argocd-upgrade-2026-08-23/argocd-cm-secret.yaml   every cm and secret
    ~/argocd-upgrade-2026-08-23/argocd-crds.yaml        the argoproj CRDs
    ~/argocd-upgrade-2026-08-23/argocd-apps.yaml        Applications (empty - none exist)

  What went wrong, and it will happen again:
    The v3.5.1 step timed out waiting on rollout. Cause was NOT the upgrade - every node
    briefly went under DiskPressure, which taints all nodes and stalls scheduling:
      FailedScheduling: 0/3 nodes are available: 3 node(s) had untolerated taint(s)
    Five upgrade hops pull a ~210MB image per component per version onto the same node.
    Kubelet garbage-collected old images, pressure cleared, and the pods scheduled on their
    own after ~4.5 minutes. Nothing needed fixing.
    Root cause is the 15G disks sitting at ~83% full. See DISK below - the crictl prune
    originally recorded here does almost nothing on this cluster and the real occupants
    are elsewhere.

  Post-upgrade state:
    argocd-server / repo-server / application-controller /
      applicationset-controller / notifications-controller   quay.io/argoproj/argocd:v3.5.1
    argocd-dex-server                                        ghcr.io/dexidp/dex:v2.45.0
    argocd-redis                          public.ecr.aws/docker/library/redis:8.2.3-alpine
    Both logins verified working after the upgrade.

  THE CLI IS NOW STALE. ~/go/bin/argocd is v3.0.12 against a v3.5.1 server:
    argocd version --short   ->   argocd: v3.0.12   argocd-server: v3.5.1
  It still works, but upgrade it to match:
    curl -sSL -o ~/go/bin/argocd \
      https://github.com/argoproj/argo-cd/releases/download/v3.5.1/argocd-linux-amd64
    chmod +x ~/go/bin/argocd


ARGOCD ACCOUNTS - changed 2026-08-23
------------------------------------

  Argo CD's built-in "admin" account cannot be renamed, so slash3b was added alongside it
  as a local account. Both logins work.

    slash3b            accounts.slash3b = "apiKey, login" in cm/argocd-cm
                       bcrypt hash in secret/argocd-secret key accounts.slash3b.password
                       "g, slash3b, role:admin" in cm/argocd-rbac-cm policy.csv
                       (local accounts have NO permissions without that grant)
                       password: 24 random chars, set 2026-08-23 via the argocd CLI,
                       kept out of this file on purpose - infra/ has a GitHub remote
    admin              built-in, password never rotated since 2025-07-31, left enabled
                       as a fallback, still readable from argocd-initial-admin-secret

  History: this account was first created with the password "root". That is 4 characters and
  argocd account update-password enforces an 8-character minimum, so it had to be written as
  a raw bcrypt hash straight into argocd-secret, skipping the policy check. It was rotated
  out the same day via the CLI, so the policy now genuinely holds. Do not re-set passwords by
  patching argocd-secret directly - it makes the password policy a no-op.

  To rotate slash3b again:
    argocd account update-password --account slash3b --new-password <8+ chars>

  To disable the admin account once you trust the slash3b login:
    kubectl -n argocd patch cm argocd-cm --type merge -p '{"data":{"admin.enabled":"false"}}'
    kubectl -n argocd rollout restart deployment argocd-server

  policy.default in argocd-rbac-cm is deliberately left empty (deny), same as before.

  Config backed up before the change, on the control plane:
    ~/argocd-cm-backup-2026-08-23.yaml
    ~/argocd-rbac-cm-backup-2026-08-23.yaml
    ~/argocd-secret-backup-2026-08-23.yaml


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


DISK - MEASURED AND RECLAIMED 2026-08-24
----------------------------------------

  RECLAIMED ON THE CONTROL PLANE 2026-08-24. 83% -> 50%, free space 2.5G -> 7.1G.
  What was deleted, all of it regenerable or already-spent:

    go clean -modcache                    2.4G   Go module cache under
                                                 /home/slash3b/go-pkgs/pkg/mod
    rm -rf /etc/kubernetes/tmp/kubeadm-*  1.2G   kubeadm upgrade backups, see below
    apt-get clean                         1.1G   apt archives

  No backup taken and none needed - the module cache re-downloads on demand, the apt
  archives re-download on demand, and the kubeadm backups are rollback material for an
  upgrade that completed and was verified on 2026-08-24.
  Verified after: all three nodes Ready, /etc/kubernetes/{pki,manifests} and every
  kubeconfig intact.

  GOTCHA THAT COST TIME. /etc/kubernetes/tmp is mode 0700 root. A glob like
    sudo rm -rf /etc/kubernetes/tmp/kubeadm-*
  is expanded by YOUR shell, not by sudo, so as a normal user it cannot read the
  directory, the glob does not expand, and rm silently receives a literal string and
  deletes nothing. It reports success. Use instead:
    sudo bash -c 'rm -rf /etc/kubernetes/tmp/kubeadm-*'

  STATE AFTER, via the kubelet summary API
  (kubectl get --raw /api/v1/nodes/<node>/proxy/stats/summary):

    node             nodefs used   free    images
    k8s-ctrl-plane   47%           7.5G    0.6G
    k8s-node-1       47%           7.5G    3.1G
    k8s-node-2       41%           8.6G    2.7G

  THE 83% WAS CONTROL-PLANE ONLY. The workers were never the problem, and understanding
  why is the useful part: during the Argo CD upgrade all three nodes went under
  DiskPressure, but the workers' pressure was caused by IMAGES, so kubelet's image
  garbage collector reclaimed them automatically and the workers self-healed. The
  control plane's usage was a Go module cache, apt archives and kubeadm leftovers -
  none of which kubelet can touch - so it stayed at 83% for a year.

  Rule of thumb from this: image-driven disk pressure fixes itself and crictl prune only
  ever hurries it along. Disk pressure from anything else never fixes itself. Check what
  the disk is actually holding before reaching for crictl.

  crictl prune remains the right tool for the WORKERS later, once per-commit image tags
  from a multi-service deployment accumulate - they hold 3.1G and 2.7G of images now,
  against 0.6G on the control plane. There is no urgency at 47% and 41%.

  ORIGINAL MEASUREMENT, before reclamation, kept because it is what the breakdown looked
  like and the pattern is worth remembering:

  IT WAS NOT CONTAINER IMAGES. Measured on the control plane:
    sudo crictl images     ->  9 images, 710M total, every one in use
    /var/lib/containerd    ->  710M
  So "sudo crictl rmi --prune", which this file used to recommend as the fix, frees
  approximately nothing today. Keep it in mind for later, when per-commit image tags
  from a multi-service deployment have actually piled up - it is the right tool, just
  for a problem this cluster does not have yet.

  What is actually using the disk, control plane, biggest first:

    2.7G  /home/slash3b/go-pkgs      Go module cache. There is a Go toolchain building
                                     on the control plane. Reclaim: go clean -modcache
    2.8G  /usr                       the OS. Legitimate.
    1.2G  /etc/kubernetes/tmp        THREE kubeadm etcd + manifest backups from the
                                     2026-08-23 upgrade session:
                                       kubeadm-backup-etcd-2026-08-23-19-08-37
                                       kubeadm-backup-etcd-2026-08-23-19-16-48
                                       kubeadm-backup-etcd-2026-08-23-19-25-03
                                     plus matching manifest and kubelet-config dirs.
                                     kubeadm writes these on every upgrade and NEVER
                                     cleans them up. The upgrade is verified and these
                                     are rollback material for an upgrade that already
                                     succeeded. Safe to delete.
    1.1G  /var/cache/apt             Reclaim: sudo apt-get clean
    710M  /var/lib/containerd        images, see above
    488M  /home/slash3b/go           GOPATH
    397M  /var/lib/etcd              the database. Legitimate.
    115M  journals                   Reclaim: sudo journalctl --vacuum-size=50M

  Roughly 5G was reclaimable in three commands without touching anything the cluster
  needs, and was reclaimed. Note the pattern: on this cluster disk pressure came from a
  developer home directory and from kubeadm's own leftovers, not from Kubernetes
  workloads. There is a Go toolchain building on the control plane, which is why the
  module cache is there at all - worth deciding whether that should be the case.

  MEMORY IS NOT UNIFORM, which matters for placing anything stateful:
    k8s-ctrl-plane   8039156Ki  (~7.7Gi)
    k8s-node-1       7943448Ki  (~7.6Gi)
    k8s-node-2       5928220Ki  (~5.7Gi)   <- notably smaller
  Anything wanting page cache (a database, a Kafka broker) should avoid node-2, and
  local-path storage means that placement is permanent once a PVC binds.


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
