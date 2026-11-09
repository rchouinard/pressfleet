# PressFleet on Kubernetes

PressFleet runs on a Kubernetes cluster as well as on a single Docker host. This page installs it, upgrades it, and recovers it on a cluster, and ends with a [walkthrough on k3s](#walkthrough-a-small-cluster-on-k3s), a small cluster that one server can run. The design is in the design doc, §4.8.

The orchestrator is chosen at install time (`driver`) and never changes for that install. Moving an existing Compose install to Kubernetes isn't supported. If you run one server and have no other reason for a cluster, use the [Compose install](installation.md): it has fewer moving parts.

## What is different from Compose

The control plane, the dashboard, the API, and the site images are the same. Around them:

| | Docker Compose | Kubernetes |
| --- | --- | --- |
| Install | `pressfleet init` on the host | `pressfleet init --target kubernetes` writes a values file and Secrets, offline; you apply them and install the Helm chart |
| Upgrade | `pressfleet upgrade` | `helm upgrade` ([Upgrading](#upgrading)); `pressfleet upgrade` refuses to run |
| Rollback of an upgrade | Automatic, or `pressfleet upgrade --rollback` | `helm rollback`, plus the PostgreSQL backup you took before the upgrade |
| Recovery of a lost server | `pressfleet recover` | The chart's `recovery` values ([Recovering](#recovering-a-lost-cluster)); `pressfleet recover` refuses to run |
| State database | SQLite (default) or PostgreSQL | PostgreSQL that you provide; the chart doesn't install one |
| State database backups | A copy before each migration (SQLite) | Yours. PressFleet takes none of PostgreSQL |
| Commands that use the state database | `docker compose exec` | `kubectl exec` ([Day to day](#day-to-day)) |
| Sites | A Compose project each | A namespace each (`pf-<site-id>`) with a Deployment, a volume, and NetworkPolicies |
| Isolation | Docker networks | NetworkPolicies, so the cluster's network plugin must enforce them |
| Host metrics | CPU, memory, disk, and disk I/O of the host | CPU and memory per node; **no disk metrics** ([Monitoring](#monitoring-and-backups-that-are-yours)) |
| Version skew rules between the CLI and the control plane | Apply | Don't apply: the chart and the control plane are the same release |
| Metrics API for Grafana (`metrics.public_domain`) | Optional | Not published by the chart |

More differences are in [Behavior that differs from Compose](#behavior-that-differs-from-compose).

## Requirements

Cluster:

- **Kubernetes 1.35 or later.** The chart refuses older clusters (`kubeVersion`), and it uses `ValidatingAdmissionPolicy`. PressFleet supports the three newest Kubernetes minor releases when a release ships.
- **amd64 nodes with cgroup v2.** Doctor checks both on every node.
- **A network plugin that enforces NetworkPolicy**, such as Calico or Cilium, or k3s's own controller. Flannel on its own ignores NetworkPolicy, as did kind's kindnet before kind 0.24; the control plane **refuses to start** on such a cluster, because site isolation would be missing.
- **A ReadWriteOnce storage class** (usually the cluster's default) for site volumes, MariaDB, VictoriaMetrics, and Traefik's certificates.
- **A ReadWriteMany storage class** for the control plane's data directory, from storage that accepts several PersistentVolumes on one volume: NFS, CephFS, or EFS. Block storage (EBS, Persistent Disk, Azure Disk) and `local-path` can't serve it. See [Storage](#storage).
- **metrics-server.** Container and node CPU and memory come from `metrics.k8s.io`.
- **LoadBalancer Services, or NodePorts** with something in front of them, to carry ports 80, 443, and the SFTP port (2222).
- **Nodes that can pull images** from `ghcr.io`. Site images must be public ([Images](#images-and-registries)).

Things you provide:

- **PostgreSQL** for the state database, reachable from the platform namespace, with a database that only PressFleet uses.
- **DNS** for the dashboard domain and a wildcard record for the preview base domain, both pointing at Traefik's address. Let's Encrypt must reach ports 80 and 443 there.
- **Helm** (3.8 or later, for OCI charts), **kubectl**, and the `pressfleet` CLI (see [Getting the CLI](installation.md#getting-the-cli)) on the machine you install from. The CLI only writes files there; it never connects to the cluster.

Capacity: the platform's pods ask for about 1.8 GiB of memory with the defaults (control plane 256 MiB, MariaDB 768, object cache 320, metrics 256, Traefik 192, and a small maintenance page), and each site asks for its reservation. The scheduler decides whether a site fits; doctor warns when the requests exceed the nodes' allocatable memory.

## Install

1. **Prepare PostgreSQL, storage, and DNS** (below).
2. **Run `pressfleet init --target kubernetes`.** It asks the same questions as a [Compose install](installation.md#settings), minus the data directory, the master key file, and the first admin user, plus: the namespace (default `pressfleet`), the cluster's pod network ranges, the two storage classes, and a file holding the PostgreSQL DSN. Everything can be given as flags and `PRESSFLEET_*` variables with `--non-interactive --confirm-key-stored`. See [Installation](installation.md#kubernetes) for what it writes.
3. **Store the master key**, then apply the Secrets and install the chart.

> **⚠️ Master key warning.** `init` generates a **master key**, prints it once, and writes it into `secrets.yaml`. Every secret PressFleet stores is encrypted with it, including the password of every backup repository. **If the cluster is lost and you don't have the key, every backup is unreadable and every secret is gone.** Copy the key into a password manager or other safe place that is *not* in the cluster and not in the backups, and keep `secrets.yaml` and `values.yaml` beside it: `init` stops and asks you to confirm. Don't commit `secrets.yaml` to version control. [Master key](installation.md#master-key) explains how to store and check it.

The commands `init` prints:

```sh
kubectl create namespace pressfleet
kubectl label namespace pressfleet pod-security.kubernetes.io/enforce=restricted
kubectl apply --server-side -f pressfleet-kubernetes/secrets.yaml
helm install pressfleet oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet --version <release> \
  --namespace pressfleet --values pressfleet-kubernetes/values.yaml --wait --timeout 15m
kubectl -n pressfleet get service traefik
helm test pressfleet --namespace pressfleet --logs
kubectl -n pressfleet exec deploy/control-plane -- pressfleet user create --email you@agency.example --role admin
```

`--version` is the release without the `v`, and it is the release of the `pressfleet` CLI that ran `init`. The chart and the control plane image are stamped with the same version when a release is built, so the chart always installs the control plane of its own release. The chart is also a release asset (`pressfleet-<version>.tgz`) for installing from a file, and `helm show values` prints every value ([Chart values](#chart-values)).

What to expect:

- The first start pulls the images, so allow several minutes. `helm install --wait` returns when the pods are ready.
- At startup the control plane runs [doctor](#doctor-on-kubernetes) and refuses to start if a required check fails. Look at its log (`kubectl -n pressfleet logs deploy/control-plane`) if the Deployment never becomes ready.
- `kubectl -n pressfleet get service traefik` shows the external address. **Point the dashboard domain and `*.<preview base domain>` at it.** Traefik gets the dashboard's certificate through HTTP-01 as soon as the name resolves, then the dashboard is at `https://<dashboard domain>/`.
- The last command creates the first admin user and prints the generated password once. Then continue with [First steps](first-steps.md).

Run `init` over the same output directory again only with `--force`; it keeps the master key and generated credentials. Apply the Secrets again, then `helm upgrade --values values.yaml`.

### PostgreSQL

The state database is a PostgreSQL that you run, in or outside the cluster, or a managed service. Give PressFleet a database of its own: disaster recovery replaces every table in it. The control plane creates its tables on first start and migrates them whenever a release needs it. Pass the DSN to `init` in a file (`--postgres-dsn-file`, first line, read once), for example `postgres://pressfleet:<password>@postgres.example:5432/pressfleet?sslmode=require`. It ends up in the Secret `pressfleet-postgres`, key `dsn`, and the control plane gets it as `PRESSFLEET_DATABASE_POSTGRES_DSN`.

The platform namespace has no egress policy, so the control plane reaches PostgreSQL wherever it is. SQLite isn't an option on Kubernetes: the data directory is network storage.

### Storage

Two storage classes, set at `init` (`kubernetes.storage_classes.sites` and `.data`; empty means the cluster's default class):

| Class | Access | Used for | Notes |
| --- | --- | --- | --- |
| `sites` | ReadWriteOnce | Each site's `wp-content` (`kubernetes.site_volume_size`, 10Gi by default); also MariaDB, VictoriaMetrics, and Traefik's certificates, unless their `storage.storageClass` values name another | Must honor `fsGroup`. A site's pod, and the task and SFTP pods that mount its volume, run on one node. Choose a class with `allowVolumeExpansion` if sites will grow, and mind its reclaim policy: deleting a site deletes its namespace and volume, and the volume's data follows the class's policy |
| `data` | ReadWriteMany | The control plane's data directory (`controlPlane.data.size`, 50Gi): transfer directories, uploads, exports, and, with the `local` backup backend, backup repositories | The volume must be writable by group 60000 and by the control plane's UID and GID (60001). Its PersistentVolume should have reclaim policy `Retain` |

The data volume's PersistentVolume must be a CSI or NFS volume, because the copies below can only be made of those. A claim belongs to one namespace, but site tasks (backups, restores, imports, exports, deploys) need the data volume. For each site, the control plane therefore creates a PersistentVolume that points at the same storage (a copy of the data volume's CSI or NFS source, reclaim policy `Retain`) and a claim in the site's namespace. Deleting the site deletes both and never touches the storage. Two admission policies the chart installs narrow what the control plane may do with PersistentVolumes to exactly this ([Kubernetes driver](configuration.md#kubernetes-driver)). This is why the data class must be storage that accepts several PersistentVolumes on one volume, and why the data volume's own PersistentVolume must be `Retain`: if you create it through a class with `reclaimPolicy: Delete`, run `kubectl patch pv <name> -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'` after the install.

`controlPlane.data.existingClaim` uses a claim you made instead of creating `control-plane-data`. The chart keeps `control-plane-data` when you uninstall the release.

Doctor rejects `local-path` and the known block-storage provisioners for the data class. It can't know whether another class really offers ReadWriteMany: that shows when the data volume binds.

**Back up to S3 if you can.** With the `local` backend, backup repositories live on the data volume, so they share a failure domain with the cluster's storage ([Backups](backups.md)).

### DNS, certificates, and ports

Traefik's Service (`traefik` in the platform namespace) carries three ports: 80, 443, and the SFTP gateway's (`sftp.port`, 2222). With `traefik.service.type: LoadBalancer` (the default), the cluster's load balancer implementation gives it an address; on a bare-metal cluster that means something like MetalLB, or k3s's built-in ServiceLB. With `NodePort`, set `traefik.service.nodePorts` and put your own load balancer or forwarding in front.

- **Point the records at the address of the Service**: an A record for the dashboard domain, and a wildcard `*.<preview base domain>`. Doctor warns while they don't resolve to it. If it is a private address in front of a public one, give the public one as `publicIP` (`--public-ip` at `init`), which is where custom domains must point for a site to go live.
- **Client addresses.** `traefik.service.externalTrafficPolicy` is `Local`, which keeps visitors' addresses for login rate limits and the audit log. Check an audit entry after the install: if you see node or pod addresses, your load balancer hides them.
- **Certificates** work as on Compose: HTTP-01 for the dashboard and each preview domain, and DNS-01 for a wildcard preview certificate if you name a DNS provider. The DNS provider's credentials live in the Secret `pressfleet-dns-credentials`, which Traefik reads (`init` writes it from `--dns-credentials-file`). Traefik keeps its certificates on a small volume (`traefik-acme`) and runs as one replica. Losing the volume only means they are issued again.
- **SFTP.** Developers connect to the dashboard domain on `sftp.port`, as on Compose ([SFTP gateway](configuration.md#sftp-gateway)); open it in your firewall or cloud security group. Each session is a short-lived pod in the site's namespace, with no network, on the node of the site's pod. Kubernetes has no per-pod limit on processes, so set the kubelet's `podPidsLimit` (the examples use 1024) to bound a session's. The gateway's SSH host key is in the Secret `pressfleet-platform`, so developers' `known_hosts` entries survive a reinstall that reuses `secrets.yaml`.

### Images and registries

The chart pins the platform images by digest, from the same list as Compose. `images.*` values replace them, for a registry mirror. A private registry for the control plane and platform images needs `kubernetes.imagePullSecrets` (Secrets in the platform namespace), which the chart's pods use and the control plane copies into every site namespace.

Site images are the exception: the control plane asks the registry for their digests **anonymously**, so in this release **site images must be public**, even though nodes pull them with the pull secrets. Mirror them to a public registry or a registry that allows anonymous reads. A registry that only answers plain HTTP, such as a development one in the cluster, is named in `kubernetes.insecure_registries` in the config file.

### Chart values

`values.yaml` from `init` sets only what you answered. Everything else keeps the chart's defaults, in `helm show values oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet --version <release>`, which comments every value. The ones operators touch:

| Value | Meaning |
| --- | --- |
| `dashboardDomain`, `previewBaseDomain`, `publicIP` | The install's domains ([Installation](installation.md#settings)) |
| `kubernetes.podCIDRs` | Required. The cluster's pod network ranges (`kubernetes.pod_cidrs`): `10.42.0.0/16` on k3s, `10.244.0.0/16` on kind |
| `kubernetes.storageClasses.sites`, `.data` | The two classes above |
| `kubernetes.siteNamespacePrefix` | `pf-`. The control plane may only create namespaces with this prefix |
| `kubernetes.imagePullSecrets` | See [Images and registries](#images-and-registries) |
| `controlPlane.memoryMB`, `controlPlane.data.size`, `controlPlane.data.existingClaim` | The control plane's memory limit and data volume |
| `mariadb.enabled`, `mariadb.address`, `mariadb.memoryMB`, `mariadb.storage.*` | `enabled: false` uses an external MariaDB at `address` (`host:port`), reachable from the control plane and every site namespace. Name it by **IP address**: a host name that resolves to a private address is blocked by the sites' egress policy |
| `redis.maxMemoryMB`, `victoriaMetrics.*`, `metrics.retentionDays` | The object cache (Valkey, always in the chart) and metrics storage |
| `traefik.service.type`, `.externalTrafficPolicy`, `.annotations`, `.nodePorts`, `.http3` | How Traefik is exposed. `annotations` is where a cloud load balancer's settings go |
| `acme.*` | Contact address, other CA, staging, DNS provider and its credentials Secret, resolvers, extra root certificates |
| `sftp.port` | The gateway's port on Traefik's Service |
| `config.secretName`, `masterKey.secretName`, `platformSecrets.*`, `database.postgresDSN.*` | Names of the Secrets the chart reads. It never creates them |
| `recovery.*` | [Recovering a lost cluster](#recovering-a-lost-cluster) |

Settings the chart doesn't expose, such as SMTP, S3 backups, alert channels, and the WordPress security set, are in the config file, which is the Secret `pressfleet-config` (key `pressfleet.yaml`). [Configuration](configuration.md) lists them all. The chart passes the settings it shares with the control plane as `PRESSFLEET_*` variables, which win over the file, so change those in `values.yaml`. To change a file setting, edit the Secret and restart:

```sh
kubectl -n pressfleet get secret pressfleet-config -o jsonpath='{.data.pressfleet\.yaml}' | base64 -d > pressfleet.yaml
# edit pressfleet.yaml
kubectl -n pressfleet create secret generic pressfleet-config --from-file=pressfleet.yaml --dry-run=client -o yaml \
  | kubectl apply --server-side -f -
kubectl -n pressfleet rollout restart deployment/control-plane
```

Secret values in that file (the SMTP password, S3 secret key) are encrypted with the master key: put new ones through `pressfleet init --force` rather than writing them by hand ([Changing settings](configuration.md#changing-settings)). Keep your copy of `secrets.yaml` in step.

## Day to day

State commands run in the control plane pod. The control plane's pod runs as UID 60001 and has the `pressfleet` binary on the `PATH`:

```sh
alias pfk='kubectl -n pressfleet exec deploy/control-plane --'
pfk pressfleet user reset-password --email admin@agency.example
pfk pressfleet state backup
pfk pressfleet state backups
pfk pressfleet doctor
pfk pressfleet repository check --help
```

[Commands that run in the control plane container](operating.md#commands-that-run-in-the-control-plane-container) lists them. Add `-i` to `kubectl exec` when you pipe input in (`--password-stdin`).

| Question | Answer |
| --- | --- |
| What runs? | `kubectl -n pressfleet get pods`: `control-plane`, `traefik`, `mariadb-0`, `redis-0`, `victoria-metrics-0`, `maintenance` |
| Control plane or Traefik log | `kubectl -n pressfleet logs deploy/control-plane`, `deploy/traefik`; likewise `statefulset/mariadb`. `log.level` is in the config file |
| A site's objects | `kubectl -n pf-<site-id> get all,pvc,networkpolicy`. Read them; don't edit them. The control plane renders and applies them again, and its rendered copy is in `<data_dir>/sites/<site-id>/kubernetes/` |
| A site's logs | The dashboard, as on Compose. A **stopped** site has no pod, so it has no logs |
| Which release? | `helm list -n pressfleet`. `pressfleet version` on Kubernetes says to ask Helm |
| Is the cluster fit? | `helm test pressfleet -n pressfleet --logs`, `pfk pressfleet doctor`, or the dashboard's **Platform overview** |

Release notices ([Release check](upgrading.md#release-check)) in the dashboard's About dialog, the control plane's log, and `pfk pressfleet version` say to read the release notes and run `helm upgrade`, as in [Upgrading](#upgrading).

### Doctor on Kubernetes

Doctor checks the cluster: the Kubernetes version, amd64 nodes, cgroup v2 (a short-lived pod on each node), the control plane's RBAC, both storage classes, metrics-server, NetworkPolicy enforcement (two pods in a temporary namespace try to reach the API server; one is denied by a policy), that the node and API server addresses sites must not reach can be read, Traefik's address, the DNS records, and cluster memory. The table with what each means and its fix is [Kubernetes checks](installation.md#kubernetes-checks). It runs when the control plane starts (required checks must pass), in `pressfleet doctor` run in the pod (exit codes as on Compose), in the chart's Helm test, and in the dashboard's platform overview, where the two checks that start pods read `UNAVAILABLE`. Run it after an install, an upgrade, or a change to the cluster:

```sh
helm test pressfleet --namespace pressfleet --logs
```

The test starts a pod, `doctor-test`, like the control plane's: its image, settings, Secrets, data volume, and ServiceAccount, so it sees the cluster as the control plane does. It fails only when a required check fails; recommended ones (Traefik's address, DNS records not set up yet) show in the report that `--logs` prints. The pod stays until the next run, so `kubectl -n pressfleet logs doctor-test` shows the last report.

The probe pods run the control plane image, so the nodes must be able to pull it, and the first start waits for that.

### Monitoring and backups that are yours

The control plane collects site metrics itself: uptime, response time, traffic and error rates, container CPU and memory, restarts, certificate expiry, backup age, and `wp-content` size. Host metrics are different on a cluster:

- **CPU and memory are per node**, from metrics-server, with a `node` label. CPU totals are kept in the control plane's memory and start over when it restarts.
- **There are no disk metrics.** The `host_disk_usage` and `host_disk_io` charts are empty, and the built-in **Disk nearly full** rule never fires. **Your cluster monitoring must watch** node disks (images, logs, `local-path` volumes), the capacity of the NFS or other storage behind the data volume, the volumes of MariaDB, VictoriaMetrics, and the sites, and the PostgreSQL server's disk. A full data volume stops backups, imports, and exports.
- **A dead cluster can't alert.** PressFleet's uptime checks run in the cluster, like Compose's on the host. Watch the dashboard domain from outside.
- The chart doesn't publish the metrics API for Grafana (`metrics.public_domain`), and `pressfleet metrics-access` refuses to run.

What must be backed up, and by whom:

| What | Who | How |
| --- | --- | --- |
| **The master key**, `secrets.yaml`, `values.yaml` | You | Off the cluster, as above. Without the key nothing else helps |
| **The PostgreSQL state database** | **You.** PressFleet takes no backup of PostgreSQL | `pg_dump`, snapshots, or point-in-time recovery, **before every upgrade** and on a schedule. PressFleet also writes an encrypted copy of the state every hour into the state repository (`backup.state.interval_hours`), which disaster recovery uses, but it is not a substitute for yours |
| **Backup repositories** (sites' backups and the state repository) | You, if `local` | With S3, the bucket and its own durability. With `local`, they are on the data volume: copy them off the cluster |
| Site files and databases | PressFleet | Site backups, on the schedule you set ([Backups](backups.md)). Volume snapshots of the sites and MariaDB are optional extras; recovery rebuilds sites from their backups |
| Certificates, metrics | Nobody | Reissued and rebuilt |

## Upgrading

Everything in [Upgrading](upgrading.md#kubernetes) applies; this is the order of work. `pressfleet upgrade` and its rollback aren't used: Helm owns both.

1. Read the [release notes](upgrading.md#notes-for-specific-releases) and the GitHub release's notes for the oldest release the new one upgrades from.
2. **Back up PostgreSQL** and take a state backup: `pfk pressfleet state backup`. The control plane migrates the schema when it starts, and a migration can't be undone except by restoring the backup.
3. **Upgrade**, with your values file. The chart has no CRDs to apply first: Traefik reads no Kubernetes API. Prefer `--values` to `--reuse-values`: with the file, new defaults of the chart reach you, and the file is the record of what you run.

   ```sh
   helm upgrade pressfleet oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet --version <new release> \
     --namespace pressfleet --values pressfleet-kubernetes/values.yaml --wait --timeout 15m
   ```

   The Job `upgrade-preflight` runs first and refuses a downgrade, an upgrade from a release older than the new one accepts, and a change of MariaDB's release series. When it refuses, nothing changed: `kubectl -n pressfleet logs job/upgrade-preflight`. The control plane then restarts on the new release; sites keep serving meanwhile.
4. Check with `helm test pressfleet --namespace pressfleet --logs` (or `pfk pressfleet doctor`) and the dashboard. The control plane then updates sites' templates and images one at a time, as on Compose.

To undo an upgrade, scale the control plane to zero, restore the PostgreSQL backup from step 2, then `helm rollback pressfleet --namespace pressfleet --wait` ([Upgrading](upgrading.md#kubernetes)). Anything the control plane recorded after the upgrade is lost with the restore.

Upgrading the cluster itself, or its nodes, is yours. Drain nodes one at a time; sites run one replica and are down while their pod moves, and a site's volume (ReadWriteOnce) follows its storage's rules about nodes.

## Recovering a lost cluster

The master key, `secrets.yaml` and `values.yaml`, and the backup location bring a lost cluster back, with the state restored from the newest state backup in the state repository. The steps are in [Disaster recovery](disaster-recovery.md#kubernetes), in short: build a cluster that meets the [requirements](#requirements), apply the Secrets, install the chart with `--set recovery.enabled=true` (the control plane stays at zero while a Job restores the state into an empty PostgreSQL), then `helm upgrade` with `recovery.enabled=false`, and the control plane recreates every site from its backup.

Which failures need what:

| What was lost | What to do |
| --- | --- |
| A node, with sites on it | Kubernetes reschedules pods, but a site whose volume is `local-path` on that node can't move. Bring the node back, or delete the site's pod and volume and restore the site from a backup |
| The control plane's data volume | Restore its backups directory (or use S3), then recover as above, with `controlPlane.data.existingClaim` if you rebuild the claim |
| PostgreSQL | Restore your own backup if it is recent; otherwise recover from the state backup into an empty database, with `recovery.force=true` only when the database still has rows |
| The whole cluster | Disaster recovery, as above, into a new cluster |
| The master key | Unrecoverable. See the warning at the top |

Rehearse once, as the [drill](disaster-recovery.md#rehearsing) says, before real sites arrive.

## Behavior that differs from Compose

- **Stopped sites have no logs**, because the pod is gone. Compose can read a stopped container.
- **CPU usage totals** are kept in the control plane's memory and restart after it restarts.
- **Site images must be public** ([Images and registries](#images-and-registries)).
- **An external MariaDB must be named by IP address** when it is in a private range.
- **Routes aren't objects of the cluster.** Traefik has no access to the Kubernetes API and no ServiceAccount token. Its own routes (dashboard, SFTP, the maintenance page) are in its ConfigMap; the control plane serves the sites' routes to Traefik's HTTP provider from a port of its own (8081, `kubernetes.traefik_provider_listen`), which only Traefik may reach, and Traefik polls it every 2 seconds, so a new site, domain, or maintenance mode routes within a couple of seconds of its job. IngressRoutes anyone creates in the cluster are ignored. While the control plane is down, Traefik keeps routing what it last read; a Traefik that starts meanwhile routes no site until the control plane is back.
- **A preview site's basic auth** is served to Traefik with the username and the password's bcrypt hash; there is no Secret for it. The provider endpoint is unauthenticated, so those hashes are protected only by the NetworkPolicy that admits Traefik's pod alone to port 8081: keep a CNI that enforces NetworkPolicy (doctor checks it).
- **Traefik's metrics** are on port 9100 of the `traefik-metrics` Service, which only the control plane may reach.
- **Egress from sites** may not reach private, shared (100.64.0.0/10), or link-local addresses, nor the pod ranges, nor any node or API server endpoint address, public ones included: nothing in the cluster or on your network other than DNS, MariaDB, the object cache, and the SMTP relay's port. A site that must call an internal service needs it on a public address, or a change to the driver's policy, which isn't a setting.
- **Namespaces.** A site's namespace is created when it is created and deleted with it. `helm uninstall` removes the platform's objects but not the sites' namespaces, the data volume (kept by `helm.sh/resource-policy: keep`), or PersistentVolumes. Delete the sites in the dashboard first if you mean to remove everything.
- **Don't edit** a site's objects, the platform namespace's policies, or the admission policies by hand.

## Walkthrough: a small cluster on k3s

[k3s](https://k3s.io) is a Kubernetes that one server runs. This walkthrough takes a fresh Ubuntu 24.04 server with 4 vCPUs and 8 GiB of memory (at least 4 GiB; sites need more) to a PressFleet install, and applies to any other single node or small cluster. Everything runs as root on the server, except `init`, which can run anywhere. Names in angle brackets are yours.

k3s brings what PressFleet needs, except two things: it includes Traefik, which you turn off because PressFleet's chart installs its own; and it has no ReadWriteMany storage, which you add (below). What it ships:

| PressFleet needs | k3s provides |
| --- | --- |
| NetworkPolicy enforcement | **Yes.** The default network plugin is flannel, and k3s runs kube-router's network policy controller beside it. Don't start k3s with `--disable-network-policy` |
| metrics-server | **Yes**, bundled |
| LoadBalancer Services | **Yes**, ServiceLB: Traefik's ports are published on the node's own address |
| ReadWriteOnce storage | **Yes**, `local-path` (the default class): a directory on the node. A site's data lives on that node, and is deleted with the site |
| ReadWriteMany storage | **No.** You add NFS |
| Pod network range | `10.42.0.0/16` |

### 1. Ports and DNS

Open ports 80, 443, and 2222 (SFTP) to the internet or your developers, and 6443 only to the places you run `kubectl` and `helm` from. Create the DNS records now, pointing at the server's public IP: `dashboard.agency.example`, and the wildcard `*.preview.agency.example`.

### 2. Install k3s

```sh
mkdir -p /etc/rancher/k3s
cat >/etc/rancher/k3s/config.yaml <<'EOF'
disable:
  - traefik
kubelet-arg:
  - pod-max-pids=1024
EOF
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable sh -
k3s --version
```

`k3s --version` must show Kubernetes 1.35 or later; if the `stable` channel is older, name a release with `INSTALL_K3S_VERSION`. `pod-max-pids` bounds the processes in each pod, which keeps an SFTP session from starving the node.

Install Helm and set up `kubectl` (use the `kubectl` k3s installed, or yours):

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl get nodes
kubectl top nodes     # metrics-server answers after a minute or so
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

To drive the cluster from your own machine, copy `k3s.yaml` there and change `server:` in it to `https://<server ip>:6443`.

### 3. A ReadWriteMany class on NFS

This uses the server itself as the NFS server and the [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) driver to hand out volumes from it, the same pair as the test cluster in `scripts/kind`. On a cluster with several nodes, use a server that every node reaches, and export to the nodes' network.

```sh
apt-get install -y nfs-kernel-server nfs-common
mkdir -p /srv/nfs/pressfleet
echo '/srv/nfs/pressfleet <node ip>/32(rw,sync,no_subtree_check,no_root_squash)' >> /etc/exports
exportfs -ra
```

`no_root_squash` is needed because the driver creates and opens up each volume's directory as root. Export only to your nodes: anyone who can mount the export owns the platform's data. Every node needs `nfs-common` to mount it.

```sh
helm repo add csi-driver-nfs https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts
helm install csi-driver-nfs csi-driver-nfs/csi-driver-nfs --version 4.13.5 \
  --namespace kube-system --set externalSnapshotter.enabled=false
kubectl -n kube-system rollout status deployment/csi-nfs-controller

kubectl apply -f - <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs
provisioner: nfs.csi.k8s.io
parameters:
  server: <node ip>
  share: /srv/nfs/pressfleet
# Retain: the data volume's PersistentVolume must survive deleting its claim.
reclaimPolicy: Retain
volumeBindingMode: Immediate
mountOptions:
  - nfsvers=4.1
EOF
```

The NFS disk is the data volume's capacity (`controlPlane.data.size` is only a request): watch its free space. With one server, it is also the single place holding both sites and local backups, so send backups to S3.

### 4. PostgreSQL

A small PostgreSQL in the cluster, on a `local-path` volume. It is a convenience for a small install; a managed service or a PostgreSQL on another server is better, and either way **its backup is yours** ([Monitoring and backups that are yours](#monitoring-and-backups-that-are-yours)).

```sh
kubectl create namespace postgres
PG_PASSWORD="$(openssl rand -hex 24)"
kubectl -n postgres create secret generic postgres --from-literal=password="$PG_PASSWORD"
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata: {name: postgres, namespace: postgres}
spec:
  selector: {app: postgres}
  ports: [{port: 5432}]
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: postgres, namespace: postgres}
spec:
  serviceName: postgres
  replicas: 1
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - name: postgres
          image: postgres:18-alpine
          env:
            - {name: POSTGRES_USER, value: pressfleet}
            - {name: POSTGRES_DB, value: pressfleet}
            - name: POSTGRES_PASSWORD
              valueFrom: {secretKeyRef: {name: postgres, key: password}}
          ports: [{containerPort: 5432}]
          readinessProbe: {exec: {command: [pg_isready, -U, pressfleet]}, periodSeconds: 5}
          volumeMounts: [{name: data, mountPath: /var/lib/postgresql}]
  volumeClaimTemplates:
    - metadata: {name: data}
      spec:
        accessModes: [ReadWriteOnce]
        resources: {requests: {storage: 10Gi}}
EOF
kubectl -n postgres rollout status statefulset/postgres
(umask 077; echo "postgres://pressfleet:$PG_PASSWORD@postgres.postgres.svc:5432/pressfleet?sslmode=disable" > dsn.txt)
```

Traffic to it stays inside the cluster, so `sslmode=disable` is acceptable here; use TLS for anything that leaves it. A backup, to start with:

```sh
kubectl -n postgres exec postgres-0 -- pg_dump -U pressfleet -Fc pressfleet > pressfleet-$(date +%F).dump
```

Move the dump off the server, and schedule it.

### 5. Install PressFleet

Get the `pressfleet` CLI ([Getting the CLI](installation.md#getting-the-cli)) on the server or your own machine, with `dsn.txt` beside it, and run init. This is the interactive form; the flags in the comment give the same answers:

```sh
pressfleet init --target kubernetes --output-dir ./pressfleet-kubernetes
# --dashboard-domain dashboard.agency.example --preview-base-domain preview.agency.example
# --public-ip <server ip> --pod-cidrs 10.42.0.0/16 --storage-class-data nfs
# --postgres-dsn-file ./dsn.txt --acme-email you@agency.example
```

Leave the sites' storage class empty (`local-path`, the default), and answer the SMTP, backup, and DNS questions as for [Compose](installation.md#interactive-install); for backups prefer `s3`. **Store the master key as `init` asks**, and keep the output directory, which holds it, safe. Then run the commands `init` prints (above): the namespace, its Pod Security label, the Secrets, and `helm install`. If you ran `init` elsewhere, copy the output directory and run them where `kubectl` reaches k3s.

```sh
kubectl -n pressfleet get pods -w          # everything Running
kubectl -n pressfleet get service traefik  # EXTERNAL-IP is the server's address
```

With ServiceLB the external address is the node's. Check it matches the DNS records from step 1; Traefik then gets the dashboard certificate.

### 6. Check and first login

```sh
helm test pressfleet --namespace pressfleet --logs
kubectl -n pressfleet exec deploy/control-plane -- pressfleet user create --email you@agency.example --role admin
```

Everything should pass. If the control plane doesn't start, `kubectl -n pressfleet logs deploy/control-plane` names the failed check; [Troubleshooting](troubleshooting.md#kubernetes) has the usual ones. Log in at `https://dashboard.agency.example/` and follow [First steps](first-steps.md): the first site on a k3s install provisions like on Compose, and shows as `kubectl get ns` entry `pf-<site-id>`.

### 7. Upgrade and recover on k3s

Upgrade as in [Upgrading](#upgrading): dump PostgreSQL, `helm upgrade --values`. For k3s itself, follow its upgrade guide, one minor release at a time, and run doctor afterwards.

To recover a lost server: install k3s as in step 2 and the NFS class as in step 3 (with S3 backups, the NFS export starts empty; with `local` backups, put the saved `backups/` directory back in the export and install with `controlPlane.data.existingClaim` naming a claim on it), recreate PostgreSQL empty (or restore your dump and skip the recovery values), then run the commands of [Disaster recovery](disaster-recovery.md#kubernetes) with your saved `secrets.yaml` and `values.yaml`.
