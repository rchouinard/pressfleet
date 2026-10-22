#!/usr/bin/env bash
# Seed a few backup snapshots per site (#98), so the backup window finds
# repositories with history, as in real use. Each round adds a few posts to
# every site, then backs every site up through the API and waits for the jobs.
# Run as root on the host under test, after seed.sh:
#
#   scripts/capacity/seed-backups.sh
#
# Environment:
#   STATE_DIR  where provision.sh left sites.tsv and token (default /root/capacity)
#   ROUNDS     snapshots per site (default 3)
#   API_CA     PEM to verify the dashboard's certificate against
set -euo pipefail

STATE_DIR="${STATE_DIR:-/root/capacity}"
ROUNDS="${ROUNDS:-3}"
API_CA="${API_CA:-/etc/pressfleet/pebble-roots.pem}"
dash="$(awk '/dashboard_domain:/ {print $2; exit}' /etc/pressfleet/pressfleet.yaml)"
api="https://$dash/api/v1"
H="Authorization: Bearer $(<"$STATE_DIR/token")"

call() { curl -sS --cacert "$API_CA" -H "$H" -H 'Content-Type: application/json' -X "$@"; }

for round in $(seq 1 "$ROUNDS"); do
	echo "round $round: $(date -u +%T)"
	: >"$STATE_DIR/backup-jobs.txt"
	while IFS=$'\t' read -r id slug _; do
		docker exec -e HOME=/tmp "pf-$id-php-1" wp post generate --count=5 --post_content --quiet
		call POST "$api/sites/$id/backups" | jq -er .job.id >>"$STATE_DIR/backup-jobs.txt"
	done <"$STATE_DIR/sites.tsv"
	pending=$(<"$STATE_DIR/backup-jobs.txt")
	failed=0
	for j in $pending; do
		while :; do
			state="$(call GET "$api/jobs/$j" | jq -er .state)"
			case "$state" in
			succeeded) break ;;
			failed | rollback_failed) echo "job $j: $state" >&2; failed=$((failed + 1)); break ;;
			esac
			sleep 5
		done
	done
	echo "round $round done $(date -u +%T), $failed failed"
done
