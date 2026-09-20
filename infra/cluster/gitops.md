GITOPS - ARGO CD AND THE APP OF APPS
====================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


GITOPS - APP OF APPS, LIVE SINCE 2026-08-24
-------------------------------------------

  Argo CD manages the platform from git@github.com:slash3b/tickets.git (PUBLIC repo, so
  no credentials are configured).

  FIFTEEN Applications, all Synced and Healthy, verified 2026-08-30. This list used to
  name four and end at ingress-nginx; it was never updated as the platform grew. Waves
  are the sync-wave annotations in deploy/argocd/apps, and they are what makes a
  from-scratch rebuild work in one pass - operators land before the CRs they must admit.

    wave  app                  source                                what it is

     -    root                 deploy/argocd/apps                    the app of apps
     0    metallb              chart 0.16.1 + $values                LoadBalancer IPs
     1    platform-manifests   deploy/manifests                      issuers, MetalLB pool
     1    cnpg-operator        chart 0.29.0                          runs Postgres
     1    strimzi-operator     chart 1.2.0                           runs Kafka
     1    metrics-server       chart 3.13.0                          kubectl top, HPA source
     1    vpa                  chart 5.0.0                           vertical pod autoscaler
     2    envoy-gateway        chart v1.5.4 + $values                the edge; replaced
                                                                     ingress-nginx
     3    gateway              deploy/gateway                        Gateway + HTTPRoutes
     4    data                 deploy/data                           pg cluster, Kafka, topics
     4    redis                deploy/redis                          seat-map cache
     4    bank                 deploy/apps/bank                      fake payment processor
     5    signoz               chart 0.138.0 + $values               traces, metrics, logs
     5    tickets              deploy/apps/tickets                   the nine workloads
     6    k8s-infra            chart 0.17.0                          SigNoz host/cluster
                                                                     collectors

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
