#!/usr/bin/env bash
# Build a synthetic client group repository with realistic history, then
# measure the memory of the group-level restic commands the control plane
# runs against it (#178).
#
#   scripts/memory/restic-bench.sh build   <work-dir>
#   scripts/memory/restic-bench.sh measure <work-dir>
#
# Both run restic from the control plane image (IMAGE, default
# ghcr.io/rchouinard/pressfleet/control-plane:latest), so the restic version
# is the one PressFleet ships. Run them as root on the Docker host:
# measure uses scripts/memory/cgmem.sh, which reads the container's cgroup.
#
# build simulates SITES sites (default 50) of one client group backed up
# once a day for DAYS days (default 90), with no retention applied, so the
# repository ends with SITES x DAYS snapshots. Each site's data is small but
# shaped like a WordPress site's, because restic's memory follows the number
# of blobs and snapshots, not their size:
#
#   - plugins: PLUGINS_PER_SITE (12) plugin directories hard-linked from a
#     shared pool of PLUGINS (40) plugins in 3 versions of PLUGIN_FILES (350)
#     files, so sites deduplicate against each other as they do for real; a
#     site updates one plugin to its next version about once a month;
#   - uploads: UPLOADS (9000) small files over the previous three years,
#     plus UPLOADS_PER_DAY (20) a day, unique to the site;
#   - database.sql: about DUMP_MB (4) MiB, a few regions rewritten and rows
#     appended every day, as a dump changes.
#
# Files are 0.2 to 6 KiB of words drawn from a fixed vocabulary, so each is
# one compressible blob. The defaults give about 620,000 blobs and 4,500
# snapshots from 2.6 GiB of source data, in a 3.9 GiB repository. Snapshot
# times are set with restic's --time, host names and tags are the backup
# engine's (site-<id>, site:<id>, wp-core:, kind:). The build is
# deterministic for given settings; it takes about 75 minutes on 4 cores.
#
# measure runs each command in a fresh container, as the control plane does
# (no cache: Manager.exec passes --no-cache), and prints one line per command
# with cgmem.sh's peaks: restic runs under a shell, so its own peak RSS is
# peak_restic_mib, and the container's are cgroup_peak_mib and
# sampled_anon_peak_mib. The forget and prune runs
# use a hard-linked copy of the repository (restic never rewrites a file in
# place), so the built repository stays as it is and measure can be rerun.
# GOGC_VARIANTS (default "100 20") repeats prune and check with those GOGC
# values.
#
# Development tooling: the "docker CLI only in provisioner/compose" rule is
# about the Go code, not this script.
set -euo pipefail

cmd="${1:?usage: $0 build|measure <work-dir>}"
work="$(realpath -m "${2:?usage: $0 build|measure <work-dir>}")"
image="${IMAGE:-ghcr.io/rchouinard/pressfleet/control-plane:latest}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

build() {
	mkdir -p "$work"
	docker run --rm --init -v "$work:/work" \
		-e SITES="${SITES:-50}" -e DAYS="${DAYS:-90}" -e PLUGINS="${PLUGINS:-40}" \
		-e PLUGIN_FILES="${PLUGIN_FILES:-350}" -e PLUGINS_PER_SITE="${PLUGINS_PER_SITE:-12}" \
		-e UPLOADS="${UPLOADS:-9000}" -e UPLOADS_PER_DAY="${UPLOADS_PER_DAY:-20}" \
		-e DUMP_MB="${DUMP_MB:-4}" -e PARALLEL="${PARALLEL:-4}" -e START="${START:-2026-01-01}" \
		--entrypoint sh "$image" -c "$build_script"
}

# The generator. A file's content is a function of a key (its path and the
# version it last changed in), so the same file in two sites or two plugin
# versions is the same blob.
# shellcheck disable=SC2016
build_script='
set -eu
export RESTIC_REPOSITORY=/work/repo RESTIC_PASSWORD=bench RESTIC_CACHE_DIR=/work/cache
src=/work/src pool=/work/pool state=/work/state
mkdir -p "$src" "$pool" "$state"

# gen: reads "path key size" lines, writes each file with content derived
# from key.
gen() {
	awk '\''
	BEGIN { srand(7); for (i = 0; i < 4096; i++) { w = ""; n = 3 + int(rand() * 9)
		for (j = 0; j < n; j++) w = w sprintf("%c", 97 + int(rand() * 26)); words[i] = w } }
	{ path = $1; key = $2; size = $3 + 0
	  h = 0; for (i = 1; i <= length(key); i++) h = (h * 31 + index("0123456789abcdefghijklmnopqrstuvwxyz-_/.", substr(key, i, 1))) % 2147483647
	  srand(h); out = key "\n"; while (length(out) < size) { out = out words[int(rand() * 4096)] (rand() < 0.1 ? "\n" : " ") }
	  printf "%s\n", out > path; close(path) }'\''
}

if [ ! -f /work/repo/config ]; then restic init --quiet; fi

if [ ! -f "$state/pool.done" ]; then
	echo "building the plugin pool"
	p=0
	while [ $p -lt "$PLUGINS" ]; do
		v=1
		while [ $v -le 3 ]; do mkdir -p "$pool/p$p-v$v/includes" "$pool/p$p-v$v/assets"; v=$((v + 1)); done
		p=$((p + 1))
	done
	awk -v n="$PLUGINS" -v f="$PLUGIN_FILES" -v pool="$pool" '\''BEGIN { srand(11)
		for (p = 0; p < n; p++) for (i = 0; i < f; i++) {
			rev = 1; size = 200 + int(rand() * rand() * 6000)
			sub_ = (i % 3 == 0) ? "assets" : "includes"
			for (v = 1; v <= 3; v++) { if (v > 1 && rand() < 0.3) rev = v
				printf "%s/p%d-v%d/%s/f%d.php p%d/f%d/r%d %d\n", pool, p, v, sub_, i, p, i, rev, size } } }'\'' | gen
	touch "$state/pool.done"
fi

day_date() { date -u -d "$START + $1 days" +%Y-%m-%d; }

if [ ! -f "$state/sites.done" ]; then
	echo "creating $SITES sites"
	s=0
	while [ $s -lt "$SITES" ]; do
		d="$src/s$s"; rm -rf "$d"; mkdir -p "$d/wp-content/plugins" "$d/transfer"
		: >"$state/s$s.plugins"
		k=0
		while [ $k -lt "$PLUGINS_PER_SITE" ]; do
			p=$(( (s * 7 + k * 3) % PLUGINS ))
			if ! grep -q "^$p " "$state/s$s.plugins"; then
				cp -al "$pool/p$p-v1" "$d/wp-content/plugins/p$p"; echo "$p 1" >>"$state/s$s.plugins"
			fi
			k=$((k + 1))
		done
		m=0
		while [ $m -lt 36 ]; do mkdir -p "$d/wp-content/uploads/$(date -u -d "$START - $((36 - m)) months" +%Y/%m)"; m=$((m + 1)); done
		s=$((s + 1))
	done
	awk -v n="$SITES" -v u="$UPLOADS" -v src="$src" -v start="$START" '\''BEGIN { srand(13)
		split(start, a, "-"); y0 = a[1] + 0; m0 = a[2] + 0
		for (s = 0; s < n; s++) for (i = 0; i < u; i++) {
			back = 36 - int(i * 36 / u); y = y0; m = m0 - back; while (m < 1) { m += 12; y-- }
			printf "%s/s%d/wp-content/uploads/%04d/%02d/u%d.jpg s%d/u%d %d\n", src, s, y, m, i, s, i, 200 + int(rand() * rand() * 6000) } }'\'' | gen
	s=0
	while [ $s -lt "$SITES" ]; do
		awk -v s=$s -v mb="$DUMP_MB" '\''BEGIN { srand(s + 17); n = mb * 1048576 / 80
			for (i = 0; i < n; i++) printf "INSERT INTO wp_posts VALUES (%d,%d,x%08x%08x,%d);\n", i, s, rand() * 2147483647, rand() * 2147483647, rand() * 1e9 }'\'' >"$src/s$s/transfer/database.sql"
		s=$((s + 1))
	done
	touch "$state/sites.done"
fi

backup_all() { # backup_all <date>
	s=0
	while [ $s -lt "$SITES" ]; do echo $s; s=$((s + 1)); done |
		xargs -P "$PARALLEL" -I{} sh -c '\''restic backup --quiet --no-scan --host site-s{} \
			--tag site:s{},wp-core:7.1.2,kind:scheduled --time "$1 03:00:00" \
			"$2/s{}/wp-content" "$2/s{}/transfer/database.sql" >/dev/null'\'' sh "$1" "$src"
}

day=$(cat "$state/day" 2>/dev/null || echo 0)
while [ "$day" -lt "$DAYS" ]; do
	date=$(day_date "$day")
	ym=$(date -u -d "$date" +%Y/%m)
	if [ "$day" -gt 0 ]; then
		s=0
		while [ $s -lt "$SITES" ]; do
			mkdir -p "$src/s$s/wp-content/uploads/$ym"
			# About once a month, a plugin update.
			if [ $(( (day * 31 + s * 17) % 30 )) -eq 0 ]; then
				line=$(sed -n "$(( (day + s) % PLUGINS_PER_SITE + 1 ))p" "$state/s$s.plugins")
				p=${line% *} v=${line#* }
				if [ -n "$line" ] && [ "$v" -lt 3 ]; then
					rm -rf "$src/s$s/wp-content/plugins/p$p"
					cp -al "$pool/p$p-v$((v + 1))" "$src/s$s/wp-content/plugins/p$p"
					sed -i "s/^$p $v\$/$p $((v + 1))/" "$state/s$s.plugins"
				fi
			fi
			f="$src/s$s/transfer/database.sql"
			awk -v seed=$((day * 1000 + s)) '\''BEGIN { srand(seed); for (r = 0; r < 4; r++) start[r] = int(rand() * 40000) }
				{ ch = 0; for (r = 0; r < 4; r++) if (NR >= start[r] && NR < start[r] + 300) ch = 1
				  if (ch) printf "INSERT INTO wp_options VALUES (%d,%d,x%08x);\n", NR, seed, rand() * 2147483647; else print }
				END { for (i = 0; i < 100; i++) printf "INSERT INTO wp_postmeta VALUES (%d,%d,%d);\n", NR + i, seed, rand() * 1e9 }'\'' "$f" >"$f.new"
			mv "$f.new" "$f"
			s=$((s + 1))
		done
		awk -v n="$SITES" -v k="$UPLOADS_PER_DAY" -v day="$day" -v ym="$ym" -v src="$src" '\''BEGIN { srand(day + 19)
			for (s = 0; s < n; s++) for (i = 0; i < k; i++)
				printf "%s/s%d/wp-content/uploads/%s/d%d-%d.jpg s%d/d%d/%d %d\n", src, s, ym, day, i, s, day, i, 200 + int(rand() * rand() * 6000) }'\'' | gen
	fi
	backup_all "$date"
	day=$((day + 1)); echo "$day" >"$state/day"
	echo "day $day of $DAYS backed up"
done
restic stats --mode raw-data --no-lock | sed -n "/Total Blob Count/p;/Total Size/p;/Snapshots processed/p"
du -sh /work/repo /work/src
'

measure_one() { # measure_one <label> <repo dir in container> <env> <restic args...>
	local label="$1" repo="$2" env="$3"
	shift 3
	local name
	name="rbench-$$-$RANDOM"
	# The sleep lets the sampler attach before restic starts; exec keeps
	# restic the container's main process.
	# shellcheck disable=SC2086
	docker run -d --init --name "$name" -v "$work:/work" \
		-e RESTIC_REPOSITORY="$repo" -e RESTIC_PASSWORD=bench $env \
		--entrypoint sh "$image" -c 'sleep 1; start=$(date +%s.%N); restic --no-cache "$@" >/dev/null 2>/work/last.err; rc=$?; echo "rc=$rc wall_s=$(awk -v a="$start" -v b="$(date +%s.%N)" "BEGIN { printf \"%.1f\", b - a }")" >/work/last.rc; exit $rc' sh "$@" >/dev/null
	local peaks
	peaks="$("$here/cgmem.sh" "$name" 0.02 | tr '\n' ' ')"
	docker wait "$name" >/dev/null
	docker rm "$name" >/dev/null
	echo "$label $(cat "$work/last.rc") $peaks"
	if ! grep -q '^rc=0' "$work/last.rc"; then tail -5 "$work/last.err" >&2; fi
}

measure() {
	[[ -f "$work/repo/config" ]] || { echo "no repository in $work; run build first" >&2; exit 1; }
	local keep=(--keep-last 3 --keep-daily 7 --keep-weekly 4 --keep-monthly 6)
	local site=s7
	echo "# $(docker run --rm --entrypoint restic "$image" version)"
	measure_one "cat-config" /work/repo "" cat config
	measure_one "snapshots-all" /work/repo "" snapshots --json --no-lock
	measure_one "snapshots-site" /work/repo "" snapshots --json --no-lock --tag "site:$site"
	measure_one "stats-raw-data" /work/repo "" stats --mode raw-data --json
	measure_one "dump-database" /work/repo "" dump --quiet latest --host "site-$site" /work/src/$site/transfer/database.sql
	measure_one "unlock" /work/repo "" unlock
	for g in ${GOGC_VARIANTS:-100 20}; do
		measure_one "check gogc=$g" /work/repo "-e GOGC=$g" check
		measure_one "check-read-10pct gogc=$g" /work/repo "-e GOGC=$g" check --read-data-subset=10%
		measure_one "prune-nothing-forgotten gogc=$g" /work/repo "-e GOGC=$g" prune --dry-run
		rm -rf "$work/repo-copy"
		cp -al "$work/repo" "$work/repo-copy"
		measure_one "forget-default-policy gogc=$g" /work/repo-copy "-e GOGC=$g" forget --json --group-by host "${keep[@]}"
		measure_one "prune-after-forget gogc=$g" /work/repo-copy "-e GOGC=$g" prune
	done
	measure_one "check-after-prune" /work/repo-copy "" check
	measure_one "stats-after-prune" /work/repo-copy "" stats --mode raw-data --json
	measure_one "snapshots-after-prune" /work/repo-copy "" snapshots --json --no-lock
	rm -rf "$work/repo-copy"
}

case "$cmd" in
build) build ;;
measure) measure ;;
*) echo "usage: $0 build|measure <work-dir>" >&2; exit 2 ;;
esac
