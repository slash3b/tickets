HOMELAB KUBERNETES - STATE OF THE CLUSTER
Snapshot 2026-09-20, taken from k8s-ctrl-plane (192.168.1.116) and the laptop.
Changes made 2026-08-23/24: cineplex removed, slash3b account added, Argo CD 3.0.6 -> 3.5.1.
2026-08-24: CLEAN SLATE. All observability leftovers deleted - 2 PVCs, 13 CRDs, 4 empty
namespaces, 3 helm repos, stale images on every node. Disk 83/50/43% -> 47/29/16%, ~30G
free cluster-wide. Worker host keys verified out-of-band and key auth fixed. See CLEANUP,
DISK and ACCESS. No change to anything that was actually running.
2026-08-30: file re-verified against the live cluster after drifting. NODES, NAMESPACES,
the PVC section and the Argo CD app list were all written before the application landed
and were wrong. Three findings worth reading: the control plane is NO LONGER TAINTED and
now runs more pods than either worker; Postgres and all three Kafka brokers are pinned by
local-path volumes to k8s-node-1, making it a single point of failure for the whole data
plane; the platform is 15 Argo Applications, not 4.
2026-09-18: the tower Xtal (written "workstation" below, before the machines had names
in this record) can now drive the cluster directly - kubeconfig copied to
~/.kube/config, context `homelab`, `kubectl get nodes` verified. Two ACCESS claims were
stale and are corrected: SSH key auth from Xtal WORKS, and the kubeconfig is
no longer control-plane-only. The API server cert SANs are now recorded there too,
because they are what constrains every off-LAN access plan. See ACCESS.
2026-09-20: OFF-LAN ACCESS. The Proxmox host is now a tailscale subnet router
advertising 192.168.1.0/24, so kubectl works from outside the house without touching the
cluster or the API server cert. Three things came out of setting it up: the laptop Q2 had
no homelab kubeconfig at all and now has one - the 2026-09-18 entry was about the tower
Xtal, and this record now names the machine everywhere instead of saying
"workstation"; BOTH worker SSH host keys were
silently rotated on 2026-09-06 and had been breaking control-plane-to-worker SSH for two
weeks; and the empty vmbr1 bridge was deleted. See access.md and virtualization.md.
Tailnet tidied the same day: Xtal removed from it for good, and the stale duplicate
`proxmox` node left behind by re-registration deleted. The tailnet is now Q2, proxmox,
Deimos and three phones/tablets, with proxmox the only subnet router.
Also 2026-09-20: this file was split. It had reached 2047 lines and nobody could find
anything in it. The detail now lives in infra/cluster/ and this file is the index.
2026-08-27: control-plane DNS fixed at the root, not patched again. resolv.conf now
points at the systemd-resolved STUB, which makes tailscaled pick its resolvedManager
and stop wanting the file; resolv-guard.path restores the symlink if anything takes it
anyway. See CONTROL PLANE DNS.


WHERE THINGS ARE
----------------

  This file is the index and the change log. The detail lives in infra/cluster/,
  split so you open the 200 lines you need instead of scrolling 2000. Every file
  there carries the same rule: keep it current in the session that changes it.

  The change log above predates the split and points at SECTION names - ACCESS,
  DISK, CLEANUP, TEARDOWN and so on. Those headings all still exist, unchanged,
  now inside the files below. grep -r for the heading if you cannot place it.

  infra/cluster/access.md
      SSH, the API server, kubeconfig on each machine, the API server cert SANs,
      NodePort range, off-LAN access via the tailscale subnet router, and the
      worker SSH host keys with the out-of-band way to verify them.
      Start here when you cannot reach something.

  infra/cluster/running.md
      What is actually running and why each piece is there, the namespaces, how
      to reach each service, and how the cluster was built.

  infra/cluster/gitops.md
      Argo CD and the app-of-apps. What is managed by GitOps and what is not.

  infra/cluster/network.md
      Control plane DNS - taken by tailscale once, fixed for good - and the
      Gateway API access layer.

  infra/cluster/observability.md
      SignOz: what it collects, how to reach it, and what it costs to run.

  infra/cluster/application.md
      The tickets platform itself as deployed on the cluster.

  infra/cluster/data.md
      Postgres via CloudNativePG, and where the data actually lives.

  infra/cluster/virtualization.md
      The Proxmox hypervisor underneath all of it, the VMs, the bridges, and
      disk - including the LVM-thin over-commit trap.

  infra/cluster/history.md
      Teardowns, cleanups and known repo drift. Read when something looks like
      it should be there and is not.


  THE SHORT VERSION, IF YOU READ NOTHING ELSE

    Control plane    ssh slash3b@192.168.1.116        API https://192.168.1.116:6443
    Hypervisor       ssh root@192.168.1.12            web UI https://192.168.1.12:8006
    Clients          Q2, the laptop, which goes out of the house and reaches the cluster
                     over tailscale; and Xtal, the tower, which never leaves the LAN and
                     was deliberately removed from the tailnet on 2026-09-20 because it
                     has no use for it. access.md describes both.
    Everything here is RFC1918 and LAN-only. This repo is PUBLIC on GitHub, so it
    records where a credential lives and how to reset it, never the credential.
    The cluster is three VMs on one Proxmox host. When that host is off, which is
    most weekdays, there is no cluster.
