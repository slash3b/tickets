HOMELAB KUBERNETES - STATE OF THE CLUSTER
Snapshot 2026-08-29, taken from k8s-ctrl-plane (192.168.1.116).
Changes made 2026-08-23/24: cineplex removed, slash3b account added, Argo CD 3.0.6 -> 3.5.1.
2026-08-24: CLEAN SLATE. All observability leftovers deleted - 2 PVCs, 13 CRDs, 4 empty
namespaces, 3 helm repos, stale images on every node. Disk 83/50/43% -> 47/29/16%, ~30G
free cluster-wide. Worker host keys verified out-of-band and key auth fixed. See CLEANUP,
DISK and ACCESS. No change to anything that was actually running.
2026-08-27: control-plane DNS fixed at the root, not patched again. resolv.conf now
points at the systemd-resolved STUB, which makes tailscaled pick its resolvedManager
and stop wanting the file; resolv-guard.path restores the symlink if anything takes it
anyway. See CONTROL PLANE DNS.


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

  argocd, cert-manager, default, ingress-nginx, kube-flannel, kube-node-lease,
  kube-public, kube-system, local-path-storage, metallb-system

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


GITOPS - APP OF APPS, LIVE SINCE 2026-08-24
-------------------------------------------

  Argo CD manages the platform from git@github.com:slash3b/tickets.git (PUBLIC repo, so
  no credentials are configured). Applications:

    root                 deploy/argocd/apps        the app of apps
    metallb              chart + $values           wave 0
    platform-manifests   deploy/manifests          wave 1
    ingress-nginx        chart + $values           wave 2

  ONLY ONE THING IS APPLIED BY HAND, ever:
    kubectl apply -f deploy/argocd/root.yaml
  Everything else is a child of it. Adding a component means adding a file to
  deploy/argocd/apps/ and pushing.

  All apps have automated sync with prune and selfHeal. VERIFIED 2026-08-24 by deleting
  deploy/ingress-nginx-controller: Argo recreated it in 10 seconds, ready in 30, with a
  new UID.

  MULTI-SOURCE APPLICATIONS are how a helm chart gets values out of this repo: one
  source is the upstream chart, a second is this repo with `ref: values`, and the chart
  source references it as $values/deploy/platform/<name>/values.yaml. Without this the
  values would have to be inlined into the Application, which puts configuration
  somewhere nobody thinks to look.

  IMAGE TAGS ARE PINNED BY CI, AND FOR A WHILE FOUR OF THEM WERE NOT. The build
  workflow rewrites newTag in the kustomization after pushing an image, which is
  what gives Argo a manifest change to act on. It used to look for a kustomization
  at deploy/apps/<service>/, which only exists when a service happens to have one
  named after it - bank and hello do. gateway, workers, simulator and seeder are
  all declared inside deploy/apps/tickets/, so they matched nothing and were
  skipped in silence. They sat at `newTag: latest` from the day they were written.

  THE SYMPTOM IS INVISIBLE, WHICH IS WHY IT LASTED. Argo saw no manifest change,
  so it never rolled anything out, and the app stayed Synced and Healthy while the
  pods ran whatever :latest happened to be when they last started. Fixed
  2026-08-27: the workflow now greps for the image name and pins it wherever it is
  declared. If a service ever shows `newTag: latest` again, it is not deploying.

  HELM WAS DELIBERATELY UNINSTALLED for anything Argo owns. MetalLB and ingress-nginx
  were first installed by helm in 0.3, then `helm uninstall`ed and rebuilt by Argo, so
  there is exactly one owner per resource. `helm list -A` should show ONLY cert-manager.
  If it ever shows more, something was installed by hand and needs adopting.

  KNOWN DRIFT: cert-manager itself is still a helm release (rev 8, from 2025) and is NOT
  managed by Argo. Only its ClusterIssuers are. Adopting the release means dealing with
  CRDs and webhooks, which is not worth doing until there is a reason to upgrade it.

  History: the only previous Application, cineplex-prod, was deleted 2026-08-23. It
  pointed at github.com/reeldex/cineplex.git, path k8s/base, into the default namespace.


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


CONTROL PLANE DNS - TAKEN BY TAILSCALE, FIXED FOR GOOD 2026-08-27
------------------------------------------------------------------

  SYMPTOM: pods scheduled on k8s-ctrl-plane could not pull images.
    Failed to pull ... lookup ghcr.io on [fd7a:115c:a1e0::53]:53: server misbehaving
  fd7a:115c:a1e0::/48 is Tailscale's range - that is MagicDNS failing on public names.

  FIRST READ: tailscale had REPLACED /etc/resolv.conf, a systemd-resolved symlink, with a
  static file pointing only at its own resolvers. Both workers were unaffected; only
  the control plane runs tailscaled.

  ROOT CAUSE, found later the same day, and this is the part that matters: replacing the
  symlink was the symptom, not the trigger. tailscaled logs its DNS mode decision at
  every start, and it read:
    dns: [resolved-ping=yes rc=resolved resolved=not-in-use ret=direct]
  resolv.conf pointed at /run/systemd/resolve/resolv.conf - the UPLINK file, which lists
  1.1.1.1 directly. tailscale checks whether resolved is really in the query path by
  looking for 127.0.0.53. Not finding it, it concluded resolved was installed but
  bypassed (resolved=not-in-use) and fell back to a manager that owns /etc/resolv.conf as
  a regular file: "direct" on 08-24, "openresolv" from 08-25 on - the latter also failing
  with health(warnable=dns-read-os-config-failed): exit status 1.
  So restoring the symlink fixed nothing structural. tailscale re-picked a file-owning
  mode at EVERY start; the trap was re-armed on every boot, upgrade and re-auth.

  WHY IT APPEARED WHEN IT DID: it was latent for months. The control plane was tainted,
  so no application pod ever ran there and it never had to pull an application image.
  UNTAINTING IT on 2026-08-24 made it a scheduling target for the first time, and the
  very next deploy landed there and failed. Removing a taint does not only add capacity -
  it starts exercising code paths on that node that were never exercised before.

  THE FIX, two layers.

  Layer 1, prevention - point resolv.conf at the resolved STUB, not the uplink file:
    sudo ln -sfn /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
    sudo systemctl restart tailscaled
  The decision trace then reads:
    dns: [resolved-ping=yes rc=resolved resolved=file nm=no resolv-conf-mode=stub ret=systemd-resolved]
    dns: using *dns.resolvedManager
  In that mode tailscale configures DNS over D-Bus against the tailscale0 link and has no
  reason to open /etc/resolv.conf at all. The failure mode is gone by construction rather
  than by preference - which is the difference between this and accept-dns=false, a
  setting that any re-auth can flip back.
  COST, accepted deliberately: the node now resolves through 127.0.0.53, so it depends on
  systemd-resolved being up. Before, if resolved died the last-written file still said
  1.1.1.1 and resolution kept working. This trades a rare silent failure for a rarer
  obvious one. resolved is enabled, and the stub is Debian's stock arrangement.

  Layer 2, guard - /usr/local/sbin/resolv-guard, fired by resolv-guard.path (enabled, so
  it survives reboot). If /etc/resolv.conf stops being a symlink to the stub it is
  restored, the event is logged under journal tag resolv-guard, and a copy of whatever
  took the file is kept at /root/resolv.conf.stolen-<timestamp> - so the next incident
  arrives with evidence attached instead of needing this investigation again.
  Layer 1 has exactly one hole and this is what covers it: mode selection runs at
  tailscaled start and reads live state, so if resolved happens to be down at that moment
  tailscale sees resolved-ping=no and goes direct again.
  Tested by replacing the symlink with a static file: restored in ~200ms.
    journalctl -t resolv-guard      # has it ever fired?
  Empty output means layer 1 has held. Any output means layer 1 fell back - read the
  saved copy and check why resolved was down.
  The script and both units are checked in at infra/node/control-plane/ - the node was
  otherwise the only copy.

  DO NOT point kubelet at the stub. /var/lib/kubelet/config.yaml keeps
    resolvConf: /run/systemd/resolve/resolv.conf
  the uplink file, because pods cannot reach the node's own 127.0.0.53. This is also why
  the original incident only ever hit containerd image pulls and never pod DNS -
  containerd reads /etc/resolv.conf, kubelet never did.

  MagicDNS stays off here (accept-dns=false) as a second line of defence; the node is
  reached at 192.168.1.116 over the LAN and does not need tailnet short names. The cost
  is log noise in tailscaled - "dns: resolver: forward: no upstream resolvers set,
  returning SERVFAIL", a few dozen an hour. It is benign and is the direct consequence of
  accept-dns=false, not a fault. Layer 1 is what would make turning MagicDNS back on safe
  if it is ever wanted: resolved would route only *.ts.net to tailscale and everything
  else to 1.1.1.1, instead of the old all-or-nothing.
  Backup of the tailscale-written file: /root/resolv.conf.tailscale-2026-08-27.bak

  NOT auto-upgraded: unattended-upgrades covers only origin=Debian and Debian-Security,
  and tailscale ships from its own apt source (/etc/apt/sources.list.d/tailscale.list).
  A tailscale upgrade here is therefore always a deliberate act, never a surprise at
  03:00. Re-check the decision trace after any such upgrade:
    journalctl -u tailscaled | grep 'dns: using' | tail -2

  ROLLBACK, nothing here is one-way and no package was installed or removed:
    sudo ln -sfn /run/systemd/resolve/resolv.conf /etc/resolv.conf
    sudo systemctl disable --now resolv-guard.path
    sudo systemctl restart tailscaled

  SSH HOST KEY also changed around the same reboot. Verified out-of-band through the
  Proxmox guest agent rather than over the connection being trusted:
    qm guest exec 800 -- /bin/cat /etc/ssh/ssh_host_ed25519_key.pub
  matched what the network offered (SHA256:Ro/SHox0uvyks6ziKPi2N3DyDxiRR54UzhPe8AfpXIk),
  so it was a regenerated key, not an interception. This is the second time this trick
  has been needed - the hypervisor and the kubelet are both channels that do not depend
  on the SSH connection in question.


OBSERVABILITY - SIGNOZ, INSTALLED 2026-08-24
--------------------------------------------

  ns signoz, managed by Argo CD. Chart signoz/signoz 0.138.0.
    signoz-0                            query service + UI, https://signoz.tickets.lan
    signoz-otel-collector               OTLP ingest, :4317 grpc / :4318 http
    chi-signoz-clickhouse-cluster-0-0-0 ClickHouse, the single store for all signals
    signoz-zookeeper-0                  ClickHouse coordination
    signoz-clickhouse-operator          manages the ClickHouseInstallation
  postgresql, redpanda and signoz-otel-gateway are DISABLED - nothing needs them and
  the gateway alone requests 2500m/2500Mi.

  ClickHouse and ZooKeeper carry node affinity keeping them OFF the control plane.
  Untainting it removed the only thing separating IO-heavy work from etcd, and
  local-path makes the placement permanent once the PVC binds.

  ENDPOINT FOR WORKLOADS - host:port, NO scheme, the OTLP HTTP exporter rejects one:
    signoz-otel-collector.signoz.svc.cluster.local:4318

  THE SECOND TRAP, and it looked exactly like the first. From 2026-08-24 to
  2026-08-27 SigNoz said "You are not sending traces yet" while logs and metrics
  arrived normally. Nothing was broken. pkg/obs built a TracerProvider, a
  MeterProvider and three OTLP exporters, installed them globally - and NO SERVICE
  EVER STARTED A SPAN OR CREATED AN INSTRUMENT. Only the hello canary did.
  Everything was wired and nothing was instrumented, and from the UI that is
  indistinguishable from a broken collector.

  The logs and metrics that WERE arriving came from the k8s-infra DaemonSet
  scraping /var/log/pods and the kubelet. That is infrastructure telemetry. It
  arrives whether or not a single service is instrumented, so "Logs ingestion is
  active" is not evidence that anything of yours is reporting.

  HOW TO TELL THE DIFFERENCE, without guessing, ask ClickHouse directly:
    kubectl -n signoz exec pod/chi-signoz-clickhouse-cluster-0-0-0 -c clickhouse -- \
      clickhouse-client -q "SELECT serviceName, name, count() \
        FROM signoz_traces.distributed_signoz_index_v3 \
        WHERE timestamp > now() - INTERVAL 15 MINUTE GROUP BY serviceName, name"
  App metrics live in signoz_metrics.distributed_samples_v4, metric_name LIKE
  'tickets%'. If serviceName only ever shows collector components, the services
  are not instrumented - do not go looking at the collector.

  VERIFIED 2026-08-27 in the cluster, a full purchase end to end:
    POST /api/holds -> inventory.Hold -> the conditional UPDATE that claims a seat
    POST /api/orders -> saga.created -> saga.awaiting_payment -> saga.paid ->
      saga.confirmed, with POST bank.bank.svc.cluster.local crossing into the bank
      service's own POST /authorize span - 114ms of a 119ms step was the fake bank
  Metrics flowing: tickets.holds, tickets.orders. tickets.hold.contention exists
  but has no points, which is correct - nothing has deadlocked yet, and an OTel
  counter reports nothing until its first Add.

  OBSERVABILITY AUDIT 2026-08-29, deliberate traffic across every route and error
  class. Two defects found that nothing else would have surfaced:

  1. THE SWEEP HAD GONE SILENT. Its span, its tickets.holds.swept counter and its
     hard-deadline warning were written into store.Sweeper when the loop ran
     inside the workers binary. The split moved the timer to workers and gave the
     work to a gRPC handler, and nothing called Sweeper again. SWEEPS KEPT
     HAPPENING; the telemetry about them stopped. Moved to the RPC that does the
     work now.

     That is the THIRD orphaned instrumentation in this repo - traces, then logs,
     now the sweep - and the shape is identical every time: the code still runs,
     nothing fails, and only missing telemetry says anything is wrong. When a loop
     or a handler MOVES, check what was measuring it moved too.

  2. A LOST RACE WAS COUNTED AS AN OUTAGE. Every layer this project controls is
     careful that losing a race is normal - logged at info, span not marked
     failed. otelgrpc marked it an error anyway, because it treats every non-OK
     gRPC code that way and offers no hook to change it. At 90% lost races during
     an on-sale the Services tab would have shown a 90% error rate on a system
     working perfectly, and an error rate that is always red is worthless on the
     day something is actually broken.

     The stats handler is wrapped on BOTH ENDS. Fixing only the server left half
     of them red, because one call makes a client span and a server span with the
     same name and each decides its own status.

  WHAT THE AUDIT CONFIRMED IS FINE: every request log carries a trace id; 409, 404
  and 400 all log at info; go runtime metrics on all nine services; pgxpool
  metrics on exactly the four that own a database and none on the gateway, which
  has no credentials for one.

  MEASURE AFTER THE ROLLOUT SETTLES. A check run immediately after a deploy caught
  the OLD pod still draining and reported the fix as not working.

  THE SAME TRAP AGAIN, ONE LAYER DOWN - LOGS. Fixed 2026-08-28. pkg/logger builds
  a careful correlation mechanism and the ONLY CALLER IN THE REPO WAS THE HELLO
  CANARY. The gateway served every request in the system and logged two lines,
  both at startup. SigNoz held, for the whole tickets namespace, 31 simulator
  lines and nginx access logs from web, and NOT ONE LOG CARRIED A TRACE ID.

  obs.Route now logs one line per request, inside otelhttp's handler so the
  context already has the span. After: gateway 143 of 147 lines correlated - the
  four without are startup, which is correct.

  CUSTOMER ID IN TRACES AND LOGS, 2026-08-29. X-Customer-Id is read at the gateway
  only, put into OTel baggage there, and picked up by every gRPC server it reaches.
  In SigNoz filter spans on customer.id and logs on customer_id.

    ui-<8 chars>              the browser, kept in localStorage, shown in the page
    sim-<profile>-<8 chars>   one per simulator session

  BAGGAGE IS A HEADER, so the value is sanitised to [A-Za-z0-9._-] and 64 bytes at
  the gateway. An unescaped comma or semicolon would not spoil one span, it would
  break the baggage header for every downstream hop.

  Verified: one purchase as ui-slash3b-demo tagged spans on gateway, catalog,
  inventory, orders and payments, and every log line for it. A 30-buyer burst
  produced 30 distinct customers in 30 distinct traces, each linked back to the
  on-sale burst span rather than nested under it.

  LOG COLLECTION, CORRECTED 2026-08-29. Volume was 22,540 lines/hour and most of
  it said nothing. Three things were wrong:

    Every service's logs were stored TWICE. pkg/logger tees to stdout and OTLP by
    design; the node agent also tails stdout. Measured 8,003 filelog records
    against 2,828 OTLP records in thirty minutes, with catalog, inventory, orders,
    payments, simulator and seeder matching their OTLP counts exactly. Only the
    OTLP copy carries trace_id - it was empty on all 8,003 filelog records.
    Fixed in deploy/platform/k8s-infra/values.yaml: those containers are on the
    logsCollection blacklist. web is NOT, because it is nginx and has no SDK.

    The OTLP half of the logger ignored the log level, so Debug lines shipped to
    the backend while stdout showed none of them. Fixed in pkg/logger.

    argocd-notifications-controller wrote 2,550 lines/hour with
    argocd-notifications-cm EMPTY - nothing configured to notify anything.
    Blacklisted. application-controller and repo-server stay: those are what you
    read when a sync fails.

  After: 10,080 lines/hour, and the only container in tickets still collected
  from stdout is web.

  WHAT THIS COSTS: a panic goes to stderr and the SDK never flushes it, so crash
  output no longer reaches SigNoz. Recover it with
    kubectl -n tickets logs <pod> --previous
  The restart itself is still visible in kubeletMetrics.

  APPLICATION METRICS, as of 2026-08-29. Infrastructure metrics were already
  complete - http.server/client.request.duration, rpc.server/client.call.duration,
  the full pgxpool set on the four services that own a database, go runtime on all
  nine. The business metrics were the gap:

    tickets.holds                by outcome: won, lost, error, exhausted
    tickets.hold.contention      retryable conflicts, the early warning
    tickets.holds.swept          by why: ttl or hard deadline
    tickets.orders               terminal state, plus failed_at naming the step
    tickets.orders.resumed       finished by the resumer, not their own request
    tickets.payments             succeeded, declined, UNKNOWN
    tickets.saga.duration        order lifetime, creation to terminal, by state
    tickets.saga.step.duration   by step and outcome
    tickets.payments.reconciled

  KAFKA METRICS, ADDED 2026-08-30 to feed https://signoz.tickets.lan/messaging-queues/kafka
  which was empty because nothing produced them. Strimzi has no metricsConfig, so
  there is no JMX exporter, and no collector was scraping the brokers.

  The kafkametrics receiver lives in deploy/platform/k8s-infra/values.yaml, in the
  otelDeployment collector - one instance, not the DaemonSet, because these are
  cluster-wide facts and scraping them per node would multiply every series by the
  node count. It goes into the chart's `metrics/scraper` pipeline, which the chart
  declares empty for exactly this and which the presets do not touch.

  Flowing: kafka.brokers, kafka.topic.partitions, kafka.partition.current_offset,
  kafka.partition.oldest_offset, kafka.partition.replicas,
  kafka.partition.replicas_in_sync. About 80 series, and stable.

  THE `consumers` SCRAPER IS OFF ON PURPOSE. Consumer lag is meaningless in this
  system: pkg/events uses a unique group per process (broadcast, not work queue),
  every reader starts at kafka.LastOffset so a healthy consumer's lag is ~0 by
  construction, and every pod restart abandons a group whose lag then grows
  forever, measuring how long ago that pod died. Measured before switching it off:
  29 groups, 72 lag series, from one day of rolling deploys. SigNoz's Consumer Lag
  panel is therefore empty; the partition and topic panels are the ones that
  answer the hot-partition question and they work.

  MESSAGING SPANS, SAME DATE. Publishes and consumes now emit producer and
  consumer spans with the messaging semantic conventions, and W3C trace context
  rides in Kafka headers, so a trace crosses the broker instead of ending at the
  publish. Verified: one trace contains simulator -> gateway -> inventory ->
  `inventory.seat.held publish` -> `gateway:inventory.seat.held process`, with the
  saga and the bank in the same trace.

    VERIFY THEM BY FORCING THE FAILURE, NOT BY READING THE CODE. Two of these were
  wrong on the first deploy and looked fine in a green test suite. Set the bank to
  decline everything, buy a ticket, then check the labels:
    curl -sk --resolve app.tickets.lan:443:192.168.1.240 \
      -X PUT https://app.tickets.lan/admin/bank/config \
      -H 'Content-Type: application/json' -d '{"decline_rate":1.0}'
  PUT IT BACK TO 0.05 AFTERWARDS. It is a live setting on a running system and
  nothing resets it for you.

  SEEDER CRONJOB SUSPENDED 2026-08-29. `kubectl -n tickets get cronjob seeder`
  shows SUSPEND=true. Nothing creates showings on its own any more; they are
  staged from https://app.tickets.lan/admin. The CronJob is kept rather than
  deleted because suspend is one flag to reverse and scripts/wipe.sh --seed reads
  the seeder image out of its spec.

  WIPING THE CLUSTER. Use the make targets, from a workstation with the repo:

    make wipe-plan                     show what would happen, change nothing
    make wipe CONFIRM=WIPE             postgres, the redis projection, the bank
    make wipe-telemetry CONFIRM=WIPE   SigNoz traces, logs and metrics
    make wipe-all CONFIRM=WIPE         both

  What they run underneath, if you want it by hand:

    ssh slash3b@192.168.1.116 'bash -s -- --data --dry-run'   < scripts/wipe.sh
    ssh slash3b@192.168.1.116 'bash -s -- --data --yes'       < scripts/wipe.sh
    ssh slash3b@192.168.1.116 'bash -s -- --all --yes'        < scripts/wipe.sh
    ssh slash3b@192.168.1.116 'bash -s -- --telemetry --yes'  < scripts/wipe.sh

  THE FLAGS GO INSIDE THE QUOTES, AFTER `--`. Written the obvious way round,
  `ssh host 'bash -s' < scripts/wipe.sh --all`, the shell gives --all to ssh
  instead of the script and bash answers "invalid option" with no hint why. The
  header of the script documented the broken form until 2026-08-29.

  --yes IS NOT OPTIONAL HERE, AND THAT IS NOT LAZINESS. The script is piped in as
  stdin, so its "type WIPE" prompt reads that same stdin, gets EOF and aborts. It
  fails closed, but it can never be answered. CONFIRM=WIPE on the make target is
  the guard that actually works.

  --data empties Postgres (venues included), flushes the Redis seat-map
  projection, and restarts the bank, whose charges live in a map rather than a
  table so the restart IS the wipe. --telemetry empties SigNoz. Kafka is not
  purged and does not need to be: consumers start at kafka.LastOffset.

  Verified 2026-08-29: 4 venues / 6 events / 608 seats / 73 orders / 8 Redis keys
  -> all zero, then a cinema showing staged from the operator page opened 96 seats
  and sold one.

  The seeder was worse. It passed nil as the log provider and never called
  obs.Setup at all, and its manifest had no OTLP endpoint, so the one job that
  decides whether there is anything to sell tomorrow reported nothing at all. A
  CronJob is exactly the workload you cannot watch by eye; its pod is gone by the
  time you wonder whether it ran. It also flushes explicitly on exit now, because
  a job that lives two seconds loses nearly everything otherwise.

  VERIFIED 2026-08-28 by taking a bank log line's trace_id and looking it up in
  the traces table: it resolves to a trace rooted at simulator "session group",
  running through the gateway into the bank. Logs and traces are joined by id
  across three services, not by squinting at timestamps.

  THE SERVICES TAB IS BUILT FROM SPANS, which is why it was missing half the
  system until 2026-08-28. workers and seeder run background loops and never
  started a span, and a process that emits no spans is not a service as far as APM
  is concerned - it does not matter how much it is doing. web is nginx and will
  never appear; it has no OTel in it at all, by choice.

  Fixed by giving the three singletons a span per pass - sweep, reconcile, resume
  - and the seeder one for its run. BACKGROUND WORK IS THE LEAST OBSERVABLE THING
  IN THE SYSTEM: nobody waits on it, so nothing complains when it stops. Each span
  carries what it actually did, because a sweeper reclaiming zero holds forever is
  either idle or broken and those look identical from outside.

  PER-SERVICE CPU AND MEMORY, added the same day. The k8s-infra DaemonSet already
  reports k8s.pod.cpu.* and k8s.pod.memory.* - that is the CONTAINER view, keyed by
  pod name, so it does not survive a rollout and cannot separate a memory climb
  caused by a leak from one caused by more work. The Go runtime instrumentation now
  reports the PROCESS view keyed by service.name:
    go.goroutine.count  go.memory.used  go.memory.allocated  go.memory.gc.goal
    go.memory.allocations  go.config.gogc  go.processor.limit
  Baseline 2026-08-28: gateway 19 goroutines / 14.5MiB, workers 18 / 12.6MiB,
  simulator 18 / 11.8MiB, bank 15 / 11.2MiB, hello 12 / 10.2MiB.

  POOL METRICS answer the open pgbouncer question in DESIGN.md with data rather
  than opinion: pgxpool.acquire_duration, pgxpool.empty_acquire_wait_time and
  pgxpool.acquired_connections are the three that matter. At current load gateway
  holds 1 connection and workers 4, so the answer today is plainly "no pgbouncer".

  NOTE THE LABEL KEY IS DOTTED. Filtering these in ClickHouse is
  JSONExtractString(labels,'service.name'), not service_name. Half an hour went
  into that.

  RESOURCE ATTRIBUTES come from OTEL_RESOURCE_ATTRIBUTES in each Deployment, read
  by resource.WithFromEnv. Spans now carry deployment.environment.name=homelab and
  the emitting k8s.pod.name. The manifests build that string with downward-API
  $(VAR) expansion, which ONLY works when the referenced variables are declared
  EARLIER in the same env list - a later definition expands to the literal text.

  THE SETUP TRAP, and it cost real time. SigNoz will not configure its collector
  until an ORGANISATION EXISTS, and an org is only created by registering the first
  user in the UI. Until then:
    - the server logs "cannot create agent without orgId" every 30 seconds
    - the collector logs "Server returned an error response" from its OpAMP client
    - the collector has NO receivers, so nothing listens on 4317/4318
    - every client fails with "connection refused" to a Service that plainly exists
  Nothing about those symptoms points at "you have not signed up yet". Check first:
    curl -k https://signoz.tickets.lan/api/v1/version     -> {"setupCompleted":false}
  The collector's OTLP pipeline is configuration delivered over OpAMP, not static
  config, which is why an empty database disables data ingestion entirely.

  LOG COLLECTION FEEDBACK LOOP - fixed 2026-08-27. ClickHouse logs every query it
  runs. The k8s-infra agent collected those logs and INSERTed them into ClickHouse,
  which logged the insert, which was collected again. Measured at ~20,000 records per
  minute on a COMPLETELY IDLE cluster - 41,912 of 42,000 in a two-minute sample came
  from the clickhouse container - with ClickHouse pinned near its 4Gi limit and burning
  three CPU cores ingesting its own chatter.
  The chart's presets.logsCollection.blacklist.signozLogs is already true and does NOT
  catch it: the ClickHouse pods are created by the clickhouse-operator from a
  ClickHouseInstallation CR, not by the Helm release, so they never carry the labels
  that filter matches on. Excluding the container by name works:
    presets.logsCollection.blacklist.containers: [clickhouse]
  Rate afterwards: ~200-400/minute. Any log store that ingests its own logs will do
  this; check for it whenever one is installed.

  CLICKHOUSE ATE A WHOLE NODE WITH ITS OWN DIAGNOSTICS - fixed 2026-08-27.
  Symptom: node-2 at 97% CPU with ZERO queries running, disk 3% -> 48%.
  Cause: ClickHouse records very detailed telemetry ABOUT ITSELF into system.*_log.
  After three days:
    system.zookeeper_log   16.98 GiB   214,005,617 rows   (every ZooKeeper op)
    system.trace_log        9.83 GiB   470,600,466 rows   (sampling profiler)
    system.text_log         3.06 GiB   101,683,787 rows
    ~31 GiB of self-diagnostics against 5.25 GiB of real telemetry.
  All of it was being continuously merged, which is what consumed the CPU. The log
  feedback loop above inflated zookeeper_log especially, since every insert is
  ZooKeeper traffic.
  Fix: config.d/disable-system-logs.xml in the chart values, remove="1" on each.
  Result: node-2 97% -> 31% CPU, disk 54G -> 8.9G.

  TWO TRAPS WHILE FIXING IT, both worth remembering:

  1. DO NOT write <query_log><ttl>...</ttl></query_log> as a sibling element. The
     chart already declares an <engine> for those system tables, and ClickHouse then
     requires TTL INSIDE the engine definition. A sibling <ttl> is FATAL:
       Code: 36. If 'engine' is specified for system table, TTL parameters should be
       specified directly inside 'engine'
     The server exits 36 and crash-loops. This looked exactly like an OOM crash-loop
     because MEMORY_LIMIT_EXCEEDED errors were also in the log - the exit code is what
     distinguishes them. CHECK THE EXIT CODE BEFORE BELIEVING THE LOUDEST ERROR.

  2. A crash-looping ClickHouse cannot be fixed through Argo CD. The chart renders
     into a ClickHouseInstallation CR, the clickhouse-operator turns that into a
     StatefulSet, and the operator will not finish reconciling while the host is
     unhealthy - so a values change never reaches the pod. Deadlock. Break it by
     editing the live object directly:
       kubectl -n signoz patch sts chi-signoz-clickhouse-cluster-0-0 ...      (limits)
       kubectl -n signoz get cm chi-signoz-clickhouse-common-configd -o json  (config)
     then delete the pod. Push the same change through git afterwards so the two agree.

  MEMORY: raised to 6Gi. ClickHouse sets max_server_memory_usage to ~90% of the
  cgroup limit and a MERGE must fit inside it; at 4Gi it OOMed mid-merge on a large
  backlog and retried forever.

  ARGO CD PERMANENT OutOfSync, fixed with ignoreDifferences in
  deploy/argocd/apps/signoz.yaml. Two server-side defaults the chart never renders:
    volumeClaimTemplates[].apiVersion and .kind   on both StatefulSets
    resourceFieldRef.divisor                      on the operator Deployment
  Argo applied, re-detected the difference, burned five self-heal retries and parked
  at OutOfSync - which destroys OutOfSync as a signal, so real drift becomes
  invisible.
  HOW TO DIAGNOSE THIS CLASS PROPERLY: read Argo's own comparison,
    GET /api/v1/applications/<app>/managed-resources
  and diff normalizedLiveState against predictedLiveState. Diffing `helm template`
  output against the live object points at the WRONG fields, because it skips the
  normalizers Argo applies first. That mistake cost an hour here.


ACCESS LAYER - GATEWAY API, MIGRATED 2026-08-27
------------------------------------------------

  WHY THIS CHANGED. ingress-nginx was RETIRED by the Kubernetes project in March
  2026 - no further releases, no security fixes, ever.
    kubernetes.io/blog/2025/11/11/ingress-nginx-retirement
    kubernetes.io/blog/2026/01/29/ingress-nginx-steering-committee-statement
  This cluster ran v1.15.1, the final release, unpatched. It was replaced the same
  day the retirement was noticed.

  A TRAP WORTH NAMING: the Helm repo still serves ingress-nginx charts and the
  chart metadata carries no deprecated:true flag. Neither means anything. A Helm
  repo keeps serving artifacts after a project dies, and the deprecation flag only
  appears if a maintainer sets it. Do not infer a project's health from its
  registry.

  Ingress was also the wrong API to stay on regardless: it is feature-frozen, which
  is why every non-trivial behaviour in ingress-nginx was an annotation rather than
  a field. Gateway API is the successor.

  Envoy Gateway v1.5.4      ns envoy-gateway-system      Argo app, wave 2
                            Bundles the upstream Gateway API CRDs.
  GatewayClass envoy        parametersRef -> EnvoyProxy tickets-proxy
  Gateway tickets           ns envoy-gateway-system, LISTENS ON 192.168.1.240
                            One wildcard cert for *.tickets.lan covers every host,
                            so adding a service no longer involves a certificate.
                            allowedRoutes.namespaces.from: All, so each HTTPRoute
                            lives beside the service it routes to.
  HTTPRoutes                argocd/argocd, signoz/signoz

  THE FAILURE THAT COST THE MOST TIME. Envoy Gateway defaults its Service to
  externalTrafficPolicy: Local. MetalLB then only announces the address from a node
  holding a ready endpoint - and the envoy pod landed on the CONTROL PLANE, whose
  speaker announces nothing on this cluster (the working address has always been
  announced by node-2). The result was silent in every direction: MetalLB allocated
  the IP, the Service showed an EXTERNAL-IP, the Gateway reported Programmed, the
  endpoints existed, and the address was simply dead on the LAN. No error anywhere.
    DIAGNOSTIC: kubectl get servicel2status -A
    If a LoadBalancer service has no ServiceL2Status, nothing is announcing it.
  Fixed by setting externalTrafficPolicy: Cluster on the EnvoyProxy, matching what
  the old ingress Service used. Client IPs still reach backends via X-Forwarded-For.

  ARGO CD NEEDED server.insecure=true, set in cm/argocd-cmd-params-cm. It was
  answering :80 with a 307 redirect to HTTPS. Gateway API has no equivalent of
  nginx's backend-protocol annotation - upstream TLS is a BackendTLSPolicy needing
  argocd's self-signed CA wired in - and terminating TLS at the Gateway is the
  conventional answer. NOTE this also means the argocd NodePort on :32640 no longer
  serves TLS; use http://192.168.1.116:31439 or the hostname.

  CERT-MANAGER needed config.enableGatewayAPI=true (helm revision 9) to issue certs
  for Gateway listeners instead of Ingress. The ExperimentalGatewayAPISupport
  feature gate is already beta-default-on in v1.19.1, so only the config key was
  needed. It will CRASH on startup if that key is set before the Gateway API CRDs
  exist - install Envoy Gateway first.

  PERMANENT OutOfSync, avoided rather than ignored. Gateway API defaults a lot
  server-side: parentRefs group/kind, backendRefs group/kind/weight,
  certificateRefs group, and rules[].matches (a PathPrefix / match). Omit them and
  Argo diffs forever. These manifests write every default out EXPLICITLY, which
  costs the same lines as an ignoreDifferences rule and documents what each route
  attaches to.

  MetalLB v0.16.1          ns metallb-system      Argo app, wave 0
                           IPAddressPool lan-pool 192.168.1.240-192.168.1.249
                           L2Advertisement lan
                           Pool is OUTSIDE the router's DHCP range (.20-.239), the
                           entire requirement. Do NOT use the router's Address
                           Reservation for it: in L2 mode the node answering ARP,
                           and so the MAC, changes on failover.
                           Use metallb.io/loadBalancerIPs to pin an address; the
                           metallb.universe.tf form still works but logs a
                           deprecation warning on every reconcile.

    PING DOES NOT WORK against a MetalLB L2 address and that is normal. The speaker
    answers ARP but nothing is bound to an interface, so ICMP gets no reply. Test
    with curl. This will waste ten minutes of someone's life otherwise.

  cert-manager v1.19.1     ClusterIssuer selfsigned-bootstrap  used once
                           Certificate cert-manager/tickets-lan-ca  ECDSA, 10 years
                           ClusterIssuer tickets-lan-ca        signs everything
                           Extract the CA to trust it locally:
                             kubectl -n cert-manager get secret tickets-lan-ca \
                               -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt

  DNS - THERE IS NONE. The router serves 1.1.1.1 and the pihole that would have
  done local resolution was deleted. Every machine that wants a cluster hostname
  needs /etc/hosts. All of them, in one line:

    192.168.1.240  app.tickets.lan api.tickets.lan argocd.tickets.lan bank.tickets.lan signoz.tickets.lan sim.tickets.lan

  app.tickets.lan is the seat map - the one a person actually opens.


THE APPLICATION - DEPLOYED 2026-08-27
-------------------------------------

  Redis has its OWN Argo application since 2026-08-29 rather than living inside
  `data` with Postgres and Kafka. It has a different lifecycle from both - no PVC,
  losing it costs latency and no data - and sharing a sync boundary meant the
  safest thing in the data plane could only be touched at the pace of the
  riskiest. Argo adopted the running pod rather than recreating it, so the split
  cost no downtime at all.

  ns tickets, Argo app `tickets`, wave 5. SPLIT INTO NINE SERVICES 2026-08-28;
  before that, catalog/inventory/orders/payments were packages inside gateway and
  workers. They speak gRPC on :9090, generated from proto/tickets/v1, and each
  serves /livez and /readyz over HTTP on :8080 because kubelet probes speak HTTP.
    catalog     what EXISTS             grpc only, no HTTPRoute
    inventory   what is AVAILABLE       grpc only. THE CONTENDED CORE
    orders      the saga                grpc only. calls inventory + payments
    payments    whether money moved     grpc only. the only link to the bank
    gateway     the public API          https://api.tickets.lan
    workers     every singleton         replicas 1, strategy Recreate
    simulator   the load                https://sim.tickets.lan  (/stats, /config)
    seeder      CronJob 03:00 daily     one showing, idempotent
                SEED_DAYS_AHEAD=N to create one on demand, N days out. The daily
                run leaves it unset.

  THE SHOWING USED TO SELL OUT IN NINETY MINUTES, and that was twice mistaken for
  broken instrumentation: with nothing left to sell, holds, orders, payments and
  the bank emit nothing, and an idle half of a system looks exactly like an
  unreported one. Repaced 2026-08-28 to last a full day - by changing the buyer
  MIX to 93% browsers, not by lowering the arrival rate, which would have hit the
  same target by making the system silent between arrivals. See milestone 5 in
  DESIGN.md for the arithmetic.

  If the interesting half of the system looks idle, check whether there is
  anything left to sell before assuming instrumentation broke.

  WIPING ACCUMULATED STATE: scripts/wipe.sh, run ON THE CONTROL PLANE.
    ssh slash3b@192.168.1.116 'bash -s' < scripts/wipe.sh --all --seed
  --data is Postgres, --telemetry is ClickHouse, --all is both, --dry-run prints
  the plan and stops. It pauses the simulator and restores it from a trap, so a
  failure or a Ctrl-C halfway cannot leave the load generator writing into a
  half-truncated database.

  IT KEEPS TWO THINGS THAT LOOK LIKE OMISSIONS. catalog.venues, sections and seats
  describe the cinema, not anything that accumulated - and the seeder looks the
  venue up BY NAME and returns an error if it is gone rather than creating one, so
  truncating venues breaks the 03:00 CronJob permanently and fails at 3am where
  nobody is watching. signoz schema_migrations_v2 is SigNoz's record of which
  migrations it has applied; truncating it clears no data and makes SigNoz believe
  it is a fresh install.

  RUN 2026-08-28: 4 showings, 184 orders, 67k spans and 41M metric rows to zero;
  venue and its 96 seats intact; one fresh showing seeded; simulator restored.
    web         the seat map, React     https://app.tickets.lan

  THE SEAT MAP IS SERVED AS STATIC FILES AND NOTHING ELSE. nginx holds the built
  bundle; it does not proxy. The HTTPRoute for app.tickets.lan has two rules -
  /api goes to the gateway Service, / goes to the bundle - so the browser sees one
  origin and there is no CORS anywhere. Gateway API picks rules by specificity, so
  the longer /api prefix wins over /.

  Do not put a proxy_pass back into that nginx. nginx resolves a literal upstream
  hostname once, when it reads the config, so the web pod would then crash-loop
  any time the gateway Service was missing and would hold a dead ClusterIP if the
  Service were recreated. Routing at the Gateway has neither problem.

  api.tickets.lan still exists and still points at the gateway directly. That is
  what curl and the simulator use, and it means debugging the API never depends on
  the frontend being deployed.
  ns bank, Argo app `bank`
    bank        the adversarial fake    https://bank.tickets.lan  (PUT /config)

  WORKERS MUST STAY AT ONE REPLICA. It runs both inventory sweepers, the payment
  reconciler and the order resumer. Nothing there is unsafe concurrently, but N
  replicas do N times the work on the same rows and multiply traffic to the bank.
  strategy: Recreate is deliberate - a rolling update would briefly run two.

  NODE-2 SAT AT 80%+ CPU FOR DAYS AND IT WAS NOT A SCHEDULING PROBLEM.
  Fixed 2026-08-29. ClickHouse alone was 3106m of node-2's 3335m; everything else
  on that node came to about 230m. Moving pods around would have achieved nothing,
  and ClickHouse cannot move anyway - pinned off the control plane for etcd's
  sake, and bound to node-2 by a local-path PVC.

  IT WAS MERGING ITS OWN DIAGNOSTICS. 2.3 GiB of system log tables against 81 MiB
  of real telemetry, 28 to 1, with every merge in flight belonging to a system
  table. system.text_log alone was 1.54 GiB and the largest table in the database.

  THE FIX THAT WAS ALREADY IN PLACE HAD NEVER WORKED, and the reason is the
  lesson: config.d files load in ALPHABETICAL order and later files override
  earlier ones. Ours was disable-system-logs.xml; the chart ships system_log.xml;
  d sorts before s. For every table that file declares - text_log, error_log,
  latency_log, query_metric_log - the chart silently won. trace_log, which it does
  not declare, had stopped correctly months earlier, which is what made the
  failure so hard to see: the fix half worked.

  Renamed zz-disable-system-logs.xml so it sorts after anything the chart ships.

  REMOVE=1 ONLY STOPS WRITES. Existing parts stay on disk and get merged forever,
  so the tables were also truncated. That is a second, separate step and skipping
  it leaves most of the CPU where it was.

  metric_log went too. It had been kept on the grounds that it and query_log were
  "both small"; once everything louder was off, it was 206 MiB and the only thing
  still merging, seven at a time. query_log stays - it is how you find a slow
  query and the one system table that has ever been useful here.

  RESULT: node-2 83% -> 5% CPU. ClickHouse 3106m -> 140m, 5308Mi -> 3036Mi, and
  547 MiB on disk. Ingestion unaffected: spans and logs still arriving. The three
  nodes now sit at 2%, 2% and 5%.

  KAFKA WENT TO THREE BROKERS 2026-08-29, RF=3, min.insync.replicas=2. Four
  things bit, and none of them are obvious:

  1. THE SYNC DEADLOCKED ON ITS OWN WAVES. KafkaTopic resources had no sync-wave
     annotation, so they defaulted to wave 0 - applied first and then waited on
     for health, while the brokers they need sat in wave 4 behind them. Topics
     could not go healthy because RF=3 wants three brokers; the brokers were never
     applied because Argo was waiting for the topics. Topics are wave 5 now. The
     dependency was always that way round; one broker at RF=1 never exposed it.

  2. STRIMZI CANNOT CHANGE A TOPIC'S REPLICATION FACTOR. The operator says so:
     "Replication factor change not supported". Topics created at RF=1 must be
     deleted and recreated. There is no in-place path without Cruise Control.

  3. DELETING THE KafkaTopic CR DOES NOT DELETE THE KAFKA TOPIC here. The topic
     survived, and the recreated CR simply re-adopted it - still at RF=1, still
     reporting NotSupported. The topic itself has to be deleted with
     kafka-topics.sh --delete before the CR is recreated.

  4. __consumer_offsets WAS LEFT AT RF=1 because Strimzi does not manage internal
     topics. That is the one that matters most and is easiest to miss: losing a
     broker would lose every consumer position, and the gateway would replay or
     skip seat changes on restart. A cluster that looks replicated and is not.
     Fixed by hand with kafka-reassign-partitions across all three brokers.

  TOOL NAMES MOVED IN KAFKA 4.x. kafka.tools.GetOffsetShell is gone; the script is
  bin/kafka-get-offsets.sh. The old invocation fails SILENTLY and reports zero
  messages on topics that are working perfectly, which wastes an hour.

  AND kubectl exec NEEDS -i TO PIPE A FILE IN. Without it the file lands empty and
  the error you get back is about JSON parsing, a long way from the cause.

  THE HOT PARTITION, MEASURED 2026-08-29 during a 3,000-buyer on-sale:
    inventory.seat.held  781 msgs, ALL on partition 0
    orders.created       781 msgs, 261/263/257 across three partitions
  Same cluster, same burst, near-identical volume, opposite distribution - the
  partition key is the whole of the difference. inventory keys by event_id and an
  on-sale is one event; orders keys by order_id and orders are independent.

  BROKER FAILURE, TESTED FOR REAL 2026-08-29. Broker 1 deleted 25 seconds into a
  3,000-buyer on-sale. ISR went 1,2,0 -> 2,0 -> 0,1,2 within about twenty seconds,
  writes never stopped because two in-sync replicas still satisfy
  min.insync.replicas=2, and the burst finished with 292 bought, 2,695 lost races
  and ZERO errors. No oversell. Six seat-change messages were dropped while the
  broker was gone - the async publish trading a stale read model for never
  blocking a sale, working exactly as intended and visible for the first time.

  STAGING A SALE FROM THE BROWSER: POST /api/admin/showings on the gateway, or the
  button on https://app.tickets.lan/admin. It creates the event with an on_sale_at
  a minute or two out and does NOT open the seats - the workers on-sale loop does
  that when the moment arrives, the same path the 03:00 CronJob's showing takes.
  One mechanism starts a sale, and the operator page did not become a second one.

  THE DAILY MOVIE IS UNAFFECTED by any of it. The seeder CronJob still creates
  exactly one cinema showing at 03:00 and knows nothing about the arena.

  REDIS IS NO LONGER INERT EITHER, as of 2026-08-29. It holds the seat-map
  projection, which is exactly the job DESIGN.md gave it and nothing more.

    key      seatstatus:<event_id>, a hash of seat_id -> available|held|sold
    writer   gateway, from the same inventory.seat.* stream that feeds browsers
    reader   gateway, on GET /api/events/{id}/sections/{sid}

  MEASURED, WHICH IS WHY IT WAS WORTH DOING. Before: a seat-map read averaged
  586ms and hit 2918ms at p95, because it read 2,000 rows from
  inventory.event_seats - THE SAME TABLE EVERY SEAT CLAIM CONTENDS ON. Under a
  2,000-buyer on-sale afterwards: median 24ms, p95 44ms, max 59ms. Roughly 25x at
  the median and 66x at p95, and browse traffic no longer touches the contended
  writer's rows at all.

  THE DESIGN'S OWN TEST, RUN FOR REAL: redis-cli FLUSHALL during traffic. The next
  read went from 16ms to 31ms and returned the correct 2,000 seats; the one after
  was warm again. Flushing costs latency and nothing else, which is the property
  the whole arrangement depends on.

  IT IS A CACHE AND NEVER THE TRUTH. A partial hit counts as a miss, every miss
  falls through to inventory, client timeouts are short so a slow Redis degrades
  into a miss rather than a slow request, and an unreachable Redis is a start-up
  warning rather than a refusal to boot. It is deliberately NOT in readiness.

  NEVER PUT HOLD TTLs HERE. Redis key expiry is not a reliable event source, and a
  hold that exists in Redis but not Postgres is a seat sold twice. The sweeper in
  Postgres owns expiry and always will.

  KAFKA IS NO LONGER INERT, as of 2026-08-29. From phase 0.7 until then a broker
  ran doing nothing while DESIGN.md described an event-driven system that was
  entirely synchronous - which is exactly the "installed but unused" state this
  file exists to call out, and did not.

    topics   inventory.seat.held / .released / .sold, as KafkaTopic CRs in
             deploy/data/topics.yaml. One partition each: there is one broker,
             so more partitions buy no parallelism.
    producer inventory, async, after the database commit
    consumer every gateway replica, EACH WITH ITS OWN GROUP ID
    exposed  GET /api/events/{id}/stream, server-sent events

  THE SEAT MAP IS PUSHED NOW. Browsers used to poll a whole section every two
  seconds - a gateway request, a catalog call and an inventory call each time,
  almost always to be told nothing had changed. That cost grew with the SIZE OF
  THE VENUE rather than with how much was happening, which is backwards for an
  on-sale.

  THE SSE ROUTE NEEDS A LONG TIMEOUT. Envoy's default is 15 seconds and would
  sever the stream every fifteen seconds forever - the same default that killed
  the first on-sale burst. deploy/apps/tickets/web.yaml gives /api/events 3600s.

  IT IS ALL OPTIONAL. No KAFKA_BROKERS means the publisher is nil, every publish
  is a no-op, and the frontend falls back to polling. The broker is not something
  these services refuse to start without.

  AUTOSCALING, BOTH KINDS, ADDED 2026-08-29 after the first on-sale OOMKilled the
  gateway and the simulator.

    VPA 1.7.1  ns vpa, Argo app `vpa`, wave 1. Chart fairwinds/vpa 5.0.0.
               Owns MEMORY only. updateMode InPlaceOrRecreate.
    HPA        autoscaling/v2 objects in deploy/apps/tickets/hpa.yaml.
               Owns CPU only.

  THEY MUST OWN DIFFERENT RESOURCES OR THEY OSCILLATE. VPA changes a pod's
  request; HPA scales on utilisation as a percentage OF that request, so pointing
  both at the same resource makes each one's action change the other's input. The
  split is also right for this system: memory is driven by the size of a seat map,
  which is a property of the venue, and CPU by how many people are asking.

  VPA IS USABLE HERE ONLY BECAUSE OF THE KUBERNETES VERSION. 1.36 exposes the
  pods/resize subresource, so VPA 1.7 changes a running pod's resources IN PLACE
  rather than evicting it. Check before assuming on another cluster:
    kubectl get --raw /api/v1 | grep pods/resize

  NEITHER TOUCHES workers, AND THAT IS NOT AN OVERSIGHT. It runs the singletons -
  both sweepers, the reconciler, the resumer. N replicas do N times the work on
  the same rows and multiply traffic to the bank by N. Argo also still enforces
  its replica count, because ignoreDifferences for /spec/replicas is listed BY
  NAME for the six scalable services rather than for all Deployments.

  Without that ignoreDifferences, selfHeal reverts every scale-up within seconds,
  during exactly the burst that needed the capacity.

  VERIFIED 2026-08-29 under 4,000 buyers: gateway 1->6, inventory 1->6, catalog,
  orders and payments 1->4, workers stayed at 1, nothing restarted, no oversell.

  THE GATEWAY HAS NO DATABASE CREDENTIALS. That is deliberate and worth not
  undoing: it cannot reach Postgres at all, so the front door cannot read or write
  a table even by mistake. Only catalog, inventory, orders and payments hold the
  secret, one schema each.

  THE ONE MANUAL STEP: database credentials. CloudNativePG writes secret
  tickets-pg-app into ns data, and Kubernetes secrets do not cross namespaces, so
  it has to be copied into ns tickets:

    kubectl -n data get secret tickets-pg-app -o json \
      | jq '.metadata = {name:"tickets-pg-app", namespace:"tickets"}' \
      | kubectl apply -f -

  IT WILL GO STALE IF THE PASSWORD IS EVER ROTATED. A secret-replicating operator
  would fix this properly; until then, rotating the Postgres password means
  redoing this copy, and the symptom will be gateway and workers failing readiness
  with an auth error.

  SCHEMAS are applied at startup by whichever of gateway/workers/seeder starts
  first, under a Postgres advisory lock so concurrent starts cannot collide. See
  pkg/migrate, which is explicit that it is NOT a migration tool: no versions, no
  ordering, no ALTER. Replace it before a column ever changes under live data.

  THE HEALTH CHECK THAT MATTERS is not a probe. The simulator counts its own
  purchases and the backend counts confirmed orders and sold seats; those come
  from independent systems and must agree:
    curl -k https://sim.tickets.lan/stats
    kubectl -n data exec tickets-pg-1 -c postgres -- psql -U postgres -d tickets \
      -tAc "SELECT count(*) FROM orders.orders WHERE state='confirmed'"
  Divergence means an oversell or a lost order. Verified equal on 2026-08-27.


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


VIRTUALIZATION - PROXMOX
------------------------

  HYPERVISOR   proxmox    192.168.1.12    web UI https://192.168.1.12:8006
               pve-manager 9.2.11, kernel 7.0.14-12-pve
               45Gi RAM, 16 cores
               root SSH by key from the workstation. Found by its VMs' MAC prefix
               bc:24:11 (Proxmox OUI) plus port 8006 open.

  STORAGE
    local       dir       98 GiB total, ~64 GiB free   ISOs, templates, backups
    local-lvm   lvmthin  338 GiB pool                  every VM disk lives here

    LVM-THIN IS THE THING TO BE CAREFUL WITH. It allows allocating more than exists. If
    guests ever actually fill an over-committed pool, the pool exhausts and EVERY VM ON
    THE HOST freezes at once, with a real risk of corruption. Total allocation is
    deliberately kept UNDER the pool size so this cannot happen:

      total allocated across all VM disks   310.00G
      pool size                             337.86 GiB      ~28G of headroom
      actual data in the pool               14.71%  (~50 GiB)

    Do not allocate past the pool size. As of 2026-08-24 there is ~78G of headroom,
    freed by deleting the minikube VM and the pihole LXC. The only remaining slack after
    that is templates 802 and 900 at 15G each - and cloning either one CONSUMES
    allocation rather than freeing it, so leave room if a fourth node is ever wanted.

  VMS - resized 2026-08-24

    id   name             vCPU   RAM       disk
    800  k8s-ctrl-plane      6   12500M     40G
    801  k8s-node-1          4   14500M    120G
    803  k8s-node-2          4   14500M    120G
    802  k8s-node-tpl        -    1024M     15G   stopped, template
    900  debian-template     -    1024M     15G   stopped, template

    Deleted 2026-08-24: VM 101 minikube (30G disk, a 30G snapshot and a 12.5G state
    file) and LXC 100 pihole (2G). Together they freed ~74G of allocation. There is no
    longer a second Kubernetes on this host - minikube was unrelated to this cluster.

    RAM: 41500M allocated to the three running guests against 45Gi on the host, leaving
    ~3.5Gi for Proxmox itself. There is no headroom to run another guest alongside the
    cluster.

  HOW THE RESIZE WAS DONE, and it is easier than expected

    NO DOWNTIME AND NO REBOOT. Proxmox grows a virtio-scsi disk online via QMP, so the
    guests kept running throughout - including the control plane, which means the
    single-member etcd was never at risk. Earlier notes in this file described a
    drain-and-shutdown procedure; that was written for raw libvirt and is wrong for
    Proxmox. Do not shut anything down for a disk grow.

    On the hypervisor, per VM:
      qm resize <vmid> scsi0 +80G          # ALWAYS use +N, never an absolute size
    Then inside the guest, because growing the virtual disk is invisible to it:
      echo 1 | sudo tee /sys/class/block/sda/device/rescan
      sudo growpart /dev/sda 1
      sudo resize2fs /dev/sda1

    Both steps are required and in that order: growpart moves the partition boundary,
    resize2fs grows the ext4 filesystem into it. resize2fs works on a MOUNTED root
    filesystem - it reports "on-line resizing required" and proceeds.

    These are cloud images: sda1 is the large partition, sda14 (3M) and sda15 (124M) are
    small boot partitions AHEAD of it, so growpart /dev/sda 1 is the correct target.

    resize2fs lives in /usr/sbin and is not on a normal user's PATH. Call it via sudo.

    Verified after: all three nodes Ready, zero pods not Running, /healthz ok.

  SCHEDULING - CONTROL PLANE UNTAINTED 2026-08-24

    k8s-ctrl-plane CARRIED the kubeadm default taint
      node-role.kubernetes.io/control-plane:NoSchedule
    which meant only node-1 and node-2 accepted ordinary workloads. That is 29G of worker RAM, or
    ~26G after kubelet reserves, for a planned stack wanting ~25G. Roughly 96% committed
    with no room for a rolling update, while the control plane runs its own components
    in about 2G of its 12.5G and idles the rest.

    ARGUMENT FOR KEEPING IT is etcd. It is brutally sensitive to disk fsync latency - it
    wants p99 under about 10ms for its write-ahead log. Put something IO-heavy beside it
    and fsyncs slow, heartbeats miss, leader elections fail, and the API goes flaky in a
    way that reads like a network fault.

    ARGUMENT AGAINST IT, AND IT IS SPECIFIC TO THIS BOX: all three nodes are guests on
    ONE Proxmox host sharing ONE LVM-thin pool on ONE physical disk. ClickHouse on node-1
    already contends with etcd on the control plane at the physical layer. The taint
    isolates CPU and memory within a guest; it does nothing about disk, because that
    isolation was always partly imaginary here.

    So only the CPU and memory risk is new, and requests and limits handle that.
    DECISION: untaint, and keep ClickHouse, Kafka and Postgres off the control plane by
    node affinity so only stateless things land there.

      remove   kubectl taint nodes k8s-ctrl-plane node-role.kubernetes.io/control-plane-
      restore  kubectl taint nodes k8s-ctrl-plane \
                 node-role.kubernetes.io/control-plane=:NoSchedule

    APPLIED 2026-08-24. All three nodes now schedulable, verified Ready with zero pods
    disturbed and /healthz ok. Allocatable capacity afterwards:

      k8s-ctrl-plane   12364512Ki  (~11.8Gi)   6 cpu
      k8s-node-1       14380308Ki  (~13.7Gi)   4 cpu
      k8s-node-2       14380308Ki  (~13.7Gi)   4 cpu
      -----------------------------------------------
      total            ~39.2Gi                14 cpu     was ~27Gi on two nodes

    THE OBLIGATION THIS CREATES: nothing now stops a scheduler from putting ClickHouse
    or a Kafka broker on the control plane. Every stateful workload MUST carry node
    affinity keeping it on node-1 or node-2. If that discipline slips, the etcd IO
    contention this taint existed to prevent comes back - and it will present as a flaky
    API server, not as a storage problem.

  MISSING: there is no metrics-server. kubectl top and the Metrics API return
  "Metrics API not available", and no HorizontalPodAutoscaler can work without it.


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
