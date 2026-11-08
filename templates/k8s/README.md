# Kubernetes site templates

The Kubernetes driver (`control-plane/internal/provisioner/kubernetes`, design doc §4.8) renders every `*.yaml.tmpl` here into `<data_dir>/sites/<site-id>/kubernetes/<name>.yaml` and applies the result to the cluster with server-side apply under the field manager `pressfleet`.

- Each template is a Go `text/template` producing one or more YAML documents. Templates run with `missingkey=error`; quote every string value with `q`, which writes a JSON string (valid YAML).
- `helpers.tmpl` defines the pieces the manifests share (the header, the labels); it isn't rendered itself.
- The data is `kubernetes.manifestData`; see its fields for what a template can use.
- Only object kinds the driver knows can be applied (`kubernetes.kinds`). Add a kind there before a template uses it.
- Every object must be in the site's namespace and carry the `labels` template, whose `pressfleet.io/rendered: "true"` is how the driver finds objects that a template no longer renders and deletes them (server-side apply doesn't). Rendering fails otherwise.
- The site's internet egress isn't rendered here: the driver writes it as the NetworkPolicy `site-internet` (`internet.go`), because its exceptions include the nodes' and the API server's addresses, which change while sites run.
- The Deployment leaves `replicas` out: starting and stopping a site scale it, and applying the manifests again must not undo that.
- `maintenance-routes.tmpl` isn't a manifest of the site's namespace: it renders the site's maintenance routes (IngressRoutes in the platform namespace, next to the `maintenance` Service, because Traefik can't route across namespaces) into `<data_dir>/sites/<site-id>/kubernetes-maintenance.yaml`. `SetMaintenance` applies them and deletes them again, and `DeleteSite` deletes them with the site (`routing.go`).
- The files are part of the template fingerprint: a change here moves running sites to the new manifests through the templates rollout, one site at a time (§4.7, §5.5). A change to the Go code that renders them, which changes the output for an existing site, bumps the driver's `renderVersion`.

## What the Helm chart must match

The manifests rely on what the chart (`charts/pressfleet`, #223) creates:

| Name | Where | Used by |
| --- | --- | --- |
| ServiceAccount `control-plane` | platform namespace | the control plane; each site namespace's RoleBinding (`rbac.yaml.tmpl`) binds it |
| ClusterRole `pressfleet:<platform namespace>:site` | cluster | what the control plane may do in a site's namespace, bound there only |
| labels `app.kubernetes.io/managed-by: pressfleet` and `pressfleet.io/site-id`, and Pod Security `restricted` | the site namespace | the chart's admission policy admits the namespace and its RoleBinding only with them, and its platform NetworkPolicies admit MariaDB and Valkey traffic from namespaces labeled so |
| labels `app.kubernetes.io/name: pressfleet`, `app.kubernetes.io/component: traefik` | Traefik's pods | the only pods the site NetworkPolicy lets reach nginx |
| `kubernetes.io/metadata.name` | the platform namespace | set by Kubernetes itself; the site NetworkPolicy allows egress to MariaDB's and Valkey's ports there |
| the ports of `mariadb.address` and `redis.address` | MariaDB's and Valkey's pods | NetworkPolicies see the pod's port, not the Service's, so the containers must listen on the ports the Services publish |

The control plane applies a site's namespace and then its RoleBinding before anything else in it, which takes the chart's cluster-wide rights on namespaces and RoleBindings and `bind` on the site ClusterRole; it lists and deletes RoleBindings in site namespaces with the same rights when it prunes. It reads the image pull Secrets in the platform namespace through the chart's Role there. Traefik runs with `allowCrossNamespace: false`, so nothing here refers to an object in another namespace.
