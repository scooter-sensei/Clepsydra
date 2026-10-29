# Measuring speed

Speed claims (S in [the claims register](claims.md)) are host-dependent: they are reported, not checked. A claim **holds** if the measured direction and rough size match. Compare ratios measured on one host, never absolute numbers across hosts ([hosts](../README.md#hosts)). All paths are relative to `go/`.

## Tools

- **`verify/ab.sh <A> <B> <bench-regex> [rounds]`.** A and B are git revisions (`.` is the working tree), optionally with patches: `HEAD`, `HEAD+verify/experiments/branchless-find.patch`, `4103013`. Each version's test binary is built once; the binaries then run alternately (A, B, A, B, ...) `rounds` times (default 8), so drift and noisy neighbours hit both sides. It prints min and median ns/op per benchmark and the B/A ratio of medians. On the Xeon host same-build A/B pairs differed by up to 3%, so smaller ratios are noise; A/A runs of one build differ by up to ~15% on a noisy host.
- **`METRIC=cycles/op`** (in `verify/ab.sh` and `verify/lrab.sh`). On Linux the benchmarks also report `cycles/op`: user-mode CPU cycles of the timed loop, counted on the benchmark's locked thread through `perf_event_open` (`cycles_linux_test.go`; nothing is reported where perf events are unavailable). The host's clock frequency moves ns/op but not cycles/op, so `cycles/op` compares the code more than the moment it ran [S56].
- **`verify/lrab.sh <bench-regex> <layouts> <rounds> <arm>...`** (placement-randomized A/B). `verify/ab.sh` (and so the perf gate, which uses it) times each version at the one code layout its build gets, and on the Ryzen host a benchmark's readings move with the code's placement by a few percent, and from run to run of one binary by as much or more [S54, S55], so a few percent between two builds says little. `lrab.sh` builds every arm at several layouts: never-run pad functions of 0 to 24 statements (~0–450 bytes), sized independently by a hash of the layout's seed and the pad's place, at the top of `filter.go`, `hot.go` and `register.go` and inside `filter.go` before `touch`, `hit` and `locate`, kept in the binary by an init behind an environment variable no run sets; layout k has the same pads in every arm. Rounds alternate layouts and arms (in reverse order on even rounds). Per benchmark it reports the first arm's median and its spread over the layouts (max/min of its per-layout medians); how much of the readings' variation is placement (`sd layout`) and how much is run to run (`sd run`), as standard deviations of log readings; then each other arm's ratio to the first, paired at each layout, as a geometric mean with a 95% t-interval. `NULL=1` adds an arm that runs the first arm's binaries again, which shows the interval a change of nothing gets; `SEED0=n` uses layouts n+1..n+`<layouts>`. Use it to judge a change of a few percent.
- **`./test.sh perf [A]`**: the 12 core benchmarks of A (default `HEAD`, or `HEAD~1` on a clean tree) against the working tree, via `verify/ab.sh`; fails when both the minimum and the median run over `PERF_MAX` (1.20) times A's.
- **`verify/speed.sh > ../docs/artifact/speed-runs.md`**: re-measures every speed claim of the Xeon host, one comparison at a time, as a Markdown report ([speed-runs.md](speed-runs.md)). About an hour and ~2 GB of RAM (the DRAM benchmarks build 600 MB tables).
- **`./bench.sh "label"`**: race tests, then `go run ./cmd/tfbench -label` (`GOMAXPROCS` 4 by default), appending a row to [the scorecard history](../record/scorecard-history.md).
- **Importers.** A program that imports only the library times loops without the test files compiled into the package (they move its code: S46). Two were used, not committed: one with InsertSteady's, TouchAndCount's, CheckAndTouchHit's and HitHot's loops (best of 5 each), and one with 1M keys touched in turn densely (within one unit), sparsely (the clock 25 ticks on per touch, so every touch realigns), and both from GOMAXPROCS goroutines. `cmd/batcharms` (in `verify/experiments/batch-arms.patch`) times one batch arm the same way.
- **Layout controls.** A control arm changes placement and nothing else: the base plus a 32-byte function called once from `New` (hpad), or a size-matched copy of a change's code at its own place, never called on the measured path (spad). A function nothing calls is dropped by the linker, so it controls nothing [X33].

## Rules for readings

Every speed reading behind a decision had its rule (what it must show for the change to be adopted) written before it ran. When placement's spread made a bar unreachable, the rule was not overridden; a new rule was written for the next readings:

- At 10 layouts the intervals' half-widths are 1–12%: a 1.03 bar on the upper bound can't be met by a change with no cost at all on HitHot, whose placement spread then read 19% (later shown to be mostly the host's run-to-run noise, X35). The single-layout bars of S51 and S52 were finer than placement allows on the Ryzen host.
- So: judge the geometric mean over layouts against the bar, report the interval, and size the number of layouts to the benchmark's placement spread; give every placement reading a null arm (`NULL=1`) and use the null arm's interval as the bar's floor; read cycles/op when the question is the code.

## The placement study

How much of a speed reading's variation comes from the code (its placement in the binary, what the benchmark makes it do, how its fields share cache lines) rather than the host, by what mechanism, and what in the code, its build or the measuring tools reduces it. All readings on the Ryzen host; cycles/op where it says so (frequency out), ns/op elsewhere.

### Before the study

- Test binaries and importers disagree: HitHot read 1.14–1.15 in the test binaries while the same loop in an importer took 47.8 ns against 50.5 ns at the base: the test files compiled into the package moved its code, not the library's speed [S46].
- Placement alone moves the importer's TouchAndCount loop by about 3%, and its HitHot loop 5.5% [S49, S50].
- A 32-byte pad underestimates placement: the same ~700 bytes of realign2 in three places moved TouchAndCount from 0.999 to 1.147 in the test binaries. The control for a change's cost on a path it doesn't run is the change's own bytes at its own place, never called (spad) [S52].
- Placement-randomized readings (10 layouts, the pads at the tops of three files) [S54]: the plain benchmarks' max/min of per-layout medians read up to 1.19 (corrected: that statistic includes the host's run-to-run noise, X35). A null arm (32 bytes before `hit`, nothing new executed) read HitHot 1.023 [1.000, 1.046]: a shift inside `filter.go` keeps the offsets within the file, which pads at the tops of files don't randomize, so even placement-randomized a difference of 2–3% on HitHot is within what a pure shift does.

### The readings

| Reading | What it measured | Claims |
|---|---|---|
| M1 | counters per layout: 5c54259, 20 layouts, Zen 2 counters by raw event, HitHot at 3e7 iterations, 3 rounds × 4 groups | S58 |
| M2 | S54's statistic, layouts against a null arm (layout 1's binary ×10): 5c54259, S54's five benchmarks, 10 layouts of the old `lrab.sh` pads, 6 rounds, ns/op | S55 |
| M3 | placement sd against run sd: the tree with `cycles/op`, 16 layouts and the null arm, 4 rounds; one-way random-effects components of log readings, layouts as groups | S55, S56 |
| M4 | 64-byte function alignment (E24) and PGO (E25) against the base, 16 layouts, 4 rounds, cycles/op | S57 |
| M5 | 32 common shifts of 0–5.2 KB (one pad at the top of `filter.go`) | S58 |
| M6 | the old 1-D pad family against the independent pads, seeds 1–16, alternated, 4 rounds, cycles/op | S59 |
| M7, M7b | false sharing: 11 goroutines of `ContainsAt` on a cached table, a 12th adding to `f.waited`, to a counter on its own line, or idle; 8 rounds alternated, ns/op (M7b replicates M7 under a corrected rule) | S60 |
| M8 | the 64-byte-aligned build's layouts 14–16 and the base's layout 15, counters | S58 |
| M9 | the field move and hot-path adjacency: 16 layouts of 81478eb's pads, 4 rounds, cycles/op, a null arm | S60, E26 |

| Reading | Result |
|---|---|
| M2: S54's statistic, layouts / null arm (ns/op, 10 × 6) | Hit 1.066 / 1.025, HitHot 1.032 / 1.031, ContainsAbsent 1.093 / 1.022, TouchAndCount 1.091 / 1.021, VelocityCount 1.068 / 1.046 |
| M3: placement sd / run sd, cycles/op (16 × 4) | Hit 0.6 / 2.4%, HitCached 1.5 / 0.9%, HitHot 1.8 / 1.0%, ContainsAbsent 0 / 2.0%, TouchAndCount 3.4 / 4.1%, VelocityCount 3.4 / 2.9%; null F 0.39–0.95 |
| M3: run sd, cycles / ns | lower in cycles on all six, base and null (10–33%) |
| M8: the aligned build's layout 15 against its neighbours | 217.8 against 179 cycles; 285 decoder ops/op against 2.5–4 |
| M5: 32 common shifts over 5.2 KB | HitHot 208–213 at every shift (layout 0's arrangement); sd over shifts 0.40–0.61 of placement's |
| M4: aligned build, PGO against the base | see S57 and below |
| M7b: 11 readers, a 12th writing `f.waited` / its own line | 8.74 / 5.20 ns as committed; 5.33 / 5.30 moved |
| M9: fields / 81478eb; adjacency / fields (16 × 4, cycles/op) | fields 0.987–1.006 on all six; adjacency raised placement sd on all four it aimed at (HitHot 2.0 → 5.5%), HitHot 1.049, TouchAndCount 0.949 (E26, not adopted) |

### How much is the host's [S55, S56; correcting S54: X35]

S54 read "placement alone moves HitHot 19%" from the spread of per-layout medians of 3 runs. One binary run as 10 "layouts" (a null arm) reads that same statistic at 1.02–1.05, against 1.03–1.09 for real layouts (M2): much of a spread of medians of 3 is the host. Separating the two, in cycles/op over 16 layouts and 4 rounds (M3):

| Benchmark | placement sd | run sd | F (5%: 1.88) |
|---|---|---|---|
| CheckAndTouchHit (DRAM) | 0.6% | 2.4% | 1.27 |
| CheckAndTouchHitCached | 1.5% | 0.9% | 12.1 |
| CheckAndTouchHitHot | 1.8% | 1.0% | 14.5 |
| ContainsAbsent | 0 | 2.0% | 0.44 |
| TouchAndCount | 3.4% | 4.1% | 3.69 |
| VelocityCount | 3.4% | 2.9% | 6.75 |

The null arm's F is 0.39–0.95 for all six, so the estimator sees no placement where there is none. Placement is real and a few percent, the size of one run's noise, not 19%. cycles/op has a smaller run sd than ns/op on every benchmark, base and null alike, by 10–33%. No GC cycle completes inside a timed loop.

### The mechanism [S58]

- **The front end** (M1, 20 layouts, Zen 2 counters by raw event): every layout runs the same 535.10 instructions per hit; cycles track IC pipe stalls and op-queue-empty cycles (r 0.86 each), not mispredictions (1.04 per op in every layout) or predictor overrides. The hit is front-end-heavy anywhere: 51 taken branches per op, ~58 L1 predictor overrides, IC stalls ~70 of ~182 cycles.
- **Not the 64-byte phase.** Functions start on 32-byte boundaries, so each sits at phase 0 or 32 of a line; layouts with the same phase for all nine hot functions differed by up to 5%, and a linker aligning every function to 64 (E24) still had one HitHot layout 20% slower.
- **The op cache.** In that layout half the hit's ops (285 of ~576 per op) come from the legacy decoder, against 2.5–4 in its neighbours, with 2.0 op-cache-to-decoder switches per op against 0.01 (M8). M1's milder layouts had 29–49 decoder ops against 5–11.
- **Relative, not absolute.** One pad at the top of `filter.go` shifted all of the package and the test code by 0–5.2 KB in 32 steps (M5): every shift kept that arrangement's level (HitHot 208–213 cycles, against 180–186 in other arrangements); what moved with the shift was 1–2%. By the rule's statistic (sd over shifts against placement's sd: 0.40–0.61) the verdict is "both"; the level says the arrangement of the hot code relative to itself carries most of it.
- **A model, partly.** An op cache of 64 sets × 8 ways indexed by address bits 6–11 (4,096 ops: Zen 2's published size), in which an entry starts at each hot entry point (a function's start, a branch target, a return point after a call) and ends at its 64-byte window, predicts overflow in M1's layouts 9–13 and nowhere else; the measured high-decoder layouts are 9–13 and 18 (r 0.72). Out of sample it doesn't predict M8's severe layout: the real rule has more to it (per-line limits on immediates, or the time the decoder keeps fetching after a switch).

### What changes it [S57, S59]

| Arm, 16 layouts × 4 rounds, cycles/op | HitCached | HitHot | TouchAndCount | VelocityCount |
|---|---|---|---|---|
| base: placement sd | 1.5% | 1.8% | 3.4% | 3.4% |
| 64-byte functions (E24): sd; ratio | 0.3%; 0.950 | 4.8%; 0.982 | 1.0%; 1.037 | 3.7%; 1.008 |
| PGO (E25): sd; ratio | 2.1%; 0.864 | 6.1%; 0.923 | 1.5%; 0.874 | 0; 0.886 |

Neither removes placement's effect; each moves it. By the rule (half the sd on two of the four, no ratio over 1.02) 64-byte alignment fails (TouchAndCount 1.037) and PGO qualifies, but isn't adopted: with a profile in the repository the benchmarks would time a build users get only with their own profile (noted before the reading). PGO's speed is the user's to take: 2–14% in cycles on all six benchmarks with a profile of this workload. Go 1.24.7 and the current 1.27.3 alike align amd64 functions to 32 bytes, keep loop alignment off (`maxLoopPad = 0`) and offer 64-byte alignment only to PGO-hot loop headers (`PCALIGNMAX 64`, which raises the function's alignment too): there is no flag to try.

`verify/lrab.sh` as committed in 8d689ce sized its three pads (seed·7919 + i·104729) mod 25, which are always (n, n+4, n+8) mod 25: one 1-D family, not three independent draws [X37]. 81478eb sizes six pads by a hash of (seed, place), three of them inside `filter.go` before `touch`, `hit` and `locate`, and reports placement's and the runs' sd and a null arm's interval. On this tree the old family read placement alike (M6: no benchmark's sd 1.5x higher with the new pads), so M3–M5 stand as measured.

### What the benchmark made the code do [X36]

`BenchmarkCheckAndTouchHitHot` has timed refused promotions since hot entries (e9974b9): its 4,096 keys return ~4,000 times within one unit, every register fills, and with the 37 hot entries taken every touch runs `bump` → `promote` → `hotEntryFor` and an atomic add on `hotRefused` (perf: 16% of its cycles in those three: hotEntryFor 8.6%, promote 3.8%, nextRegU 3.3%). Its loop stays under its name, since comparisons across commits match names; its comment says what it times. `BenchmarkCheckAndTouchHitCached` times the hit it meant: a clock of 64 ticks a touch, ~10 touches per key per unit, no register filled (checked after the loop).

### False sharing of the per-call flag [S60]

`f.ct`, read by every call, shared its cache line with `deferred` and `waited` (added on every Background or ConstantTime wait), `placeNs` and `promoted` (offsets from `unsafe.Offsetof`: offset 27,392, line 428 of the 31,656-byte `Filter`). Eleven goroutines running `ContainsAt` on a cached table (which writes nothing) while a twelfth adds to `f.waited`: 8.74 ns/op, against 5.20 with the twelfth adding to a counter of its own line (M7b; M7 read the same, 1.71x, but its rule's control condition, own/idle ≤ 1.02, failed at 1.13 because the 12th thread takes an SMT sibling in both arms, so it was replicated under a corrected rule). With `ct`, `pad`, `hot`, `hotN` and `bg` moved into the header beside `seed`: 5.33 against 5.30 (1.006), and 4.67 against 4.65 with no writer.

### Two changes measured (M9)

- **The field move** (349bff8, S60): 0.987–1.006 of 81478eb on all six benchmarks, under the 1.02 bar its rule set. Adopted.
- **Hot-path adjacency** (E26): `slotWords`, `locate`, `locateBoth`, `find` and `farAhead` moved to right after `touch`, so the hit path's code in `filter.go` is contiguous (profile-guided code positioning done by source order). The premise was that conflicts come from hot functions scattered tens of KB apart. Placement's sd rose on all four benchmarks it aimed at (HitHot 2.0 → 5.5%, HitCached 2.2 → 4.2%, VelocityCount 2.9 → 4.7%, TouchAndCount 3.4 → 4.0%) and HitHot ran 4.9% slower, though TouchAndCount and VelocityCount ran 4–5% faster. Not adopted. With the severe layout the model doesn't predict, it is the second sign that the op cache's rule is not a plain set capacity.

## Host notes (Ryzen host)

- perf works in WSL2 with the core PMU (6 counters): `/usr/lib/linux-tools/6.18.54.1-microsoft-standard-WSL2/bin/perf`. Zen 2's named events are absent, raw ones work: `r1aa` decoder ops, `r2aa` op-cache ops, `r487` IC stalls, `ra9` op-queue empty, `r20000028a` OC→IC switches; M1 also read predictor overrides (0x8A/0x8B), branches (0xC2–0xC4) and 32-byte fetches (0x80/0x81). `perf report` doesn't resolve Go symbols there; a script mapped sample IPs through `go tool nm`.
- `go test` does not use a `default.pgo` by itself: pass `-pgo=default.pgo`.
- A 12-thread benchmark on that host has every reader share an SMT sibling with something: compare arms that share the same extra threads.
- Toolchains used besides Go 1.24.7: `go1.24.7-fa64` (the linker's `funcAlign` set to 64 in `src/cmd/link/internal/amd64/l.go`, then `go install cmd/link`) and `go1.27.3` (the current release, checksum-verified; its amd64 `funcAlign` is still 32).
- Speed runs keep the host to themselves: checks that shared the host with a reading say so in their claims.

## Uncommitted tools

The scripts behind the readings above were kept outside the repository: per-layout counters (`m1.sh`, `m1sum.py`), layouts with a null arm and variance components (`lrc.sh`, `lrc2.sh`, `vc.py`), common shifts (`m5.sh`), false sharing (`m7.sh`, `m7b.sh`, `zz_share_test.go`), field offsets (`zz_offsets_test.go`), op-cache models (`opc.py`, `opc2.py`, `opc3.py`), the phase test (`phase.py`), profiles by function and line (`p1.sh`, `p2.py`, `p3.py`), the aligned and PGO builds (`fa64.sh`, `pgo.sh`), `movefn.py`, the earlier placement-randomized method (`lrpad.py`, `lrab.sh`, `lrsum.py`, `lrq1.sh`, `chain.sh`), the importers (`imp6`, `imp8`), `abn.sh`, the hot-add ceiling (`nohotadd.py`, `ceil.sh`), the per-core cells design (`cells.py`, `zz_cells_test.go`), the fixed replay on the committed tree (`post.sh`), the stale-hot stress (`s6stress_test.go`), the shared-tags racy test (`sharedtags.py`), bump's counters (`bumpcount7.py`), the realign cost scripts (`unitext`) and the planted-bug runs (`mut1.sh`). The readings themselves are recorded in the claims they support.
