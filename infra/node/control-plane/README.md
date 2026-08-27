# control-plane node files

Files that live outside Kubernetes, on k8s-ctrl-plane (192.168.1.116) itself.
They are checked in here because the node is otherwise the only copy.
The reasoning behind them is in `../../CLUSTER.md`, section CONTROL PLANE DNS.

## resolv-guard

Restores `/etc/resolv.conf` if anything replaces the systemd-resolved symlink.
Tailscale did exactly that on 2026-08-27 and broke every containerd image pull
on the node.

The real fix is that `/etc/resolv.conf` points at the resolved *stub*
(`/run/systemd/resolve/stub-resolv.conf`), which makes tailscaled select its
`resolvedManager` and configure DNS over D-Bus instead of owning the file.
This guard only covers the remaining window: mode selection runs at tailscaled
start and reads live state, so if systemd-resolved is down at that moment,
tailscaled falls back to a file-owning mode again.

Install:

    sudo install -m 755 resolv-guard /usr/local/sbin/resolv-guard
    sudo install -m 644 resolv-guard.service resolv-guard.path /etc/systemd/system/
    sudo systemctl daemon-reload
    sudo systemctl enable --now resolv-guard.path

Has it ever fired?

    journalctl -t resolv-guard

Empty means the stub fix has held. Any output means it fell back, and a copy of
whatever took the file is at `/root/resolv.conf.stolen-<timestamp>`.
