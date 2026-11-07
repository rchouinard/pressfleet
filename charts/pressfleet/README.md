# PressFleet Helm chart

Installs PressFleet on a Kubernetes cluster: the control plane and the platform services that `templates/platform` renders for Compose (design doc §4.8, decisions 3, 4, and 7 in #212). Sites are not part of the chart: the control plane's Kubernetes driver creates a namespace for each.

`pressfleet init --target kubernetes` writes the values file and the Secrets (#224). The chart needs a cluster that meets §4.8's requirements: Kubernetes 1.30 or later (the chart uses `ValidatingAdmissionPolicy`), a CNI that enforces NetworkPolicy, metrics-server, a ReadWriteOnce and a ReadWriteMany storage class, and LoadBalancer Services or NodePorts.

## What it installs

Everything goes in the release's namespace, which is the platform namespace (`kubernetes.platform_namespace`). Install one release per namespace: the services have fixed names, as on Compose.

| Object | Notes |
| --- | --- |
| `control-plane` Deployment, Service, ServiceAccount | One replica, strategy `Recreate`. Runs as UID/GID 60001 with the repository group 60000 (`provisioner.RepositoryGID`), read-only root filesystem, `GOMEMLIMIT=96MiB`. |
| `control-plane-data` PVC | ReadWriteMany, the data directory `/var/lib/pressfleet`, including `state/backups`, where the control plane writes its migration markers (#246). Kept on uninstall (`helm.sh/resource-policy: keep`). `controlPlane.data.existingClaim` uses an existing claim instead. The volume must be writable by the control plane's group: the storage must honor `fsGroup`. Site tasks mount the same storage through a PersistentVolume of their own namespace (see [The data volume in site namespaces](#the-data-volume-in-site-namespaces)), so the storage must accept several PersistentVolumes on one volume (NFS, CephFS, EFS do), and the PersistentVolume the claim binds should have the reclaim policy `Retain`: the class's `reclaimPolicy: Retain`, or `kubectl patch pv` after the install. |
| `traefik` Deployment, Service, ConfigMap, `traefik-acme` PVC | One replica (ACME storage is ReadWriteOnce). Entry points `web`, `websecure`, `traefik` (loopback: ping and metrics), resolvers `acme-http` and `acme-preview`, as in package `ingress`, plus `sftp`. The Service maps 80, 443, and `sftp.port` to the container's 8000, 8443, and 2222. |
| `mariadb` StatefulSet, Service, ConfigMap | Configured from the Compose template (`files/mariadb.cnf.tmpl`). Left out with `mariadb.enabled: false`, which needs `mariadb.address`. |
| `redis` StatefulSet, Service, ConfigMap | Valkey, from the Compose template (`files/valkey.conf.tmpl`); an init container puts in the admin password's SHA-256. Nothing persisted. |
| `victoria-metrics` StatefulSet, Service | Retention from `metrics.retentionDays`. |
| `maintenance` Deployment, Service | The maintenance page, on the control plane image. |
| IngressRoutes `pf-dashboard`, `pf-preview-wildcard`; IngressRouteTCP `pf-sftp` | The dashboard domain, the DNS-01 wildcard certificate (with `acme.dnsProvider`), and the SFTP gateway. |
| RBAC, admission policy, NetworkPolicies | See below. |
| `crds/traefik.yaml` | Traefik's CRDs, for the pinned Traefik release. |

Every pod meets Pod Security `restricted`, so the platform namespace can enforce it.

### Secrets

The chart reads Secrets; it never creates them. The names are values; the defaults are:

| Secret | Keys | Mounted at |
| --- | --- | --- |
| `pressfleet-master-key` | `master.key` | `/run/secrets/master_key` |
| `pressfleet-platform` | `mariadb-root-password`, `redis-admin-password`, `ssh-host-key` | `/run/secrets/<the Compose secret name>` |
| `pressfleet-config` (optional) | `pressfleet.yaml` | `/run/secrets/pressfleet_config` (`PRESSFLEET_CONFIG`) |
| `pressfleet-postgres` | `dsn` | `PRESSFLEET_DATABASE_POSTGRES_DSN` |
| `acme.dnsCredentialsSecret` | one key per variable the DNS provider reads | Traefik, as `<VARIABLE>_FILE` |
| `acme.caCertificates.secretName` | PEM | Traefik, as `LEGO_CA_CERTIFICATES` |

On Compose the control plane decrypts the DNS provider's credentials (`acme.dns_credentials`) for Traefik through a shared memory volume. Pods can't share one, so on Kubernetes Traefik reads them from a Secret instead, and the control plane isn't given `acme.dns_provider` or `acme.dns_credentials`.

### Settings

Values that the chart and the control plane both need are passed to the control plane as `PRESSFLEET_*` variables, which take precedence over the config file: `dashboardDomain`, `previewBaseDomain`, `publicIP`, `metrics.retentionDays`, `sftp.port`, and the `kubernetes.*` values. The chart also sets `driver`, `database.driver`, `data_dir`, `listen`, `master_key_file`, `task_image`, `metrics.url`, and the MariaDB and Valkey addresses, which are full Service names (`mariadb.<namespace>.svc:3306`) so that site namespaces can use them. Every other setting belongs in the config file.

### Images

`files/images.yaml` is a copy of `templates/platform/images.yaml`, so both targets pin the same digests. The control plane image is that repository tagged with the chart's `appVersion`. `images.*` override them, for a registry mirror.

`files/` is generated by `make generate` (package `charts`, `charts/charts.go`) and never edited by hand; `TestSharedFilesUpToDate` fails if a copy drifts. When Traefik's image changes, replace `crds/traefik.yaml` with that release's CRDs (`TestTraefikCRDsMatchImage`). Helm installs CRDs on the first install only, so the upgrade path (#224) must apply them.

## RBAC and isolation

The control plane's ServiceAccount, `control-plane`, gets:

- **ClusterRole `pressfleet:<namespace>:control-plane`:** create, change, and delete namespaces; write RoleBindings and `bind` the site ClusterRole only; create, label, and delete PersistentVolumes (the data volume's clones, below); read nodes, node metrics, and storage classes. No Secrets, `nodes/proxy`, or `pods/exec` outside its namespaces.
- **ClusterRole `pressfleet:<namespace>:site`:** a site's objects (pods, exec, attach, logs, Secrets, Services, PVCs, ConfigMaps, Deployments, NetworkPolicies, IngressRoutes, Middlewares, pod metrics). Never bound cluster-wide: the driver binds it in each site namespace with a RoleBinding to `system:serviceaccount:<namespace>:control-plane`.
- **Role `control-plane`** in the platform namespace: repository task pods, their Secrets and NetworkPolicies, exec and attach, IngressRoutes and Middlewares, and read access to Services, PVCs, events, workloads, and pod metrics.

RBAC can't name namespaces that don't exist yet, so the namespace and RoleBinding rights are cluster-wide. The `ValidatingAdmissionPolicy` `control-plane.<namespace>.pressfleet.io` narrows them to the control plane's own namespaces:

- **Site namespaces** are named `<siteNamespacePrefix>...` and labeled `app.kubernetes.io/managed-by: pressfleet`.
- **Doctor's probe namespaces** (#225) are named `<namespace>-doctor-...` and labeled `pressfleet.io/doctor-probe: "true"`.

Both kinds must enforce Pod Security `restricted`. A RoleBinding must be in one of them and bind the site ClusterRole.

### The data volume in site namespaces

Site tasks (backups, restores, imports, exports, deploys) mount transfer and repository directories from the data volume, but its claim belongs to the platform namespace. For each site namespace, the driver creates a PersistentVolume `<site namespace>-data`, with the data volume's CSI or NFS source and mount options, reclaim policy `Retain`, and no storage class, bound in advance to a claim `pressfleet-data` in the site's namespace (decision of 2026-11-06 in #219). Deleting a site deletes both; the storage stays.

A PersistentVolume could point at anything, a node's filesystem included, and Pod Security doesn't look at volumes behind a claim. `data-volume-policy.yaml` therefore narrows the control plane's PersistentVolume rights with two more `ValidatingAdmissionPolicies`:

- **`data-volume.<namespace>.pressfleet.io`:** a PersistentVolume the control plane creates must be a clone as described, labeled `pressfleet.io/data-clone: <namespace>`; it may delete only such clones; and the only change it may make is to label the PersistentVolume bound to the data claim `pressfleet.io/data-volume: <namespace>`, changing nothing else.
- **`data-volume-clones.<namespace>.pressfleet.io`:** takes the PersistentVolume with that label as its parameter, and admits a clone only if its source and mount options are the same. Until the control plane has labeled the data volume (before its first site task), it admits no PersistentVolume at all.

Traefik's ServiceAccount gets the cluster-wide read access its CRD provider needs, which includes Secrets (basicAuth middlewares name them).

NetworkPolicies deny ingress to the platform namespace by default and then allow:

- Traefik from anywhere.
- The control plane and the maintenance page from Traefik.
- MariaDB and Valkey from the control plane and from pods in site namespaces (labeled `app.kubernetes.io/managed-by: pressfleet` and `pressfleet.io/site-id`).
- VictoriaMetrics from the control plane.

Policies select platform pods by `app.kubernetes.io/name: pressfleet` and `app.kubernetes.io/component`. Site NetworkPolicies should select Traefik the same way: `app.kubernetes.io/component: traefik` in the platform namespace. Pods the driver starts in the platform namespace (repository tasks) must not carry these labels.

## Development

```sh
mise exec -- go test ./charts/                           # lint, golden render, schema, Pod Security
mise exec -- go test ./charts/ -run TestTemplate -update # rewrite the golden files after a deliberate change
mise exec -- helm template pressfleet charts/pressfleet -n pressfleet -f charts/testdata/values/minimal.yaml
```

The release pipeline (#227) stamps `version` and `appVersion` when it packages the chart; in the repository both stay `0.0.0-dev`.
