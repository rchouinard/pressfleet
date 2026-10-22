#!/usr/bin/env bash
# Prepare a fresh Ubuntu 24.04 amd64 host for the #98 capacity test and build
# PressFleet from the checkout in the current directory. Run as root, from the
# repository root, on the host under test:
#
#   scripts/capacity/host-setup.sh
#
# It installs Docker Engine (with buildx and Compose v2) and the build tools,
# sets Docker's default-address-pools and log rotation as docs/installation.md
# asks, installs mise's toolchain, then builds the CLI, the control plane
# image, and the site images. Safe to rerun.
#
# Environment:
#   VERSION   version stamped into the binary and the control plane image
#             (default: git describe). The two must match, or `init` would
#             look for an image tagged with a different version.
#   SITE_PHP  PHP branches to build site images for (default "8.5 8.4")
set -euo pipefail

[[ "$(id -u)" -eq 0 ]] || { echo "run as root" >&2; exit 1; }
[[ "$(stat -fc %T /sys/fs/cgroup)" == cgroup2fs ]] || { echo "cgroup v2 required" >&2; exit 1; }
[[ -f Makefile && -d control-plane ]] || { echo "run from the repository root" >&2; exit 1; }

export DEBIAN_FRONTEND=noninteractive
# The checkout is usually rsynced from another machine, so its owner differs.
git config --global --add safe.directory "$PWD"
export VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
export SITE_PHP="${SITE_PHP:-8.5 8.4}"

apt-get update -qq
apt-get install -y -qq ca-certificates curl git jq make gcc rsync sysstat >/dev/null

if ! docker compose version >/dev/null 2>&1; then
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
	echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
		>/etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
fi

# 10.200.0.0/16 in /24s gives 256 networks: one per site, plus the platform's.
cat >/etc/docker/daemon.json <<'EOF'
{
  "default-address-pools": [ { "base": "10.200.0.0/16", "size": 24 } ],
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
EOF
systemctl restart docker
docker info --format 'docker {{.ServerVersion}}, cgroup {{.CgroupVersion}}'

if ! command -v mise >/dev/null 2>&1 && [[ ! -x /root/.local/bin/mise ]]; then
	curl -fsSL https://mise.run | sh
fi
export PATH="/root/.local/bin:$PATH"
mise trust
mise install

mise exec -- make build UI=yes VERSION="$VERSION"
mise exec -- make image VERSION="$VERSION"
mise exec -- make images SITE_PHP="$SITE_PHP"

install -m 0755 bin/pressfleet /usr/local/bin/pressfleet
echo "built $(pressfleet version --client)"
docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -E 'control-plane|site-(php|nginx)'
