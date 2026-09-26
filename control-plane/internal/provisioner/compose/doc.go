// Package compose is the Docker Compose driver: the only package allowed to
// import Docker/Compose SDKs or run the docker CLI (design doc §4.2; archtest
// enforces the imports, review enforces the CLI rule).
//
// [Project] runs `docker compose` against one rendered project directory. It
// is shared by the platform services project (installed by the host CLI) and
// the per-site projects (pf-<site-id>) that the Compose provisioner manages.
// The Compose v2 CLI plugin is used instead of the Compose Go SDK so the
// control plane behaves exactly like an operator running `docker compose` in
// the same directory.
//
// [RenderSite] renders a site's project (compose.yaml and wp-config.php)
// from templates/compose/site.yaml.tmpl, and [Provisioner] implements
// provisioner.Provisioner on top of both: it renders and applies each site's
// project and connects the platform services to the site's network.
package compose
