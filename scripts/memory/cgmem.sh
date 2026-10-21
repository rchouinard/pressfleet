#!/usr/bin/env bash
# Sample a container's memory until stopped, then print its peaks (#178).
#
#   scripts/memory/cgmem.sh <container> [interval-seconds] [stop-file]
#
# Run it as root on the Docker host (it reads the container's cgroup and its
# processes' /proc entries), start the workload, then stop it with Ctrl-C,
# SIGTERM, or by creating stop-file. It prints key=value lines:
#
#   cgroup_version         1 or 2
#   cgroup_peak_mib        the kernel's peak charge: v1 memory.max_usage_in_bytes
#                          (reset to 0 at start), v2 memory.peak (since the
#                          container started, so start from a fresh container)
#   sampled_usage_peak_mib the highest memory.usage_in_bytes / memory.current seen
#   sampled_anon_peak_mib  the highest anonymous memory seen (v1 total_rss,
#                          v2 anon): what can't be reclaimed, so what an OOM
#                          kill is really about
#   main_hwm_mib           the container's main process's VmHWM (peak RSS,
#                          reset at start); with `init: true` that is the
#                          child of docker-init. It counts the mapped binary.
#   main_anon_peak_mib     the highest RssAnon seen of the main process: for
#                          a Go program, its heap, stacks, and runtime
#   children_rss_peak_mib  the highest sum of the other processes' VmRSS
#   children_max           the most other processes alive at once
#   peak_<comm>_mib        the highest VmHWM seen of any process named <comm>,
#                          e.g. peak_docker_mib, peak_restic_mib
#   count_<comm>           how many such processes were seen
#
# Process peaks come from VmHWM read at each sample, so a short-lived
# process's growth after its last sample is missed; keep the interval small
# (the default is 0.02 s). It also stops when the container exits, keeping
# the last values it read. Development tooling: the "docker CLI only in
# provisioner/compose" rule is about the Go code, not this script.
set -euo pipefail

container="${1:?usage: $0 <container> [interval-seconds] [stop-file]}"
interval="${2:-0.02}"
stop_file="${3:-}"

init_pid="$(docker inspect -f '{{.State.Pid}}' "$container")"
[[ "$init_pid" -gt 0 ]] || { echo "container $container is not running" >&2; exit 1; }

# The main process: with `init: true` PID 1 in the container is docker-init
# and the program is its only child.
main_pid="$init_pid"
read -r comm <"/proc/$init_pid/comm"
if [[ "$comm" == docker-init || "$comm" == tini ]]; then
	for c in /proc/"$init_pid"/task/*/children; do
		read -r first _ <"$c" || true
		[[ -n "${first:-}" ]] && { main_pid="$first"; break; }
	done
fi

read -r main_comm <"/proc/$main_pid/comm"
# Resets the main process's VmHWM to its current RSS.
echo 5 >"/proc/$main_pid/clear_refs"

if [[ "$(stat -fc %T /sys/fs/cgroup)" == cgroup2fs ]]; then
	version=2
	rel="$(awk -F: '$1 == "0" { print $3 }' "/proc/$init_pid/cgroup")"
	cg="/sys/fs/cgroup$rel"
	usage_file="$cg/memory.current"
	peak_file="$cg/memory.peak"
	anon_key=anon
else
	version=1
	rel="$(awk -F: '$2 ~ /(^|,)memory(,|$)/ { print $3 }' "/proc/$init_pid/cgroup")"
	cg="/sys/fs/cgroup/memory$rel"
	usage_file="$cg/memory.usage_in_bytes"
	peak_file="$cg/memory.max_usage_in_bytes"
	anon_key=total_rss
	echo 0 >"$peak_file"
fi
[[ -r "$usage_file" ]] || { echo "cannot read $usage_file" >&2; exit 1; }

declare -A peak_by_comm count_by_comm seen_pid
usage_peak=0 anon_peak=0 main_hwm=0 main_anon_peak=0 kernel_peak=0 children_peak=0 children_max=0
stop=0
trap 'stop=1' INT TERM

kib_field() { # kib_field <file> <field>: the field's value in KiB, or 0
	local k v rest
	{ while read -r k v rest; do
		if [[ "$k" == "$2:" ]]; then echo "$v"; return; fi
	done <"$1"; } 2>/dev/null || true
	echo 0
}

while [[ $stop -eq 0 ]]; do
	[[ -n "$stop_file" && -e "$stop_file" ]] && break
	# The cgroup goes away when the container exits.
	{ read -r usage <"$usage_file" && read -r peak_now <"$peak_file"; } 2>/dev/null || break
	kernel_peak=$peak_now
	if ((usage > usage_peak)); then usage_peak=$usage; fi
	while read -r k v; do
		if [[ "$k" == "$anon_key" ]]; then if ((v > anon_peak)); then anon_peak=$v; fi; break; fi
	done 2>/dev/null <"$cg/memory.stat" || break

	children=0 nchildren=0
	while read -r pid; do
		[[ "$pid" == "$init_pid" ]] && continue
		hwm="$(kib_field "/proc/$pid/status" VmHWM)"
		if [[ "$pid" == "$main_pid" ]]; then
			if ((hwm > main_hwm)); then main_hwm=$hwm; fi
			anon="$(kib_field "/proc/$pid/status" RssAnon)"
			if ((anon > main_anon_peak)); then main_anon_peak=$anon; fi
			continue
		fi
		{ read -r c <"/proc/$pid/comm"; } 2>/dev/null || continue
		# A child the main process has forked but not yet exec'd still has
		# its name, and shares its memory (Go forks with CLONE_VM).
		[[ "$c" == "$main_comm" ]] && continue
		c="${c//[^A-Za-z0-9_]/_}"
		rss="$(kib_field "/proc/$pid/status" VmRSS)"
		children=$((children + rss)) nchildren=$((nchildren + 1))
		if [[ -z "${seen_pid[$pid:$c]:-}" ]]; then
			seen_pid[$pid:$c]=1
			count_by_comm[$c]=$((${count_by_comm[$c]:-0} + 1))
		fi
		if ((hwm > ${peak_by_comm[$c]:-0})); then peak_by_comm[$c]=$hwm; fi
	done 2>/dev/null <"$cg/cgroup.procs" || break
	if ((children > children_peak)); then children_peak=$children; fi
	if ((nchildren > children_max)); then children_max=$nchildren; fi
	sleep "$interval"
done

mib() { awk -v b="$1" -v d="$2" 'BEGIN { printf "%.1f", b / d }'; }
{ read -r peak_now <"$peak_file" && kernel_peak=$peak_now; } 2>/dev/null || true
echo "cgroup_version=$version"
echo "cgroup_peak_mib=$(mib "$kernel_peak" 1048576)"
echo "sampled_usage_peak_mib=$(mib "$usage_peak" 1048576)"
echo "sampled_anon_peak_mib=$(mib "$anon_peak" 1048576)"
echo "main_hwm_mib=$(mib "$main_hwm" 1024)"
echo "main_anon_peak_mib=$(mib "$main_anon_peak" 1024)"
echo "children_rss_peak_mib=$(mib "$children_peak" 1024)"
echo "children_max=$children_max"
for c in $(printf '%s\n' "${!peak_by_comm[@]}" | sort); do
	echo "peak_${c}_mib=$(mib "${peak_by_comm[$c]}" 1024)"
	echo "count_${c}=${count_by_comm[$c]}"
done
