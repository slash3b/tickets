VIRTUALIZATION - PROXMOX, AND DISK
==================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


VIRTUALIZATION - PROXMOX
------------------------

  HYPERVISOR   proxmox    192.168.1.12    web UI https://192.168.1.12:8006
               pve-manager 9.2.11, kernel 7.0.14-12-pve
               45Gi RAM, 16 cores
               root SSH by key from Q2, verified 2026-09-20; not checked from Xtal.
               Reachable as `pve` via Q2's ~/.ssh/config. Found by its VMs' MAC prefix
               bc:24:11 (Proxmox OUI) plus port 8006 open.

  NETWORKING

    vmbr0   192.168.1.12/24, gateway 192.168.1.1, bridge-ports enp2s0. Carries the
            physical NIC and the taps of all three k8s VMs. Since 2026-09-20 it is the
            ONLY bridge on the host.

    TAILSCALE runs here as the subnet router for the whole LAN. See OFF-LAN ACCESS under
    ACCESS for what it does and why it is on this host rather than the control plane.

    VMBR1 DELETED 2026-09-20 - a one-way change. It held 192.168.1.13/24 with
    bridge-ports none: no physical port, no VM attached, commented "#k8s dedicated
    network", and it had received 0 bytes in 0 packets across the entire uptime. Nobody
    remembered what it was for.

    It was inert but NOT harmless, which is the part worth remembering. It put a SECOND
    connected route for 192.168.1.0/24 into the table at equal metric:

      192.168.1.0/24 dev vmbr0 proto kernel scope link src 192.168.1.12
      192.168.1.0/24 dev vmbr1 proto kernel scope link src 192.168.1.13

    With equal metrics the winner is decided by interface ordering. vmbr0 happened to
    win, so nothing ever broke - but a reboot or an ifupdown reorder could have flipped
    it, and every packet the subnet router forwards would then have gone into a bridge
    with no members and vanished. On a host that power-cycles weekly that is a trap.

    Removed at runtime first with `ifdown vmbr1` plus `ip link del vmbr1`, connectivity
    re-checked, and only then the stanza deleted from /etc/network/interfaces.
    Deliberately NOT `ifreload -a`, so vmbr0 was never bounced and the SSH session could
    not be lost. Backup of the old file: /root/interfaces.bak-2026-09-20-vmbr1.

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
