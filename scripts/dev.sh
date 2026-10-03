#!/usr/bin/env bash
# Install or remove a throwaway local PressFleet platform for development.
#
#   scripts/dev.sh up      install the platform into ./.dev and start it
#   scripts/dev.sh update  recreate the control plane on a rebuilt image
#   scripts/dev.sh down    remove it: containers, sites, volumes, ./.dev
#   scripts/dev.sh status  show what is running
#
# Settings come from ./.env (gitignored); see .env.example and CONTRIBUTING.md.
# This is development tooling: the "docker CLI only in provisioner/compose"
# rule is about the Go code, not this script.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

if [[ -f .env ]]; then
	set -a
	# shellcheck disable=SC1091
	. ./.env
	set +a
fi

dev_dir="$root/.dev"
data_dir="$dev_dir/data"
project="pressfleet"
image="${PF_DEV_IMAGE:-pressfleet-control-plane:dev}"
bin="$root/bin/pressfleet"

die() {
	echo "dev: $*" >&2
	exit 1
}

up() {
	[[ -n "${PF_DEV_DOMAIN:-}" ]] || die "PF_DEV_DOMAIN is not set (add it to .env; see .env.example)"
	[[ -n "${CF_DNS_API_TOKEN:-}" ]] || die "CF_DNS_API_TOKEN is not set (add it to .env; see .env.example)"
	[[ -x "$bin" ]] || die "$bin not found; run 'make build' (or 'make dev-up', which builds it)"
	if [[ -e "$dev_dir/pressfleet.yaml" ]]; then
		die "a dev platform is already installed in $dev_dir; run 'make dev-down' first"
	fi

	local dashboard="$PF_DEV_DOMAIN"
	local preview="preview.$PF_DEV_DOMAIN"
	local email="${PF_DEV_ADMIN_EMAIL:-admin@$PF_DEV_DOMAIN}"
	local password="${PF_DEV_ADMIN_PASSWORD:-pressfleet-dev-password}"

	# Names that don't resolve to this machine fail late and confusingly, as a
	# stuck certificate or a browser error, so say so up front.
	local want="127.0.0.1" name got
	for name in "$dashboard" "probe.$preview"; do
		got="$(getent ahostsv4 "$name" 2>/dev/null | awk 'NR==1 {print $1}')"
		if [[ "$got" != "$want" ]]; then
			echo "dev: warning: $name resolves to '${got:-nothing}', not $want." >&2
			echo "     Create DNS-only A records for it; see CONTRIBUTING.md, \"Running a local platform\"." >&2
			echo "     (A resolver with DNS rebinding protection may also be hiding the answer.)" >&2
		fi
	done

	mkdir -p "$data_dir"
	local creds
	creds="$(mktemp "$dev_dir/dns-credentials.XXXXXX")"
	trap "rm -f '$creds'" EXIT
	chmod 600 "$creds"
	printf 'CF_DNS_API_TOKEN=%s\n' "$CF_DNS_API_TOKEN" >"$creds"

	local args=(
		--non-interactive --confirm-key-stored --skip-preflight
		--admin-password-stdin
		--data-dir "$data_dir"
		--control-plane-image "$image"
		--dashboard-domain "$dashboard"
		--preview-base-domain "$preview"
		--admin-email "$email"
		--dns-provider cloudflare
		--dns-credentials-file "$creds"
		# The host's resolver may answer from a stale cache or refuse names
		# that point at loopback, which would stall the DNS-01 check.
		--acme-dns-resolvers 1.1.1.1:53
	)
	if [[ "${PF_DEV_STAGING:-0}" == 1 ]]; then
		args+=(--acme-staging)
	fi

	PRESSFLEET_CONFIG="$dev_dir/pressfleet.yaml" "$bin" init "${args[@]}" <<<"$password"

	cat <<EOF

Dev platform is up.
  Dashboard: https://$dashboard/
  Previews:  https://<slug>.$preview/
  Admin:     $email / $password
  Config:    $dev_dir/pressfleet.yaml  (PRESSFLEET_CONFIG=$dev_dir/pressfleet.yaml for the CLI)
Tear it down with 'make dev-down'.
EOF
}

# update recreates the control plane container on the image as it is now,
# after 'make image' rebuilt it under the same tag. Sites, data, and the
# other platform services are left alone; tasks pick up the new image as they
# start, since they run the same tag.
update() {
	local compose="$data_dir/platform/compose.yaml"
	[[ -f "$compose" ]] || die "no dev platform installed in $dev_dir; run 'make dev-up' first"
	local installed
	installed="$(docker compose -p "$project" -f "$compose" config --images control-plane)"
	if [[ "$installed" != "$image" ]]; then
		die "the platform runs $installed, not $image; rebuild that tag (make dev-update DEV_IMAGE=$installed)"
	fi
	docker image inspect "$image" >/dev/null 2>&1 || die "image $image not found; run 'make dev-update', which builds it"

	local old new
	old="$(docker inspect -f '{{.Image}}' "$(docker compose -p "$project" -f "$compose" ps -aq control-plane)" 2>/dev/null || true)"
	new="$(docker image inspect -f '{{.Id}}' "$image")"
	if [[ "$old" == "$new" ]]; then
		echo "dev: the control plane already runs this build"
		return
	fi
	# Compose recreates the container because the tag points at a new image.
	docker compose -p "$project" -f "$compose" up -d --no-deps --wait control-plane
	# The previous build lost its tag; remove it unless a task still uses it.
	[[ -z "$old" ]] || docker image rm "$old" >/dev/null 2>&1 || true
	echo "dev: control plane updated to ${new:7:12}"
}

# remove_sites deletes every container, network, and volume that belongs to a
# site of this platform. Their Compose files live in the data directory, which
# is about to go, so this works from labels instead.
remove_sites() {
	local containers projects p
	containers="$(docker ps -aq --filter "label=pressfleet.platform=$project")"
	if [[ -n "$containers" ]]; then
		projects="$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' $containers | sort -u)"
		# shellcheck disable=SC2086
		docker rm -f -v $containers >/dev/null
		for p in $projects; do
			[[ -n "$p" && "$p" != "$project" ]] || continue
			docker network ls -q --filter "label=com.docker.compose.project=$p" | xargs -r docker network rm >/dev/null || true
			docker volume ls -q --filter "label=com.docker.compose.project=$p" | xargs -r docker volume rm >/dev/null || true
		done
	fi
}

down() {
	# The platform's project name is the same for every install, so only touch
	# containers and volumes when this checkout's dev platform put them there.
	if [[ ! -e "$dev_dir/pressfleet.yaml" ]]; then
		echo "dev: no dev platform installed in $dev_dir; nothing to remove"
		return
	fi
	# Sites first: their networks have the platform's Traefik and MariaDB
	# attached, which blocks removing them while the platform is up.
	if [[ -f "$data_dir/platform/compose.yaml" ]]; then
		# Stop the control plane first so it doesn't react to sites vanishing.
		docker compose -p "$project" -f "$data_dir/platform/compose.yaml" stop control-plane >/dev/null 2>&1 || true
	fi
	remove_sites

	if [[ -f "$data_dir/platform/compose.yaml" ]]; then
		docker compose -p "$project" -f "$data_dir/platform/compose.yaml" down --remove-orphans
	fi

	# Let's Encrypt allows 5 certificates for the same names a week, which a
	# few up/down cycles would use up, so keep Traefik's certificate store
	# unless PURGE_CERTS=1. The next 'up' finds the certificate in it.
	local keep=traefik-acme v
	for v in $(docker volume ls -q --filter "label=com.docker.compose.project=$project"); do
		if [[ "$v" == "${project}_${keep}" && "${PURGE_CERTS:-0}" != 1 ]]; then
			echo "dev: keeping volume $v (PURGE_CERTS=1 removes it)"
			continue
		fi
		docker volume rm "$v" >/dev/null
	done

	if [[ -d "$dev_dir" ]]; then
		# Site files belong to the sites' users, so a plain rm can fail.
		rm -rf "$dev_dir" 2>/dev/null || {
			docker run --rm -v "$dev_dir:/dev-dir" alpine:3 sh -c 'rm -rf /dev-dir/* /dev-dir/.[!.]*'
			rm -rf "$dev_dir"
		}
	fi
	echo "dev: removed the dev platform"
}

status() {
	if [[ ! -e "$dev_dir/pressfleet.yaml" ]]; then
		echo "dev: no dev platform installed"
		return
	fi
	docker ps -a --filter "label=com.docker.compose.project=$project" \
		--format 'table {{.Names}}\t{{.Status}}'
	docker ps -a --filter "label=pressfleet.platform=$project" \
		--format 'table {{.Names}}\t{{.Status}}'
}

case "${1:-}" in
up) up ;;
update) update ;;
down) down ;;
status) status ;;
*) die "usage: $0 up|update|down|status" ;;
esac
