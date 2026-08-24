# tickets

Monorepo (`git@github.com:slash3b/tickets.git`). A ticket-selling platform built to be
operated for real on the homelab cluster — see `DESIGN.md` for the system and `PLAN.md`
for what gets installed and in what order.

Everything lives in this one repo: `cineplex`, `infra`, `tpl`. The `.github/` directory
is a separate repo (the `reeldex` GitHub org profile, an earlier name for this project)
and is gitignored here.

## infra/CLUSTER.md is the state file — keep it current

`infra/CLUSTER.md` is the written record of the homelab Kubernetes cluster
(control plane `ssh slash3b@192.168.1.116`). It is plain text on purpose — no markdown
tables, no bold, it gets read in a terminal.

**Any change to the cluster gets reflected in that file in the same session that makes the
change.** That means:

- installing, upgrading, or removing anything (Helm release, raw manifests, operator, CRD)
- adding or deleting a Deployment, Service, namespace, PVC, StorageClass
- adding, changing, or removing an Argo CD Application
- accounts, RBAC, or credential changes
- node changes, kubeadm upgrades, networking or storage changes

Why: nothing else records this. There is no Terraform, no Helm release for most of what
runs, and `scripts/install.sh` is stale. If CLUSTER.md is wrong, the only source of truth
is a live cluster you have to interrogate by hand — which is how it got out of sync before.

How to write it:

- Record what is running AND why it is needed, not just a resource dump.
- When something is installed but unused or inert, say so explicitly — that is the most
  valuable line in the file.
- Log destructive and one-way changes with the date, what was deleted, and where the backup
  went (see the TEARDOWN and ARGOCD UPGRADE sections for the shape).
- Note the gotcha that cost time, so the next upgrade does not rediscover it.
- Do not put passwords in it. `infra/` has a GitHub remote. Record where a credential lives
  and the command to reset it instead.

Update the `Snapshot <date>` line at the top whenever the file changes.
