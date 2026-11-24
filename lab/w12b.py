#!/usr/bin/env python3
# lab/sw W12b (SW-RESULTS.md, W12b): reads go/swsplit_test.go's lines of both builds and prints the tables and
# clauses 1 to 5 as the rule words them. Usage: w12b.py <w12b dir> <w12 dir> (the w12b dir holds progress.txt,
# sight-{default,drip}.txt, runs-{default,drip}.txt and join.txt; the w12 dir holds a-default.txt and a-drip.txt, W12's
# own lines). A line's build, seed and delay come from its fields, never from its file's name. A clause with nothing
# to read is NOTHING READ, and one with points missing and none failing is NOTHING READ (in part): never a pass.
# Every float is computed here from the lines' integers. Exits 1 if a clause does not hold.
import re
import sys
from collections import defaultdict

O, W12 = sys.argv[1], sys.argv[2]
WINS = [1, 5, 15, 30, 60]
BUILDS = ("default", "tfdrip")
SEEDS = ["9,9", "2026,41", "2026,42", "2026,43", "2026,44"]
DELAYS = ["none"] + [f"{m}:{j}" for m in ("jitter", "stall") for j in (1, 10, 100, 1000)]
POINTS = [(s, d) for s in SEEDS for d in DELAYS]  # 45 a build
STALL = "stall:1000"
RECS = 240_000
STATES = ["1", "2", "3a", "3b", "3c", "4a", "4b", "4c", "5a", "5b", "5c", "6", "7", "8", "k8"]  # 14 states and key 8's none line
CLASSES = ("none", "dead", "clean", "off")
AGES = tuple(f"age{i}" for i in range(5))
GROUPS = CLASSES + ("deadhot", "deadreg") + AGES
SIGHT_TEST = "TestShortWindowLagSplitSight"
TOL = 1e-12

bad = 0
malformed = []


def read(name, base=None):
    try:
        return open(f"{base or O}/{name}", encoding="utf-8").read().splitlines()
    except FileNotFoundError:
        print(f"{name}: MISSING")
        return []


def kv(s):
    return dict(x.split("=", 1) for x in s.split() if "=" in x)


def tagged(lines, tag):
    # the fields of every line holding tag (a substring: a go test -v prefix comes before it); tokens without an
    # = are dropped, so a line cut short parses and is judged by its missing fields (see fine).
    return [kv(x[x.find(tag) + len(tag):]) for x in lines if tag in x]


W12_INTS = [f"{p}{w}" for p in ("es", "xs") for w in WINS] + \
    [f"{p}_{c}" for p in ("q", "x", "e") for c in ("none", "nolog", "dead", "valid")] + ["mism"]
W12B_INTS = ["q_none", "q_nolog", "q_dead", "q_clean", "q_off", "hotdead", "lastbad", "modelmism", "clsmism", "qmoved", "recs"]


def fine(r, ints, floats=()):
    # the line names its build, seed and delay and every field the joiner reads is there and a number
    try:
        for k in ("build", "seed", "delay"):
            r[k]
        for k in ints:
            int(r[k])
        for k in floats:
            float(r[k])
        return True
    except (KeyError, ValueError):
        return False


def mean(r):
    return sum(int(r[f"es{w}"]) / int(r[f"xs{w}"]) for w in WINS) / 5


def short(items, n=8):
    items = list(items)
    return items if len(items) <= n else items[:n] + [f"... {len(items) - n} more"]


def by_seed(items):
    # a long list of missing points, one entry a seed: "(2026,43): 40 points" beside the entries naming no seed
    seeds, rest = defaultdict(int), []
    for x in items:
        m = re.search(r"\((\d+,\d+)\)", x)
        if m:
            seeds[m.group(1)] += 1
        else:
            rest.append(x)
    return rest + [f"({s}): {n} points" for s, n in seeds.items()]


def verdict(n, what, ok, read_any, detail, missing=()):
    global bad
    if not read_any and ok:
        v = "NOTHING READ"
    elif not ok:
        v = "FAILS"
    elif missing:
        v = "NOTHING READ (in part)"
        detail += f"; not read: {short(by_seed(missing))}"
    else:
        v = "holds"
    if v != "holds":
        bad = 1
    print(f"W12b clause {n} ({what}): {v}: {detail}")


def group(rows, key):
    g = defaultdict(list)
    for r in rows:
        g[key(r)].append(r)
    return g


# --- the files -----------------------------------------------------------------------------------------------
SIGHT = re.compile(r"W12b sight (.+?): build=(\S+) cls=(\S+) field=(\S+) model=(\S+) since=(-?\d+) hot=(\S+) ok=(\S+)")
sight = defaultdict(list)  # build -> (state, ok)
sight_done = defaultdict(list)  # build -> states
sight_pass = defaultdict(bool)
sight_fail = defaultdict(bool)
for name in ("sight-default.txt", "sight-drip.txt"):
    ls = read(name)
    here = set()
    for x in ls:
        m = SIGHT.search(x)
        if m:
            sight[m.group(2)].append((m.group(1), m.group(8) == "true"))
            here.add(m.group(2))
    for r in tagged(ls, "W12b sight done: "):
        try:
            sight_done[r["build"]].append(int(r["states"]))
        except (KeyError, ValueError):
            malformed.append(f"{name}: sight done line")
        here.add(r.get("build"))
    for b in here:
        sight_pass[b] = sight_pass[b] or any(f"--- PASS: {SIGHT_TEST}" in x for x in ls)
        sight_fail[b] = sight_fail[b] or any(f"--- FAIL: {SIGHT_TEST}" in x for x in ls)

run_lines, split_lines, join_done = [], [], []
for name in ("runs-default.txt", "runs-drip.txt"):
    run_lines += read(name)
join = read("join.txt")
progress = read("progress.txt")
w12 = tagged(read("a-default.txt", W12), "W12 run: ") + tagged(read("a-drip.txt", W12), "W12 run: ")
ref = {(r["build"], r["seed"], r["delay"]): r for r in w12 if "build" in r}

lines12 = tagged(run_lines, "W12 run: ")
lines12b = tagged(run_lines, "W12b run: ")
done12b = tagged(run_lines, "W12b done: ")
join_done = tagged(join, "W12b join done: ")
raw_split = tagged(join, "W12b split: ")

stray = []  # lines whose build, seed or delay is outside the fixed lists, or that appear twice
R12, R12B = {}, {}
for rows, store, what, ints, floats in ((lines12, R12, "W12 run", W12_INTS, ()), (lines12b, R12B, "W12b run", W12B_INTS, ("secs",))):
    for r in rows:
        if not fine(r, ints, floats):
            malformed.append(f"{what} ({r.get('build', '?')}, ({r.get('seed', '?')}), {r.get('delay', '?')}) cut or unreadable, not used")
            continue
        k = (r.get("build"), r.get("seed"), r.get("delay"))
        if k[0] not in BUILDS or (k[1], k[2]) not in POINTS:
            stray.append(f"{what} {k} is outside the fixed lists")
        elif k in store:
            stray.append(f"{what} {k} twice")
        else:
            store[k] = r

SPLIT = {}  # (seed, delay) -> parsed line
for r in raw_split:
    try:
        k = (r["seed"], r["delay"])
        if k not in POINTS:
            stray.append(f"split {k} is outside the fixed lists")
            continue
        if k in SPLIT:
            stray.append(f"split {k} twice")
            continue
        sp = {x: int(r[x]) for x in ("recs", "keymism", "winmism", "exactmism", "keymism0", "winmism0", "deadmism", "clsbad")}
        sp["X"] = [int(v) for v in r["X"].split(",")]
        sp["X0"] = [int(v) for v in r["X0"].split(",")]
        sp["head"] = r["head"]
        sp["c"] = {}
        for g in GROUPS:
            ws = [[int(v) for v in w.split("/")] for w in r[f"c_{g}"].split(";")]
            if len(ws) != 5 or any(len(w) != 6 for w in ws) or len(sp["X"]) != 5 or len(sp["X0"]) != 5 or 0 in sp["X"] + sp["X0"]:
                raise ValueError(g)
            sp["c"][g] = ws
        SPLIT[k] = sp
    except (KeyError, ValueError):
        malformed.append(f"split line of {r.get('seed')} {r.get('delay')}")


def part(sp, g, w):
    q, x, ed, et, ed0, et0 = sp["c"][g][w]
    return (ed0 - et0) / (5 * sp["X0"][w]) - (ed - et) / (5 * sp["X"][w])


PARTS = {k: {g: [part(sp, g, w) for w in range(5)] for g in GROUPS} for k, sp in SPLIT.items()}
LOSS = {k: {g: sum(v) for g, v in p.items()} for k, p in PARTS.items()}


def totals(k):
    loss = sum(LOSS[k][c] for c in CLASSES)
    gross = sum(LOSS[k][c] for c in CLASSES if LOSS[k][c] > 0)
    return loss, gross


# --- clause 1 ------------------------------------------------------------------------------------------------
c1, c1fail, c1miss, c1read = [], [], [], False
for b in BUILDS:
    if not sight[b] and not sight_done[b]:
        c1miss.append(b)
        continue
    c1read = True
    names = [n for n, _ in sight[b]]
    lost = [n for n in STATES if n not in names]
    extra = sorted({n for n in names if n not in STATES})
    twice = sorted({n for n in names if names.count(n) > 1})
    notok = [n for n, ok in sight[b] if not ok]
    dn = sight_done[b]
    if lost:
        c1fail.append(f"{b}: states missing {lost}")
    if extra or twice:
        c1fail.append(f"{b}: states outside the list {extra}, twice {twice}")
    if notok:
        c1fail.append(f"{b}: not ok {notok}")
    if dn != [len(STATES)] or len(names) != len(STATES):
        c1fail.append(f"{b}: done lines {dn}, {len(names)} state lines, want one done line of {len(STATES)}")
    if not sight_pass[b] or sight_fail[b]:
        c1fail.append(f"{b}: {SIGHT_TEST} not passed")
    c1.append(f"{b}: {len(names)} lines, {len(notok)} not ok, done {dn}, test {'passed' if sight_pass[b] else 'NOT PASSED'}")
verdict(1, "the sight: every state reads ok in both builds, the done line counts them, the test passed", not c1fail, c1read,
        "; ".join(c1fail or c1), c1miss)

# --- clause 2 ------------------------------------------------------------------------------------------------
c2fail, c2miss, c2n = [], [], 0
for s in SEEDS:
    for b in BUILDS:
        r = R12.get((b, s, "none"))
        rb = R12B.get((b, s, "none"))
        if r is None or (b == "tfdrip" and rb is None):
            c2miss.append(f"({s}) {b}")
            continue
        c2n += 1
        if r.get("mism") != "0":
            c2fail.append(f"({s}) {b}: mism={r.get('mism')}")
        if b == "tfdrip":
            if r.get("q_dead") != "0" or rb.get("q_dead") != "0":
                c2fail.append(f"({s}) tfdrip: q_dead={r.get('q_dead')}/{rb.get('q_dead')}")
            if rb.get("q_off") != "0":
                c2fail.append(f"({s}) tfdrip: q_off={rb.get('q_off')}")
verdict(2, "no delay: mism 0 in both builds, tfdrip has no dead or off query, every seed", not c2fail, c2n > 0,
        "; ".join(c2fail) or f"{c2n} runs", c2miss)

# --- clause 3 ------------------------------------------------------------------------------------------------
INT = ("es", "xs", "q_", "x_", "e_")
c3fail, c3miss, c3read = list(stray) + [f"unreadable line: {m}" for m in malformed], [], False
# a pair the join refused: its messages, and a split line absent while the four W12b run lines (both builds, at the
# delay and at none) are there, since a run line is printed after its file is written.
refusals = [x.strip().split("swsplit_test.go:")[-1].strip() for x in join if "the pair is skipped" in x]
if refusals or any("--- FAIL: TestShortWindowLagSplitJoin" in x for x in join):
    c3fail += [f"the join refused: {m}" for m in refusals] or ["the join failed: --- FAIL: TestShortWindowLagSplitJoin"]
    c3read = True
refused = {(s, d) for s, d in POINTS if (s, d) not in SPLIT and all((b, s, dd) in R12B for b in BUILDS for dd in (d, "none"))}
c3fail += [f"split ({s}) {d}: no split line though its four W12b run lines are there, the join refused the pair" for s, d in sorted(refused)]
base = {}
for x in progress:
    for tag, key in (("library files that differ from 4a586d8's: [", "lib"), ("reused test files that differ from 073d7da's: [", "tests")):
        i = x.find(tag)
        if i >= 0 and x.strip().endswith("]") and re.match(r"^(\[[0-9:]+\] )?0: ", x.strip()):
            base[key] = x[i + len(tag):x.rstrip().rfind("]")].strip()
for key in ("lib", "tests"):
    if key not in base:
        c3miss.append(f"progress.txt's {key} base line")
    else:
        c3read = True
        if base[key]:
            c3fail.append(f"base: the {key} files that differ are [{base[key]}]")
n12, n12b = {}, {}
for b in BUILDS:
    n12[b] = sum(1 for k in R12 if k[0] == b)
    n12b[b] = sum(1 for k in R12B if k[0] == b)
    dn = [int(r["runs"]) for r in done12b if r.get("build") == b and r.get("runs", "").isdigit()]
    if n12[b] or n12b[b] or dn:
        c3read = True
        if dn != [n12b[b]]:
            c3fail.append(f"{b}: done lines {dn}, {n12b[b]} W12b run lines")
    for s, d in POINTS:
        r, rb = R12.get((b, s, d)), R12B.get((b, s, d))
        if r is None:
            c3miss.append(f"{b} W12 ({s}) {d}")
        if rb is None:
            c3miss.append(f"{b} W12b ({s}) {d}")
        rf = ref.get((b, s, d))
        if r is not None:
            if rf is None:
                c3miss.append(f"{b} lab/w12 line ({s}) {d}")
            else:
                diff = [k for k in rf if (k.startswith(INT) or k in ("lagged", "mism")) and r.get(k) != rf[k]]
                if diff:
                    c3fail.append(f"{b} ({s}) {d}: W12 fields differ from lab/w12's: {diff}")
        if rb is not None:
            try:
                if int(rb["recs"]) != RECS:
                    c3fail.append(f"{b} ({s}) {d}: recs={rb['recs']}")
                for k in ("lastbad", "modelmism", "clsmism", "qmoved"):
                    if rb[k] != "0":
                        c3fail.append(f"{b} ({s}) {d}: {k}={rb[k]}")
                if r is not None:
                    same = [rb[k] == r[k] for k in ("q_none", "q_nolog", "q_dead")]
                    same.append(int(rb["q_clean"]) + int(rb["q_off"]) == int(r["q_valid"]))
                    if not all(same):
                        c3fail.append(f"{b} ({s}) {d}: the W12b run's classes differ from its W12 line's")
            except (KeyError, ValueError):
                c3fail.append(f"{b} ({s}) {d}: unreadable W12b run line")
pairs = len(SPLIT)
jd = [int(r["pairs"]) for r in join_done if r.get("pairs", "").isdigit()]
if pairs or jd:
    c3read = True
    if jd != [pairs]:
        c3fail.append(f"join done lines {jd}, {pairs} split lines")
IDENT = {}
for s, d in POINTS:
    sp = SPLIT.get((s, d))
    if sp is None:
        if (s, d) not in refused:
            c3miss.append(f"split ({s}) {d}")
        continue
    c3read = True
    t = f"({s}) {d}"
    for k in ("keymism", "winmism", "exactmism", "keymism0", "winmism0", "clsbad"):
        if sp[k] != 0:
            c3fail.append(f"{t}: {k}={sp[k]}")
    if sp["recs"] != RECS:
        c3fail.append(f"{t}: recs={sp['recs']}")
    lines = {(b, dd): R12.get((b, s, dd)) for b in BUILDS for dd in (d, "none")}
    if not all(lines.values()):
        c3miss.append(f"split ({s}) {d}: W12 lines to compare")
        continue
    for b in BUILDS:
        for w in WINS:
            if int(lines[b, d][f"xs{w}"]) != sp["X"][WINS.index(w)]:
                c3fail.append(f"{t}: X differs from {b}'s xs{w}")
            if int(lines[b, "none"][f"xs{w}"]) != sp["X0"][WINS.index(w)]:
                c3fail.append(f"{t}: X0 differs from {b}'s none xs{w}")
    for i, w in enumerate(WINS):
        for col, name, ln in ((2, "ed", lines["default", d]), (3, "et", lines["tfdrip", d]),
                              (4, "ed0", lines["default", "none"]), (5, "et0", lines["tfdrip", "none"])):
            if sum(sp["c"][c][i][col] for c in CLASSES) != int(ln[f"es{w}"]):
                c3fail.append(f"{t}: {name} summed over the classes is not es{w}")
        for col in range(6):
            if sum(sp["c"][c][i][col] for c in ("deadhot", "deadreg")) != sp["c"]["dead"][i][col] \
                    or sum(sp["c"][a][i][col] for a in AGES) != sp["c"]["dead"][i][col]:
                c3fail.append(f"{t}: the dead group's hot and regular parts, or its age bins, do not sum to it in window {w}")
                break
    # the cells' per-class sums over the five windows against tfdrip's W12 line and W12b run line at this delay: a
    # class misread the same way in every window would pass every check above
    rt, rbt = lines["tfdrip", d], R12B.get(("tfdrip", s, d))

    def cell(gs, col):
        return sum(sp["c"][g][i][col] for g in gs for i in range(5))

    want = []  # (what, cell sum, the line's value)
    for gs, cl in ((("dead",), "dead"), (("none",), "none"), (("clean", "off"), "valid")):
        for col, p, name in ((0, "q", "q"), (1, "x", "x"), (3, "e", "et")):
            want.append((f"c_{'+'.join(gs)} {name}", cell(gs, col), int(rt[f"{p}_{cl}"]), f"{p}_{cl}"))
    if rbt is not None:
        want += [("c_clean q", cell(("clean",), 0), int(rbt["q_clean"]), "q_clean"), ("c_off q", cell(("off",), 0), int(rbt["q_off"]), "q_off"),
                 ("c_deadhot q", cell(("deadhot",), 0), int(rbt["hotdead"]), "hotdead")]
    else:
        c3miss.append(f"split ({s}) {d}: tfdrip's W12b run line to compare")
    for what, got, line_v, field in want:
        if got != line_v:
            c3fail.append(f"{t}: {what} summed over the windows is {got}, the line's {field} is {line_v}")
    g0 = mean(lines["default", "none"]) - mean(lines["tfdrip", "none"])
    g = mean(lines["default", d]) - mean(lines["tfdrip", d])
    IDENT[s, d] = (sum(LOSS[s, d][c] for c in CLASSES), g0 - g)
    if abs(IDENT[s, d][0] - IDENT[s, d][1]) > TOL:
        c3fail.append(f"{t}: the parts sum to {IDENT[s, d][0]:.15f}, g0 - g is {IDENT[s, d][1]:.15f}")
heads = {sp["head"] for sp in SPLIT.values()}
if len(heads) > 1:
    c3fail.append(f"the split lines name {len(heads)} heads")
verdict(3, "the harness: base unchanged, W12's lines equal lab/w12's, the checks 0, X and the sums equal W12's, the parts sum to the loss",
        not c3fail, c3read, "; ".join(short(c3fail)) or f"{sum(n12.values())} W12 and {sum(n12b.values())} W12b run lines, {pairs} split lines, "
        f"identity to {TOL:g}", c3miss)

# --- clause 4 ------------------------------------------------------------------------------------------------
c4fail, c4miss, c4read = [], [], False
qd = {}
for s, d in POINTS:
    sp = SPLIT.get((s, d))
    rb = R12B.get(("tfdrip", s, d))
    if rb is not None and rb.get("q_dead", "").isdigit():
        qd[s, d] = int(rb["q_dead"])
    if sp is None:
        if (s, d) in refused:
            c4fail.append(f"({s}) {d}: the join refused the pair, no deadmism read")
            c4read = True
        else:
            c4miss.append(f"({s}) {d}")
        continue
    c4read = True
    if sp["deadmism"] != 0:
        c4fail.append(f"({s}) {d}: deadmism={sp['deadmism']}")
zero = [s for s in SEEDS if qd.get((s, STALL)) == 0]
nodead = [s for s in SEEDS if (s, STALL) not in qd]
c4miss += [f"q_dead ({s}) {STALL}" for s in nodead if (s, STALL) in SPLIT]
if zero and not c4fail:
    c4read = False
c4ok = ["q_dead at " + STALL + ": " + ", ".join(f"({s}) {qd.get((s, STALL), '?')}" for s in SEEDS)]
if zero:
    c4ok.insert(0, f"{STALL} has q_dead=0 on {zero}")
verdict(4, "a dead log's answer equals the default build's, at every pair; read at stall:1000 with dead queries on every seed",
        not c4fail, c4read, "; ".join(c4fail or c4ok), c4miss)

# --- clause 5 ------------------------------------------------------------------------------------------------
c5fail, c5miss, c5read, c5ok = [], [], False, []
for s in SEEDS:
    k = (s, STALL)
    if k not in SPLIT:
        c5miss.append(f"({s}) {STALL}: no split line")
        continue
    loss, gross = totals(k)
    if loss <= 0 or gross <= 0:
        c5miss.append(f"({s}) {STALL}: loss {loss:.6f} is not above 0")
        continue
    c5read = True
    share = LOSS[k]["dead"] / gross
    if share < 0.5:
        c5fail.append(f"({s}) {STALL}: dead share {share:.4f} < 0.5")
    else:
        c5ok.append(f"({s}) {share:.4f}")
if c3fail or c4fail:
    # the design reads clause 5 only after a clause 3 or 4 difference has been read against the code
    bad = 1
    print("W12b clause 5 (at stall:1000 the dead logs hold at least half the gross loss, every seed): "
          "NOT READ: clause 3 or 4 fails; read the difference against the code first")
else:
    verdict(5, "at stall:1000 the dead logs hold at least half the gross loss, every seed", not c5fail, c5read,
            "; ".join(c5fail) or ("dead share of the gross loss " + ", ".join(c5ok)), c5miss)

# --- tables --------------------------------------------------------------------------------------------------
print("\nThe loss by class (tfdrip against the default build; a part is g0's share less g's, per window 1 5 15 30 60 s, then the sum):")
for s in SEEDS:
    for d in DELAYS:
        if (s, d) not in SPLIT:
            continue
        for c in CLASSES:
            print(f"  ({s:7}) {d:12} {c:5} " + " ".join(f"{v:+.6f}" for v in PARTS[s, d][c]) + f" | {LOSS[s, d][c]:+.6f}")

print("\nThe dead part by hot and regular keys and by age bin ([0,1) [1,2.5) [2.5,10) [10,60) 60+ s):")
for s in SEEDS:
    for d in DELAYS:
        if (s, d) in SPLIT and LOSS[s, d]["dead"] != 0:
            L = LOSS[s, d]
            print(f"  ({s:7}) {d:12} dead {L['dead']:+.6f} = hot {L['deadhot']:+.6f} + regular {L['deadreg']:+.6f}"
                  f" = ages " + " ".join(f"{L[a]:+.6f}" for a in AGES))

print("\nThe loss: net (the sum of the parts), gross (the parts above 0), the dead part and its shares:")
for s in SEEDS:
    for d in DELAYS:
        if (s, d) not in SPLIT:
            continue
        loss, gross = totals((s, d))
        dead = LOSS[s, d]["dead"]
        gs = f"{dead / gross:.4f}" if gross > 0 else "n/a"
        ns = f"{dead / loss:.4f}" if loss > 0 else "n/a"
        print(f"  ({s:7}) {d:12} loss {loss:+.6f} gross {gross:.6f} dead {dead:+.6f} share of gross {gs} share of loss {ns}")

print("\nThe estimate: tfdrip's mean less the dead part (target 7's bar is 0.050):")
for s in SEEDS:
    for d in DELAYS:
        r = R12.get(("tfdrip", s, d))
        if r is None or (s, d) not in SPLIT:
            continue
        m, dead = mean(r), LOSS[s, d]["dead"]
        print(f"  ({s:7}) {d:12} mean {m:.4f} less dead {dead:+.4f} = {m - dead:.4f}{' (<= 0.050)' if m - dead <= 0.050 else ''}")

print("\nThe dead queries by seed and delay (tfdrip's W12b run):")
for s in SEEDS:
    print(f"  ({s:7}) " + " ".join(f"{d} {qd.get((s, d), '?')}" for d in DELAYS))

print("\nRun times (secs, mean over the seeds):")
for b in BUILDS:
    row = []
    for d in DELAYS:
        v = [float(R12B[b, s, d]["secs"]) for s in SEEDS if (b, s, d) in R12B and R12B[b, s, d].get("secs", "").replace(".", "", 1).isdigit()]
        row.append(f"{d} {sum(v) / len(v):.1f}" if v else f"{d} ?")
    print(f"  {b:8} " + " ".join(row))

sys.exit(bad)
