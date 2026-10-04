# Compose templates

Templates the control plane renders into one Docker Compose project per site (`pf-<site-id>`) (design doc §4.1).

- [`site.yaml.tmpl`](site.yaml.tmpl): the site's `nginx` and `php` services, network, and volumes. Rendered by `compose.RenderSite` (`control-plane/internal/provisioner/compose/site.go`) into `<data_dir>/sites/<site-id>/compose.yaml`, next to the generated `wp-config.php`. The container settings follow the site image runtime contract in [`images/README.md`](../../images/README.md).

A change to these templates reaches existing sites through the templates rollout after an upgrade (design doc §4.7, §5.5): the Compose driver fingerprints them (`compose.Provisioner.TemplateFingerprint`, in `control-plane/internal/provisioner/compose/templates.go`), and running sites rendered with another fingerprint are re-rendered one at a time, each with a health check and its previous files put back if the check fails. A change to the Go code that renders a site differently needs a bump of `renderVersion` in the same file, which the fingerprint includes.
