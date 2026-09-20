NETWORKING - CONTROL PLANE DNS AND THE GATEWAY API ACCESS LAYER
===============================================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


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
