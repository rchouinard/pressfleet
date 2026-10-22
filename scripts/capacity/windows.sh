#!/usr/bin/env bash
# Set the backup window and check interval for the #98 capacity test, then
# apply them with `pressfleet upgrade` (which recreates the control plane). Run as root on the host under test:
#
#   scripts/capacity/windows.sh HH:MM [UPDATE_WINDOW_HH:MM]
#
# HH:MM is the UTC time the one-hour backup window opens; pick one shortly
# after traffic will start. Checks run daily (backup.check.interval_days: 1)
# so a check falls in the same session as the prune. With a second argument
# the update window opens then, for an optional site update rollout.
# Rerunning replaces the block this script adds.
set -euo pipefail

start="${1:?backup window start, HH:MM UTC}"
update_start="${2:-}"
config="${PRESSFLEET_CONFIG:-/etc/pressfleet/pressfleet.yaml}"
begin='# capacity-test-begin'
end='# capacity-test-end'

sed -i "/^$begin\$/,/^$end\$/d" "$config"
{
	echo "$begin"
	echo "backup:"
	echo "  schedule:"
	echo "    window_start: \"$start\""
	echo "    window_hours: 1"
	echo "  check:"
	echo "    interval_days: 1"
	if [[ -n "$update_start" ]]; then
		echo "updates:"
		echo "  window_start: \"$update_start\""
		echo "  window_hours: 1"
	fi
	echo "$end"
} >>"$config"

# The config file is bind-mounted into the control plane, so restarting it is
# enough. A development build (a version like v0.1.0-141-gabc) can't run
# `upgrade` against itself, since it sorts before v0.1.0; fall back then.
if ! pressfleet upgrade; then
	echo "upgrade refused; restarting the control plane instead" >&2
	docker restart pressfleet-control-plane-1
fi
