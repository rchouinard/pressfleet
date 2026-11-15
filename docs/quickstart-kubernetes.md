# Quick start: Kubernetes

This is the short path from a Kubernetes cluster to a running PressFleet with one WordPress site. It condenses [PressFleet on Kubernetes](kubernetes.md), which has the full requirements, storage details, upgrade and recovery procedures, and a [k3s walkthrough](kubernetes.md#walkthrough-a-small-cluster-on-k3s). For one server and no other reason to use a cluster, the [Compose quick start](quickstart-compose.md) has fewer moving parts. The driver is chosen at install time and can't change later.

> **⚠️ Master key.** `pressfleet init` prints a master key once and writes it into `secrets.yaml`. Every secret and every backup password is encrypted with it. **Without it, a lost cluster means unreadable backups.** Store it, `secrets.yaml`, and `values.yaml` somewhere that is not the cluster, and never commit `secrets.yaml`. See [Master key](installation.md#master-key).

## What the cluster must provide

The control plane runs [doctor](kubernetes.md#doctor-on-kubernetes) at startup and **refuses to start** if a required check fails, so check these first:

| Need | Notes |
| --- | --- |
| Kubernetes **1.35+**, amd64 nodes, cgroup v2 | |
| A CNI that **enforces NetworkPolicy** | Calico, Cilium, or k3s's built-in controller. Flannel alone doesn't; kind's kindnet only does from kind 0.24 |
| **ReadWriteOnce** storage class | Usually the default. Must honor `fsGroup` |
| **ReadWriteMany** storage class | NFS, CephFS, or EFS, for the control plane's data directory. `local-path` and block storage (EBS, Persistent Disk, Azure Disk) can't serve it |
| **metrics-server** | |
| LoadBalancer Services (or NodePorts) | For ports 80, 443, and 2222 |
| **PostgreSQL** you provide | A database only PressFleet uses. SQLite isn't an option here |
| DNS for the dashboard domain and a preview wildcard | Both pointing at Traefik's address, reachable on 80 and 443 for Let's Encrypt |

On your workstation: `kubectl`, **Helm 3.8+**, and the `pressfleet` CLI ([Getting the CLI](installation.md#getting-the-cli)). The CLI only writes files there and never connects to the cluster.

## 1. Prepare a cluster

Pick one:

- **A real cluster**: confirm the table above. Two things are usually missing: ReadWriteMany storage and a PostgreSQL.
- **k3s on one server**: follow the [k3s walkthrough](kubernetes.md#walkthrough-a-small-cluster-on-k3s), steps 1–4. It disables k3s's own Traefik, adds an NFS-backed `nfs` ReadWriteMany class, and installs a small PostgreSQL. Pod CIDR: `10.42.0.0/16`.
- **kind or minikube, on a laptop**: see [Local clusters](#local-clusters-kind-and-minikube) below.

Create a file holding the PostgreSQL connection string (first line only), for example:

```sh
(umask 077; echo 'postgres://pressfleet:<password>@postgres.example:5432/pressfleet?sslmode=require' > dsn.txt)
```

## 2. Generate the install files

`init --target kubernetes` is offline: it asks the usual install questions and writes `values.yaml` (no secrets) and `secrets.yaml` (mode 0600) into an output directory.

```sh
pressfleet init --target kubernetes --output-dir ./pressfleet-kubernetes \
  --dashboard-domain fleet.agency.example \
  --preview-base-domain preview.agency.example \
  --pod-cidrs 10.42.0.0/16 \
  --storage-class-data nfs \
  --postgres-dsn-file ./dsn.txt \
  --acme-email ops@agency.example
```

Leave out the flags to be prompted. Leave `--storage-class-sites` empty to use the cluster's default class. `--public-ip` is only needed when Traefik's address is private behind a public one. Add `--non-interactive --confirm-key-stored` for unattended runs. `--pod-cidrs` is required and must match your cluster: `10.42.0.0/16` on k3s, `10.244.0.0/16` on kind.

**Store the master key** when `init` prints it, and confirm.

## 3. Install

```sh
kubectl create namespace pressfleet
kubectl label namespace pressfleet pod-security.kubernetes.io/enforce=restricted
kubectl apply --server-side -f pressfleet-kubernetes/secrets.yaml
helm install pressfleet oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet \
  --version <release> \
  --namespace pressfleet --values pressfleet-kubernetes/values.yaml \
  --wait --timeout 15m
```

`<release>` is the CLI's version without the `v` (`pressfleet version --client`). `init` prints these exact commands with it filled in. The first start pulls images, so allow several minutes. If the Deployment never becomes ready, read `kubectl -n pressfleet logs deploy/control-plane`; it names the failed doctor check ([Troubleshooting](troubleshooting.md#kubernetes)).

## 4. Point DNS at Traefik

```sh
kubectl -n pressfleet get service traefik
```

Point an `A` record for the dashboard domain and a wildcard `*.<preview base domain>` at the `EXTERNAL-IP`. Traefik gets the dashboard's certificate once the name resolves. Open ports 80, 443, and 2222 to the right audiences.

## 5. Check and create the first admin

```sh
helm test pressfleet --namespace pressfleet --logs
kubectl -n pressfleet exec deploy/control-plane -- \
  pressfleet user create --email you@agency.example --role admin
```

The test fails only when a required check fails; recommended ones (such as DNS not set up yet) show in its report. The second command prints a generated password once. Log in at `https://<dashboard domain>/`.

## 6. Create your first site

Same as Compose: change your password and set up two-factor, then **Client groups** > **New client group**, then **Sites** > **New site** ([First steps](first-steps.md)). Each site becomes a namespace, `pf-<site-id>` (`kubectl get ns`), and is live at `https://<slug>.<preview domain>/`.

## Before real client sites arrive

- [ ] Master key, `secrets.yaml`, and `values.yaml` stored off the cluster.
- [ ] **Your own PostgreSQL backups** (`pg_dump` or snapshots), and one before every upgrade. PressFleet takes none.
- [ ] Backups on **S3** where you can; with `local` they sit on the data volume with the sites ([Backups](backups.md)).
- [ ] Cluster monitoring on node disks, the data volume's storage, and PostgreSQL. PressFleet reports no disk metrics on Kubernetes.
- [ ] The [disaster recovery drill](disaster-recovery.md#rehearsing) rehearsed once.

## Everyday commands

```sh
alias pfk='kubectl -n pressfleet exec deploy/control-plane --'
pfk pressfleet doctor
pfk pressfleet user reset-password --email <email>
pfk pressfleet state backup
helm list -n pressfleet
```

| Task | How |
| --- | --- |
| Upgrade | Back up PostgreSQL, `pfk pressfleet state backup`, then `helm upgrade ... --values pressfleet-kubernetes/values.yaml`. `pressfleet upgrade` refuses to run ([Upgrading](kubernetes.md#upgrading)) |
| Roll back | Scale the control plane to zero, restore the PostgreSQL backup, `helm rollback` |
| Recover a lost cluster | Chart `recovery.*` values ([Recovering](kubernetes.md#recovering-a-lost-cluster)) |

## Local clusters: kind and minikube

PressFleet's docs target real clusters, so there is no supported laptop recipe. What the repository does provide is the kind cluster used by its end-to-end tests, and the same requirements apply to any local cluster. Treat this as a trial, not a hosting setup.

**kind.** `scripts/kind/cluster.sh up` builds the cluster the tests run on: two nodes, Calico, metrics-server, `cloud-provider-kind` for LoadBalancers, an `nfs` ReadWriteMany class, a PostgreSQL, a local image registry, and `podPidsLimit`. It needs Docker and the kind tools from `mise.toml` (`mise trust && mise install`, then `mise exec -- scripts/kind/cluster.sh up`), and the host's `nfsd` module for NFS. Its pod CIDR is `10.244.0.0/16` and its RWX class is `nfs`. `scripts/kind/cluster.sh env` prints how the tests reach it, and `down` removes it. Read the header of [`scripts/kind/cluster.sh`](../scripts/kind/cluster.sh) and [Kubernetes end-to-end tests](testing.md) before relying on it.

**minikube.** Nothing in the repository covers it, so these steps are not tested. From the requirements, you would need: Kubernetes 1.35+ (`--kubernetes-version`), a NetworkPolicy-enforcing CNI (`--cni=calico`), the `metrics-server` addon, a LoadBalancer path (`minikube tunnel`), a ReadWriteMany class (not built in; install an NFS provisioner as in the [k3s walkthrough](kubernetes.md#3-a-readwritemany-class-on-nfs)), a PostgreSQL, and a `--pod-cidrs` that matches the cluster's. Run `helm test` to see which requirement is still unmet.

**DNS and certificates are the hard part locally.** The dashboard and preview domains must resolve to Traefik's address, and Let's Encrypt must reach ports 80 and 443 there to issue certificates. A laptop cluster usually can't do that. The docs don't describe a local-DNS or private-CA setup, so for a trial, use a cluster with a public address and real DNS, or `--acme-staging` to avoid rate limits (browsers then warn about the certificate).

## Where to go next

[PressFleet on Kubernetes](kubernetes.md) · [Chart values](kubernetes.md#chart-values) · [Backups](backups.md) · [Operating](operating.md) · [Security](security.md) · [Troubleshooting](troubleshooting.md#kubernetes)
