#!/usr/bin/env bash
# Packages charts/pressfleet for a release (used by .github/workflows/release.yml):
#
#   scripts/package-chart.sh v1.2.3 [output directory]
#
# The chart's version and appVersion are both the release's version without the
# "v", so the chart and the control plane image (the repository in
# files/images.yaml, tagged with appVersion) always come from the same release
# (design doc §4.8). Prints the path of the .tgz.
set -euo pipefail

tag="${1:?usage: package-chart.sh <tag> [output directory]}"
out="${2:-dist}"
root="$(cd "$(dirname "$0")/.." && pwd)"

if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "package-chart.sh: $tag is not a semantic version tag" >&2
	exit 1
fi
version="${tag#v}"

mkdir -p "$out"
helm package "$root/charts/pressfleet" --version "$version" --app-version "$version" --destination "$out" >&2
chart="$out/pressfleet-$version.tgz"

# Check what was stamped, and that the packaged chart (not the source tree)
# lints and renders. The chart has required values; the test values supply them.
values="$root/charts/testdata/values/minimal.yaml"
helm show chart "$chart" | grep -qx "version: $version"
helm show chart "$chart" | grep -qx "appVersion: $version"
helm lint --strict "$chart" --namespace pressfleet --values "$values" >&2
helm template pressfleet "$chart" --namespace pressfleet --values "$values" |
	grep -q "control-plane:$version" || {
	echo "package-chart.sh: the chart doesn't render control-plane:$version" >&2
	exit 1
}
echo "$chart"
