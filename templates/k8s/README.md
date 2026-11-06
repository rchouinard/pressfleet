# Kubernetes site templates

The Kubernetes driver (`control-plane/internal/provisioner/kubernetes`, design doc §4.8) renders every `*.yaml.tmpl` here into `<data_dir>/sites/<site-id>/kubernetes/<name>.yaml` and applies the result to the cluster with server-side apply under the field manager `pressfleet`.

- Each template is a Go `text/template` producing one or more YAML documents. Templates run with `missingkey=error`; quote every string value with `q`, which writes a JSON string (valid YAML).
- The data is `kubernetes.manifestData`; see its fields for what a template can use.
- Only object kinds the driver knows can be applied (`kubernetes.kinds`). Add a kind there before a template uses it.
- The files are part of the template fingerprint: a change here moves running sites to the new manifests through the templates rollout, one site at a time (§4.7, §5.5). A change to the Go code that renders them, which changes the output for an existing site, bumps the driver's `renderVersion`.
