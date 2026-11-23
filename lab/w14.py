#!/usr/bin/env python3
# lab/sw W14 (SW-RESULTS.md, W14): reads go/swload_test.go's lines of both builds and prints the tables and
# clauses 1 to 5 as the rule words them, the reach, and what the rule reports. Usage: w14.py <outputs dir>
# <default exact> <tfdrip exact> (the outputs dir holds where-, check-, l1- and l2-{default,drip}.txt and
# race.txt). A clause with nothing to read is NOTHING READ, and one with points missing and none failing is
# NOTHING READ (in part): never a pass. Exits 1 if a clause does not hold, or a file lacks its last line.
import re
import sys
from collections import defaultdict

O, EXACT_DEF, EXACT_DRIP = sys.argv[1], sys.argv[2], sys.argv[3]
WINS = [1, 5, 15, 30, 60]
BUILDS = ("default", "tfdrip")
FILE = {"default": "default", "tfdrip": "drip"}
SEEDS = ["9,9", "2026,41", "2026,42", "2026,43", "2026,44"]
MODES = ["plain", "bg", "ct"]
REPS = {"plain": 1, "ct": 1, "bg": 5}
LOADS = ["none", "1.00", "1.15"]
CAPS = ["1739", "1000", "500"]
STATES = ["table", "past", "never", "dead", "stash", "dropped", "shed", "wait room", "CT wait"]
SRCS = ["hit", "stash", "wait", "dropped", "floor", "shed", "new"]
AWAY = ["stash", "wait", "dropped", "floor", "shed"]  # a restore from anywhere but the table (or a new life)
CTRS = ["evictions", "relocations", "shed", "lost", "floored", "folded", "waited", "promoted", "hotrefused"]
PER_LEG = len(SEEDS) * 3 * sum(REPS.values())  # 105

bad = 0


def read(name):
    try:
        return open(f"{O}/{name}", encoding="utf-8").read().splitlines()
    except FileNotFoundError:
        print(f"{name}: MISSING")
        return []


def kv(s):
    return dict(x.split("=", 1) for x in s.split())


def exact_file(path):
    e = {}
    for line in open(path, encoding="utf-8"):
        m = re.match(r"velocity window=(\d+)s abs_error_sum=(\d+) exact_sum=(\d+)", line)
        if m:
            e[int(m.group(1))] = (int(m.group(2)), int(m.group(3)))
    return e


def verdict(n, ok, read_any, detail, missing=()):
    global bad
    if not read_any:
        v = "NOTHING READ"
    elif not ok:
        v = "FAILS"
    elif missing:
        v = "NOTHING READ (in part)"
        detail += f"; not read: {list(missing)}"
    else:
        v = "HOLDS"
    if v != "HOLDS":
        bad = 1
    print(f"W14 clause {n}: {v}: {detail}")


def avg(rs, k):
    return sum(float(r[k]) for r in rs) / len(rs)


def spread(rs, k):
    v = [float(r[k]) for r in rs]
    return max(v) - min(v)


runs, done = {}, {}
for leg in ("1", "2"):
    for b in BUILDS:
        ls = read(f"l{leg}-{FILE[b]}.txt")
        runs[leg, b] = [kv(m.group(1)) for m in (re.search(r"W14 run: (.*)$", x) for x in ls) if m]
        done[f"l{leg}-{b}"] = any("W14 done:" in x for x in ls)
race = read("race.txt")
done["race"] = any("W14 done:" in x for x in race)


def group(leg, b):
    g = defaultdict(list)
    for r in runs[leg, b]:
        g[r["seed"], r["load"] if leg == "1" else r["cap"], r["mode"]].append(r)
    return g


G = {(leg, b): group(leg, b) for leg in ("1", "2") for b in BUILDS}

# clause 1: sight
c1, c1ok, c1read = [], True, False
for b in BUILDS:
    ls = read(f"where-{FILE[b]}.txt")
    seen = {}
    for x in ls:
        m = re.search(r'W14 where (.+?): where=(\S+) slot=(-?\d+) part=(\S+) log=(\S+) sign="[^"]*" signok=(\S+)', x)
        if m:
            seen[m.group(1)] = m.groups()
    passed = all(any(f"--- PASS: {t}" in x for x in ls) for t in ("TestShortWindowLoadWhere", "TestShortWindowLoadSource"))
    missing = [s for s in STATES if s not in seen]
    c1read = c1read or bool(seen)
    c1ok = c1ok and passed and not missing and all(seen[s][5] == "true" for s in seen)
    c1.append(f"{b}: {len(seen)} states, missing {missing or 'none'}, tests {'pass' if passed else 'NOT PASSED'}")
verdict(1, c1ok, c1read, "; ".join(c1))

# clause 2: no load
c2, c2read, c2ok, c2miss = [], False, True, []
for b, path in (("default", EXACT_DEF), ("tfdrip", EXACT_DRIP)):
    ex = exact_file(path)
    ctl = {r["seed"]: r for r in runs["1", b] if r["load"] == "none" and r["mode"] == "plain"}
    c2miss += [f"{b} {s}" for s in SEEDS if s not in ctl]
    mism = {s: r["mism"] for s, r in ctl.items() if r["mism"] != "0"}
    sums = "not read"
    if "9,9" in ctl:
        r = ctl["9,9"]
        diff = [w for w in WINS if (int(r[f"es{w}"]), int(r[f"xs{w}"])) != ex.get(w)]
        sums = "equal" if not diff else f"DIFFER at {diff}"
        c2ok = c2ok and not diff
    c2read = c2read or bool(ctl)
    c2ok = c2ok and not mism
    c2.append(f"{b}: {len(ctl)} seeds, mism {mism or 'all 0'}, (9,9) sums {sums}")
verdict(2, c2ok, c2read, "; ".join(c2), c2miss)

# clause 3: the harness
c3, c3ok, c3read = [], True, False
for b in BUILDS:
    ls = read(f"check-{FILE[b]}.txt")
    ch = [kv(m.group(1)) for m in (re.search(r"W14 check churn: (.*)$", x) for x in ls) if m]
    ck = [kv(m.group(1)) for m in (re.search(r"W14 check: (.*)$", x) for x in ls) if m]
    c3read = c3read or bool(ch or ck)
    chok = len(ch) == 2 and all(r["mism"] == "0" and r["prefix"] == r["four"] and r["prefix"] != "0"
                                and r["velkeys"] == "0" and r["lastok"] == "true" for r in ch)
    ckok = len(ck) == 4 and all(r["offmism"] == "0" and (r["mode"] != "ct" or r["replaymism"] == "0") for r in ck)
    c3ok = c3ok and chok and ckok and any("W14 done:" in x for x in ls)
    c3.append(f"{b}: churn {[(r['load'], r['mism'], r['prefix'] == r['four'], r['velkeys']) for r in ch]}, "
              f"watcher off and replay {[(r['leg'], r['mode'], r['offmism'], r['replaymism']) for r in ck]}")
for leg in ("1", "2"):
    for b in BUILDS:
        n = len(runs[leg, b])
        c3ok = c3ok and n == PER_LEG and done[f"l{leg}-{b}"]
        c3.append(f"leg {leg} {b}: {n} runs of {PER_LEG}{'' if done[f'l{leg}-{b}'] else ', NO DONE LINE'}")
races = sum("WARNING: DATA RACE" in x for x in race)
c3ok = c3ok and races == 0 and done["race"]
c3.append(f"-race: {races} data races{'' if done['race'] else ', NO DONE LINE'}")
verdict(3, c3ok, c3read, "; ".join(c3))


def away(rs):
    return sum(int(r[f"src_{x}"]) for r in rs for x in AWAY) + sum(int(r["left"]) for r in rs)


# clause 4: the gain under the scorecard's load
c4, c4ok, c4read, c4miss = [], True, False, []
print("\nLeg 1 (mean of the five windows; Background over its repeats): seed load mode | default tfdrip gain | half of no load's")
for s in SEEDS:
    for m in MODES:
        base = [G["1", b].get((s, "none", m)) for b in BUILDS]
        for ld in LOADS:
            cur = [G["1", b].get((s, ld, m)) for b in BUILDS]
            if not all(cur) or (ld != "none" and not all(base)):
                c4miss.append(f"{s} {ld} {m}")
                continue
            d, t = avg(cur[0], "mean"), avg(cur[1], "mean")
            line = f"  {s:8} {ld:5} {m:5} | {d:.4f} {t:.4f} {d - t:+.4f}"
            if ld != "none":
                g0 = avg(base[0], "mean") - avg(base[1], "mean")
                ok = d - t >= 0.5 * g0
                c4read = True
                c4ok = c4ok and ok
                line += f" | {0.5 * g0:.4f} {'kept' if ok else 'LOST'}"
                if not ok:
                    c4.append(f"{s} {ld} {m}: gain {d - t:.4f} < {0.5 * g0:.4f}")
            print(line)
kept = []
for ld in LOADS[1:]:
    ms = [m for m in MODES if all(G["1", b].get((s, ld, m)) for b in BUILDS for s in SEEDS)
          and away([r for b in BUILDS for s in SEEDS for r in G["1", b][s, ld, m]]) == 0]
    if ms:
        kept.append(f"judged on keys the table kept: {ld} in {', '.join(ms)}")
verdict(4, c4ok, c4read, "; ".join((c4 or ["every seed, load and mode read keeps at least half the gain"] * c4read) + kept), c4miss)

# clause 5: no harm where the velocity keys are the load
c5, c5ok, c5read, c5miss = [], True, False, []
print("\nLeg 2: seed cap mode | default tfdrip (mean) | harm (tfdrip above 1.01 x default; in Background also by more than the default's spread)")
for s in SEEDS:
    for cp in CAPS:
        for m in MODES:
            cur = [G["2", b].get((s, cp, m)) for b in BUILDS]
            if not all(cur):
                c5miss.append(f"{s} {cp} {m}")
                continue
            c5read = True
            harms = []
            for k in [f"w{w}" for w in WINS] + ["mean"]:
                d, t = avg(cur[0], k), avg(cur[1], k)
                if t > 1.01 * d and (m != "bg" or t - d > spread(cur[0], k)):
                    harms.append(f"{k}: {t:.4f} vs {1.01 * d:.4f}, spread {spread(cur[0], k):.4f}")
            if harms:
                c5ok = False
                c5.append(f"{s} {cp} {m}: " + "; ".join(harms))
            print(f"  {s:8} {cp:5} {m:5} | {avg(cur[0], 'mean'):.4f} {avg(cur[1], 'mean'):.4f} | {'; '.join(harms) or 'none'}")
verdict(5, c5ok, c5read, "; ".join(c5) or ("tfdrip within 1% of the default build everywhere read" if c5read else ""), c5miss)

# reach: the restore sources, by leg, point, mode and build
print("\nReach (touches by source over the seeds' runs; a source at 0 is NOTHING READ for that path there):")
for leg, pts in (("1", LOADS), ("2", CAPS)):
    for pt in pts:
        for m in MODES:
            for b in BUILDS:
                rs = [r for s in SEEDS for r in G[leg, b].get((s, pt, m), [])]
                if not rs:
                    print(f"  leg {leg} {pt:5} {m:5} {b:7}: no runs: NOTHING READ")
                    continue
                src = {x: sum(int(r[f"src_{x}"]) for r in rs) for x in SRCS}
                zero = [x for x in AWAY if src[x] == 0]
                print(f"  leg {leg} {pt:5} {m:5} {b:7}: {src} left {sum(int(r['left']) for r in rs)} "
                      f"moved {sum(int(r['moved']) for r in rs)}; NOTHING READ: {zero or 'none'}")


def reported(leg, pts, title):
    print(f"\nReported, leg {leg} ({title}; over the seeds, Background over its repeats; 0.050 is target 7's bar):")
    for pt in pts:
        for m in MODES:
            rs = {b: [r for s in SEEDS for r in G[leg, b].get((s, pt, m), [])] for b in BUILDS}
            if not all(rs.values()):
                continue
            for b in BUILDS:
                ws = " ".join(f"{avg(rs[b], f'w{w}'):.4f}" for w in WINS)
                sp = ""
                if m == "bg":
                    sp = " spread(mean) " + " ".join(f"{spread(G[leg, b][s, pt, m], 'mean'):.4f}" for s in SEEDS if (s, pt, m) in G[leg, b])
                print(f"  {pt:5} {m:5} {b:7} | {ws} | mean {avg(rs[b], 'mean'):.4f}{' (<= 0.050)' if avg(rs[b], 'mean') <= 0.050 else ''}"
                      f" | secs {avg(rs[b], 'secs'):.1f}{sp}")
            print(f"  {pt:5} {m:5} gain    | " + " ".join(f"{avg(rs['default'], f'w{w}') - avg(rs['tfdrip'], f'w{w}'):+.4f}" for w in WINS)
                  + f" | mean {avg(rs['default'], 'mean') - avg(rs['tfdrip'], 'mean'):+.4f}")


reported("1", LOADS, "the five windows 1 s to 60 s, the mean, the gain")
reported("2", CAPS, "the five windows 1 s to 60 s, the mean, the gain")

print("\nReported, classes (share of queries; error per exact event), by leg, point, mode, build:")
for leg, pts in (("1", LOADS), ("2", CAPS)):
    for pt in pts:
        for m in MODES:
            for b in BUILDS:
                rs = [r for s in SEEDS for r in G[leg, b].get((s, pt, m), [])]
                if not rs:
                    continue
                q, x, e = defaultdict(int), defaultdict(float), defaultdict(float)
                for r in rs:
                    for k, v in r.items():
                        if k.startswith("q_"):
                            q[k[2:]] += int(v)
                        elif k.startswith("x_"):
                            x[k[2:]] += float(v)
                        elif k.startswith("e_"):
                            e[k[2:]] += float(v)
                tq = sum(q.values()) or 1
                cls = ", ".join(f"{c} {q[c] / tq:.4f} ({e[c] / x[c]:.3f})" if x[c] else f"{c} {q[c] / tq:.4f}" for c in sorted(q))
                print(f"  leg {leg} {pt:5} {m:5} {b:7}: {cls}")

print("\nReported, counters (mean a run), by leg, point, mode, build:")
for leg, pts in (("1", LOADS), ("2", CAPS)):
    for pt in pts:
        for m in MODES:
            for b in BUILDS:
                rs = [r for s in SEEDS for r in G[leg, b].get((s, pt, m), [])]
                if rs:
                    print(f"  leg {leg} {pt:5} {m:5} {b:7}: " + " ".join(f"{c} {avg(rs, c):.1f}" for c in CTRS if c in rs[0]))

for k, v in done.items():
    if not v:
        print(f"{k}: NO W14 DONE LINE")
        bad = 1
sys.exit(bad)
