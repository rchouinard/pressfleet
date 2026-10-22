#!/usr/bin/env bash
# Collectors for the #98 capacity test. Run as root on the host under test.
#
#   scripts/capacity/collect.sh start [outdir]   start sampling (default /root/capacity/run-<utc timestamp>)
#   scripts/capacity/collect.sh mark <label>     add a line to the timeline (traffic start, backup window, ...)
#   scripts/capacity/collect.sh stop             stop sampling and take the final snapshot
#
# Everything lands in the output directory, whose path is kept in
# $STATE_DIR/current-run:
#
#   timeline.log     "<utc ISO time> <label>" lines, from `mark`
#   host.csv         every INTERVAL s: MemAvailable, MemFree, swap used, memory
#                    pressure (PSI some/full avg10/avg60), load, swap in/out
#   containers.csv   every INTERVAL s, per container: cgroup memory.current,
#                    memory.peak, the cgroup's oom_kill count, and its anon
#                    and file memory (cgroup v2); anon is what can't be reclaimed
#   events.log       docker events: oom, die, restart, health_status
#   api.jsonl        every API_INTERVAL s: platform capacity, site statuses,
#                    failed job count, firing alerts
#   cgmem.txt        scripts/memory/cgmem.sh for the control plane, at the end
#   final/           snapshot from `stop`: State.OOMKilled and RestartCount of
#                    every container, kernel OOM messages, failed jobs,
#                    docker stats, versions and configuration
#
# Environment: STATE_DIR (default /root/capacity), INTERVAL (default 5),
# API_INTERVAL (default 60), API_CA (default /etc/pressfleet/pebble-roots.pem).
set -euo pipefail

STATE_DIR="${STATE_DIR:-/root/capacity}"
INTERVAL="${INTERVAL:-5}"
API_INTERVAL="${API_INTERVAL:-60}"
API_CA="${API_CA:-/etc/pressfleet/pebble-roots.pem}"
here="$(cd "$(dirname "$0")" && pwd)"
now() { date -u +%FT%TZ; }
cur() { cat "$STATE_DIR/current-run"; }

sample_host() {
	echo "ts,mem_available_kb,mem_free_kb,swap_used_kb,psi_some_avg10,psi_some_avg60,psi_full_avg10,psi_full_avg60,load1,pswpin,pswpout" >"$1/host.csv"
	while :; do
		local avail free st sf some full l1 si so
		avail="$(awk '/^MemAvailable/ {print $2}' /proc/meminfo)"
		free="$(awk '/^MemFree/ {print $2}' /proc/meminfo)"
		st="$(awk '/^SwapTotal/ {print $2}' /proc/meminfo)"
		sf="$(awk '/^SwapFree/ {print $2}' /proc/meminfo)"
		some="$(awk '/^some/ {split($3,a,"=");split($4,b,"="); print a[2]","b[2]}' /proc/pressure/memory)"
		full="$(awk '/^full/ {split($3,a,"=");split($4,b,"="); print a[2]","b[2]}' /proc/pressure/memory)"
		l1="$(cut -d' ' -f1 /proc/loadavg)"
		si="$(awk '/^pswpin/ {print $2}' /proc/vmstat)"
		so="$(awk '/^pswpout/ {print $2}' /proc/vmstat)"
		echo "$(now),$avail,$free,$((st - sf)),$some,$full,$l1,$si,$so" >>"$1/host.csv"
		sleep "$INTERVAL"
	done
}

sample_containers() {
	echo "ts,name,memory_current,memory_peak,oom_kill,anon,file" >"$1/containers.csv"
	while :; do
		local ts
		ts="$(now)"
		docker ps --no-trunc --format '{{.ID}} {{.Names}}' | while read -r id name; do
			d="/sys/fs/cgroup/system.slice/docker-$id.scope"
			[[ -d "$d" ]] || continue
			echo "$ts,$name,$(<"$d/memory.current"),$(<"$d/memory.peak"),$(awk '/^oom_kill / {print $2}' "$d/memory.events"),$(awk '/^anon / {a=$2} /^file / {f=$2} END {print a","f}' "$d/memory.stat")"
		done >>"$1/containers.csv" 2>/dev/null || true
		sleep "$INTERVAL"
	done
}

sample_api() {
	local dash api H
	dash="$(awk '/dashboard_domain:/ {print $2; exit}' /etc/pressfleet/pressfleet.yaml)"
	api="https://$dash/api/v1"
	H="Authorization: Bearer $(<"$STATE_DIR/token")"
	get() { curl -sS --max-time 20 --cacert "$API_CA" -H "$H" "$api$1" || echo null; }
	while :; do
		jq -cn --arg ts "$(now)" \
			--argjson capacity "$(get /platform/capacity)" \
			--argjson sites "$(get '/sites?limit=200' | jq -c '[.items[]? | .status] | group_by(.) | map({(.[0]): length}) | add')" \
			--argjson failed_jobs "$(get '/jobs?state=failed&limit=200' | jq -c '[.items[]? | {id, type, error}]')" \
			--argjson alerts "$(get '/alerts?limit=200' | jq -c '[.items[]? | select(.state != "resolved")]')" \
			'{ts: $ts, capacity: $capacity, sites: $sites, failed_jobs: $failed_jobs, alerts: $alerts}' >>"$1/api.jsonl" 2>/dev/null || true
		sleep "$API_INTERVAL"
	done
}

case "${1:-}" in
start)
	out="${2:-$STATE_DIR/run-$(date -u +%Y%m%dT%H%M%SZ)}"
	mkdir -p "$out/final"
	echo "$out" >"$STATE_DIR/current-run"
	rm -f "$out/pids" "$out/cgmem.stop"
	echo "$(now) collectors started" >>"$out/timeline.log"
	# Detach the samplers from the terminal, or an ssh session running this
	# command waits for them to exit.
	sample_host "$out" >/dev/null 2>&1 </dev/null & echo $! >>"$out/pids"
	sample_containers "$out" >/dev/null 2>&1 </dev/null & echo $! >>"$out/pids"
	sample_api "$out" >/dev/null 2>&1 </dev/null & echo $! >>"$out/pids"
	docker events --filter type=container \
		--filter event=oom --filter event=die --filter event=restart --filter event=health_status \
		--format '{{.TimeNano}} {{.Action}} {{.Actor.Attributes.name}} exit={{.Actor.Attributes.exitCode}}' >"$out/events.log" 2>&1 </dev/null &
	echo $! >>"$out/pids"
	cp=pressfleet-control-plane-1
	"$here/../memory/cgmem.sh" "$cp" 0.05 "$out/cgmem.stop" >"$out/cgmem.txt" 2>&1 </dev/null &
	echo $! >>"$out/pids"
	disown -a
	echo "collecting into $out"
	;;
mark)
	echo "$(now) ${2:?label}" >>"$(cur)/timeline.log"
	;;
stop)
	out="$(cur)"
	touch "$out/cgmem.stop"
	sleep 3
	while read -r p; do kill "$p" 2>/dev/null || true; done <"$out/pids"
	echo "$(now) collectors stopped" >>"$out/timeline.log"
	f="$out/final"
	docker ps -a --format '{{.Names}}' | while read -r n; do
		docker inspect -f '{{.Name}} oom_killed={{.State.OOMKilled}} restarts={{.RestartCount}} exit={{.State.ExitCode}} status={{.State.Status}}' "$n"
	done >"$f/containers-state.txt"
	journalctl -k --no-pager 2>/dev/null | grep -i -E 'out of memory|oom-kill|killed process' >"$f/kernel-oom.txt" || true
	docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.MemPerc}}' >"$f/docker-stats.txt"
	free -m >"$f/free.txt"
	cp /etc/pressfleet/pressfleet.yaml "$f/pressfleet.yaml"
	cp /var/lib/pressfleet/platform/compose.override.yaml "$f/compose.override.yaml" 2>/dev/null || true
	{ pressfleet version; uname -r; docker version --format '{{.Server.Version}}'; stat -fc %T /sys/fs/cgroup; grep MemTotal /proc/meminfo; nproc; swapon --show; } >"$f/versions.txt" 2>&1 || true
	echo "stopped; results in $out"
	;;
*)
	sed -n '2,/^set -/p' "$0" | sed '$d' >&2
	exit 2
	;;
esac
