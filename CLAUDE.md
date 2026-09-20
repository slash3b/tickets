# tickets

Monorepo (`git@github.com:slash3b/tickets.git`). A ticket-selling platform built to be
operated for real on the homelab cluster — see `DESIGN.md` for the system and `PLAN.md`
for what gets installed and in what order.

Everything lives in this one repo: `services`, `pkg`, `gen`, `proto`, `web`, `deploy`,
`infra`.
`cineplex`, the earlier project this grew out of, was deleted on 2026-08-28 once `pkg/`
had been lifted from it. The `.github/` directory is a separate repo (the `reeldex`
GitHub org profile, an earlier name for this project) and is gitignored here.

## infra/cluster/ is the state file — keep it current

The written record of the homelab Kubernetes cluster
(control plane `ssh slash3b@192.168.1.116`) lives in `infra/cluster/`, with
`infra/CLUSTER.md` as its index and change log. It was one 2047-line file until
2026-09-20; it was split because nobody could find anything in it. It is plain text on
purpose — no markdown tables, no bold, it gets read in a terminal.

Which file gets the change:

- `access.md` — SSH, API server, kubeconfig, cert SANs, off-LAN/tailscale, host keys
- `running.md` — what runs and why, namespaces, how to reach it, how it was built
- `gitops.md` — Argo CD and the app-of-apps
- `network.md` — control plane DNS, Gateway API access layer
- `observability.md` — SignOz
- `application.md` — the tickets platform as deployed
- `data.md` — Postgres / CloudNativePG
- `virtualization.md` — Proxmox host, VMs, bridges, disk
- `history.md` — teardowns, cleanups, known drift

**Any change to the cluster gets reflected in the right file above, in the same session
that makes the change.** That means:

- installing, upgrading, or removing anything (Helm release, raw manifests, operator, CRD)
- adding or deleting a Deployment, Service, namespace, PVC, StorageClass
- adding, changing, or removing an Argo CD Application
- accounts, RBAC, or credential changes
- node changes, kubeadm upgrades, networking or storage changes

Why: nothing else records this. There is no Terraform, no Helm release for most of what
runs, and `scripts/install.sh` is stale. If it is wrong, the only source of truth is a
live cluster you have to interrogate by hand — which is how it got out of sync before.

How to write it:

- Record what is running AND why it is needed, not just a resource dump.
- When something is installed but unused or inert, say so explicitly — that is the most
  valuable line in the file.
- Log destructive and one-way changes with the date, what was deleted, and where the backup
  went (see the TEARDOWN sections in `history.md` for the shape).
- Note the gotcha that cost time, so the next upgrade does not rediscover it.
- **Say WHICH machine.** Two clients drive this cluster: **Q2**, a ThinkPad laptop that
  leaves the house and reaches the cluster off-LAN through the tailscale subnet router,
  and **Xtal**, a tower that never leaves the LAN and was deliberately removed from the
  tailnet on 2026-09-20 — that is a decision, not drift; do not re-add it. Older entries
  say "the workstation", which always means Xtal. A claim about one is not a claim about
  the other — that ambiguity already caused a wrong entry once. Never write "the
  workstation" again; write the name, and verify on the machine in front of you.
- Do not put passwords in it. **This repo is PUBLIC on GitHub** — a deliberate choice, so
  that Argo CD can clone it without credentials, accepted because everything it describes
  is RFC1918 and reachable only from the LAN. That makes the no-secrets rule absolute
  rather than a nicety: record where a credential lives and the command to reset it,
  never the credential.

Update the `Snapshot <date>` line at the top of `infra/CLUSTER.md`, and add a dated line
to its change log, whenever anything under `infra/cluster/` changes.
