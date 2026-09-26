# Compose templates

Templates the control plane renders into one Docker Compose project per site (`pf-<site-id>`) (design doc §4.1).

- [`site.yaml.tmpl`](site.yaml.tmpl): the site's `nginx` and `php` services, network, and volumes. Rendered by `compose.RenderSite` (`control-plane/internal/provisioner/compose/site.go`) into `<data_dir>/sites/<site-id>/compose.yaml`, next to the generated `wp-config.php`. The container settings follow the site image runtime contract in [`images/README.md`](../../images/README.md).
