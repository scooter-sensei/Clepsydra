#!/usr/bin/env python3
"""lab/sw W13's joiner (SW-RESULTS.md). Reads the outputs lab/w13.sh leaves in the directory given and prints the
sight runs' legs, paths and races, the library's checks, the push counts of W7's scenario and of each fuzzer leg by
mode, and W13's clauses. A clause prints HOLDS only if it judged something and nothing in it failed, was missing or
went unjudged: NOTHING READ if it judged nothing, NOTHING READ in part if a run it needs has no lines or a mode no
calls. If clause 1 does not hold, clauses 3, 4 and 6 read NOT JUDGED (the rule's gate), with what they read beside.
Clause 6 (the power leg) qualifies the verdict and is not folded into the exit. Exits 0 only if clauses 1 to 5 and 7
hold; 1 otherwise; 2 if the directory is missing."""
import os
import re
import sys

LEGS = ['c1', '1', 'c2', '2', 'c3', '3', 'c4', '4', 'c5', '5', 'c6', '6', '7', '8', '9']
PLACED = ['1', '2', '4', '5']  # the history goes in, then its entry is reclaimed
WAITED = ['3', '6']            # the history waits, then its entry is reclaimed
BEFORE = ['7', '8', '9']       # the entry reclaimed before K's touch


def expect(tree, i):
    if i.startswith('c'):
        return 'once'
    if i in PLACED:
        return 'twice' if tree == 'w4d' else 'once'
    if i in WAITED:
        return 'fresh'
    return 'once afresh'


# a log class and the push class that tells the same story (W11b's log beside W13's count)
AGREE = {('once', 'once'), ('twice', 'twice'), ('fresh', 'fresh'), ('fresh', 'once afresh')}
PATHS = {'realign': 'once: stores 1 [realign into last 10000, flag false]',
         'stamp': 'once: stores 1 [stamp into last 10000, flag false]',
         'silent': 'none: stores 0 [], skips table 0 miss 0, silent 1,'}
# TestPushPaths's filter, its 48 touches counted (every count not named is 0)
PATH_COUNTS = {'calls': 48, 'once': 27, 'none': 21, 'silent': 21, 'first fresh': 3, 'stores claim': 3,
               'stores table': 22, 'stores realign': 1, 'stores stamp': 1}
REACH = ['missPlaced', 'skips miss', 'skips table', 'again', 'fresh', 'stores stamp', 'stores realign', 'stores wait',
         'silent']
LEGRUNS = {'w4d': 'legs-w4d.txt', 'head': 'legs-head.txt', 'head-race': 'legs-head-race.txt'}
RACES = {'w4d': 'race-w4d.txt', 'head': 'race-head.txt', 'head-race': 'race-head-race.txt'}
FUZZ_HEAD = {'as is': 'cfuzz-head.txt', 'straggler': 'cfuzz-head-straggle.txt', 'tags 8': 'cfuzz-head-tags.txt'}
FUZZ_POWER = 'cfuzz-w4d-straggle.txt'
MODES = ['plain', 'ConstantTime', 'Background']
PLACED_NOTE = sorted(['+\t\tif pushOn {', '+\t\t\tpt.placed()', '+\t\t}'])
NOTES = 'go/cuckoo.go go/filter.go go/push.go go/push_off.go go/push_on.go go/touch.go'
GATES = ['stores table', 'stores stamp', 'stores realign', 'stores claim', 'stores wait', 'skips table', 'skips miss',
         'missPlaced', 'fresh', 'first fresh', 'again', 'behind', 'silent', 'none']


def readlines(o, name):
    try:
        with open(os.path.join(o, name), errors='replace') as f:
            return f.read().splitlines()
    except FileNotFoundError:
        return []


def pclass(rest):
    """the push class of a leg's 'K's touch' text: once, once afresh, fresh, again, twice or none"""
    c = rest.split(':', 1)[0].strip()
    if c == 'once' and re.search(r'stores 1 \[\w+ afresh', rest):
        return 'once afresh'
    return c


def parse_legs(lines):
    legs, paths, fails, passes = {}, {}, set(), set()
    for s in lines:
        m = re.search(r"W13 leg (\S+) \| (.*?) \| log (\S+) \| K's touches (\d+) \| K's touch: (.*)$", s)
        if m:
            legs[m.group(1)] = {'name': m.group(2), 'log': m.group(3), 'n': int(m.group(4)), 'touch': m.group(5),
                                'push': pclass(m.group(5))}
        m = re.search(r'W13 path (\S+)[^|]*\| (.*)$', s)
        if m:
            paths[m.group(1).rstrip(',')] = m.group(2)
        m = re.search(r'--- FAIL: (\S+)', s)
        if m:
            fails.add(m.group(1))
        m = re.search(r'--- PASS: (\S+)', s)
        if m:
            passes.add(m.group(1))
    return legs, paths, fails, passes, any('WARNING: DATA RACE' in s for s in lines)


def parse_race(lines):
    """W13's rounds by (log, push), the run's rounds, its passing tests, a data race, and the rounds W11b's lines say
    looked again (None without them)"""
    rounds = {}
    total, looked = None, None
    for s in lines:
        m = re.search(r'W13 race: (\d+) of (\d+) rounds: log (\S+), push (.*)$', s)
        if m:
            rounds[(m.group(3), m.group(4))] = int(m.group(1))
            total = int(m.group(2))
        m = re.search(r'W11b race: (\d+) of \d+ rounds: .*K looked again (true|false)$', s)
        if m:
            looked = (looked or 0) + (int(m.group(1)) if m.group(2) == 'true' else 0)
    passes = {m.group(1) for s in lines for m in [re.search(r'--- PASS: (\S+)', s)] if m}
    return rounds, total, passes, any('WARNING: DATA RACE' in s for s in lines), looked


def parse_counts(s):
    d = {}
    for item in s.split(', '):
        k, _, v = item.rpartition(' ')
        if k and v.lstrip('-').isdigit():
            d[k] = int(v)
    return d


def parse_fuzz(lines):
    """the summary's cases, duration, rate, push counts by mode and cases with a touch twice; failure lines"""
    out = None
    for s in lines:
        m = re.search(r'(\d+) cases from seed \d+ in (\S+) \(([\d.]+) M ops/s\): .*W13: push counts: plain \{(.*?)\}; '
                      r'ConstantTime \{(.*?)\}; Background \{(.*?)\}; cases with a touch twice (\d+)', s)
        if m:
            out = {'cases': int(m.group(1)), 'took': m.group(2), 'rate': float(m.group(3)),
                   'modes': {md: parse_counts(m.group(4 + i)) for i, md in enumerate(MODES)}, 'twice cases': int(m.group(7))}
    nopush = any('W13: no push count' in s for s in lines)
    failing = [s.strip() for s in lines if re.match(r'\s*fuzzconc_test\.go:\d+: case \d+: ', s)]
    return out, nopush, failing, any('WARNING: DATA RACE' in s for s in lines), any(s.startswith('panic') for s in lines)


def norm_conf(lines):
    out = []
    for s in lines:
        s = re.sub(r'\(\d+(\.\d+)?s\)', '(t)', s)
        s = re.sub(r'\t\d+(\.\d+)?s$', '\tt', s)
        out.append(s)
    return out


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


results = {}


def show(n, title, c, gate=None):
    s = c.status()
    if gate and s != 'NOTHING READ':
        s = f'NOT JUDGED ({gate}); as read: {s}'
    results[n] = s
    print(f'{n}. {title}: {s}')
    for kind, items in (('fails', c.fails), ('nothing read', c.nothing), ('not judged', c.unjudged), ('', c.notes)):
        for d in items:
            print(f'   - {kind + ": " if kind else ""}{d}')


def main():
    if len(sys.argv) != 2 or not os.path.isdir(sys.argv[1]):
        print('usage: w13.py <lab/w13>')
        return 2
    o = sys.argv[1]
    legruns = {r: parse_legs(readlines(o, f)) for r, f in LEGRUNS.items()}
    races = {r: parse_race(readlines(o, f)) for r, f in RACES.items()}
    progress = readlines(o, 'progress.txt')
    fuzz = {n: parse_fuzz(readlines(o, f)) for n, f in FUZZ_HEAD.items()}
    power = parse_fuzz(readlines(o, FUZZ_POWER))
    w7 = readlines(o, 'w7-head.txt')
    w11 = readlines(o, 'w11-head.txt')

    print('W13, the push count and its readings')
    for r, (legs, paths, fails, passes, race) in legruns.items():
        print(f'{r}: {len(legs)} leg lines, {len(paths)} path lines; failing: {sorted(fails) or "none"}; data race: {race}')
        for i in LEGS:
            d = legs.get(i)
            print(f'  {i:>3}: ' + (f"log {d['log']:<6} push {d['push']:<11} ({d['n']} touch): {d['touch']}" if d else 'no line'))
        for p, t in paths.items():
            print(f'  path {p}: {t}')
    for r, (rounds, total, _, race, looked) in races.items():
        print(f'race {r}: ' + ('; '.join(f'{n} log {lg}, push {pu}' for (lg, pu), n in sorted(rounds.items())) or 'no lines')
              + f' (of {total}); looked again {looked}; data race: {race}')
    print()
    for n, (f, nopush, failing, race, panic) in list(fuzz.items()) + [('power, 23f03b6 straggler', power)]:
        if not f:
            print(f'fuzzer {n}: no summary with push counts{" (built without the count)" if nopush else ""}')
            continue
        print(f"fuzzer {n}: {f['cases']} cases in {f['took']} ({f['rate']} M ops/s); cases with a touch twice "
              f"{f['twice cases']}; failing case lines {len(failing)}; data race {race}; panic {panic}")
        for md in MODES:
            cnt = f['modes'][md]
            print(f'  {md}: ' + ', '.join(f'{k} {v}' for k, v in cnt.items()))
        kinds = {}
        for s in failing:
            for m in re.finditer(r'(\d+) ([a-zA-Z ]+?) \(first: ', s):
                kinds[m.group(2)] = kinds.get(m.group(2), 0) + int(m.group(1))
            if 'stored their push twice' in s and 'push twice' not in kinds:
                kinds['push twice (23f03b6\'s format)'] = kinds.get('push twice (23f03b6\'s format)', 0) + 1
        if kinds:
            print('  failing answers and logs by kind: ' + ', '.join(f'{k} {v}' for k, v in sorted(kinds.items())))
    w7c = [m for s in w7 for m in [re.search(r'W13 (\S+) build, mode (\S+), shape (\S+): push counts: (.*)$', s)] if m]
    for m in w7c:
        print(f'W7 mode {m.group(2)}, shape {m.group(3)}: {m.group(4)}')
    for s in w7:
        if re.search(r'build, mode \S+, shape \S+: \d+ rounds', s):
            print('W7: ' + s.strip()[:400])
    print()

    # 1. sight
    c = Clause()
    for r, (legs, paths, fails, passes, race) in legruns.items():
        tree = 'w4d' if r == 'w4d' else 'head'
        if not legs:
            c.nothing.append(f'{r}: no leg lines')
        else:
            c.judged += 1
            for i in LEGS:
                d = legs.get(i)
                if not d:
                    c.fails.append(f'{r} leg {i}: no line')
                    continue
                if d['n'] != 1:
                    c.fails.append(f"{r} leg {i}: {d['n']} notes for K's touch")
                if d['push'] != expect(tree, i):
                    c.fails.append(f"{r} leg {i}: push {d['push']}, predicted {expect(tree, i)}")
                if (d['log'], d['push']) not in AGREE:
                    c.fails.append(f"{r} leg {i}: log {d['log']} beside push {d['push']}")
                if tree == 'head' and i in PLACED and not ('skips table 1' in d['touch'] and 'missPlaced 1' in d['touch']):
                    c.fails.append(f"{r} leg {i}: not missPlaced then the table's skip: {d['touch']}")
                if tree == 'head' and i in WAITED and not ('missPlaced 1' in d['touch'] and 'behind false' in d['touch']):
                    c.fails.append(f"{r} leg {i}: not missPlaced, or behind: {d['touch']}")
            want = {'TestDripHotRestoreFresh'} | {t for t in fails if '/' in t and t.split('/')[1].split('_')[0] in PLACED} if tree == 'w4d' else set()
            if tree == 'w4d':
                sub = {t.split('/')[1].split('_')[0] for t in fails if t.startswith('TestDripHotRestoreFresh/')}
                if sub != set(PLACED) or fails - want:
                    c.fails.append(f'{r}: failing {sorted(fails)}, want TestDripHotRestoreFresh on legs 1, 2, 4 and 5 alone')
            elif fails:
                c.fails.append(f'{r}: failing {sorted(fails)}')
            if race:
                c.fails.append(f'{r}: a data race')
        if not paths:
            c.nothing.append(f'{r}: no path lines')
        else:
            c.judged += 1
            for p, want in PATHS.items():
                got = paths.get(p)
                if got is None or not got.startswith(want):
                    c.fails.append(f'{r} path {p}: {got}, predicted {want}...')
            got = parse_counts(paths.get('counts', ''))
            off = {k: v for k, v in got.items() if v != PATH_COUNTS.get(k, 0)}
            if not got or off or set(PATH_COUNTS) - set(got):
                c.fails.append(f"{r} path counts: {off or 'no line'}, predicted {PATH_COUNTS} and 0 elsewhere")
    for r, (rounds, total, _, race, _) in races.items():
        if not rounds:
            c.nothing.append(f'race {r}: no lines')
            continue
        c.judged += 1
        if sum(rounds.values()) != total:
            c.fails.append(f'race {r}: {sum(rounds.values())} rounds classed of {total}')
        for (lg, pu), n in rounds.items():
            if (lg, pu) not in AGREE:
                c.fails.append(f'race {r}: {n} rounds log {lg} beside push {pu}')
        if race:
            c.fails.append(f'race {r}: a data race')
        if r == 'w4d':
            tw = sum(n for (lg, pu), n in rounds.items() if pu == 'twice')
            c.notes.append(f'race on 23f03b6: {tw} rounds twice')
            if tw == 0:
                c.fails.append('race on 23f03b6: no round twice, no sight in the race')
    show(1, "sight: each leg's and round's push class as predicted and agreeing with its log, both trees; "
            'the paths as predicted', c)
    gate = None if results[1] == 'HOLDS' else 'clause 1 does not hold'

    # 2. the library
    c = Clause()
    lib = [s for s in progress if "0: library files that differ from c8eab03's" in s]
    if lib:
        c.judged += 1
        got = re.search(r"c8eab03's: \[(.*)\]", lib[0]).group(1).strip()
        if got != NOTES:
            c.fails.append(f'library files changed: [{got}], want [{NOTES}]')
    else:
        c.nothing.append("no line for the library's files")
    port = [k for k, s in enumerate(progress) if "0: the notes' changed lines" in s]
    if port:
        c.judged += 1
        k = port[0]
        c.notes.append(progress[k].split('] ', 1)[-1])
        extra = []
        for s in progress[k + 1:]:
            if not s.startswith('    ') or s[4:6] not in ('< ', '> '):
                break
            extra.append(s[4:])
        head_only = sorted(s[2:] for s in extra if s.startswith('< '))
        port_only = [s[2:] for s in extra if s.startswith('> ')]
        if head_only != PLACED_NOTE or port_only:
            c.fails.append(f"the port's lines differ from the head's by {extra}, want the missPlaced note's three alone")
    else:
        c.nothing.append("no line for the port's changed lines")
    for key, want in (('1a default exact', 'IDENTICAL'), ('1b default digest', 'IDENTICAL'),
                      ('1c tfdrip exact', "IDENTICAL to W4e's"), ('1d tfdrip,tfpush exact', "IDENTICAL to W4e's")):
        ln = [s for s in progress if key + ':' in s]
        if not ln:
            c.nothing.append(f'{key}: no line' + (' (skipped)' if any('W13_NOEXACT' in s for s in progress) else ''))
        else:
            c.judged += 1
            if not ln[0].rstrip().endswith(want):
                c.fails.append(ln[0].split('] ', 1)[-1])
    cd, cp = readlines(o, 'conf-drip.txt'), readlines(o, 'conf-push.txt')
    if not cd or not cp or not any('TestConformance' in s for s in cd + cp):
        c.nothing.append('TestConformance: no lines in one build')
    else:
        c.judged += 1
        if norm_conf(cd) != norm_conf(cp):
            c.fails.append('TestConformance prints other lines under tfdrip,tfpush than under tfdrip')
        for s in cd:
            if 'differ in all' in s:
                c.notes.append('TestConformance, both builds: ' + s.strip()[:160])
    vet = [s for s in progress if '1f go vet, tags' in s]
    if len(vet) != 4:
        c.nothing.append(f'go vet: {len(vet)} of 4 tag sets logged')
    if vet:
        c.judged += 1
        for s in vet:
            if not s.rstrip().endswith('exit 0 (0 lines)'):
                c.fails.append(s.split('] ', 1)[-1])
    show(2, "the library's answers unchanged: the notes alone; the exact outputs, digest and TestConformance's lines as "
            'before; go vet clean in the four tag sets', c)

    # 3. the fuzzer on the head
    c = Clause()
    for n, (f, nopush, failing, race, panic) in fuzz.items():
        if not f:
            c.nothing.append(f'{n}: no summary with push counts' + (' (built without the count)' if nopush else ''))
            continue
        c.judged += 1
        if f['twice cases']:
            c.fails.append(f"{n}: {f['twice cases']} cases with a touch twice")
        for md in MODES:
            cnt = f['modes'][md]
            if cnt.get('calls', 0) == 0:
                c.nothing.append(f'{n}, {md}: no calls')
                continue
            if cnt.get('twice', 0):
                c.fails.append(f"{n}, {md}: {cnt['twice']} touches twice")
            c.notes.append(f"{n}, {md}: {cnt.get('calls', 0)} touches, twice {cnt.get('twice', 0)}")
        if race or panic:
            c.fails.append(f'{n}: data race {race}, panic {panic}')
    # reach: each path's gate summed over the three legs, by mode; a 0 is NOTHING READ for that path
    read = [f for f, *_ in fuzz.values() if f]
    for g in REACH:
        by = {md: sum(f['modes'][md].get(g, 0) for f in read) for md in MODES}
        if read:
            zero = [md for md in MODES if by[md] == 0]
            c.notes.append(f'reach, {g}: ' + ', '.join(f'{md} {by[md]}' for md in MODES)
                           + (f' (not reached in {", ".join(zero)}: NOTHING READ for its path there)' if zero else ''))
    if read:
        c.notes.append('behind (reported; from the code, 0 on the head): '
                       + ', '.join(f"{md} {sum(f['modes'][md].get('behind', 0) for f in read)}" for md in MODES))
    show(3, 'the fuzzer on the head: no touch stores its push twice, in each mode of the three legs '
            '(each path read only where its reach line is not 0)', c, gate)

    # 4. W7's scenario and W11b's race on the head
    c = Clause()
    if not w7c:
        c.nothing.append("W7: no push count line")
    for m in w7c:
        cnt = parse_counts(m.group(4))
        if cnt.get('calls', 0) == 0:
            c.nothing.append(f'W7 {m.group(2)} {m.group(3)}: no calls')
            continue
        c.judged += 1
        if cnt.get('twice', 0):
            c.fails.append(f"W7 {m.group(2)} {m.group(3)}: {cnt['twice']} touches twice")
        c.notes.append(f"W7 {m.group(2)} {m.group(3)}: {cnt['calls']} touches, twice {cnt.get('twice', 0)}, behind {cnt.get('behind', 0)}")
    for r in ('head', 'head-race'):
        rounds, looked = races[r][0], races[r][4]
        if not rounds:
            c.nothing.append(f'race {r}: no lines')
            continue
        c.judged += 1
        tw = sum(n for (_, pu), n in rounds.items() if pu == 'twice')
        c.notes.append(f'race {r}: {tw} rounds twice, {looked} looked again')
        if tw:
            c.fails.append(f'race {r}: {tw} rounds with K\'s touch twice')
        if looked is None or looked < 10:
            c.unjudged.append(f'race {r}: {looked} rounds looked again, under 10 (W11b\'s condition)')
    show(4, "W7's scenario (Background, spread) and W11b's race on the head: no touch stores its push twice", c, gate)

    # 5. every touch noted
    c = Clause()
    reads = [(n, f[0]) for n, f in fuzz.items()] + [('power', power[0])]
    for n, f in reads:
        if not f:
            c.nothing.append(f'{n}: no summary with push counts')
            continue
        c.judged += 1
        for md in MODES:
            b = f['modes'][md].get('bare', 0)
            if b:
                c.fails.append(f'{n}, {md}: {b} touches returned with no note')
    for m in w7c:
        c.judged += 1
        b = parse_counts(m.group(4)).get('bare', 0)
        if b:
            c.fails.append(f'W7 {m.group(2)} {m.group(3)}: {b} touches returned with no note')
    if not w7c:
        c.nothing.append('W7: no push count line')
    show(5, 'every touch noted: none returned with no store and no silent note, in every count read', c)

    # 6. the power leg
    c = Clause()
    f = power[0]
    if f:
        c.judged = 1
        tw = sum(f['modes'][md].get('twice', 0) for md in MODES)
        c.notes.append(f"23f03b6 straggler (its own older fuzzer): {f['cases']} cases, {tw} touches twice in "
                       f"{f['twice cases']} cases; behind (a look again after a placed history, the reach W9's path has "
                       "there): " + ', '.join(f"{md} {f['modes'][md].get('behind', 0)}" for md in MODES))
        hp = [f2 for f2, *_ in fuzz.values() if f2]
        if hp:
            c.notes.append("the head's missPlaced over its three legs, the same reach there: "
                           + ', '.join(f"{md} {sum(f2['modes'][md].get('missPlaced', 0) for f2 in hp)}" for md in MODES))
        if tw == 0:
            c.fails.append('no touch twice on 23f03b6 under the fuzzer: the count\'s power there not shown')
    else:
        c.nothing.append('no summary with push counts')
    show(6, 'power: the fuzzer on 23f03b6 (straggler) reads a touch twice (qualifies the verdict)', c, gate)

    # 7. the head's tests pass
    c = Clause()
    passed = [t for t in ('TestDripHotRestoreLegs', 'TestDripHotRestorePushOnce') if any(f'--- PASS: {t} ' in s for s in w11)]
    if w11:
        c.judged += 1
        if len(passed) < 2 or any('--- FAIL' in s for s in w11):
            c.fails.append(f"W11's and W4e's tests: passed {passed or 'none'}")
    else:
        c.nothing.append("W11's and W4e's tests: no lines")
    for r in ('head', 'head-race'):
        legs, paths, fails, passes, _ = legruns[r]
        if not legs and not paths:
            continue
        c.judged += 1
        for t in ('TestDripHotRestoreFresh', 'TestPushPaths', 'TestPushNames'):
            if t not in passes:
                c.fails.append(f'{r}: {t} did not pass')
        rp = races[r][2]
        if races[r][0] and 'TestDripHotFreshRace' not in rp:
            c.fails.append(f'race {r}: TestDripHotFreshRace did not pass')
    show(7, "the head's tests pass under tfdrip,tfpush: W11b's legs and race, the paths, W11's and W4e's tests", c)

    judged = [results[k] for k in (1, 2, 3, 4, 5, 7)]
    ok = all(s == 'HOLDS' for s in judged)
    print()
    print(f"W13: clauses 1 to 5 and 7 {'all hold' if ok else 'do not all hold'}; clause 6 (power): {results[6]}")
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
