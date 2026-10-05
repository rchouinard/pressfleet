# Releasing

Pushing a `v*` tag publishes a release ([`.github/workflows/release.yml`](../.github/workflows/release.yml)):

1. The tag is checked: it must be a semantic version (`vMAJOR.MINOR.PATCH`, optionally `-PRERELEASE`), on a commit that is on `main`.
2. The [Images](../.github/workflows/images.yml) workflow pushes `control-plane:<tag>` and the site image matrix to GHCR.
3. The host CLI is built for linux/amd64 with the dashboard embedded. It embeds the release's platform templates and pinned image list ([`templates/`](../templates/)), and installs the control plane image tagged with its own version (design doc §4.7).
4. A GitHub release is created with these assets:

   | Asset | Contents |
   | --- | --- |
   | `pressfleet-linux-amd64` | The host CLI (the control plane image runs the same binary) |
   | `SHA256SUMS` | Checksums of the other assets |
   | `pressfleet-linux-amd64.spdx.json` | SPDX SBOM of the binary |
   | `control-plane.spdx.json` | SPDX SBOM of the control plane image |

   The notes, from [`scripts/release-notes.sh`](../scripts/release-notes.sh), name the oldest release the CLI upgrades from, the control plane image digest, the pinned platform images, and the commits since the previous release.

The release is created only after the images are pushed, so a published CLI never points at a missing control plane image. A tag with a pre-release suffix (`v1.2.0-rc.1`) becomes a GitHub pre-release. Pre-releases are left out when the notes look for the previous release.

## Cutting a release

1. Check that CI and E2E passed on the `main` commit to release.
2. Check `MinUpgradeFrom` in [`control-plane/internal/skew/skew.go`](../control-plane/internal/skew/skew.go) (below).
3. Tag and push:

   ```sh
   git tag -a v0.2.0 -m "PressFleet v0.2.0"
   git push origin v0.2.0
   ```

4. Watch the Release workflow; the release appears when it finishes. If it fails before creating the release, fix the cause on `main`, delete the tag (`git push --delete origin v0.2.0 && git tag -d v0.2.0`), and tag again. Don't move a tag after its release is published: installs and upgrades reference its images by tag.

## Minimum upgradable-from version

`skew.MinUpgradeFrom` is the oldest release that `pressfleet upgrade` of this release upgrades from. Raise it only when a release can no longer upgrade older control planes directly, for example because it drops a migration path. Operators on older releases then upgrade through an intermediate release first, and the release notes say from which version.

## Version skew

The CLI and the control plane follow the version skew policy in [installation.md](installation.md#cli-and-control-plane-versions). Before 1.0, the minor versions must match. From 1.0, CI will test each release's CLI against control planes at n−1, n, and n+1 minor versions (#144).
