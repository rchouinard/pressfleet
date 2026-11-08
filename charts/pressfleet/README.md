# PressFleet Helm chart

Installs PressFleet on a Kubernetes cluster: the control plane and the platform services that `templates/platform` renders for Compose (design doc §4.8, decisions 3, 4, and 7 in #212). Sites are not part of the chart: the control plane's Kubernetes driver creates a namespace for each.

`pressfleet init --target kubernetes` writes the values file and the Secrets (#224). The chart needs a cluster that meets §4.8's requirements: Kubernetes 1.35 or later (the chart uses `ValidatingAdmissionPolicy`), a CNI that enforces NetworkPolicy, metrics-server, a ReadWriteOnce and a ReadWriteMany storage class, and LoadBalancer Services or NodePorts.

## What it installs

Everything goes in the release's namespace, which is the platform namespace (`kubernetes.platform_namespace`). Install one release per namespace: the services have fixed names, as on Compose. Several installs may share a cluster: each labels its site namespaces `pressfleet.io/platform-namespace: <namespace>`, and its admission policy and NetworkPolicies keep to the namespaces with its own label ([#326](https://github.com/rchouinard/pressfleet/issues/326)). Still give each install its own `kubernetes.siteNamespacePrefix`: the data volume clones' policy checks a clone's namespace by prefix alone, and site namespaces from before the label (below) are told apart only by it.

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
| `upgrade-preflight` Job | A `pre-upgrade` hook; see [Upgrades](#upgrades). |
| `state-recover` Job | A `post-install`/`post-upgrade` hook, only with `recovery.enabled`; see [Recovery](#recovery). |
| `doctor-test` Pod | The Helm test; see [Helm test](#helm-test). |

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

`files/` is generated by `make generate` (package `charts`, `charts/charts.go`) and never edited by hand; `TestSharedFilesUpToDate` fails if a copy drifts. When Traefik's image changes, replace `crds/traefik.yaml` with that release's CRDs (`TestTraefikCRDsMatchImage`). Helm installs CRDs on the first install only, so an upgrade applies them first (see [Upgrades](#upgrades)).

## RBAC and isolation

The control plane's ServiceAccount, `control-plane`, gets:

- **ClusterRole `pressfleet:<namespace>:control-plane`:** create, change, and delete namespaces; write RoleBindings and `bind` the site ClusterRole only; create, label, and delete PersistentVolumes (the data volume's clones, below); read nodes, node metrics, and storage classes. No Secrets, `nodes/proxy`, or `pods/exec` outside its namespaces.
- **ClusterRole `pressfleet:<namespace>:site`:** a site's objects (pods, exec, attach, logs, Secrets, Services, PVCs, Deployments, NetworkPolicies, IngressRoutes, Middlewares, pod metrics). Pods are created and deleted, never changed in place. Never bound cluster-wide: the driver binds it in each site namespace with a RoleBinding to `system:serviceaccount:<namespace>:control-plane`.
- **Role `control-plane`** in the platform namespace: repository task pods (created and deleted, never changed), their Secrets and NetworkPolicies, exec and attach, IngressRoutes and Middlewares, and read access to Services, PVCs, events, workloads, and pod metrics.

RBAC can't name namespaces that don't exist yet, so the namespace and RoleBinding rights are cluster-wide. The `ValidatingAdmissionPolicy` `control-plane.<namespace>.pressfleet.io` narrows them to the control plane's own namespaces:

- **Site namespaces** are named `<siteNamespacePrefix>...` and labeled `app.kubernetes.io/managed-by: pressfleet`.
- **Doctor's probe namespaces** (#225) are named `<namespace>-doctor-...` and labeled `pressfleet.io/doctor-probe: "true"`.

Both kinds must be labeled `pressfleet.io/platform-namespace: <namespace>` and enforce Pod Security `restricted`, at the latest version. A RoleBinding must be in one of them and bind the site ClusterRole to the control plane's own ServiceAccount. Site namespaces created before the platform namespace label lack it: the control plane may change or delete such a namespace, and the change must add the label, which the template rollout after the upgrade does for every site. Until then, it can't write RoleBindings there, and another install that shares the prefix could claim it the same way.

The `ValidatingAdmissionPolicy` `workloads.<namespace>.pressfleet.io` (`workload-policy.yaml`) covers the pods and Services the control plane writes, in any namespace. A pod must run as its namespace's `default` ServiceAccount without a token, share no host namespace, mount no hostPath, and run nothing privileged, so the control plane can't borrow another ServiceAccount (Traefik's, for one) or the node, even in a platform namespace that doesn't enforce Pod Security. A Service must be a ClusterIP Service without `externalIPs` (CVE-2020-8554).

### The data volume in site namespaces

Site tasks (backups, restores, imports, exports, deploys) mount transfer and repository directories from the data volume, but its claim belongs to the platform namespace. For each site namespace, the driver creates a PersistentVolume `<site namespace>-data`, with the data volume's CSI or NFS source and mount options, reclaim policy `Retain`, and no storage class, bound in advance to a claim `pressfleet-data` in the site's namespace (decision of 2026-11-06 in #219). Deleting a site deletes both; the storage stays.

A PersistentVolume could point at anything, a node's filesystem included, and Pod Security doesn't look at volumes behind a claim. `data-volume-policy.yaml` therefore narrows the control plane's PersistentVolume rights with two more `ValidatingAdmissionPolicies`:

- **`data-volume.<namespace>.pressfleet.io`:** a PersistentVolume the control plane creates must be a clone as described, labeled `pressfleet.io/data-clone: <namespace>`; it may delete only such clones; and the only change it may make is to label the PersistentVolume bound to the data claim `pressfleet.io/data-volume: <namespace>`, changing nothing else.
- **`data-volume-clones.<namespace>.pressfleet.io`:** takes the PersistentVolume with that label as its parameter, and admits a clone only if its source and mount options are the same. Until the control plane has labeled the data volume (before its first site task), it admits no PersistentVolume at all.

Traefik's ServiceAccount gets the cluster-wide read access its CRD provider needs, which includes Secrets (basicAuth middlewares name them). That is more than it should have: [#322](https://github.com/rchouinard/pressfleet/issues/322) tracks narrowing it (security review finding K1).

NetworkPolicies deny ingress to the platform namespace by default and then allow:

- Traefik from anywhere.
- The control plane and the maintenance page from Traefik.
- MariaDB and Valkey from the control plane and from pods in this install's site namespaces (labeled `app.kubernetes.io/managed-by: pressfleet`, `pressfleet.io/site-id`, and `pressfleet.io/platform-namespace: <namespace>`). Site namespaces without `pressfleet.io/platform-namespace`, from before the label, are admitted too until the template rollout labels them.
- VictoriaMetrics from the control plane.

Policies select platform pods by `app.kubernetes.io/name: pressfleet` and `app.kubernetes.io/component`. Site NetworkPolicies should select Traefik the same way: `app.kubernetes.io/component: traefik` in the platform namespace. Pods the driver starts in the platform namespace (repository tasks) must not carry these labels.

## Upgrades

`helm upgrade` takes the place of `pressfleet upgrade` (design doc §4.7, §4.8; docs/upgrading.md, "Kubernetes"). Helm never upgrades CRDs, so the operator applies the new release's first: `helm show crds <chart> --version <release> | kubectl apply --server-side -f -`.

The `pre-upgrade` hook `upgrade-preflight` runs `pressfleet admin upgrade-preflight` on the new control plane image. The chart passes it what runs now, found with `lookup`: the control plane Deployment's `app.kubernetes.io/version` and the MariaDB StatefulSet's image, and the new MariaDB image (none with an external MariaDB). It refuses a downgrade, an upgrade from below `skew.MinUpgradeFrom`, and a MariaDB series change, so the hook needs no ServiceAccount token. `helm template` and client-side dry runs find nothing to look up, and pass. The repository's `0.0.0-dev` version isn't ordered. A failed Job is kept for `kubectl logs`; a successful one is deleted.

The control plane migrates the state database when it starts. Backing up PostgreSQL before an upgrade, and restoring that backup before `helm rollback`, are the operator's.

## Recovery

`recovery.enabled` restores a state backup (design doc §5.10; docs/disaster-recovery.md, "Kubernetes"): the control plane Deployment goes to zero replicas, and the hook Job `state-recover` runs `pressfleet state recover --snapshot <recovery.snapshot>` (with `--force` for `recovery.force`) once the install or upgrade has applied, with the control plane's pod spec (`templates/_control-plane.tpl`): its settings, Secrets, data volume, and ServiceAccount, which starts the repository task pods. `helm` waits for it, and fails the release if it fails. The Job is kept for a day. Another `helm upgrade` with `recovery.enabled=false` starts the control plane, which recovers the sites.

On a new cluster, install with `recovery.enabled` from the start. On a running install, scale the control plane to zero and wait for its pod to go first: the chart can't order the old pod's exit before the Job's start.

## Helm test

`helm test pressfleet --namespace <namespace> --logs` runs `pressfleet doctor` (design doc §4.8; docs/kubernetes.md, "Doctor on Kubernetes") in the pod `doctor-test` (`templates/tests/doctor.yaml`), with the control plane's pod spec (`templates/_control-plane.tpl`): its image, settings, Secrets, data volume, and ServiceAccount. So doctor checks the control plane's own RBAC and starts its probe pods (cgroup v2, NetworkPolicy) as the control plane, which the admission policies cover as at startup; Helm creates the test pod itself as the operator. Doctor only reads the data volume, which is ReadWriteMany, so the test runs beside the control plane. Its component label is `doctor-test`, so the control plane's Service and NetworkPolicies don't select it; it reaches only the API server, which no policy of the chart restricts.

The test fails when a required check fails. Doctor exits 2 when only recommended checks fail or warn, which the test counts as passing; the report shows them. The pod is kept, for `kubectl logs doctor-test`, and replaced by the next run (`before-hook-creation`).

## Development

```sh
mise exec -- go test ./charts/                           # lint, golden render, schema, Pod Security
mise exec -- go test ./charts/ -run TestTemplate -update # rewrite the golden files after a deliberate change
mise exec -- helm template pressfleet charts/pressfleet -n pressfleet -f charts/testdata/values/minimal.yaml
```

The release pipeline (#227) stamps `version` and `appVersion` when it packages the chart; in the repository both stay `0.0.0-dev`.
