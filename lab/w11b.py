#!/usr/bin/env python3
"""lab/sw W11b's joiner (SW-RESULTS.md). Reads the outputs lab/w11b.sh leaves in the directory given and prints the
probe's inputs, each run's legs, Upper beside the default build's and the events given, the race's classes, and W11b's
clauses. A clause prints HOLDS only if it judged something and nothing in it failed, was missing or went unjudged:
NOTHING READ if it judged nothing, NOTHING READ in part if a run it needs has no lines, HOLDS where judged / NOT JUDGED
for the parts its rule leaves unjudged (a leg that said nothing, no sight). If clause 1 does not hold, clauses 3 to 6
read NOT JUDGED (the rule's gate), with what they read beside. Exits 0 only if every clause holds; 1 otherwise; 2 if
the directory is missing."""
import os
import re
import sys

LEGS = ['c1', '1', 'c2', '2', 'c3', '3', 'c4', '4', 'c5', '5', 'c6', '6', '7', '8', '9']
CONTROLS = ['c1', 'c2', 'c3', 'c4', 'c5', 'c6']
RECLAIM = ['1', '2', '3', '4', '5', '6', '7', '8', '9']
PLACED = ['1', '2', '4', '5']   # the history goes in, then its entry is reclaimed: once, beside unpoint's register
WAITED = ['3', '6']             # the history waits, then its entry is reclaimed: fresh (A25, b187156's wording)
BEFORE = ['7', '8', '9']        # the entry reclaimed before K's touch: fresh (A25)
WINDOWS = [250, 1000, 2000, 10000, 60000]
KEYS = ['build', 'hook', 'before', 'promoted', 'placed', 'retried', 'taken', 'restored', 'evictions', 'relocations',
        'waited', 'K in', 'last', 'log', 'class', 'hot', 'mine', 'reads']
RUNS = {'head': 'fresh-head.txt', 'head-race': 'fresh-head-race.txt', 'w4d': 'fresh-w4d.txt',
        'default': 'fresh-default.txt'}
RACES = {'head': 'race-head.txt', 'head-race': 'race-head-race.txt', 'w4d': 'race-w4d.txt'}


def readlines(o, name):
    try:
        with open(os.path.join(o, name), errors='replace') as f:
            return f.read().splitlines()
    except FileNotFoundError:
        return []


def parse_reads(s):
    out = {}
    for item in s.split():
        m = re.fullmatch(r'(\d+):(-?\d+)/(-?\d+)/(\d+)', item)
        if m:
            out[int(m.group(1))] = tuple(int(m.group(i)) for i in (2, 3, 4))
    return out


def parse_run(lines):
    """the run's leg lines by id, the ids that stopped on 'path not taken', its inputs line, and any data race"""
    legs, notaken, cur, inputs = {}, set(), None, None
    for s in lines:
        m = re.search(r'=== RUN\s+TestDripHotRestoreFresh/(\S+)', s)
        if m:
            cur = m.group(1).split('_')[0]
            continue
        m = re.search(r'W11b inputs: (.*)$', s)
        if m:
            inputs = m.group(1)
        m = re.search(r'W11b leg (.*)$', s)
        if m:
            parts = m.group(1).split(' | ')
            d = {'id': parts[0], 'name': parts[1] if len(parts) > 1 else ''}
            for p in parts[2:]:
                for k in KEYS:
                    if p.startswith(k + ' '):
                        d[k] = p[len(k) + 1:]
                        break
            d['reads'] = parse_reads(d.get('reads', ''))
            legs[d['id']] = d
        if 'path not taken' in s and cur:
            notaken.add(cur)
    return legs, notaken, inputs, any('WARNING: DATA RACE' in s for s in lines)


def parse_race(lines):
    classes = {}
    for s in lines:
        m = re.search(r'W11b race: (\d+) of (\d+) rounds: log (.+?), L promoted (true|false), K looked again (true|false)$', s)
        if m:
            classes[(m.group(3), m.group(4), m.group(5))] = (int(m.group(1)), int(m.group(2)))
    return classes, any('WARNING: DATA RACE' in s for s in lines)


class Clause:
    def __init__(self):
        self.fails, self.nothing, self.unjudged, self.notes, self.judged = [], [], [], [], 0

    def status(self):
        if self.fails:
            return 'FAILS'
        if self.judged == 0:
            return 'NOTHING READ'
        if self.nothing:
            return 'NOTHING READ in part'
        if self.unjudged:
            return 'HOLDS where judged; NOT JUDGED in part'
        return 'HOLDS'


results = []


def show(n, title, c, gate=None):
    s = c.status()
    if gate and s != 'NOTHING READ':
        s = f'NOT JUDGED ({gate}); as read: {s}'
    results.append(s)
    print(f'{n}. {title}: {s}')
    for kind, items in (('fails', c.fails), ('nothing read', c.nothing), ('not judged', c.unjudged), ('', c.notes)):
        for d in items:
            print(f'   - {kind + ": " if kind else ""}{d}')


def main():
    if len(sys.argv) != 2 or not os.path.isdir(sys.argv[1]):
        print('usage: w11b.py <lab/w11b>')
        return 2
    o = sys.argv[1]
    runs = {r: parse_run(readlines(o, f)) for r, f in RUNS.items()}
    races = {r: parse_race(readlines(o, f)) for r, f in RACES.items()}
    progress = readlines(o, 'progress.txt')
    w11 = readlines(o, 'w11-head.txt')

    print('W11b, the probe and its readings')
    for r in ('head', 'w4d'):
        print(f'inputs ({r}): {runs[r][2] or "none"}')
    print()
    for r in RUNS:
        legs, notaken, _, race = runs[r]
        print(f'{r}: {len(legs)} leg lines; path not taken: {sorted(notaken) or "none"}; data race: {race}')
        for i in LEGS:
            d = legs.get(i)
            if not d:
                print(f'  {i:>3}: no line')
                continue
            rd = ' '.join(f'{w}:{c}/{u}/{g}' for w, (c, u, g) in sorted(d['reads'].items()))
            print(f"  {i:>3}: {d.get('class', '?'):<6} hot {d.get('hot', '?'):<5} mine {d.get('mine', '?'):<5} "
                  f"K in {d.get('K in', '?')}; restored {d.get('restored', '?')}, evictions {d.get('evictions', '?')}, "
                  f"relocations {d.get('relocations', '?')}, waited {d.get('waited', '?')}; reads (Count/Upper/given) {rd}")
    print()
    head, default = runs['head'][0], runs['default'][0]
    print('Upper in the reclaim legs: tfdrip (head) / default / given, at ' + ', '.join(f'{w} ms' for w in WINDOWS))
    for i in RECLAIM:
        if i in head and i in default:
            cells = []
            for w in WINDOWS:
                a, b = head[i]['reads'].get(w), default[i]['reads'].get(w)
                cells.append(f'{a[1] if a else "?"}/{b[1] if b else "?"}/{a[2] if a else "?"}')
            print(f'  {i:>3}: ' + '  '.join(cells))
    print()
    for r in RACES:
        classes, race = races[r]
        total = sum(n for n, _ in classes.values())
        print(f'race {r}: {total} rounds classed; data race: {race}')
        for (c, lp, la), (n, of) in sorted(classes.items()):
            print(f'  {n} of {of}: log {c}, L promoted {lp}, K looked again {la}')
    print()

    # A leg says nothing in a run if it has no line there or stopped on its path (clause 2).
    unread = {r: {i for i in LEGS if i not in runs[r][0]} | runs[r][1] for r in RUNS}

    # 1. the classifier's sight
    c = Clause()
    for r in ('head', 'w4d'):
        legs, _, inputs, _ = runs[r]
        if not legs and inputs is None:
            c.nothing.append(f'{r}: no lines')
            continue
        if inputs is None:
            c.fails.append(f'{r}: no inputs line')
        elif 'distinct true' not in inputs:
            c.fails.append(f'{r}: the inputs line has not three distinct logs, or an input is off')
        else:
            c.judged += 1
        for i in BEFORE + CONTROLS:
            if i in unread[r]:
                c.unjudged.append(f'{r}: leg {i} said nothing (clause 2)')
                continue
            c.judged += 1
            d = legs[i]
            if i in BEFORE and d.get('class') != 'fresh':
                c.fails.append(f"{r}: leg {i} reads {d.get('class')}, not fresh")
            if i in CONTROLS and (d.get('class') != 'once' or d.get('hot') != 'true' or d.get('mine') != 'true'):
                c.fails.append(f"{r}: control {i} reads {d.get('class')}, hot {d.get('hot')}, K's entry {d.get('mine')}")
    show(1, 'the classifier: on the head and 23f03b6 three logs at the inputs, legs 7 to 9 fresh, the controls once with K\'s entry', c)
    gate = None if results[-1] == 'HOLDS' else 'clause 1 does not hold: the classes have no sight'

    # 2. paths
    c = Clause()
    for r in RUNS:
        legs, notaken, _, _ = runs[r]
        if not legs and not notaken:
            c.nothing.append(f'{r}: no lines')
            continue
        c.judged += 1
        missing = [i for i in LEGS if i not in legs]
        if missing or notaken:
            c.fails.append(f'{r}: no line for {missing or "none"}; path not taken in {sorted(notaken) or "none"}')
    show(2, 'every leg takes its path in the four runs', c)

    # 3. sight of a double push on 23f03b6
    c = Clause()
    legs = runs['w4d'][0]
    sight, upper_sight = [], False
    if not legs:
        c.nothing.append('23f03b6: no leg lines')
    for i in PLACED:
        if not legs:
            break
        if i in unread['w4d']:
            c.unjudged.append(f'leg {i} said nothing (clause 2)')
            continue
        c.judged += 1
        d = legs[i]
        over = [w for w, (_, u, g) in sorted(d['reads'].items()) if u > g]
        if d.get('class') == 'twice':
            sight.append(i)
            upper_sight = upper_sight or bool(over)
        else:
            c.fails.append(f"leg {i} reads {d.get('class')}: no sight; its head reading is reported, not judged")
        c.notes.append(f"leg {i}: {d.get('class')}, Upper over the events given at {over or 'no window'}")
    if sight and not upper_sight:
        c.fails.append('no twice leg reads Upper over the events given: clause 5\'s second half is not judged')
    for i in WAITED + BEFORE:
        if legs and i not in unread['w4d']:
            c.notes.append(f"leg {i} (reported): {legs[i].get('class')}")
    show(3, '23f03b6: legs 1, 2, 4 and 5 read twice, and one at least reads Upper over the events given', c, gate)

    # 4. the head
    c = Clause()
    for r in ('head', 'head-race'):
        legs, _, _, race = runs[r]
        if not legs:
            c.nothing.append(f'{r}: no leg lines')
            continue
        if race:
            c.fails.append(f'{r}: a data race')
        for i in PLACED + WAITED:
            if i in unread[r]:
                c.unjudged.append(f'{r}: leg {i} said nothing (clause 2)')
                continue
            d = legs[i]
            if i in PLACED:
                good = d.get('class') == 'once' and d.get('hot') == 'false'
                if i not in sight:
                    c.unjudged.append(f"{r}: leg {i} has no sight (clause 3); reads {d.get('class')}, hot {d.get('hot')}")
                    continue
                c.judged += 1
                if not good:
                    c.fails.append(f"{r}: leg {i} reads {d.get('class')}, hot {d.get('hot')}")
            else:
                c.judged += 1
                if d.get('class') != 'fresh':
                    c.fails.append(f"{r}: leg {i} reads {d.get('class')}, not fresh")
    a, b = runs['head'][0], runs['head-race'][0]
    if a and b:
        diff = [i for i in LEGS if i in a and i in b and a[i] != b[i]]
        if diff:
            c.fails.append(f'the -race run\'s lines differ from the plain run\'s in legs {diff}')
    show(4, 'the head: legs 1, 2, 4 and 5 once, beside a register; legs 3 and 6 fresh; plain and under -race alike, no data race', c, gate)

    # 5. Upper
    c = Clause()
    if not head:
        c.nothing.append('head: no leg lines')
    if not default:
        c.nothing.append('default: no leg lines')
    over = []
    for i in RECLAIM:
        if not head or not default:
            break
        if i in unread['head'] or i in unread['default']:
            c.unjudged.append(f'leg {i} said nothing in the head\'s or the default build\'s run (clause 2)')
            continue
        for w in WINDOWS:
            x, y = head[i]['reads'].get(w), default[i]['reads'].get(w)
            if not x or not y:
                c.fails.append(f'leg {i}: no read at {w} ms')
                continue
            c.judged += 1
            if x[1] < y[1]:
                c.fails.append(f'leg {i} at {w} ms: tfdrip Upper {x[1]} under the default build\'s {y[1]}')
            if x[1] > x[2]:
                over.append(f'leg {i} at {w} ms: tfdrip Upper {x[1]} over the {x[2]} events given')
    if upper_sight:
        c.fails += over
    else:
        c.unjudged.append('the second half (at or below the events given): no sight on 23f03b6 (clause 3)')
        c.notes += [o + ' (reported)' for o in over]
    c.notes.append(f'{c.judged} reads compared')
    show(5, 'Upper in the reclaim legs: tfdrip\'s at or above the default build\'s, and at or below the events given', c, gate)

    # 6. the race
    c = Clause()
    wclasses = races['w4d'][0]
    hw = sum(n for (cl, _, _), (n, _) in wclasses.items() if cl == 'twice')
    if not wclasses:
        c.unjudged.append('no sight: 23f03b6 has no race lines')
    elif hw == 0:
        c.unjudged.append('no sight: 23f03b6 read no round twice')
    c.notes.append(f'23f03b6: {hw} rounds read twice')
    for r in ('head', 'head-race'):
        classes, race = races[r]
        if not classes:
            c.nothing.append(f'{r}: no race lines')
            continue
        c.judged += 1
        tw = sum(n for (cl, _, _), (n, _) in classes.items() if cl == 'twice')
        looked = sum(n for (_, _, la), (n, _) in classes.items() if la == 'true')
        c.notes.append(f'{r}: {tw} rounds read twice, {looked} looked again, data race {race}')
        if tw:
            c.fails.append(f'{r}: {tw} rounds read twice')
        if race:
            c.fails.append(f'{r}: a data race')
        if looked < 10:
            c.unjudged.append(f'{r}: {looked} rounds looked again, under 10')
    show(6, 'the race: no round twice on the head, plain and under -race', c, gate)

    # 7. W11's tests on the head
    c = Clause()
    passed = [t for t in ('TestDripHotRestoreLegs', 'TestDripHotRestorePushOnce') if any(f'--- PASS: {t} ' in s for s in w11)]
    failed = [s.strip() for s in w11 if '--- FAIL' in s]
    if w11:
        c.judged = 1
        if len(passed) < 2 or failed:
            c.fails.append(f'passed: {passed or "none"}')
            c.fails += failed[:12]
    show(7, 'W11\'s TestDripHotRestoreLegs (legs 3 and 6 reading "not twice") and W4e\'s TestDripHotRestorePushOnce pass', c)

    # 8. the library
    c = Clause()
    lib = [s for s in progress if '0: library files that differ from 2393104' in s]
    if lib:
        c.judged = 1
        c.notes.append(lib[0])
        if not lib[0].rstrip().endswith('[]'):
            c.fails.append('the library differs from 2393104\'s')
    show(8, 'the library is 2393104\'s', c)

    return 0 if all(s == 'HOLDS' for s in results) else 1


if __name__ == '__main__':
    sys.exit(main())
