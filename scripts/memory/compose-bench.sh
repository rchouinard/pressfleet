#!/usr/bin/env bash
# Measure what concurrent docker compose invocations cost inside a
# container's memory limit, as the control plane runs them (#178).
#
#   scripts/memory/compose-bench.sh [concurrency...]
#
# For each concurrency N (default "1 2 4 8"), a container from the control
# plane image (IMAGE, default ghcr.io/rchouinard/pressfleet/control-plane:latest)
# with the Docker socket brings N small two-service Compose projects up at
# once (up --wait, ps, exec, down, like a site's lifecycle), and
# scripts/memory/cgmem.sh reports the container's peaks. Each run is
# repeated for every environment in ENV_VARIANTS (default: none,
# GOMEMLIMIT=160MiB, GOMEMLIMIT=16MiB, GOGC=50), which the docker CLI inherits
# from the control plane as it would. Run as root on the Docker host.
#
# cgroup_peak_mib is what counts against the control plane's limit: the
# docker binaries' pages are shared between invocations and charged once,
# so it grows by much less per invocation than a process's VmHWM
# (peak_docker_mib, peak_docker_compose_mib) suggests.
#
# Development tooling: the "docker CLI only in provisioner/compose" rule is
# about the Go code, not this script.
set -euo pipefail

image="${IMAGE:-ghcr.io/rchouinard/pressfleet/control-plane:latest}"
service_image="${SERVICE_IMAGE:-$image}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
concurrency=("$@")
[[ ${#concurrency[@]} -gt 0 ]] || concurrency=(1 2 4 8)
IFS=';' read -r -a variants <<<"${ENV_VARIANTS:-;GOMEMLIMIT=160MiB;GOMEMLIMIT=16MiB;GOGC=50}"

# shellcheck disable=SC2016
script='
set -eu
mkdir -p /tmp/proj
cat >/tmp/proj/compose.yaml <<EOF
services:
  php:
    image: $SERVICE_IMAGE
    entrypoint: ["sleep", "infinity"]
    init: true
    volumes: [content:/data]
    healthcheck: { test: ["CMD", "true"], interval: 1s, start_period: 1s, start_interval: 1s }
  nginx:
    image: $SERVICE_IMAGE
    entrypoint: ["sleep", "infinity"]
    init: true
    volumes: [content:/data:ro]
    depends_on: [php]
volumes:
  content: {}
EOF
sleep 1
one() {
	p=pfmem-$$-$1
	docker compose -f /tmp/proj/compose.yaml -p $p up -d --wait --quiet-pull >/dev/null 2>&1
	docker compose -f /tmp/proj/compose.yaml -p $p ps --format json >/dev/null
	docker compose -f /tmp/proj/compose.yaml -p $p exec -T php true
	docker compose -f /tmp/proj/compose.yaml -p $p down --volumes >/dev/null 2>&1
}
i=0
while [ $i -lt "$N" ]; do one $i & i=$((i + 1)); done
wait
'

for n in "${concurrency[@]}"; do
	for v in "${variants[@]}"; do
		name="composebench-$$-$RANDOM"
		envargs=(-e N="$n" -e SERVICE_IMAGE="$service_image")
		[[ -n "$v" ]] && envargs+=(-e "$v")
		docker run -d --init --name "$name" -v /var/run/docker.sock:/var/run/docker.sock \
			"${envargs[@]}" --entrypoint sh "$image" -c "$script" >/dev/null
		start=$(date +%s.%N)
		peaks="$("$here/cgmem.sh" "$name" 0.02 | tr '\n' ' ')"
		rc="$(docker wait "$name")"
		wall=$(awk -v a="$start" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }')
		docker rm "$name" >/dev/null
		echo "n=$n env=${v:-none} rc=$rc wall_s=$wall $peaks"
	done
done
