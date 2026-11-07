# Releasing

Pushing a `v*` tag publishes a release ([`.github/workflows/release.yml`](../.github/workflows/release.yml)):

1. The tag is checked: it must be a semantic version (`vMAJOR.MINOR.PATCH`, optionally `-PRERELEASE`), on a commit that is on `main`.
2. The [Images](../.github/workflows/images.yml) workflow pushes `control-plane:<tag>` and the site image matrix to GHCR.
3. The host CLI is built for linux/amd64 with the dashboard embedded. It embeds the release's platform templates and pinned image list ([`templates/`](../templates/)), and installs the control plane image tagged with its own version (design doc §4.7).
4. The Helm chart is packaged ([`scripts/package-chart.sh`](../scripts/package-chart.sh)) and pushed to `oci://ghcr.io/rchouinard/pressfleet/charts` (below).
5. A GitHub release is created with these assets:

   | Asset | Contents |
   | --- | --- |
   | `pressfleet-linux-amd64` | The host CLI (the control plane image runs the same binary) |
   | `pressfleet-<version>.tgz` | The Helm chart package, the same one pushed to GHCR |
   | `SHA256SUMS` | Checksums of the other assets |
   | `pressfleet-linux-amd64.spdx.json` | SPDX SBOM of the binary |
   | `pressfleet-chart.spdx.json` | SPDX SBOM of the chart package |
   | `control-plane.spdx.json` | SPDX SBOM of the control plane image |

   The notes, from [`scripts/release-notes.sh`](../scripts/release-notes.sh), name the oldest release the CLI upgrades from, how to install the chart and the Kubernetes version it needs, the control plane image digest, the pinned platform images, and the commits since the previous release.

The release is created only after the images are pushed, so a published CLI never points at a missing control plane image. A tag with a pre-release suffix (`v1.2.0-rc.1`) becomes a GitHub pre-release. Pre-releases are left out when the notes look for the previous release.

## The Helm chart

The chart job runs after the images are pushed, so the chart never points at a missing control plane image. It stamps the chart's `version` and `appVersion` (`0.0.0-dev` in the source tree) with the tag without its `v`: `v1.2.0` is chart `1.2.0` with `appVersion: 1.2.0`. The chart tags the control plane image with `appVersion`, so a chart and the control plane it installs always come from the same release, and the platform image digests are those in `templates/platform/images.yaml` at the tag (design doc §4.8). The job lints and renders the packaged chart before it pushes.

```sh
helm install pressfleet oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet --version 1.2.0 --values values.yaml
```

The chart's `kubeVersion` and `doctor.MinKubernetesMinor` (`control-plane/internal/doctor`) are the oldest supported Kubernetes release, the three newest minors, and move together each release; `TestKubeVersionMatchesDoctor` fails if they differ.

The notes carry that command, the chart's digest, and the minimum Kubernetes version (`kubeVersion` in `Chart.yaml`). A tag with a pre-release suffix is a pre-release chart version (`1.2.0-rc.1`), which Helm installs only with an explicit `--version`.

To try the packaging without a release: `mise exec -- scripts/package-chart.sh v0.0.1-test /tmp/dist`.

If the chart job fails after pushing, the tag can't be pushed again with different content: delete the tag and the chart version from GHCR (the `charts/pressfleet` package's versions page) before tagging again. Charts, like images, aren't signed; the release's `SHA256SUMS` and SBOMs are the integrity check, as for the other assets.

## Cutting a release

1. Check that CI and E2E passed on the `main` commit to release.
2. Check `MinUpgradeFrom` in [`control-plane/internal/skew/skew.go`](../control-plane/internal/skew/skew.go) (below).
3. Tag and push:

   ```sh
   git tag -a v0.2.0 -m "PressFleet v0.2.0"
   git push origin v0.2.0
   ```

4. Watch the Release workflow; the release appears when it finishes. If it fails before creating the release, fix the cause on `main`, delete the tag (`git push --delete origin v0.2.0 && git tag -d v0.2.0`), and tag again. Don't move a tag after its release is published: installs and upgrades reference its images by tag.

## Vulnerability scanning

[Trivy](https://trivy.dev) scans every image PressFleet ships, looking for HIGH and CRITICAL vulnerabilities that have a fixed version (`--ignore-unfixed`: nothing can be done about the rest). The scan is the composite action [`.github/actions/trivy-scan`](../.github/actions/trivy-scan/action.yml). Its report goes to the job summary and lists every such finding. Which findings fail a build is the policy below.

| Where | What is scanned | Fails on |
| --- | --- | --- |
| Pull requests ([Images](../.github/workflows/images.yml)) | The control plane image, and the site images of the smoke-tested matrix entries | Fixed CRITICAL |
| Release (`v*` tag, Images workflow) | Every control plane and site image, built and scanned **before** it is pushed | Fixed CRITICAL |
| Release ([Release](../.github/workflows/release.yml)) | The third-party platform images in `templates/platform/images.yaml` | Fixed CRITICAL |
| Weekly, and on demand ([Security](../.github/workflows/security.yml)) | The platform images | Fixed CRITICAL |

A failing release scan stops the release: the Release workflow publishes only after the Images and platform image jobs succeed. Fix it as for any failed release (see "Cutting a release"). Pushes to `main` and the weekly site image rebuild don't gate: the images they publish replace ones that already exist, and a rebuild that picks up patches should never be blocked by one finding it hasn't fixed yet.

**Why CRITICAL, and only CRITICAL.** Most fixed HIGH findings are in code PressFleet doesn't build and can't patch: the Go standard library and `x/net` inside Traefik, the socket proxy, VictoriaMetrics, restic, and the Docker CLI and Compose plugin, and Debian or Alpine packages that the base image's maintainer rebuilds days or weeks later. A HIGH gate would fail every pull request and release on upstream lag. HIGH findings are still reported, and bumping the pinned images to clear them is part of cutting a release (below). A CRITICAL finding is rare enough to stop for.

**The weekly platform scan fails too, rather than only reporting.** It blocks nothing, and a red scheduled run is the only alert that a pinned image has gained a fixed CRITICAL finding between releases. The next release is blocked by the same finding.

**Exceptions** go in [`.trivyignore.yaml`](../.trivyignore.yaml), and only for a fixed CRITICAL finding that nothing in this repository can fix. Each entry names one vulnerability and the `paths` (or `purls`) it applies to, a `statement` saying why the finding is safe to ship and what unblocks the fix, and an `expired_at` date no more than 60 days away. Trivy stops ignoring an entry on that date, so the gate fails again and someone reviews it: bump the pin if a fixed image exists, or renew the entry with a new date and a restated reason. `TestTrivyIgnoreEntriesAreReviewed` (`control-plane/internal/archtest`) rejects an entry without these fields. The ignore file applies to the gate only; the report always shows everything.

### Before each release

1. Check the weekly Security run and the Images run on `main` for new findings.
2. Bump what has fixed findings, each to the newest stable release that clears them: the images in `templates/platform/images.yaml` and the `ARG`s in `control-plane/Dockerfile` (`GO_IMAGE`, `DOCKER_CLI_IMAGE`, `NODE_IMAGE`, `BASE_IMAGE`, `RESTIC_VERSION`). Stay in the same series where a bump changes behavior: MariaDB stays in its X.Y series (upgrades refuse a series change), and Traefik and Valkey stay in their major versions. Pin by digest. Look it up with `docker buildx imagetools inspect --raw <ref> | jq -r '.manifests[] | select(.platform.architecture=="amd64" and .platform.os=="linux") | .digest'`; for a single-platform image, use the digest of `docker buildx imagetools inspect <ref>`. Read the release notes for configuration changes first. A restic bump also needs a look at its repository format notes.
3. Update the pins that tests repeat (`control-plane/internal/platform/platform_test.go` and the `testdata` goldens), run the unit and e2e tests, and scan the new images.
4. Delete `.trivyignore.yaml` entries that no longer match anything.

The site images aren't pinned by digest: they follow `php:<version>-fpm-trixie`, `nginx:<series>-alpine`, and `debian:trixie-slim`, and the weekly Images rebuild (without cache) picks up base image patches. The site-nginx image also runs `apk upgrade`, so Alpine fixes arrive before the nginx image is rebuilt.

## Running workflows on a self-hosted runner

Every job's `runs-on` is `${{ vars.RUNNER || 'ubuntu-latest' }}`, so the repository variable `RUNNER` moves all of them (CI, E2E, Images, Release, Security, Image updates) onto a self-hosted runner. Jobs on a self-hosted runner don't use GitHub Actions minutes, so releases and CI keep working when the minutes run out. Unset the variable to go back to GitHub-hosted runners.

1. Register the runner: Settings → Actions → Runners → New self-hosted runner, Linux x64, and follow the steps there (`config.sh`, then `run.sh` or `svc.sh install` to keep it running). It gets the labels `self-hosted`, `Linux`, and `X64`.
2. Set the variable: Settings → Secrets and variables → Actions → Variables → `RUNNER` = `self-hosted`.

The runner's host needs what `ubuntu-latest` provides and the jobs don't install themselves:

- Docker Engine with the buildx and Compose plugins, usable by the runner's user (the `docker` group). The Test job runs PostgreSQL as a service container published on port 5432, so nothing else on the host can hold that port.
- `git`, `bash`, `curl`, `jq`, `sha256sum`, and the GitHub CLI (`gh`, for the Release job). mise installs Go, Node, pnpm, and the other pinned tools (`mise.toml`).
- Room for the image builds and the e2e suites: tens of GB of disk, and enough memory for a platform with a few sites.

One runner runs one job at a time, so a release, which builds the whole site image matrix, takes longer than on GitHub's parallel runners. Jobs share the host's Docker, so leftover e2e containers from a cancelled run can get in the next one's way: `docker ps -a --filter label=com.docker.compose.project` shows them.

While the repository is private, only its collaborators can trigger the workflows. If it ever accepts pull requests from forks, unset `RUNNER` or keep the `pull_request` workflows on GitHub-hosted runners: on a self-hosted runner, a fork's pull request would run its own code on the host.

## Minimum upgradable-from version

`skew.MinUpgradeFrom` is the oldest release that `pressfleet upgrade` of this release upgrades from. Raise it only when a release can no longer upgrade older control planes directly, for example because it drops a migration path. Operators on older releases then upgrade through an intermediate release first, and the release notes say from which version.

## Version skew

The CLI and the control plane follow the version skew policy in [installation.md](installation.md#cli-and-control-plane-versions). Before 1.0, the minor versions must match. From 1.0, CI will test each release's CLI against control planes at n−1, n, and n+1 minor versions (#144).
