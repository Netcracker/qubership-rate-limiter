#!/usr/bin/env python3
"""Where a rate limit decision spends its time, from a run of run-deep.sh.

    deep-analyze.py <name> [base]     # <base>/results/<name>, <base>/results/<name>-deep,
                                      # <base>/values/<name>.yaml; base defaults to this directory

Reads the JMeter results, the 5 s samples of deep-sampler.sh, and the logs,
and prints: the time of each hop over the measured part of the run; the same
per minute; and every 5 s interval with a response over 50 ms, an undecided
response, a timeout of the gateway's call, or a slow check, with what every
hop showed in that interval.
"""
import csv
import datetime
import glob
import json
import math
import os
import re
import sys
from collections import defaultdict

HERE = sys.argv[2] if len(sys.argv) > 2 else os.path.dirname(os.path.abspath(__file__))
NAME = sys.argv[1]
RES = os.path.join(HERE, "results", NAME)
DEEP = os.path.join(HERE, "results", NAME + "-deep")
LINE = re.compile(r'^(\w+)\{([^}]*)\} (\S+)(?: \d+)?$')  # cAdvisor appends a timestamp
LABEL = re.compile(r'(\w+)="([^"]*)"')


def labels(s):
    return dict(LABEL.findall(s))


# ---------- JMeter samples ----------
gen_log = open(os.path.join(RES, "generator.log")).read()
start = int(re.search(r"the schedule starts at (\d+) ms", gen_log).group(1))
values = open(os.path.join(HERE, "values", NAME + ".yaml")).read()
steps = [(float(r), int(s)) for r, s in re.findall(r"\{rate: ([\d.]+), seconds: (\d+)\}", values)]
warmup = int(re.search(r"warmupSeconds: (\d+)", values).group(1))
m0 = start + warmup * 1000
m1 = start + sum(s for _, s in steps) * 1000

samples = []  # (start ms, elapsed ms, outcome, code)
with open(os.path.join(RES, "results", "results.jtl")) as f:
    rows = csv.DictReader(f)
    for r in rows:
        if not r.get("kind"):
            continue
        t, e, code = int(r["timeStamp"]), int(r["elapsed"]), r["responseCode"]
        rem = r.get("remaining", "")
        if code == "429":
            o = "refused"
        elif rem and rem != "-":
            o = "admitted"
        elif code.isdigit():
            o = "undecided"
        else:
            o = "failed"
        samples.append((t, e, o, code))
samples.sort()


def q(vals, p):
    if not vals:
        return float("nan")
    vals = sorted(vals)
    return vals[max(0, math.ceil(p * len(vals)) - 1)]


# ---------- 5 s samples ----------
def files(kind):
    out = []
    for p in glob.glob(os.path.join(DEEP, kind, "*")):
        try:
            out.append((int(os.path.basename(p)), p))
        except ValueError:
            pass
    return sorted(out)


def read_prom(path, prefix_strip=False):
    """{(name, frozenset(labels)): value}; svc lines carry a pod prefix."""
    out = {}
    for line in open(path):
        line = line.strip()
        pod = None
        if prefix_strip:
            pod, _, line = line.partition(" ")
        m = LINE.match(line)
        if not m:
            parts = line.split(" ")
            if len(parts) == 2 and not parts[0].endswith("}"):
                name, lab, val = parts[0], {}, parts[1]
            else:
                continue
        else:
            name, lab, val = m.group(1), labels(m.group(2)), m.group(3)
        if pod:
            lab["pod"] = pod
        try:
            out[(name, frozenset(lab.items()))] = float(val)
        except ValueError:
            pass
    return out


def hist(snap, base, match):
    """Summed cumulative histogram {le: count}, sum, count over series whose labels satisfy match."""
    b, s, c = defaultdict(float), 0.0, 0.0
    for (name, lab), v in snap.items():
        d = dict(lab)
        if not match(d):
            continue
        if name == base + "_bucket":
            le = d["le"]
            b[float("inf") if le == "+Inf" else float(le)] += v
        elif name == base + "_sum":
            s += v
        elif name == base + "_count":
            c += v
    return dict(b), s, c


def counter(snap, base, match):
    return sum(v for (name, lab), v in snap.items() if name == base and match(dict(lab)))


def hdiff(a, b):
    """Growth of a histogram between two samples; a pod restart is not handled (none expected)."""
    ba, sa, ca = a
    bb, sb, cb = b
    return {k: bb[k] - ba.get(k, 0) for k in bb}, sb - sa, cb - ca


def hq(h, p, truncated=False):
    """Quantile from a cumulative histogram: interpolated, or for Envoy's truncated ms the bound it lies under."""
    b, _, c = h
    if c <= 0:
        return float("nan")
    rank = p * c
    lo, below = 0.0, 0.0
    for le in sorted(b):
        if b[le] >= rank:
            if truncated:
                return math.floor(le) + 1 if le != float("inf") else float("inf")
            if le == float("inf"):
                return lo
            return lo + (le - lo) * (rank - below) / max(b[le] - below, 1e-12)
        lo, below = le, b[le]
    return lo


def over(h, bound):
    b, _, c = h
    keys = [k for k in b if k >= bound and k != float("inf")]
    if not keys:
        return float("nan")
    return c - b[min(keys)]


rls = lambda d: "9000||rate-limiter" in d.get("cluster_name", "")
backend = lambda d: "loadtest-backend" in d.get("cluster_name", "")
ingress = lambda d: d.get("http_conn_manager_prefix", "").startswith("outbound_0.0.0.0_8080")
domain = lambda d: d.get("domain") == "gateway.public"
anyl = lambda d: True

gw = [(t, read_prom(p)) for t, p in files("gw")]
svc = [(t, read_prom(p, prefix_strip=True)) for t, p in files("svc")]


def gwview(snap):
    return {
        "rls": hist(snap, "envoy_cluster_upstream_rq_time", rls),
        "backend": hist(snap, "envoy_cluster_upstream_rq_time", backend),
        "total": hist(snap, "envoy_http_downstream_rq_time", ingress),
        "timeouts": counter(snap, "envoy_cluster_upstream_rq_timeout", rls),
        "rlerror": counter(snap, "envoy_cluster_ratelimit_public_gateway_error", anyl),
        "failopen": counter(snap, "envoy_cluster_ratelimit_public_gateway_failure_mode_allowed", anyl),
        "overlimit": counter(snap, "envoy_cluster_ratelimit_public_gateway_over_limit", anyl),
        "cxfail": counter(snap, "envoy_cluster_upstream_cx_connect_fail", rls),
        "cxactive": counter(snap, "envoy_cluster_upstream_cx_active", rls),
    }


def svcview(snap):
    gc_max = max((v for (n, lab), v in snap.items() if n == "go_gc_duration_seconds" and dict(lab).get("quantile") == "1"),
                 default=0.0)
    return {
        "check": hist(snap, "ratelimit_check_duration_seconds", domain),
        "store": hist(snap, "ratelimit_store_roundtrip_seconds", domain),
        "storeerr": counter(snap, "ratelimit_store_errors_total", anyl),
        "unavail": counter(snap, "ratelimit_checks_total", lambda d: d.get("verdict") == "unavailable"),
        "gc_count": counter(snap, "go_gc_duration_seconds_count", anyl),
        "gc_sum": counter(snap, "go_gc_duration_seconds_sum", anyl),
        "gc_max": gc_max,
        "cpu": counter(snap, "process_cpu_seconds_total", anyl),
    }


gwv = [(t, gwview(s)) for t, s in gw]
svv = [(t, svcview(s)) for t, s in svc]


# ---------- Redis ----------
def read_redis(path):
    sec, out = None, {"slow": [], "hist": {}}
    lines = open(path).read().splitlines()
    for i, line in enumerate(lines):
        if line.startswith("## "):
            sec = line[3:]
            continue
        if sec == "commandstats" and line.startswith("cmdstat_evalsha:"):
            kv = dict(x.split("=") for x in line.split(":", 1)[1].split(","))
            out["calls"], out["usec"] = float(kv["calls"]), float(kv["usec"])
            out["failed"] = float(kv.get("failed_calls", 0))
        elif sec == "cpu" and ":" in line:
            k, v = line.split(":", 1)
            if k in ("used_cpu_sys", "used_cpu_user", "used_cpu_sys_main_thread", "used_cpu_user_main_thread"):
                out[k] = float(v)
        elif sec == "clients" and line.startswith("connected_clients:"):
            out["clients"] = float(line.split(":")[1])
        elif sec == "slowlog" and line.lower() == "evalsha" and i >= 3:
            try:
                out["slow"].append((int(lines[i - 2]), int(lines[i - 1])))  # unix s, usec
            except ValueError:
                pass
    # LATENCY HISTOGRAM in raw output: "evalsha" "calls" N "histogram_usec" b1 c1 b2 c2 ...
    sec_lines = []
    capture = False
    for line in lines:
        if line == "## histogram":
            capture = True
            continue
        if line.startswith("## "):
            capture = False
        if capture and line:
            sec_lines.append(line)
    if "histogram_usec" in sec_lines:
        j = sec_lines.index("histogram_usec") + 1
        nums = []
        while j < len(sec_lines) and re.fullmatch(r"-?\d+", sec_lines[j]):
            nums.append(int(sec_lines[j]))
            j += 1
        out["hist"] = {nums[k]: nums[k + 1] for k in range(0, len(nums) - 1, 2)}
    return out


rv = [(t, read_redis(p)) for t, p in files("redis")]


# ---------- CPU throttling ----------
def read_cadv(path):
    out = defaultdict(float)
    for line in open(path):
        m = LINE.match(line.strip())
        if not m:
            continue
        name, lab, v = m.group(1), labels(m.group(2)), float(m.group(3))
        key = (lab.get("namespace", ""), lab.get("pod", ""), lab.get("container", ""))
        out[(name, key)] += v
    return out


# cadv2 is the throttling sampler that also covers the service's container;
# cadv, the first sampler's, is the fallback.
cv = [(t, read_cadv(p)) for t, p in files("cadv2")] or [(t, read_cadv(p)) for t, p in files("cadv")]


def group_of(key):
    ns, pod, cont = key
    if cont in ("rate-limiter", "service"):
        return "rls"
    if pod.startswith("public-gateway-istio"):
        return "gateway"
    if pod.startswith("ztunnel"):
        return "ztunnel"
    if cont == "redis":
        return "redis"
    if cont == "ztunnel":
        return "ztunnel"
    if cont == "generator":
        return "generator"
    if "backend" in pod:
        return "backend"
    return None


def throttle(a, b):
    """{group: (throttled periods, periods, cpu seconds)} between two samples."""
    out = defaultdict(lambda: [0.0, 0.0, 0.0])
    for (name, key), v in b.items():
        g = group_of(key)
        if not g:
            continue
        d = v - a.get((name, key), 0.0)
        if d < 0:
            continue
        if name == "container_cpu_cfs_throttled_periods_total":
            out[g][0] += d
        elif name == "container_cpu_cfs_periods_total":
            out[g][1] += d
        elif name == "container_cpu_usage_seconds_total":
            out[g][2] += d
    return out


# ---------- the runner's CPU ----------
# The cpu line of /proc/stat counts user, nice, system, idle, iowait, irq, softirq, and steal ticks, then guest time,
# which user and nice already include. The report sums user with nice, and system with irq and softirq.
hv = []
try:
    for line in open(os.path.join(DEEP, "host-cpu.log")):
        p = line.split()
        if len(p) >= 10 and p[1] == "cpu":
            hv.append((int(p[0]), [int(x) for x in p[2:10]]))
except FileNotFoundError:
    pass
lscpu = {}
try:
    for line in open(os.path.join(DEEP, "lscpu.txt")):
        k, sep, v = line.partition(":")
        if sep:
            lscpu.setdefault(k.strip(), v.strip())
except FileNotFoundError:
    pass


# ---------- windows ----------
def at(series, t0, t1):
    """First and last sample inside [t0, t1]."""
    inside = [x for x in series if t0 <= x[0] <= t1]
    if len(inside) < 2:
        return None
    return inside[0], inside[-1]


def window(t0, t1):
    w = {}
    g = at(gwv, t0, t1)
    if g:
        (_, a), (_, b) = g
        for k in ("rls", "backend", "total"):
            w["gw_" + k] = hdiff(a[k], b[k])
        for k in ("timeouts", "rlerror", "failopen", "overlimit", "cxfail"):
            w["gw_" + k] = b[k] - a[k]
    s = at(svv, t0, t1)
    if s:
        (_, a), (_, b) = s
        w["check"], w["store"] = hdiff(a["check"], b["check"]), hdiff(a["store"], b["store"])
        for k in ("storeerr", "unavail", "gc_count", "gc_sum", "cpu"):
            w[k] = b[k] - a[k]
        w["gc_max"] = max(v["gc_max"] for t, v in svv if t0 <= t <= t1)
    r = at(rv, t0, t1)
    if r:
        (ta, a), (tb, b) = r
        if "calls" in a and "calls" in b:
            w["r_calls"], w["r_usec"] = b["calls"] - a["calls"], b["usec"] - a["usec"]
            w["r_failed"] = b.get("failed", 0) - a.get("failed", 0)
        w["r_cpu"] = sum(b.get(k, 0) - a.get(k, 0) for k in ("used_cpu_sys", "used_cpu_user"))
        w["r_secs"] = (tb - ta) / 1000
        ha, hb = a.get("hist", {}), b.get("hist", {})
        w["r_hist"] = {k: hb[k] - rcum(ha, k) for k in hb}
        w["r_slow"] = [x for t, v in rv if t0 < t <= t1 for x in v["slow"]]
    c = at(cv, t0, t1)
    if c:
        (_, a), (_, b) = c
        w["thr"] = throttle(a, b)
    h = at(hv, t0, t1)
    if h:
        (_, a), (_, b) = h
        d = [y - x for x, y in zip(a, b)]
        if sum(d) > 0:
            w["host"] = [x / sum(d) * 100 for x in d]  # % of all the runner's CPU time, in /proc/stat's order
    return w


def rcum(h, bound):
    """Redis lists a cumulative count only at the buckets where it grows: the count at bound is the one of the
    largest listed bucket at or under it."""
    keys = [k for k in h if k <= bound]
    return h[max(keys)] if keys else 0


def rhist_q(h, p):
    """Quantile bound from Redis's cumulative latency histogram, in usec."""
    if not h:
        return float("nan")
    total = max(h.values())
    rank = p * total
    for k in sorted(h):
        if h[k] >= rank:
            return k
    return max(h)


def fmt(x, d=2):
    if x is None or (isinstance(x, float) and math.isnan(x)):
        return "-"
    if x == float("inf"):
        return "inf"
    return f"{x:.{d}f}"


# ---------- report ----------
inrun = [s for s in samples if m0 <= s[0] < m1]
el = [s[1] for s in inrun]
outc = defaultdict(int)
for s in inrun:
    outc[s[2]] += 1
print(f"# {NAME}: {len(inrun)} samples in the measured part, {(m1 - m0) / 1000:.0f} s")
print(f"outcomes: {dict(outc)}; non-200/429 codes: "
      f"{sorted({s[3] for s in inrun if s[3] not in ('200', '429')})}")
print(f"over 50 ms at the client: {sum(1 for e in el if e > 50)}; over 20 ms: {sum(1 for e in el if e > 20)}")

W = window(m0, m1)


def line_hist(label, h, unit_ms=1.0, truncated=False, bound=50):
    b, s, c = h
    qs = [hq(h, p, truncated) * unit_ms for p in (0.5, 0.9, 0.99, 0.999)]
    mean = s / c * unit_ms if c else float("nan")
    ov = over(h, bound / unit_ms)
    print(f"| {label} | {c:.0f} | {fmt(qs[0])} | {fmt(qs[1])} | {fmt(qs[2])} | {fmt(qs[3])} | {fmt(mean)} | {fmt(ov, 0)} |")


print("\n| Hop | Count | p50 ms | p90 ms | p99 ms | p99.9 ms | mean ms | over 50 ms |")
print("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |")
print(f"| client (JMeter, whole request) | {len(el)} | {q(el, .5)} | {q(el, .9)} | {q(el, .99)} | {q(el, .999)} | "
      f"{fmt(sum(el) / len(el))} | {sum(1 for e in el if e > 50)} |")
if "gw_total" in W:
    line_hist("gateway, whole request (Envoy, bound)", W["gw_total"], truncated=True)
    line_hist("gateway -> RLS call (Envoy, bound)", W["gw_rls"], truncated=True)
    line_hist("gateway -> backend (Envoy, bound)", W["gw_backend"], truncated=True)
if "check" in W:
    line_hist("RLS check, whole handler (service)", W["check"], unit_ms=1000)
    line_hist("RLS -> Redis round trip (service)", W["store"], unit_ms=1000)
if W.get("r_calls"):
    rh = W["r_hist"]
    print(f"| Redis evalsha execution (server) | {W['r_calls']:.0f} | <={fmt(rhist_q(rh, .5) / 1000, 3)} | "
          f"<={fmt(rhist_q(rh, .9) / 1000, 3)} | <={fmt(rhist_q(rh, .99) / 1000, 3)} | "
          f"<={fmt(rhist_q(rh, .999) / 1000, 3)} | {fmt(W['r_usec'] / W['r_calls'] / 1000, 3)} | "
          f"{sum(1 for _, u in W['r_slow'] if u > 50000)} |")
print("\nEnvoy truncates to whole ms: its quantiles are the bound the value lies under, and its mean is low by up to"
      " 1 ms. The service's quantiles interpolate inside buckets of 0.5, 1, 2.5, 5, 10, 25, 50 ms. Redis's histogram"
      " gives power-of-two bounds.")
if "gw_timeouts" in W:
    print(f"\ngateway: timeouts of the RLS call {W['gw_timeouts']:.0f}, filter errors {W['gw_rlerror']:.0f}, "
          f"fail-open {W['gw_failopen']:.0f}, over_limit {W['gw_overlimit']:.0f}, RLS connect failures {W['gw_cxfail']:.0f}")
if "storeerr" in W:
    print(f"service: store errors {W['storeerr']:.0f}, unavailable verdicts {W['unavail']:.0f}, GCs {W['gc_count']:.0f} "
          f"(total pause {W['gc_sum'] * 1000:.1f} ms, longest {W['gc_max'] * 1000:.2f} ms), CPU {W['cpu']:.1f} s "
          f"({W['cpu'] / ((m1 - m0) / 1000) * 100:.1f}% of a core, both pods)")
if W.get("r_calls") is not None:
    print(f"redis: evalsha {W['r_calls']:.0f}, failed {W.get('r_failed', 0):.0f}, CPU {W['r_cpu']:.1f} s "
          f"({W['r_cpu'] / W['r_secs'] * 100:.1f}% of a core), slowlog over 2 ms: {len(W['r_slow'])}, "
          f"longest {max((u for _, u in W['r_slow']), default=0) / 1000:.1f} ms")
if "thr" in W:
    print("\n| Container | CPU, % of a core | throttled periods | of periods |")
    print("| --- | ---: | ---: | ---: |")
    for g, (th, per, cpu) in sorted(W["thr"].items()):
        print(f"| {g} | {cpu / ((m1 - m0) / 1000) * 100:.1f} | {th:.0f} | {fmt(th / per * 100 if per else float('nan'), 2)}% |")
if lscpu or "host" in W:
    h = W.get("host")
    print(f"\nrunner: {lscpu.get('Model name', 'unknown CPU model')}, {lscpu.get('CPU(s)', '?')} CPUs, "
          f"hypervisor {lscpu.get('Hypervisor vendor', 'not reported')}"
          + (f"; of all its CPU time user {h[0] + h[1]:.1f}%, system {h[2] + h[5] + h[6]:.1f}%, iowait {h[4]:.1f}%, "
             f"steal {h[7]:.2f}%, idle {h[3]:.1f}%" if h else ""))

# per minute
print("\n## Per minute\n")
print("| Minute | client p99 | client max | >50 ms | undecided | gw RLS p99 < | RLS timeouts | check p99 | check max bucket | "
      "store p99 | redis mean us | slow>2ms | GC max ms | RLS throttled | gw throttled | node CPU % | runner steal % |")
print("| --- " * 17 + "|")
node = []
try:
    for line in open(os.path.join(DEEP, "node-cpu.log")):
        p = line.split()
        if len(p) >= 2:
            node.append((int(p[0]), float(p[1].rstrip("%"))))
except FileNotFoundError:
    pass


def maxbucket(h, unit=1000):
    b, _, c = h
    prev = 0.0
    top = None
    for le in sorted(b):
        if b[le] > prev:
            top = le
        prev = b[le]
    return top * unit if top is not None and top != float("inf") else (float("inf") if top else float("nan"))


t = m0
while t < m1:
    t2 = min(t + 60000, m1)
    mm = [s for s in inrun if t <= s[0] < t2]
    e = [s[1] for s in mm]
    w = window(t - 2500, t2 + 2500)
    thr = w.get("thr", {})
    nd = [c for ts, c in node if t <= ts < t2]
    print(f"| {(t - m0) // 60000:>3} | {q(e, .99)} | {max(e) if e else '-'} | {sum(1 for x in e if x > 50)} | "
          f"{sum(1 for s in mm if s[2] == 'undecided')} | {fmt(hq(w['gw_rls'], .99, True), 0) if 'gw_rls' in w else '-'} | "
          f"{fmt(w.get('gw_timeouts'), 0)} | {fmt(hq(w['check'], .99) * 1000) if 'check' in w else '-'} | "
          f"{fmt(maxbucket(w['check']), 1) if 'check' in w else '-'} | {fmt(hq(w['store'], .99) * 1000) if 'store' in w else '-'} | "
          f"{fmt(w['r_usec'] / w['r_calls'], 0) if w.get('r_calls') else '-'} | {len(w.get('r_slow', []))} | "
          f"{fmt(w.get('gc_max', float('nan')) * 1000)} | "
          f"{fmt(thr['rls'][0] / thr['rls'][1] * 100 if 'rls' in thr and thr['rls'][1] else float('nan'))}% | "
          f"{fmt(thr['gateway'][0] / thr['gateway'][1] * 100 if 'gateway' in thr and thr['gateway'][1] else float('nan'))}% | "
          f"{fmt(max(nd) if nd else float('nan'), 0)} | {fmt(w['host'][7] if 'host' in w else float('nan'))} |")
    t = t2

# incidents
print("\n## Intervals with a slow or failed decision\n")
bad = sorted({s[0] for s in inrun if s[1] > 50 or s[2] in ("undecided", "failed")})
gw_ts = [t for t, _ in gwv]
seen = set()
for ts in bad:
    # the 5 s sample interval holding ts
    prev = max((x for x in gw_ts if x <= ts), default=None)
    nxt = min((x for x in gw_ts if x > ts), default=None)
    if prev is None or nxt is None or prev in seen:
        continue
    seen.add(prev)
    w = window(prev, nxt)
    mm = [s for s in inrun if prev <= s[0] < nxt]
    slow = [s for s in mm if s[1] > 50 or s[2] in ("undecided", "failed")]
    thr = w.get("thr", {})
    nd = [c for t, c in node if prev - 7500 <= t <= nxt + 7500]
    when = datetime.datetime.fromtimestamp(prev / 1000, datetime.timezone.utc).strftime("%H:%M:%S")
    print(f"- {when}: {len(slow)} of {len(mm)} requests slow or undecided "
          f"(max {max(s[1] for s in mm)} ms, undecided {sum(1 for s in mm if s[2] == 'undecided')}); "
          f"gw RLS call over 50 ms {fmt(over(w['gw_rls'], 50), 0) if 'gw_rls' in w else '-'}, "
          f"timeouts {fmt(w.get('gw_timeouts'), 0)}, backend over 50 ms {fmt(over(w['gw_backend'], 50), 0) if 'gw_backend' in w else '-'}; "
          f"check max bucket {fmt(maxbucket(w['check']), 1) if 'check' in w else '-'} ms, "
          f"store max bucket {fmt(maxbucket(w['store']), 1) if 'store' in w else '-'} ms, store errors {fmt(w.get('storeerr'), 0)}; "
          f"redis slow>2ms {len(w.get('r_slow', []))}"
          f"{' max ' + fmt(max(u for _, u in w['r_slow']) / 1000, 1) + ' ms' if w.get('r_slow') else ''}; "
          f"GC max {fmt(w.get('gc_max', float('nan')) * 1000)} ms; throttled: "
          + ", ".join(f"{g} {th:.0f}/{per:.0f}" for g, (th, per, _) in sorted(thr.items()) if th)
          + f"; node CPU {fmt(max(nd) if nd else float('nan'), 0)}%"
          + f"; runner steal {fmt(w['host'][7] if 'host' in w else float('nan'))}%")
