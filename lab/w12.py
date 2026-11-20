#!/usr/bin/env python3
# lab/sw W12 (SW-RESULTS.md, Phase 12): reads TestShortWindowLag's "W12 run:" lines of both builds and prints
# the tables and clauses 2 to 5 as the rule words them. Usage: w12.py <outputs dir> <default exact> <tfdrip exact>
# (the outputs dir holds a-default.txt, a-drip.txt, b-default.txt, b-drip.txt). Exits 1 if a file lacks its
# "W12 done:" line.
import re
import sys
from collections import defaultdict

O, EXACT_DEF, EXACT_DRIP = sys.argv[1], sys.argv[2], sys.argv[3]
WINS = [1, 5, 15, 30, 60]
FLOOR = 10  # clause 4: ticks with one key's events at two workers in two units


def runs(name):
    out, done = [], False
    for line in open(f"{O}/{name}", encoding="utf-8"):
        if "W12 done:" in line:
            done = True
        m = re.search(r"W12 run: (.*)$", line)
        if m:
            out.append(dict(kv.split("=", 1) for kv in m.group(1).split()))
    return out, done


def exact_file(path):
    e = {}
    for line in open(path, encoding="utf-8"):
        m = re.match(r"velocity window=(\d+)s abs_error_sum=(\d+) exact_sum=(\d+)", line)
        if m:
            e[int(m.group(1))] = (int(m.group(2)), int(m.group(3)))
    return e


bad = 0
data = {}
for leg in "ab":
    for b in ("default", "drip"):
        rs, done = runs(f"{leg}-{b}.txt")
        if not done:
            print(f"{leg}-{b}.txt: NO W12 DONE LINE")
            bad = 1
        data[leg, b] = rs

f = lambda r: float(r["mean"])
key = lambda r: (r["leg"], r["seed"], r["delay"], r["mode"], r["keys"], r["rep"])
# leg A's lines from leg A's run; leg B's from leg B's (its own leg A lines are checked against leg A's in clause 3)
idx = {b: {key(r): r for r in data["a", b] + [r for r in data["b", b] if r["leg"] == "B"]} for b in ("default", "drip")}


def dead(r):
    q = sum(int(r[f"q_{c}"]) for c in ("none", "nolog", "dead", "valid"))
    x = sum(float(r[f"x_{c}"]) for c in ("none", "nolog", "dead", "valid"))
    return int(r["q_dead"]) / q, float(r["x_dead"]) / x if x else 0.0


def rate(r, c):
    x = float(r[f"x_{c}"])
    return float(r[f"e_{c}"]) / x if x else float("nan")


# --- tables -------------------------------------------------------------------------------------------------
print("Leg A (one goroutine, plain): mean of the five windows, default / tfdrip, gain, gain against no delay; "
      "tfdrip's dead logs (by queries, by exact count); events delivered an earlier unit behind; error per event, "
      "valid and dead logs")
seeds = sorted({r["seed"] for r in data["a", "drip"]}, key=lambda s: (s != "9,9", s))
delays = []
for r in data["a", "drip"]:
    if r["delay"] not in delays:
        delays.append(r["delay"])
gainA = {}
for s in seeds:
    for d in delays:
        k = ("A", s, d, "plain", "-", "0")
        rd, rf = idx["default"].get(k), idx["drip"].get(k)
        if not rd or not rf:
            print(f"  ({s}) {d}: MISSING")
            bad = 1
            continue
        gainA[s, d] = f(rd) - f(rf)
for s in seeds:
    print(f"  seed ({s})")
    for d in delays:
        k = ("A", s, d, "plain", "-", "0")
        if (s, d) not in gainA:
            continue
        rd, rf = idx["default"][k], idx["drip"][k]
        g0 = gainA.get((s, "none"))
        dq, dx = dead(rf)
        print(f"    {d:12} {f(rd):.4f} / {f(rf):.4f}  gain {gainA[s, d]:.4f} ({gainA[s, d] / g0:.2f} of none)  "
              f"dead {100 * dq:.2f}% / {100 * dx:.2f}%  lagged {rf['lagged']}  "
              f"err/event valid {rate(rf, 'valid'):.4f} dead {rate(rf, 'dead'):.4f}  "
              f"1 s {float(rd['w1']):.4f} / {float(rf['w1']):.4f}")

print("Leg B ((9,9), 8 workers): per mode and delay, owned keys' answers differing from leg A's (default, tfdrip); "
      "shared keys: mean of the repeats, default / tfdrip, [min-max], gain against no delay, dead logs (tfdrip, "
      "by queries), answers differing from leg A's (tfdrip, per repeat), overlaps (ovl, ovlu)")
modes = []
for r in data["b", "drip"]:
    if r["leg"] == "B" and r["mode"] not in modes:
        modes.append(r["mode"])
bseed = "9,9"
gainB = {}


def shared(b, m, d):
    return [r for r in data["b", b] if r["leg"] == "B" and r["mode"] == m and r["delay"] == d and r["keys"] == "shared"]


for m in modes:
    for d in delays:
        sd, sf = shared("default", m, d), shared("drip", m, d)
        if sd and sf:
            gainB[m, d] = sum(map(f, sd)) / len(sd) - sum(map(f, sf)) / len(sf)
for m in modes:
    print(f"  mode {m}")
    for d in delays:
        od = idx["default"].get(("B", bseed, d, m, "owned", "0"))
        of = idx["drip"].get(("B", bseed, d, m, "owned", "0"))
        sd, sf = shared("default", m, d), shared("drip", m, d)
        if not (od and of and sd and sf):
            print(f"    {d:12} MISSING")
            bad = 1
            continue
        md, mf = sum(map(f, sd)) / len(sd), sum(map(f, sf)) / len(sf)
        g0 = gainB.get((m, "none"))
        print(f"    {d:12} owned {od['mism']}, {of['mism']}  shared {md:.4f} [{min(map(f, sd)):.4f}-{max(map(f, sd)):.4f}] / "
              f"{mf:.4f} [{min(map(f, sf)):.4f}-{max(map(f, sf)):.4f}]  gain {gainB[m, d]:.4f} ({gainB[m, d] / g0:.2f} of none)  "
              f"dead {', '.join(f'{100 * dead(r)[0]:.2f}%' for r in sf)}  vs A {', '.join(r['mism'] for r in sf)}  "
              f"ovl {sf[0]['ovl']} ovlu {sf[0]['ovlu']}")

# --- clauses ------------------------------------------------------------------------------------------------
print()

def verdict(fails, n):
    return "NOTHING READ" if n == 0 else ("holds" if not fails else "FAILS: " + "; ".join(fails))


c2, n2 = [], 0
ex = {"default": exact_file(EXACT_DEF), "drip": exact_file(EXACT_DRIP)}
if "9,9" not in seeds:
    c2.append("no run of (9,9)")
for s in seeds:
    for b in ("default", "drip"):
        r = idx[b].get(("A", s, "none", "plain", "-", "0"))
        if not r:
            c2.append(f"({s}) {b}: no run")
            continue
        n2 += 1
        if r["mism"] != "0":
            c2.append(f"({s}) {b}: {r['mism']} answers differ from swRecord's")
        if b == "drip" and r["q_dead"] != "0":
            c2.append(f"({s}) tfdrip: {r['q_dead']} queries with a dead log")
        if s == "9,9":
            for w in WINS:
                got = (int(r[f"es{w}"]), int(r[f"xs{w}"]))
                if got != ex[b][w]:
                    c2.append(f"(9,9) {b} {w} s: sums {got}, the exact file's {ex[b][w]}")
print("W12 clause 2 (no delay: leg A equals swRecord query for query, both builds, every seed; no dead log; (9,9)'s "
      f"sums equal the exact files'; {n2} runs): " + verdict(c2, n2))

c3, n3 = [], 0
for b in ("default", "drip"):
    for r in data["b", b]:
        if r["leg"] == "B" and r["keys"] == "owned":
            n3 += 1
            if r["mism"] != "0":
                c3.append(f"{b} {r['mode']} {r['delay']}: {r['mism']} answers differ from leg A's")
        if r["leg"] == "A":
            ra = next((x for x in data["a", b] if key(x) == key(r)), None)
            same = ra is not None and all(ra[k] == r[k] for k in r)
            if not same:
                c3.append(f"{b} leg A {r['delay']} in leg B's run differs from leg A's run")
print("W12 clause 3 (leg B, owned keys: answers equal leg A's query for query, every mode, delay and build; leg B's "
      f"own leg A lines equal leg A's; {n3} owned runs): " + verdict(c3, n3))

judged = {}
for d in delays:
    ov = {r["ovlu"] for b in ("default", "drip") for r in data["b", b] if r["leg"] == "B" and r["keys"] == "shared" and r["delay"] == d}
    judged[d] = len(ov) == 1 and int(next(iter(ov))) >= FLOOR
print(f"W12 clause 4 (the shared keys judged at a delay where at least {FLOOR} ticks put one key's events at two "
      f"workers in two units): judged at [" + ", ".join(d for d in delays if judged[d]) +
      "]; not at [" + ", ".join(d for d in delays if not judged[d]) + "]")

c5, c5n = [], []
small = [d for d in delays if d.split(":")[-1] in ("1", "10") and d != "none"]
for s in seeds:
    g0 = gainA.get((s, "none"))
    for d in small:
        if (s, d) in gainA and g0 is not None:
            c5n.append(1)
            if gainA[s, d] < 0.5 * g0:
                c5.append(f"leg A ({s}) {d}: gain {gainA[s, d]:.4f} < half of {g0:.4f}")
for m in modes:
    g0 = gainB.get((m, "none"))
    for d in small:
        if not judged.get(d):
            continue
        if (m, d) in gainB and g0 is not None:
            c5n.append(1)
            if gainB[m, d] < 0.5 * g0:
                c5.append(f"leg B {m} {d}: gain {gainB[m, d]:.4f} < half of {g0:.4f}")
print(f"W12 clause 5 (the gain at delays up to 10 ms at least half its gain with none; {len(c5n)} comparisons): " +
      verdict(c5, len(c5n)))
sys.exit(bad)
