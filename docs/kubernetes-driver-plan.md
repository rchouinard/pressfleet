# Kubernetes Driver: Implementation Plan

**Status:** Accepted, 2026-11-04. Every decision in [#212](https://github.com/rchouinard/pressfleet/issues/212) was confirmed as proposed; see the decision log. The design doc describes the result in §4.8. Milestone: [Post-MVP: Kubernetes Driver](https://github.com/rchouinard/pressfleet/milestone/8); tracking issue: [#102](https://github.com/rchouinard/pressfleet/issues/102).

This plan adds Kubernetes as a second deployment target, next to Docker Compose. A Kubernetes install runs the same control plane and site images. A Helm chart installs it, and a Kubernetes driver manages sites in place of the Compose driver. The design doc has planned this since the start (§1.1, §3, §4.2, §8). Compose stays the default for single-host installs.

## What the MVP already provides

The MVP followed the §4.2 constraints, so much of the driver's contract already exists:

- **One path to the orchestrator.** Everything goes through the `provisioner` interfaces: `Provisioner`, `LogReader`, `StatsReader`, `TaskRunner`, `RepositoryTaskRunner`, `SessionRunner`, `Prober`, and `ImagePinner`, plus the optional `FPMStatusReader`, `TraefikMetricsReader`, `TemplateFingerprinter`, and `RenderSnapshotter`. Archtest keeps Docker out of every other package.
- **Contracts written for both drivers.** The interface comments already describe the Kubernetes mapping: tasks as Jobs, sessions as attached pods with a deny-all NetworkPolicy, `RepositoryGID` as `supplementalGroups`, the site UID as `runAsUser`. Logs are classified by content because Kubernetes merges stdout and stderr. Task secrets are files rather than environment variables, so they map to Secret volumes. `SiteConfig` is data, and holds references to secrets rather than the secrets themselves.
- **PostgreSQL.** Every store test runs on PostgreSQL. The job queue leases with `SKIP LOCKED`. The state backup format loads into either database.
- **Templates.** `templates/k8s/` is reserved for the driver's site manifests.

These pieces are missing:

- **Wiring.** `cli` builds a `*compose.Provisioner` and passes it around by its concrete type. Startup calls Compose-only hooks (`RemoveStaleTaskSecrets`, `AttachSharedServices`).
- **Lifecycle.** `pressfleet init`, `doctor`, `upgrade`, and `recover` render and drive the platform's Compose project on one host.
- **Shared paths.** Transfer and repository directories rely on the data directory being at the same path on the host and in the control plane container (`Task.TransferDir`, `Task.RepositoryDir`).
- **Host facts.** Host metrics read the host's procfs. Container stats are Docker's cumulative CPU counters. Site nginx trusts "the networks it is attached to" (`NGINX_TRUSTED_PROXIES=local`), which describes the cluster wrongly in a pod.
- **Routing.** Site routes are Docker labels and files in Traefik's file provider directory.

## Decisions

Accepted in [#212](https://github.com/rchouinard/pressfleet/issues/212), which lists the alternatives.

| Topic | Proposal |
| --- | --- |
| Scope | One cluster per install, with the orchestrator chosen at install time and fixed. One control plane replica, one replica per site, amd64 nodes. |
| Client | `k8s.io/client-go`, imported only in `provisioner/kubernetes`: the typed clientset, plus the dynamic client and server-side apply for Traefik's CRDs. No informer caches. |
| Packaging | A Helm chart, published on GHCR as an OCI artifact with each release. `pressfleet init --target kubernetes` writes the values and Secrets offline. Upgrades are `helm upgrade`, behind a preflight hook. |
| Platform services | The chart runs Traefik, MariaDB, Valkey, VictoriaMetrics, and the maintenance page. The operator provides PostgreSQL. MariaDB can be external. |
| Storage | Sites use ReadWriteOnce PVCs, and tasks and SFTP workers are scheduled on the site's node. The control plane's data directory is a ReadWriteMany volume. |
| Isolation | A namespace per site with default-deny NetworkPolicies, Pod Security `restricted`, no service account tokens, and RBAC with no rights outside PressFleet's namespaces. |
| Ingress | Traefik's Kubernetes CRDs, named as on Compose. Certificates come from Traefik's ACME. Maintenance mode is a higher-priority route. |
| Metrics | Container stats come from `metrics.k8s.io`, with no `nodes/proxy` access. Host metrics are per node; disk metrics aren't available. |
| Clusters | The three newest minor releases, plus a CNI that enforces NetworkPolicy, metrics-server, and ReadWriteOnce and ReadWriteMany storage classes. CI runs on kind. |
| Capacity | A site's reservation and limit become its pods' requests and limits. Doctor warns when requests exceed allocatable memory. |

## Scope

**In scope:**

- the Kubernetes driver, implementing every `provisioner` interface the Compose driver does, and passing the same conformance suite
- the Helm chart
- install, upgrade, and recovery
- doctor's cluster checks
- metrics on Kubernetes
- the chart in the release pipeline
- kind-based end-to-end tests
- a security review
- operator docs

**Out of scope, for later:**

- a highly available control plane (several replicas)
- sites scaled out to several replicas
- multi-cluster installs
- other ingress controllers, Gateway API, and cert-manager
- several Traefik replicas
- a bundled PostgreSQL
- transfers streamed through task standard streams, which would remove the ReadWriteMany requirement
- moving a Compose install to Kubernetes through state recovery (§5.10)
- arm64 nodes
- a capacity test on a multi-node cluster

## Layout on a cluster

```
namespace pressfleet (platform)                      namespace pf-<site-id> (one per site)
  control-plane   Deployment ×1, RWX data PVC          site       Deployment ×1 (Recreate)
  traefik         Deployment ×1, ACME PVC,               pod      nginx + php, fastcgi cache emptyDir
                  LoadBalancer 80/443/2222             wp-content PVC (RWO)
  mariadb         StatefulSet                          wp-config  Secret
  valkey          StatefulSet                          nginx      Service
  victoria-metrics StatefulSet                         routes     IngressRoute, Middleware
  maintenance     Deployment                           policies   NetworkPolicy (default deny)
  repository tasks Pods (restic, group level)          tasks      Pods (backup, restore, ...) on the site's node
                                                       sessions   Pods (SFTP workers) on the site's node
```

PostgreSQL, for the state database, is outside the chart.

### From Compose to Kubernetes

| Compose | Kubernetes |
| --- | --- |
| Project `pf-<site-id>` | Namespace `pf-<site-id>` |
| `nginx` and `php` containers | One pod with two containers, in a single-replica Deployment |
| Site network; platform services attached to it | NetworkPolicies: ingress from Traefik, egress to MariaDB, Valkey, DNS, SMTP, and the internet |
| `wp-content` volume | ReadWriteOnce PVC |
| FastCGI cache volume shared by the two containers | `emptyDir` in the pod |
| Read-only bind mount of `wp-config.php` | Secret volume |
| Site UID | `runAsUser`, `runAsGroup`, `fsGroup` |
| Memory reservation and limit | Requests and limits |
| Healthchecks | Probes |
| Traefik labels and file provider routes | IngressRoute and Middleware objects |
| Maintenance routes in Traefik's file provider | A higher-priority IngressRoute |
| `docker exec` (WP-CLI, purge, probe, PHP-FPM status, Traefik metrics) | Exec API (WebSocket, SPDY fallback) |
| Task container | Pod on the site's node, mounting the site PVC, the data volume, and a Secret |
| Repository task container | Pod in the platform namespace with a deny-all or S3-only NetworkPolicy |
| SFTP worker container | Attached pod with a deny-all NetworkPolicy |
| Container logs | Pod logs API |
| Docker stats | `metrics.k8s.io`, accumulated into cumulative CPU |
| Image digest from a local pull | Image digest from the registry API |
| Rendered project in the data directory | Rendered manifests in the data directory, applied with server-side apply |
| Docker socket through a proxy | The control plane's ServiceAccount with namespace-limited RBAC |
| Platform Compose project rendered by `pressfleet init` | Helm chart, with values from `pressfleet init --target kubernetes` |
| `pressfleet upgrade` | `helm upgrade` with a preflight hook |
| `docker compose exec control-plane pressfleet …` | `kubectl exec deploy/control-plane -- pressfleet …` |

## Design

### Driver wiring

[#213](https://github.com/rchouinard/pressfleet/issues/213) adds a driver factory, chosen by the `driver` setting, that returns `provisioner.Driver` and the optional capabilities. After it, only the factory and the Compose-only host commands name a Compose type. Driver-specific startup work, such as removing stale task secrets or attaching platform services to site networks, moves behind an optional interface. The refactor changes nothing for Compose installs, and doesn't depend on the decisions.

[#214](https://github.com/rchouinard/pressfleet/issues/214) extracts a conformance suite from the Compose driver's Docker tests. It is parameterized by a driver constructor, so both drivers are held to one contract.

### Client and rendering

The driver ([#215](https://github.com/rchouinard/pressfleet/issues/215)) renders each site's manifests from `templates/k8s`, embedded like the Compose templates, into the site's directory under the data directory, where operators can inspect them. It applies them with server-side apply under the field manager `pressfleet`. `TemplateFingerprinter` hashes the templates and a render version, so template changes reach running sites through the existing templates rollout, one site at a time (§4.7, §5.5). `RenderSnapshotter` keeps the previous manifests for that rollout's undo.

client-go adds a large module graph to a binary that Compose installs run too. #215 measures the binary size and the idle memory of a Compose control plane, which has to stay within its 256 MiB limit (§4.1, #178).

### Sites

[#216](https://github.com/rchouinard/pressfleet/issues/216) covers the site objects in the table above.

- **Starting and stopping** scale the Deployment between 1 and 0. Deleting a site deletes its namespace.
- **Resources.** Requests and limits split the site's reservation and limit between nginx and php, as the Compose driver does. PHP-FPM's `pm.max_children` still comes from the limit.
- **Volume ownership** comes from `fsGroup`, with `fsGroupChangePolicy: OnRootMismatch`. Storage that ignores `fsGroup` isn't supported, and doctor says so.
- **Image pinning.** `ImagePinner` resolves a tag to its digest through the registry API rather than by pulling, because nodes pull by digest.

### Storage and scheduling

A site's wp-content is a ReadWriteOnce PVC, so it can be mounted by several pods only on the same node. Tasks and SFTP workers that mount it get pod affinity to the site's pod. For a stopped site, the PV's own topology decides where they run. The control plane's data directory is a ReadWriteMany volume, mounted by the control plane and by task pods as subpaths. That keeps the `Task` contract: transfer directories (database dumps, import archives, deploy packages), uploads, exports, and local backup repositories work as they do on Compose. Removing the ReadWriteMany requirement means streaming transfers through tasks' standard streams and requiring S3 for backups; that comes later.

### Isolation

- **Namespaces.** Each site has its own namespace, labeled as PressFleet's and enforcing Pod Security `restricted`.
- **NetworkPolicies.** Each site namespace denies all traffic by default. Ingress to nginx is allowed only from Traefik's pods. Egress is allowed to DNS, MariaDB, Valkey, the SMTP relay, and the internet, but not to the cluster's pod, service, or node ranges, or the cloud metadata address.
- **No service account tokens** are mounted in site, task, or session pods.
- **Trusted proxies.** nginx trusts forwarded headers from the pod network's CIDRs, which the driver passes in `NGINX_TRUSTED_PROXIES` ([#222](https://github.com/rchouinard/pressfleet/issues/222)). The NetworkPolicy then narrows that to Traefik.
- **RBAC.** The control plane may create and delete namespaces and bind a site ClusterRole in the namespaces it creates. Beyond that, it has rights only in the platform namespace. It has no `nodes/proxy` access and no cluster-wide Secret access.
- **The CNI** must enforce NetworkPolicy. Doctor proves it with two probe pods ([#225](https://github.com/rchouinard/pressfleet/issues/225)), and the security review checks the whole set ([#229](https://github.com/rchouinard/pressfleet/issues/229)).

### Routing and TLS

[#217](https://github.com/rchouinard/pressfleet/issues/217) writes IngressRoute and Middleware objects with the names package `ingress` already defines:

- **The preview router**, with noindex and optional basic auth
- **Once live**, the primary domain and alias routers, with HSTS and the redirects
- **Maintenance mode**, a higher-priority twin of each router pointing at the maintenance page

Certificates work as on Compose: DNS-01 for the preview wildcard, and HTTP-01 for each domain. Traefik keeps its ACME storage on a PVC, which limits it to one replica.

### Exec, tasks, sessions, logs, and stats

- **Exec** ([#218](https://github.com/rchouinard/pressfleet/issues/218)). WP-CLI, cache purges, code reloads, probes, the PHP-FPM status, and Traefik's metrics all run through the exec API. Traefik's metrics entry point stays on loopback, which security review finding F5 requires.
- **Tasks** ([#219](https://github.com/rchouinard/pressfleet/issues/219)) are pods:
  - **Site tasks** run in the site's namespace on the site's node.
  - **Repository tasks** run in the platform namespace, with no network, or egress only to S3.
  - **Secrets.** Each task gets a Secret volume that only its user can read, deleted with the pod.
  - **Restic locks.** Each task has a unique host name.
  - **Memory.** The task's memory limit is the container limit.
  - **Cleanup.** Pods left behind by a control plane restart are cleaned up at startup.
- **Sessions** ([#220](https://github.com/rchouinard/pressfleet/issues/220)) are attached pods with a deny-all NetworkPolicy.
- **Logs and stats** ([#221](https://github.com/rchouinard/pressfleet/issues/221)). Logs come from the Pod logs API, classified by content. Stats come from `metrics.k8s.io`: the driver accumulates CPU usage into the cumulative `CPUUsage` the contract asks for, and reads restarts from Pod status. The kubelet summary API would give cumulative CPU directly, but only through `nodes/proxy`, which can run commands on nodes.

### The chart

[#223](https://github.com/rchouinard/pressfleet/issues/223) adds `charts/pressfleet`, with what `templates/platform` renders for Compose:

- **Services.** The control plane, Traefik (with pinned CRDs), MariaDB, Valkey, VictoriaMetrics, and the maintenance page, configured as on Compose.
- **Pinned images.** Digests come from `templates/platform/images.yaml` through `make generate`, so both targets pin the same builds.
- **Validation.** `values.schema.json` checks the values, and CI runs `helm lint` and golden `helm template` tests.

[#227](https://github.com/rchouinard/pressfleet/issues/227) publishes the chart with each release.

### Install, upgrade, and recover

[#224](https://github.com/rchouinard/pressfleet/issues/224) covers the install lifecycle on Kubernetes.

- **Install.** `pressfleet init --target kubernetes` asks init's questions and writes a values file and Secret manifests. It shows the master key warning and asks for confirmation (§4.1), and needs no cluster access.
- **Upgrade.** `helm upgrade` runs a pre-upgrade hook on the new image that applies the upgrade preflight: no downgrade, no MariaDB series change, and at least the minimum release it can upgrade from. The control plane migrates at startup, as on Compose. Backing up PostgreSQL before an upgrade is the operator's job (§4.7).
- **State commands** run with `kubectl exec` in the control plane pod.
- **Disaster recovery** runs `pressfleet state recover` as a one-off Job with the control plane scaled to zero, then `site.recover` rebuilds the sites (§5.10).

### Doctor

[#225](https://github.com/rchouinard/pressfleet/issues/225) checks the cluster at control plane startup and as a Helm test:

- **Required:** the Kubernetes version; amd64 nodes with cgroup v2; the RBAC the driver needs; the storage classes and their access modes; `metrics.k8s.io`; and NetworkPolicy enforcement.
- **Recommended:** the Traefik Service's external address, and DNS for the dashboard and preview domains.
- **Capacity:** a warning when site requests exceed the cluster's allocatable memory.

Host-only checks are skipped.

### Metrics

[#226](https://github.com/rchouinard/pressfleet/issues/226) covers metrics and the dashboard:

- **Host metrics** become per-node CPU and memory from `metrics.k8s.io`. Host disk metrics aren't available, so the built-in "disk nearly full" rule has nothing to evaluate on Kubernetes, and the docs point to the cluster's own monitoring.
- **Uptime and certificate checks** connect to the Traefik Service.
- **The dashboard** names the orchestrator, shows nodes in the host charts, and uses the cluster's allocatable memory for the capacity warning.

## Compatibility and release

- **Compose installs** behave the same. The refactor (#213) changes no behavior, and the conformance suite (#214) runs against Compose first.
- **Binary.** One binary serves both targets. #215 measured the cost of client-go v0.37: the binary grew from 31 to 66 MiB (without the dashboard), and an idle Compose control plane's resident memory from about 43 to 61 MiB, well within its 256 MiB limit.
- **Configuration.** `driver: kubernetes` requires `database.driver: postgres`. The new settings follow the `config.Config` convention and get rows in `docs/configuration.md`.
- **Release.** Shipping it is a minor release with a new artifact, the chart. The host CLI's admin interface doesn't change, and the version skew rules (§4.7) don't apply to Kubernetes installs, whose chart and control plane always come from the same release.
- **Staging clones** landed first (v1.1.0), so this milestone covers them: the Kubernetes driver renders `SiteConfig.Environment` and `DisableMail` through the shared `wpconfig.Params` (#216), the conformance suite checks them and the mail switch (#214), and the kind tests clone, refresh, and push a staging site (#228). The mail switch check goes through `ExecWPCLI`, so it needs nothing driver-specific.
- **Restic memory.** `GOMEMLIMIT` for restic tasks (#281) is set in the backup engine's task environment, not in a driver, so Kubernetes tasks get it unchanged; #219 checks that it reaches the pod.

## Issues

Sizes and priorities follow the [roadmap](roadmap.md#conventions). All are in the [Post-MVP: Kubernetes Driver](https://github.com/rchouinard/pressfleet/milestone/8) milestone and are sub-issues of [#102](https://github.com/rchouinard/pressfleet/issues/102).

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#212](https://github.com/rchouinard/pressfleet/issues/212) | Decision: Kubernetes deployment model | P0 | S | — |
| [#213](https://github.com/rchouinard/pressfleet/issues/213) | Control plane: build the site driver from configuration | P0 | M | — |
| [#214](https://github.com/rchouinard/pressfleet/issues/214) | Driver conformance test suite | P0 | M | — |
| [#215](https://github.com/rchouinard/pressfleet/issues/215) | Kubernetes driver: configuration, client, and manifest rendering | P0 | M | #212, #213 |
| [#216](https://github.com/rchouinard/pressfleet/issues/216) | Kubernetes driver: site lifecycle | P0 | L | #215 |
| [#217](https://github.com/rchouinard/pressfleet/issues/217) | Kubernetes driver: routing, TLS, and maintenance mode | P0 | M | #216 |
| [#218](https://github.com/rchouinard/pressfleet/issues/218) | Kubernetes driver: WP-CLI and other exec operations | P0 | M | #216 |
| [#219](https://github.com/rchouinard/pressfleet/issues/219) | Kubernetes driver: site and repository tasks | P0 | L | #216 |
| [#220](https://github.com/rchouinard/pressfleet/issues/220) | Kubernetes driver: SFTP sessions | P1 | M | #219 |
| [#221](https://github.com/rchouinard/pressfleet/issues/221) | Kubernetes driver: logs and stats | P1 | M | #216 |
| [#222](https://github.com/rchouinard/pressfleet/issues/222) | Images: run under Kubernetes | P0 | S | #212 |
| [#223](https://github.com/rchouinard/pressfleet/issues/223) | Helm chart: platform services and control plane | P0 | L | #212, #215 |
| [#224](https://github.com/rchouinard/pressfleet/issues/224) | Install, upgrade, and recover on Kubernetes | P0 | L | #223 |
| [#225](https://github.com/rchouinard/pressfleet/issues/225) | Doctor: Kubernetes cluster checks | P0 | M | #212, #215 |
| [#226](https://github.com/rchouinard/pressfleet/issues/226) | Metrics, uptime, and dashboard on Kubernetes | P1 | M | #218, #219, #221 |
| [#227](https://github.com/rchouinard/pressfleet/issues/227) | Release pipeline: publish the Helm chart | P1 | S | #223 |
| [#228](https://github.com/rchouinard/pressfleet/issues/228) | E2E: Kubernetes acceptance tests on kind | P0 | L | #214, #217, #218, #220, #222, #224, #225, #226 |
| [#229](https://github.com/rchouinard/pressfleet/issues/229) | Security review: Kubernetes target | P0 | M | #228 |
| [#230](https://github.com/rchouinard/pressfleet/issues/230) | Docs: Kubernetes installation and operations | P1 | M | #224, #228 |

### Order

1. **Groundwork that needs no decisions (#213, #214)**, alongside the decisions (#212). Both improve the Compose driver on their own.
2. **Driver base (#215), then the sites (#216).** From here, a developer can run sites on a cluster with a kubeconfig.
3. **The rest of the driver, in parallel:** routing (#217), exec (#218), tasks (#219, then sessions #220), logs and stats (#221), and images (#222).
4. **Packaging and lifecycle:** the chart (#223), install and upgrade (#224), doctor (#225), metrics (#226), and the release pipeline (#227).
5. **Proof:** kind e2e (#228), the security review (#229), and docs (#230).

The critical path is #212 → #215 → #216 → #219 → #220 → #228 → #229, with the chart branch (#223 → #224) running alongside it.

### Execution

The issues land in waves; each wave starts once the previous one is merged.

| Wave | Issues | Why together |
| --- | --- | --- |
| 0 | #212, #213, #214 | The decisions, and the groundwork that doesn't need them |
| 1 | #215, #222 | The driver base, and the images, which need only the decisions |
| 2 | #216, #223, #225 | Sites; the chart and doctor need only the driver base. The chart's RBAC covers everything the driver will need, so later PRs don't conflict over it |
| 3 | #217, #218, #219, #221, #224, #227 | The rest of the driver, in separate files, and the lifecycle on top of the chart |
| 4 | #220, #226 | Sessions follow tasks; metrics follow exec, tasks, and stats |
| 5 | #228 | |
| 6 | #229, #230 | |

The MCP server milestone follows this one.

### Working

Each issue is worked on its own branch and merged through a pull request. PRs that touch the Compose driver add the `e2e` label, or `e2e:full` when they reach across workflows (docs/testing.md). PRs that touch the Kubernetes driver or the chart add `e2e:k8s` once #228 adds that job, which also runs nightly.

### Testing

- **Unit:** client-go's fake clientset, dynamic client, and a fake executor, for every driver method, including rollback and cleanup paths. No cluster.
- **Conformance:** the suite from #214, on Docker behind `PRESSFLEET_DOCKER_TESTS=1`, and on kind in #228.
- **Chart:** `helm lint`, golden `helm template` output, and a test that the image digests match `images.yaml`.
- **End to end:** kind with Calico or Cilium, metrics-server, a local registry, and a ReadWriteMany provisioner. The tests cover provisioning, HTTPS, go-live with Pebble, isolation (between sites, and from sites to the API server and metadata address), backup and restore with rollback, SFTP, maintenance mode, deletion, an upgrade from the previous chart, and a disaster recovery drill.

## Decision log

**2026-11-04 ([#212](https://github.com/rchouinard/pressfleet/issues/212)):** all ten decisions were accepted as recommended, with no changes:

1. **Scope:** one cluster per install, the orchestrator fixed at install time, one control plane replica, one replica per site, amd64 nodes.
2. **Client:** `k8s.io/client-go`, only in `provisioner/kubernetes`: the typed clientset, plus the dynamic client and server-side apply for Traefik's CRDs. No informer caches.
3. **Packaging and lifecycle:** a Helm chart published on GHCR; `pressfleet init --target kubernetes` writes the values and Secrets offline; `helm upgrade` with a preflight hook; state commands through `kubectl exec`.
4. **Platform services:** the chart runs Traefik, MariaDB, Valkey, VictoriaMetrics, and the maintenance page. The operator provides PostgreSQL. MariaDB can be external; Valkey can't.
5. **Storage:** ReadWriteOnce site PVCs, with tasks and SFTP workers on the site's node, and a ReadWriteMany data volume. Streaming transfers come later.
6. **Isolation:** a namespace per site, default-deny NetworkPolicies, Pod Security `restricted`, no service account tokens, and RBAC with no rights outside PressFleet's namespaces.
7. **Ingress and TLS:** Traefik's Kubernetes CRDs, named as on Compose; Traefik's ACME; maintenance as a higher-priority route; a LoadBalancer Service.
8. **Metrics:** `metrics.k8s.io`, with no `nodes/proxy` access. Per-node host metrics, no disk metrics.
9. **Supported clusters:** the three newest minor releases, with a NetworkPolicy-enforcing CNI, metrics-server, and ReadWriteOnce and ReadWriteMany storage classes. CI on kind; k3s in the docs.
10. **Capacity:** reservations and limits become requests and limits; doctor and the dashboard warn when requests exceed allocatable memory.

The design doc records them in §1, §1.1, §3, §3.1, §4.4, §4.7, the new §4.8, §5.10, §6, §7.2, §8, and §10.
