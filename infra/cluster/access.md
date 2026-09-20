ACCESS - HOW TO REACH THE CLUSTER, FROM THE LAN AND FROM OUTSIDE
================================================================
Part of the homelab cluster record. Index and change log: infra/CLUSTER.md
Keep this current in the same session as any change it describes.


THE TWO CLIENT MACHINES
----------------------

  Two machines drive this cluster. Nothing else does. When writing anything in this
  record, say WHICH ONE - a claim about one is not a claim about the other, and that
  ambiguity already produced a wrong entry on 2026-09-18.

  Q2 - THE LAPTOP. Portable, leaves the house, and the reason the tailscale subnet
  router below exists.

    hostname        Q2   (tailnet name Q2, 100.96.106.128)
    hardware        Lenovo ThinkPad P14s Gen 6 AMD, chassis laptop
    os              Debian GNU/Linux 13 trixie, kernel 6.18.12+deb13-amd64
    on the LAN      192.168.1.213 over wifi, interface wlp194s0
    tailscale       1.102.3, --accept-routes ON since 2026-09-20
    kubectl         client v1.36.0, context `homelab`, set up 2026-09-20
    disk            ext4 on /dev/nvme0n1p2, NOT ENCRYPTED - no LUKS anywhere.
                    This matters: it holds the kubeadm admin credential, which is
                    cluster-admin and cannot be revoked. See the kubeconfig entry below.
    also present    docker0 and a br-* bridge, both DOWN; a minikube-era 192.168.49.1.
                    Unrelated to this cluster, ignore them.

  Xtal - THE TOWER. A desktop that never moves and never leaves the LAN. This is the
  machine older entries in this record call "the workstation" - in particular the
  2026-09-18 entry about kubeconfig and the v1.35.5 kubectl client.

    DELETED FROM THE TAILNET ON 2026-09-20, deliberately, because it never leaves the
    house and so has no use for it. This is a decision, not drift - do not "fix" it by
    re-adding the machine. Re-adding means a fresh auth, because deleting a node
    discards its node key.

    What that costs, so it is a conscious trade: the tailnet now has exactly ONE subnet
    router, and it runs on the same Proxmox host as the cluster. If that host is off -
    which is most weekdays - there is no off-LAN path to anything on the LAN, and no way
    to reach in and wake it. The only other always-on LAN machine in the tailnet is
    Deimos, the Synology at 192.168.1.151, which is up except overnight and could be made
    a second subnet router if that ever matters. It is NOT one today.

    Xtal was powered off on 2026-09-20 and could not be inspected, so the fields below
    are deliberately blank rather than guessed. Fill them in from the machine itself:

    hostname        ? run: hostnamectl --static
    on the LAN      ? run: ip -4 -br addr show
    os / kernel     ? run: hostnamectl | grep -E 'Operating System|Kernel'
    kubectl         v1.35.5 as recorded 2026-09-18, NOT re-verified since.
                    run: kubectl config get-contexts && kubectl version --client
    disk            ? encrypted or not - decide whether the admin kubeconfig on it is
                    an acceptable risk the same way it was decided for Q2.

    Do not assume Xtal is set up just because Q2 is, or the reverse. Check on the
    machine in front of you.


ACCESS
------

  SSH control plane   ssh slash3b@192.168.1.116        (user slash3b, sudo NOPASSWD)
  API server          https://192.168.1.116:6443
  kubeconfig          on the control plane at ~/.kube/config, and since 2026-09-20 also
                      on the laptop at ~/.kube/config, context renamed to `homelab`, so
                      plain `kubectl` works with no KUBECONFIG export. It was MERGED
                      rather than copied over, because an unrelated LFS158 course context
                      `bob-context` was already in the file and is still there:
                        scp slash3b@192.168.1.116:~/.kube/config /tmp/homelab.yaml
                        KUBECONFIG=~/.kube/config:/tmp/homelab.yaml \
                          kubectl config view --flatten > /tmp/merged && \
                          install -m 600 /tmp/merged ~/.kube/config
                        kubectl config rename-context kubernetes-admin@kubernetes homelab
                      This is Q2, the laptop - see THE TWO CLIENT MACHINES above. Until
                      2026-09-20 it had never been set up: ~/.kube/config held only an
                      unrelated LFS158 course leftover with clusters: null, and kubectl
                      could not reach anything. The 2026-09-18 entry describing "the
                      workstation" was about Xtal, the tower, not this machine.
                      It is the kubeadm admin credential - cluster-admin, a client cert
                      valid to 2027-08-23, and kubeadm has no CRL, so it cannot be revoked
                      short of rotating the CA. Treat the file as a root password.
                      IT NOW LIVES ON A PORTABLE LAPTOP WITH AN UNENCRYPTED ROOT
                      FILESYSTEM - ext4 on /dev/nvme0n1p2, no LUKS. That was accepted
                      deliberately on 2026-09-20 to get off-LAN kubectl working. If the
                      laptop is ever lost the only remedy is rotating the cluster CA,
                      because the cert cannot be revoked. A scoped, shorter-lived
                      credential bound to its own RBAC identity is the outstanding fix.
  kubectl versions    Q2 client v1.36.0 against server v1.36.4, inside the supported
                      +/-1 skew. The v1.35.5 recorded on 2026-09-18 is Xtal's client,
                      not Q2's, and has not been re-checked since.
  API server SANs     DNS k8s-ctrl-plane, kubernetes, kubernetes.default,
                      kubernetes.default.svc, kubernetes.default.svc.cluster.local
                      IP  10.96.0.1, 192.168.1.116
                      THERE IS NO TAILNET NAME AND NO PUBLIC IP IN THE CERT. Anything that
                      reaches the API from off-LAN must therefore make 192.168.1.116 itself
                      routable - a subnet router, not a reverse proxy on another address -
                      or the cert has to be reissued with the new name:
                        kubeadm init phase certs apiserver --apiserver-cert-extra-sans=...
                      Read them back with
                        sudo openssl x509 -in /etc/kubernetes/pki/apiserver.crt -noout -text \
                          | grep -A2 'Subject Alternative Name'
  NodePort range      30000-32767 (default)

  OFF-LAN ACCESS - TAILSCALE SUBNET ROUTER, LIVE 2026-09-20

    The Proxmox host 192.168.1.12 - tailnet name proxmox, 100.111.103.70 - advertises
    192.168.1.0/24 to the tailnet. From anywhere, 192.168.1.116:6443 is then reachable at
    its REAL address, so the kubeconfig above and the API server cert both work unchanged.
    Nothing is installed on the cluster and no manifest changed.

      on the hypervisor
        /etc/sysctl.d/99-tailscale-subnet-router.conf    net.ipv4.ip_forward = 1
        tailscale up --advertise-routes=192.168.1.0/24 --accept-dns=false
      on the laptop
        tailscale up --accept-routes

    WHY NOT ON THE CONTROL PLANE. Tailscale was deleted from the control plane after the
    2026-08-27 DNS incident below and stays off. A subnet router anywhere else on the LAN
    gives the same result with none of that risk. It SNATs by default, so traffic reaches
    the nodes sourced from 192.168.1.12 and they never learn tailscale exists - which is
    why no return route and no node-side change is needed.

    WHY A SUBNET ROUTER AND NOT A PROXY. The API server cert carries no tailnet name and
    no public IP in its SANs, so anything reaching it from off-LAN has to make
    192.168.1.116 itself routable. A reverse proxy on a different address cannot work
    without reissuing the cert - see API server SANs above.

    --accept-dns=false is deliberate. It is what stops tailscaled deciding it owns
    /etc/resolv.conf on the hypervisor, the way it did on the control plane on 2026-08-27.

    GOTCHAS, each of which cost time
      The route must be APPROVED in the tailscale admin console, separately from
      authorising the machine. Until it is, every status command looks healthy and
      nothing routes.
      The node had been left in `tailscale down` with WantRunning=false. LoggedOut was
      false, which looks like it only needs starting, but the control server still
      answered machineAuthorized=false and it needed a browser round trip. Read the
      journal for `RegisterReq: got response` rather than trusting prefs.
      Re-registering changed its tailnet IP from 100.111.103.71 to 100.111.103.70 and
      left a stale duplicate `proxmox` machine in the console.
      `tailscale up` REPLACES the entire flag set rather than merging, so anything set
      previously is silently dropped. Read `tailscale debug prefs` before running it.
      TESTING FROM THE LAN PROVES NOTHING - the directly connected route wins over the
      advertised one, correctly. Verify with
        curl --interface tailscale0 https://192.168.1.116:6443/version
      which forces the tailscale path, or tether to a phone.
      The household connection is behind CGNAT - netcheck reports public 95.65.11.83 but
      NAT-PMP hands back a private 192.168.18.2, and two UPnP gateways answer. Relayed
      via DERP Frankfurt at ~44ms. Do not bother with port forwarding.
      The cluster only exists while the Proxmox host is on, which is mostly weekends. The
      router shares that schedule exactly, so it never limits access any further than the
      cluster already does. Waking the host remotely would need a second always-on
      router elsewhere on the LAN plus Wake-on-LAN; not set up.

  WORKER HOST KEYS - ROTATED 2026-09-06, re-verified and re-pinned 2026-09-20.

    k8s-node-1  192.168.1.88   ED25519 SHA256:lmVlYjtGuayjFxJ0VuwXObFpEoEucZC7ErkcfTP0AEY
    k8s-node-2  192.168.1.24   ED25519 SHA256:v8nvXUayLv1Ok4jdB0KyUD/Wste1ICF2+uK4tVzjplk

  The two fingerprints recorded here on 2026-08-24 are DEAD - do not re-add them. Both
  workers regenerated all three host key types at 2026-09-06 17:17 UTC, within 17 seconds
  of each other, which is the signature of an ssh-keygen -A or an openssh-server
  reconfigure rather than an attack. It went unnoticed for two weeks and had been
  breaking SSH from the control plane to BOTH workers with REMOTE HOST IDENTIFICATION HAS
  CHANGED the whole time. Repaired 2026-09-20 by pinning the keys above into the control
  plane's known_hosts; backup at ~/.ssh/known_hosts.bak-2026-09-20 on the control plane.

  HOW THEY WERE VERIFIED IN 2026-09-20 - an easier channel than the pod trick below, and
  the one to reach for first. Read the key off the guest's own disk through the
  HYPERVISOR rather than the network, using the QEMU guest agent, which is already
  enabled on 801 and 803 (agent: 1):

    ssh root@192.168.1.12 qm guest exec 801 -- /bin/cat /etc/ssh/ssh_host_ed25519_key.pub
    ssh root@192.168.1.12 qm guest exec 803 -- /bin/cat /etc/ssh/ssh_host_ed25519_key.pub

  Pipe each through ssh-keygen -lf - and compare against ssh-keyscan. Because it goes
  over virtio-serial and not the LAN, a man in the middle on the network cannot forge it.
  It needs no kubectl and no pod, so unlike the method below it still works when the
  cluster is down. The same channel dated the rotation, via
    qm guest exec <vmid> -- /bin/sh -c 'stat -c "%y %n" /etc/ssh/ssh_host_*_key.pub'

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

  NEITHER CLIENT MACHINE can SSH the workers directly - confirmed for Q2 on 2026-09-20,
  and it was already true of Xtal. Neither has an authorized key there, and Q2 has no
  worker host keys either. Go through the control plane with -J as above, or run
  ssh-copy-id from the machine that wants direct access. The `node1`/`node2` aliases in
  Q2's ~/.ssh/config use ProxyJump through the control plane for exactly this reason.

  WORKSTATION -> CONTROL PLANE, repaired 2026-09-10, and it is what broke `make wipe-*`.
  Every wipe target is `ssh $(CONTROL_PLANE) 'bash -s ...' < scripts/wipe.sh`, so it fails
  at ssh long before the script runs and the error says nothing about wiping.

    1. STALE HOST KEY. The workstation's ~/.ssh/known_hosts line 17 still held the ECDSA
       key from before the reboot that regenerated the control plane's keys, so ssh
       refused with REMOTE HOST IDENTIFICATION HAS CHANGED. Not an attack: the ED25519
       key the host now offers is SHA256:Ro/SHox0uvyks6ziKPi2N3DyDxiRR54UzhPe8AfpXIk,
       the same fingerprint already verified out-of-band through the Proxmox guest agent
       (see CONTROL PLANE DNS). Stale entry removed with `ssh-keygen -R 192.168.1.116`
       and the verified ED25519 key added. ssh keeps the old file as known_hosts.old.

    2. AUTHORIZED KEY - RESOLVED 2026-09-18, the paragraph below is kept for the
       diagnosis but no longer describes the state. `ssh -o BatchMode=yes
       slash3b@192.168.1.116` now succeeds from the workstation, so key auth works and
       nothing here needs a password any more. What follows was true until then.

       The key
         ssh-ed25519 ... slash3b@gmail.com  SHA256:kx9PExloGZnwnOmcXF14oGZo8ddtXRKNxVLyeP2eJWQ
       was offered and rejected - it was not in ~slash3b/.ssh/authorized_keys on .116. The
       control plane's OWN key (gcgPtrJ4...) is the one recorded above as installed on the
       workers; the workstation's was never installed here, or was lost with the rebuild.
       Install it once, which needs the account password that one time:
         ssh-copy-id -i ~/.ssh/id_ed25519.pub slash3b@192.168.1.116

       NOT A BLOCKER THOUGH, and the earlier note here claiming it was is wrong. ssh
       falls through to its PASSWORD PROMPT, and that prompt works even though the wipe
       script occupies stdin, because ssh reads passwords from /dev/tty rather than from
       stdin. `make wipe-all CONFIRM=WIPE` in a real terminal simply asks for the
       password. Only two things genuinely cannot read a password: the script's own
       "type WIPE" prompt (`read` from stdin, which IS the script - hence --yes and the
       CONFIRM= guard in the makefile), and any non-interactive caller with no tty.

       WHAT MISLEADS YOU HERE is `-o BatchMode=yes`, which turns the prompt into a flat
       "Permission denied (publickey,password)" that reads like key auth is required.
       Diagnose with a plain `ssh -v`, not a BatchMode one.

       For a machine with no key and no tty, the makefile takes overrides:
         make wipe-plan SSH_OPTS='-o PubkeyAuthentication=no'   straight to the prompt
         SSHPASS=... make wipe-plan SSH='sshpass -e ssh'        no prompt at all
       sshpass -e reads the SSHPASS environment variable, so the password stays out of
       the repo, out of `ps` and out of this file.

  PASSWORD AUTH IS STILL ENABLED on both workers. Now that key auth works it should be
  turned off - set PasswordAuthentication no in /etc/ssh/sshd_config on each worker and
  reload sshd. The account password is in your password manager, not in this file.

There is no Ingress controller and no LoadBalancer. Anything reachable from outside the
cluster is a NodePort; everything else needs kubectl port-forward.
