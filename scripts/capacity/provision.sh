#!/usr/bin/env bash
# Create the capacity test's client groups and sites through the API (#98).
# Run as root on the host under test:
#
#   ADMIN_PASSWORD_FILE=<file> scripts/capacity/provision.sh
#
# It logs in with a dashboard session (API tokens can't create tokens, #147),
# creates a full-scope API token for the other scripts, then GROUPS client
# groups of SITES_PER_GROUP sites each with that token, a few at a time, and waits for every
# create job. The token goes to $STATE_DIR/token (mode 0600); the site list
# to $STATE_DIR/sites.tsv (id, slug, preview domain, client group id).
# Rerunning creates more sites; it does not look for existing ones.
#
# Environment:
#   ADMIN_PASSWORD_FILE  file holding the admin's password (required)
#   ADMIN_EMAIL          the admin's email (default: read from the config)
#   GROUPS_N             client groups to create (default 5)
#   SITES_PER_GROUP      sites in each group (default 10)
#   PARALLEL             site create jobs to keep in flight (default 4)
#   STATE_DIR            where to keep state (default /root/capacity)
#   API_CA               PEM to verify the dashboard's certificate against
#                        (default /etc/pressfleet/pebble-roots.pem)
set -euo pipefail

: "${ADMIN_PASSWORD_FILE:?file holding the admin password}"
GROUPS_N="${GROUPS_N:-5}"
SITES_PER_GROUP="${SITES_PER_GROUP:-10}"
PARALLEL="${PARALLEL:-4}"
STATE_DIR="${STATE_DIR:-/root/capacity}"
API_CA="${API_CA:-/etc/pressfleet/pebble-roots.pem}"
config="${PRESSFLEET_CONFIG:-/etc/pressfleet/pressfleet.yaml}"

dashboard="$(awk '/dashboard_domain:/ {print $2; exit}' "$config")"
ADMIN_EMAIL="${ADMIN_EMAIL:-$(awk '/admin_email:/ {print $2; exit}' "$config" 2>/dev/null || true)}"
: "${ADMIN_EMAIL:?admin email}"
api="https://$dashboard/api/v1"

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"
jar="$STATE_DIR/cookies.txt"
csrf=""
bearer=""

# call METHOD PATH [JSON]: prints the body, fails on a non-2xx status.
call() {
	local method="$1" path="$2" body="${3:-}" out status
	local args=(-sS --cacert "$API_CA" -X "$method" -H 'Content-Type: application/json' -w '\n%{http_code}')
	if [[ -n "$bearer" ]]; then
		args+=(-H "Authorization: Bearer $bearer")
	else
		# Parallel calls would rewrite the cookie jar under each other, which
		# loses the session; only the session's own calls use it.
		args+=(-b "$jar" -c "$jar")
		[[ -n "$csrf" ]] && args+=(-H "X-CSRF-Token: $csrf")
	fi
	[[ -n "$body" ]] && args+=(--data "$body")
	out="$(curl "${args[@]}" "$api$path")"
	status="${out##*$'\n'}"
	out="${out%$'\n'*}"
	if [[ "$status" != 2* ]]; then
		echo "$method $path: HTTP $status: $out" >&2
		return 1
	fi
	printf '%s' "$out"
}

login() {
	local body
	body="$(jq -cn --arg e "$ADMIN_EMAIL" --rawfile p "$ADMIN_PASSWORD_FILE" '{email: $e, password: ($p | rtrimstr("\n"))}')"
	rm -f "$jar"
	csrf="$(call POST /auth/login "$body" | jq -er .csrf_token)"
}

# wait_job ID: waits for a terminal state; fails unless it succeeded.
wait_job() {
	local id="$1" state
	while :; do
		state="$(call GET "/jobs/$id" | jq -er .state)"
		case "$state" in
		succeeded) return 0 ;;
		failed | rollback_failed) echo "job $id: $state" >&2; return 1 ;;
		esac
		sleep 3
	done
}

login
if [[ ! -s "$STATE_DIR/token" ]]; then
	call POST /auth/tokens '{"name":"capacity-test","scope":"full"}' | jq -er .token >"$STATE_DIR/token"
	chmod 600 "$STATE_DIR/token"
fi
bearer="$(<"$STATE_DIR/token")"

tsv="$STATE_DIR/sites.tsv"
touch "$tsv"
for g in $(seq 1 "$GROUPS_N"); do
	gid="$(call POST /client-groups "$(jq -cn --arg n "capacity-$(date +%s)-$g" '{name: $n}')" | jq -er .id)"
	echo "client group $g: $gid"
	pids=()
	for s in $(seq 1 "$SITES_PER_GROUP"); do
		(
			resp="$(call POST /sites "$(jq -cn --arg g "$gid" --arg n "Capacity g$g s$s $RANDOM" --arg e "$ADMIN_EMAIL" \
				'{client_group_id: $g, name: $n, admin_email: $e}')")"
			wait_job "$(jq -er .job.id <<<"$resp")"
			jq -r '[.site.id, .site.slug, .site.preview_domain, .site.client_group_id] | @tsv' <<<"$resp" >>"$tsv"
			echo "  created $(jq -r .site.slug <<<"$resp")"
		) &
		pids+=($!)
		while [[ "$(jobs -rp | wc -l)" -ge "$PARALLEL" ]]; do sleep 1; done
	done
	for p in "${pids[@]}"; do wait "$p"; done
done
echo "$(wc -l <"$tsv") sites in $tsv"
