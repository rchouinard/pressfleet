#!/usr/bin/env bash
# Prints the notes of a release in Markdown (used by .github/workflows/release.yml):
#
#   scripts/release-notes.sh v1.2.3 [control plane image digest [chart digest]]
#
# The notes name the oldest release this one upgrades from (skew.MinUpgradeFrom,
# design doc §4.7), the release's images, and the commits since the previous
# release, and the Helm chart's reference and minimum Kubernetes version.
set -euo pipefail

tag="${1:?usage: release-notes.sh <tag> [control plane image digest]}"
digest="${2:-}"
chart_digest="${3:-}"
root="$(cd "$(dirname "$0")/.." && pwd)"

min_upgrade_from="$(sed -n 's/^const MinUpgradeFrom = "\(.*\)"$/\1/p' "$root/control-plane/internal/skew/skew.go")"
if [[ -z "$min_upgrade_from" ]]; then
	echo "release-notes.sh: MinUpgradeFrom not found in control-plane/internal/skew/skew.go" >&2
	exit 1
fi
# kubeVersion is a constraint such as ">=1.30.0-0"; the notes say "1.30".
kube_version="$(sed -n 's/^kubeVersion: *"\{0,1\}>=\([0-9]*\.[0-9]*\).*$/\1/p' "$root/charts/pressfleet/Chart.yaml")"
if [[ -z "$kube_version" ]]; then
	echo "release-notes.sh: kubeVersion not found in charts/pressfleet/Chart.yaml" >&2
	exit 1
fi
registry="$("$root/images/matrix.sh" registry)"

# The previous release, skipping pre-releases, which aren't upgrade targets.
previous="$(git -C "$root" describe --tags --abbrev=0 --match 'v*' --exclude 'v*-*' "$tag^" 2>/dev/null || true)"

cat <<EOF
## Install

Download \`pressfleet-linux-amd64\` and \`SHA256SUMS\`, then:

\`\`\`sh
sha256sum --check --ignore-missing SHA256SUMS
install -m 0755 pressfleet-linux-amd64 /usr/local/bin/pressfleet
pressfleet init
\`\`\`

See [installation.md](https://github.com/rchouinard/pressfleet/blob/$tag/docs/installation.md).

## Install on Kubernetes

The Helm chart is published with this release. It needs Kubernetes **$kube_version** or later.

\`\`\`sh
pressfleet init --target kubernetes --output-dir pressfleet-kubernetes
# apply the Secrets it wrote, then:
helm install pressfleet oci://ghcr.io/rchouinard/pressfleet/charts/pressfleet --version ${tag#v} \\
  --namespace pressfleet --values pressfleet-kubernetes/values.yaml --wait --timeout 15m
\`\`\`

${chart_digest:+Chart digest: \`$chart_digest\`

}The chart package (\`pressfleet-${tag#v}.tgz\`) and its SBOM are also release assets. See [kubernetes.md](https://github.com/rchouinard/pressfleet/blob/$tag/docs/kubernetes.md) and the [chart README](https://github.com/rchouinard/pressfleet/blob/$tag/charts/pressfleet/README.md).

## Upgrade

\`pressfleet upgrade\` upgrades control planes from **$min_upgrade_from** or later. Older installs upgrade through an intermediate release first. On Kubernetes, \`helm upgrade\` does the upgrade instead. See [upgrading.md](https://github.com/rchouinard/pressfleet/blob/$tag/docs/upgrading.md).

## Images

The CLI installs the control plane image released with it, and the platform images pinned in [images.yaml](https://github.com/rchouinard/pressfleet/blob/$tag/templates/platform/images.yaml).

EOF
if [[ -n "$digest" ]]; then
	echo "- Control plane: \`$registry/control-plane:$tag@$digest\`"
else
	echo "- Control plane: \`$registry/control-plane:$tag\`"
fi
# shellcheck disable=SC2016 # the backticks are Markdown
grep -E '^[a-z-]+: [^ ]+@sha256:' "$root/templates/platform/images.yaml" | sed 's/^\([a-z-]*\): \(.*\)$/- \1: `\2`/'

echo
echo "## Changes"
echo
if [[ -n "$previous" ]]; then
	git -C "$root" log --no-merges --reverse --format='- %s' "$previous..$tag"
	echo
	echo "Full diff: [$previous...$tag](https://github.com/rchouinard/pressfleet/compare/$previous...$tag)"
else
	echo "First release."
fi
