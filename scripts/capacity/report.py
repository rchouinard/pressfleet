#!/usr/bin/env python3
"""Turn a capacity test run (#98) into the tables of the report.

    scripts/capacity/report.py RUN_DIR --k6-summary summary.json [--k6-points points.csv]
        [--limit pressfleet-traefik-1=192]

RUN_DIR is a directory collect.sh wrote, copied off the host. The k6 files come
from the load generator. The output is Markdown on stdout: the timeline, memory
per platform service and per site, host memory and pressure per phase, health,
and traffic results. It has no opinion on pass or fail beyond the criteria
listed in "Health" and "Traffic"; the prose around the tables is the author's.
Standard library only.
"""
import argparse
import collections
import csv
import datetime
import json
import os
import statistics

MIB = 1048576

# Limits under test, from control-plane/internal/platform/platform.go and
# design doc section 4.1, in MiB. Sites are not listed: php and nginx are
# summarized as distributions.
LIMITS = {
    "pressfleet-mariadb-1": 768,
    "pressfleet-redis-1": 320,
    "pressfleet-victoria-metrics-1": 256,
    "pressfleet-control-plane-1": 256,
    "pressfleet-traefik-1": 96,
    "pressfleet-socket-proxy-1": 24,
    "pressfleet-maintenance-1": 24,
}


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


def pct(values, p):
    values = sorted(values)
    return values[min(len(values) - 1, int(len(values) * p))] if values else float("nan")


def read_csv(path):
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


def timeline(run):
    marks = []
    with open(os.path.join(run, "timeline.log")) as f:
        for line in f:
            t, _, label = line.strip().partition(" ")
            if t:
                marks.append((ts(t), label))
    return marks


def phases(marks):
    """Consecutive intervals between marks, named by the mark that opens them."""
    out = []
    for (a, label), (b, _) in zip(marks, marks[1:]):
        out.append((label, a, b))
    return out


def section_timeline(marks):
    print("## Timeline (UTC)\n")
    print("| Time | Event |\n| --- | --- |")
    for t, label in marks:
        print(f"| {t:%H:%M:%S} | {label} |")
    print()


def section_containers(run):
    rows = read_csv(os.path.join(run, "containers.csv"))
    peak = collections.defaultdict(float)
    cur = collections.defaultdict(list)
    anon = collections.defaultdict(list)
    ooms = collections.defaultdict(int)
    for r in rows:
        n = r["name"]
        peak[n] = max(peak[n], float(r["memory_peak"]))
        cur[n].append(float(r["memory_current"]))
        if r.get("anon"):
            anon[n].append(float(r["anon"]))
        ooms[n] = max(ooms[n], int(r["oom_kill"] or 0))

    print("## Memory per platform service (MiB)\n")
    print("`peak` is the cgroup's `memory.peak` since the container started (it includes page cache);"
          " `max sampled` and `median` are `memory.current` samples; `max anon` is the highest anonymous"
          " memory sampled, which is what can't be reclaimed.\n")
    print("| Service | Limit | Peak | Max sampled | Median | Max anon | oom_kill |\n| --- | --- | --- | --- | --- | --- | --- |")
    for n, limit in LIMITS.items():
        if n not in cur:
            continue
        a = f"{max(anon[n]) / MIB:.0f}" if anon[n] else "n/a"
        print(f"| {n.removeprefix('pressfleet-').removesuffix('-1')} | {limit} | {peak[n] / MIB:.0f} |"
              f" {max(cur[n]) / MIB:.0f} | {statistics.median(cur[n]) / MIB:.0f} | {a} | {ooms[n]} |")
    print()

    print("## Memory per site (MiB)\n")
    print("| Container | Count | Limit | Peak: max / p95 / median | Max sampled: max / p95 / median | Max anon: max / p95 / median | oom_kill |\n| --- | --- | --- | --- | --- | --- | --- |")
    for suffix, limit in (("-php-1", "448"), ("-nginx-1", "64")):
        names = [n for n in cur if n.startswith("pf-") and n.endswith(suffix)]
        if not names:
            continue
        pk = [peak[n] / MIB for n in names]
        mx = [max(cur[n]) / MIB for n in names]
        an = [max(anon[n]) / MIB for n in names if anon[n]]

        def trio(v):
            return f"{max(v):.0f} / {pct(v, .95):.0f} / {statistics.median(v):.0f}" if v else "n/a"

        print(f"| {suffix.strip('-').replace('-1', '')} | {len(names)} | {limit} | {trio(pk)} | {trio(mx)} | {trio(an)} | {sum(ooms[n] for n in names)} |")
    print()

    tasks = [n for n in cur if "-task-" in n]
    if tasks:
        tp = [peak[n] / MIB for n in tasks]
        print(f"Task containers sampled: {len(tasks)}; highest memory.peak seen {max(tp):.0f} MiB"
              " (sampling every few seconds misses short tasks; see the control plane's cgmem output).\n")


def section_host(run, marks):
    rows = read_csv(os.path.join(run, "host.csv"))
    for r in rows:
        r["t"] = ts(r["ts"])
    print("## Host memory and pressure per phase\n")
    print("| Phase | From | To | Min MemAvailable (MiB) | Max load1 | PSI some avg10 max | PSI full avg10 max | Swap used max (MiB) |\n| --- | --- | --- | --- | --- | --- | --- | --- |")
    for label, a, b in phases(marks) + [("whole run", marks[0][0], marks[-1][0])]:
        sel = [r for r in rows if a <= r["t"] < b]
        if not sel:
            continue
        print(f"| {label} | {a:%H:%M} | {b:%H:%M} | {min(float(r['mem_available_kb']) for r in sel) / 1024:.0f} |"
              f" {max(float(r['load1']) for r in sel):.1f} | {max(float(r['psi_some_avg10']) for r in sel):.2f} |"
              f" {max(float(r['psi_full_avg10']) for r in sel):.2f} | {max(float(r['swap_used_kb']) for r in sel) / 1024:.0f} |")
    swapin = int(rows[-1]["pswpin"]) - int(rows[0]["pswpin"])
    swapout = int(rows[-1]["pswpout"]) - int(rows[0]["pswpout"])
    print(f"\nPages swapped in/out over the run: {swapin} / {swapout}.\n")


def section_health(run):
    print("## Health\n")
    final = os.path.join(run, "final")
    state = os.path.join(final, "containers-state.txt")
    if os.path.exists(state):
        lines = [l.split() for l in open(state) if l.strip()]
        oom = [l[0] for l in lines if "oom_killed=true" in l]
        restarts = [(l[0], l[2]) for l in lines if l[2] != "restarts=0"]
        print(f"- Containers at the end: {len(lines)}; OOM-killed: {len(oom)} {oom or ''}; with restarts: {len(restarts)} {restarts or ''}")
    kernel = os.path.join(final, "kernel-oom.txt")
    if os.path.exists(kernel):
        n = sum(1 for l in open(kernel) if l.strip())
        print(f"- Kernel OOM messages: {n}")
    events = os.path.join(run, "events.log")
    if os.path.exists(events):
        c = collections.Counter(l.split()[1] for l in open(events) if l.strip())
        unhealthy = [l.strip() for l in open(events) if "unhealthy" in l]
        nonzero = [l.strip() for l in open(events) if " die " in l and "exit=0" not in l and "exit=" in l]
        print(f"- Docker events: {dict(c)}; unhealthy events: {len(unhealthy)}; die with non-zero exit: {len(nonzero)}")
        for l in (unhealthy + nonzero)[:10]:
            print(f"    - `{l}`")
    api = os.path.join(run, "api.jsonl")
    if os.path.exists(api):
        recs = [json.loads(l) for l in open(api) if l.strip()]
        failed = {}
        alerts = set()
        for r in recs:
            for j in r.get("failed_jobs") or []:
                failed[j["id"]] = j
            for a in r.get("alerts") or []:
                alerts.add(json.dumps(a, sort_keys=True)[:200])
        levels = collections.Counter((r.get("capacity") or {}).get("level") for r in recs)
        sites = collections.Counter(json.dumps(r.get("sites"), sort_keys=True) for r in recs)
        print(f"- Platform capacity level over the run: {dict(levels)}")
        print(f"- Site statuses seen: {dict(sites)}")
        print(f"- Failed jobs: {len(failed)}")
        for j in list(failed.values())[:10]:
            print(f"    - {j.get('type')} {j['id']}: {j.get('error')}")
        print(f"- Distinct alerts seen not resolved: {len(alerts)}")
        for a in list(alerts)[:10]:
            print(f"    - `{a}`")
    cg = os.path.join(run, "cgmem.txt")
    if os.path.exists(cg):
        print("\nControl plane (`scripts/memory/cgmem.sh`):\n\n```")
        print(open(cg).read().strip())
        print("```")
    print()


def section_traffic(summary_path, points_path, marks):
    print("## Traffic\n")
    s = json.load(open(summary_path))
    m = s["metrics"]
    print(f"- Requests: {m['http_reqs']['values']['count']}, failed: {m['http_req_failed']['values']['rate'] * 100:.3f}%")
    print("\n| Class | avg | med | p90 | p95 | p99 | max | Threshold |\n| --- | --- | --- | --- | --- | --- | --- | --- |")
    for name, label, thr in (("http_req_duration{type:cached}", "cached", "p95 < 500 ms"),
                             ("http_req_duration{type:uncached}", "uncached", "p95 < 2000 ms"),
                             ("http_req_duration", "all", "")):
        v = m.get(name, {}).get("values")
        if v:
            print(f"| {label} | {v['avg']:.0f} | {v['med']:.0f} | {v['p(90)']:.0f} | {v['p(95)']:.0f} | {v['p(99)']:.0f} | {v['max']:.0f} | {thr} |")
    print()
    if not points_path:
        return
    by_phase = collections.defaultdict(lambda: collections.defaultdict(list))
    bad = collections.defaultdict(lambda: [0, 0])
    ph = phases(marks)
    for r in read_csv(points_path):
        if r["metric_name"] not in ("http_req_duration", "http_req_failed"):
            continue
        t = datetime.datetime.fromtimestamp(int(r["timestamp"]), datetime.timezone.utc)
        label = next((l for l, a, b in ph if a <= t < b), None)
        if label is None:
            continue
        tags = dict(kv.split("=", 1) for kv in r["extra_tags"].split("&") if "=" in kv)
        kind = tags.get("type", "other")
        if r["metric_name"] == "http_req_duration":
            by_phase[label][kind].append(float(r["metric_value"]))
        else:
            bad[label][0] += int(float(r["metric_value"]))
            bad[label][1] += 1
    print("Latency per phase (ms):\n")
    print("| Phase | Class | Requests | p50 | p95 | p99 | max | Failed |\n| --- | --- | --- | --- | --- | --- | --- | --- |")
    for label, _, _ in ph:
        for kind, v in sorted(by_phase[label].items()):
            print(f"| {label} | {kind} | {len(v)} | {pct(v, .5):.0f} | {pct(v, .95):.0f} | {pct(v, .99):.0f} | {max(v):.0f} | {bad[label][0]} of {bad[label][1]} |")
    print()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run")
    ap.add_argument("--k6-summary")
    ap.add_argument("--k6-points")
    ap.add_argument("--limit", action="append", default=[], metavar="SERVICE=MIB",
                    help="a limit that differs from the defaults, e.g. pressfleet-traefik-1=192")
    args = ap.parse_args()
    for item in args.limit:
        name, _, mib = item.partition("=")
        LIMITS[name] = int(mib)
    marks = timeline(args.run)
    section_timeline(marks)
    section_containers(args.run)
    section_host(args.run, marks)
    section_health(args.run)
    if args.k6_summary:
        section_traffic(args.k6_summary, args.k6_points, marks)


if __name__ == "__main__":
    main()
