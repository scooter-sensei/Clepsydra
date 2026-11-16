# lab/sw-2026-11: the short-window weakness

From 8b3ba44. Host: the WSL2 guest of the Ryzen host (12 threads, Go 1.24.7).
Every rule below is committed before the reading it judges. Arms are named
W1, W2, ...; the earlier labs' arms (E, S, N, H, B) are not repeated.

## The weakness

Target 7 asks a velocity error of at most 0.05: tfbench's velocity MAE, the
mean over the 1, 5, 15, 30 and 60 s windows of sum|estimate - exact| / sum
exact on 2,000 keys of five traffic shapes. The filter reads 0.0671
(0.1671 / 0.0503 / 0.0506 / 0.0435 / 0.0239), a sum of 0.3354 against the
0.25 the target allows. With the other four windows as they are, the 1 s
window must read at most 0.082. The record says the gap is information, not
estimation: the register's own floor is ~0.058 (D18), a few more bits of
side information read 0.058-0.070 (D19), medians, fitted cuts and
piecewise-linear cuts do not move it (E34, E59, E12), finer buckets near now
paid by approximate old counters lose the long windows (E35), 14 freed bits
as a wider register read 0.0637 (E48), and a side table sized at 1.2 B per
key churns (E55).

The workload and the metric are not changed here, and no window is weighted
differently: those are the user's (paper §8.4).

## Constraints every mechanism is held to

- Memory: target 5 (<= 20 B per key at design load, 18.8 now) leaves
  ~1.2 B per live key; target 6 (<= 32 B per key under flood, 31.1 now)
  leaves ~0.9 B. State sized by Capacity is reported per live key and read
  with the velocity keys beside the churn stream (loads 1.00 and 1.15), not
  only in the velocity workload's 2,000-key filter of Capacity 200,000,
  where a table sized by Capacity covers nearly every key.
- Placement: word1 is the one word every event writes; word0 is skipped by
  hits within one tick. Per-event state in word0 costs that skip, or must
  not need it.
- `Upper` never below the truth; only `Count` may move.

## Phase 1: pricing the information (W1)

**W1, the information sweep** (`TestShortWindowInformation`, `TF_EXP=1`, a
model: no library change). It replays the velocity workload on seed (9, 9)
and on a held-out seed (3, 7), and scores models built from each key's exact
event times with one estimator that reads every model as a list of disjoint
intervals with counts (uniform inside an interval; a known event is an
interval of count 1). It must reproduce the register with its 4-bit onset
(the filter) to within rounding before any other row is read. Families:

- W1a recent event times: the key's last m events (m = 3..8) as stored
  quantized times at resolution r (40, 80, 160, 300 ms), kept only while
  younger than an age limit A (1.25, 2.5, 5 s), absolute (k bits each, A/r
  levels) or delta-coded (d bits a gap, a full gap ends the log);
- W1b sub-unit counts of the newest unit (2, 4 or 8 aligned parts), and of
  the two newest units;
- W1c exponential traces anchored at the last touch, s_tau = sum over events
  of exp(-(last - t)/tau), exact values, tau = 0.25, 0.5, 1, 2, 4 s, one or
  two of them, read jointly with the register by the linear minimum
  mean-square-error estimate of the cut bucket's split under the register's
  uniform model (the ideal linear reading of that information);
- each of the above with and without the onset.

Each row reports its bits (the exact values' rows report none) and its
error per window. W1 adopts nothing; its output is the frontier of bits
against the 1 s error that a mechanism must beat.

**Rule for a family to go on to an encoding (before the reading):** at its
exact (or finest) form, on both seeds, 1 s <= 0.082 and no window above the
filter's own reading on that seed by more than 1%.

### W1's reading (ea96ec7's code, run once; `lab/w1.txt`)

The interval model of the register with its 4-bit onset equals the filter
on both seeds, per window (the test's check). The filter reads 0.1671 /
0.0503 / 0.0506 / 0.0435 / 0.0239, mean 0.0671, on (9, 9) and 1 s 0.1663,
mean 0.0666 on the held-out (3, 7).

| Model (with the onset unless said) | bits | 1 s | 5 s | 15 s | 30 s | 60 s | mean | held-out 1 s, mean | rule |
|---|---|---|---|---|---|---|---|---|---|
| last 4 events exact (D19's row: 0.0582 without the onset) | exact | 0.0902 | 0.0475 | 0.0503 | 0.0432 | 0.0238 | 0.0510 | 0.0919, 0.0510 | fails (1 s) |
| last 5 events exact | exact | 0.0692 | 0.0467 | 0.0502 | 0.0432 | 0.0237 | 0.0466 | 0.0693, 0.0463 | passes |
| last 8 events exact | exact | 0.0275 | 0.0431 | 0.0496 | 0.0430 | 0.0237 | 0.0374 | 0.0271, 0.0371 | passes |
| log, delta-coded, 5 gaps of 3 bits at 160 ms | 15 | 0.0754 | 0.0477 | 0.0506 | 0.0435 | 0.0239 | 0.0482 | 0.0734, 0.0475 | passes |
| log, delta-coded, 7 gaps of 2 bits at 300 ms | 14 | 0.0785 | 0.0476 | 0.0505 | 0.0435 | 0.0239 | 0.0489 | (frontier: 0.0785, 0.0489 worse seed) | passes |
| log, delta-coded, 4 gaps of 3 bits at 160 ms | 12 | 0.0901 | | | | | 0.0513 | | fails (1 s) |
| sub-unit counts, 4 parts of the two newest units | 30 | 0.0813 | 0.0423 | 0.0502 | 0.0435 | 0.0239 | 0.0483 | 0.0826, 0.0482 | fails (held-out 1 s) |
| sub-unit counts, 8 parts of the two newest units | 70 | 0.0509 | 0.0398 | 0.0501 | 0.0435 | 0.0239 | 0.0416 | 0.0511, 0.0413 | passes |
| one exact trace, tau 1 s | exact | 0.0971 | 0.0473 | 0.0503 | 0.0435 | 0.0239 | 0.0524 | 0.0967, 0.0521 | fails |
| two exact traces, tau 0.25 s and 0.5 s | exact | 0.0726 | 0.0467 | 0.0503 | 0.0435 | 0.0239 | 0.0474 | 0.0727, 0.0471 | passes |

Frontier, priced rows, worse seed (bits: 1 s, mean): 4: 0.1322, 0.0598;
6: 0.1209, 0.0576; 8: 0.1094, 0.0552; 10: 0.0963, 0.0525; 12: 0.0901,
0.0513; 14: 0.0785, 0.0489; 15: 0.0754, 0.0482; 20: 0.0627, 0.0456;
28: 0.0416, 0.0412; 42: 0.0339, 0.0398 (every point from 4 to 28 bits is a
delta-coded log with the onset).

**Verdict.** Three families pass: logs of recent events, sub-unit counts
(only at 70 bits) and two traces. What D19 priced as "tens of bits" was
side information read *without* the onset: read with it, the exact times
of the key's last 5 events meet target 7 (0.0466), and a delta-coded log of
14-15 bits does (0.0482-0.0489) on both seeds. The onset and a log are
complements: the onset places a bucket's oldest events, the log its newest,
and the log's end bounds where the events it does not hold can lie. The
1 s window's 0.082 is met from 14 bits; nothing under 14 bits meets it.

## Phase 2: encodings (W2)

**W2, encodings at a fixed budget** (`TestShortWindowEncodings`, `TF_EXP=1`,
a model). Each candidate is maintained as an implementation would keep it:
its state after an event is a function of its state before, the last touch
before and the event's tick only, the shape a word0 CAS can carry beside the
last touch. Budgets 12, 14, 15 and 16 bits. Candidates:

- W2a fixed-width delta logs (W1's), maintained incrementally;
- W2b variable-length logs: a shift register of B bits into which each
  event pushes its quantized gap to the touch before as a Rice code
  (k = 0, 1, 2) or an Elias gamma code, the oldest bits falling off the far
  end; a code cut by the end bounds its gap from below;
- W2c two traces in floating point, b bits each (b = 6, 7, 8), rounded
  stochastically by a hash of the event's tick (deterministic);
- each read with the onset, on both seeds.

**Rule (before the reading).** A candidate goes on to the real filter if,
within B <= 15 bits, on both seeds: the mean is at most 0.050, no window
reads above the filter's own on that seed by more than 1%, and its state is
maintained as above. The candidate with the lowest worse-seed mean goes
first.

### W2's reading (a3f87ff's code, run once; `lab/w2.txt`)

40 candidates pass; the best per budget are all variable-length logs whose
codes are unary (Rice k = 0) at a quantum of unit/16 (156 ms at a 60 s
horizon), read with the onset:

| Candidate (with the onset) | bits | 1 s | 5 s | 15 s | 30 s | 60 s | mean | held-out 1 s, mean |
|---|---|---|---|---|---|---|---|---|
| the filter | 0 | 0.1671 | 0.0503 | 0.0506 | 0.0435 | 0.0239 | 0.0671 | 0.1663, 0.0666 |
| unary log, 12 bits, unit/16 | 12 | 0.0522 | 0.0474 | 0.0505 | 0.0435 | 0.0239 | 0.0435 | 0.0510, 0.0429 |
| unary log, 12 bits, unit/8 | 12 | 0.0637 | 0.0472 | 0.0505 | 0.0435 | 0.0239 | 0.0458 | 0.0648, 0.0456 |
| unary log, 12 bits, unit/32 | 12 | 0.0877 | 0.0481 | 0.0505 | 0.0435 | 0.0239 | 0.0508 | 0.0873, 0.0503 |
| fixed log, 4 gaps of 3 bits, unit/16 | 12 | 0.0895 | 0.0482 | 0.0506 | 0.0435 | 0.0239 | 0.0512 | 0.0888, 0.0507 |
| unary log, 14 bits, unit/16 | 14 | 0.0415 | 0.0470 | 0.0505 | 0.0435 | 0.0239 | 0.0413 | 0.0412, 0.0409 |
| unary log, 15 bits, unit/16 | 15 | 0.0379 | 0.0469 | 0.0505 | 0.0435 | 0.0239 | 0.0405 | 0.0375, 0.0401 |
| fixed log, 5 gaps of 3 bits, unit/16 | 15 | 0.0739 | 0.0477 | 0.0506 | 0.0435 | 0.0239 | 0.0479 | 0.0728, 0.0474 |
| two traces (0.25 s, 0.5 s), 7 bits each | 14 | 0.0812 | 0.0474 | 0.0504 | 0.0435 | 0.0239 | 0.0493 | 0.0819, 0.0491 |
| two traces (0.25 s, 0.5 s), 8 bits each | 16 | 0.0755 | 0.0472 | 0.0504 | 0.0435 | 0.0239 | 0.0481 | 0.0759, 0.0478 |

Gamma and Rice k = 1, 2 codes come next (0.043-0.047 at 12-15 bits).

**Verdict by the rule:** the unary log of 15 bits at unit/16 goes first
(worse-seed mean 0.0405); every budget from 12 bits passes. Why unary wins:
a busy key's gaps are a quantum or less, so most events cost one or two
bits and a 12-bit field holds the 6-10 events of the last second; a quiet
key's one event costs more bits but its short windows hold little; a code
the field cuts still bounds the next gap, so the unlogged events are placed
below it. Unit/16 balances how many events fit against how exactly each is
placed for the 1 s window; at unit/32 a busy key's log covers half a second,
at unit/8 each event is uncertain by 312 ms. It is also the onset's grid,
and quanta never straddle a unit, so a logged event's bucket is exact.

That optimum is a property of this workload's rates (0.1-10 events/s): W3
reads it on others before anything is built.

## Phase 3: robustness (W3)

**W3** (`TestShortWindowRobust`, `TF_EXP=1`, a model). The unary log at
unit/16 (and unit/8, unit/32 beside it) at budgets 6, 8, 10, 12 and 15 bits,
with and without the onset, on seed (9, 9) of four workloads: tfbench's
own; every rate halved (bursts every ~20 s); every rate doubled (bursts
every ~5 s, the ramp to 20/s); and 2,000 Poisson keys at rates log-uniform
in 0.05-12 events/s. Each against the filter on the same workload.

**Rule (before the reading).** The candidate holds if the unary log at
unit/16 with the onset, at 12 and at 15 bits, on every workload reads a
mean at most 0.8 of the filter's on that workload, and no window above the filter's by more than 1%. Target 7 is judged on
tfbench's workload alone (W2); W3 asks only that the gain is not an
artifact of its rates.

### W3's reading (9ac9076's code, run once; `lab/w3.txt`)

Mean as a share of the filter's on the same workload (filter's mean in
brackets), unary log at unit/16 with the onset:

| Workload | filter | 8 bits | 10 bits | 12 bits | 15 bits |
|---|---|---|---|---|---|
| tfbench's own | (0.0671) | 0.767 | 0.697 | 0.649 | 0.604 |
| every rate halved | (0.0748) | 0.666 | 0.616 | 0.596 | 0.588 |
| every rate doubled | (0.0564) | 0.886 | 0.835 | 0.789 | 0.725 |
| log-uniform 0.05-12/s | (0.0578) | 0.749 | 0.684 | 0.628 | 0.571 |

No window reads above the filter's on any workload at 12 or 15 bits. **The
rule holds.** The doubled workload gains least: its busy keys have more
events in a second than 12 bits hold (they are the hot entries' keys, whose
own fovea B3 modelled). Without the onset the log loses 0.002-0.006 of
mean; on log-uniform Poisson keys the onset adds nothing.

## Phase 4: the drip log on the filter (W4)

**W4, the drip log** (build tag `tfdrip`: `drip.go`, `drip_on.go`,
`drip_off.go`; the default build has no log). word0 = fp(16) | displaced(1)
| saturated(1) | drip log(12) | last touch(34): `MaxTick` falls to
2^34-1 (4.8 hours at 1 µs; 199 days at 1 ms) in this arm; the bits' lasting
source, a last touch that wraps under a reclaimer with a deadline, is W5's.
Every event pushes its gap to the touch before as a unary code in quanta of
unit/16 (`next0`, in the CAS that moves the last touch; a hit that leaves
word0 as it was still skips it); a new key's field is all ones; 0 says
nothing (a merge, or 13 events in one quantum) until a gap of 12 quanta;
Count reads the log with the register and onset (`dripCount`, W1's interval
model); `Upper`, `Seen`, saturation, eviction and every other answer read
what they read today.

**Rule (before the reading).** Worth building (a candidate for main) if:

1. the default build is unchanged: `tfbench -exact` equals
   `verify/expected/exact.txt`, and the answer digest equals
   `verify/expected/answer-digest.txt`;
2. with `tfdrip`, the filter's velocity error on tfbench's workload is at
   most 0.050 on seed (9, 9) and on the held-out (3, 7), no window above the
   default build's by more than 1%, and within 0.002 of W2's model (the
   12-bit unary log at unit/16 with the onset) at every window;
3. only Count moves: with Count left out (and the batch's counts), the
   answer digest is identical between the builds in all 30 cells;
4. `Upper` never below the truth: `TestAttackHeavyHitter` and
   `TestUpperBoundRandom` pass with `tfdrip`;
5. with `tfdrip`, the unit suite (`-short`, plain, ConstantTime, Background,
   `TF_NOCAS2=1`), `TestSlotUnitRaces`, and 5 minutes each of the soak and
   the concurrent fuzzer under `-race` pass, but for tests that fail for a
   reason named before the run: ticks past the arm's `MaxTick` (2^34-1), the
   conformance vectors' pinned Counts (a format change, as D29's), and
   snapshot tests that write 46-bit ticks or compare Counts with a build
   without the log;
6. speed is reported, not judged (hits, counts, misses; 3 interleaved
   rounds in one binary on the WSL2 guest: indicative only).

### W4's reading (b8739a9's tree; `lab/w4.sh`, outputs in `lab/w4/`)

1. **Holds.** The default build's `tfbench -exact` is byte-identical to
   `verify/expected/exact.txt`, and its answer digest to
   `verify/expected/answer-digest.txt`.
2. **Holds.** With `tfdrip`, `tfbench -exact` reads velocity 0.0535 /
   0.0477 / 0.0505 / 0.0435 / 0.0239, **mean 0.0438** (default 0.1671 /
   0.0503 / 0.0506 / 0.0435 / 0.0239, 0.0671); on the held-out seed 1 s
   0.0525, mean 0.0433 (default 0.1663, 0.0666). The model reads 0.0522 /
   0.0474 / 0.0505 / 0.0435 / 0.0239 (0.0435): the filter is within 0.0013
   at 1 s, 0.0003 at 5 s, equal beyond (a field of 13 events in one
   quantum says nothing on the filter, and not in the model). On W3's other
   workloads: half 0.0448 (model 0.0446), double 0.0449 (0.0445), log-uniform
   0.0363 (0.0363). No window above the default build's.
3. **Holds.** Without Count, the 30 cells' digests are identical.
4. **Holds.** `Upper` below the truth: 0 in both tests, both builds; Count
   more than 10% low (heavy hitter) 299 → 297; Upper's excess unchanged
   (17.3% of events).
5. **Fails.** With `tfdrip`, the plain and `TF_NOCAS2=1` suites fail 6
   tests, ConstantTime and Background 4 (Merge and Resize skip there):
   `TestConformance` and `TestUnitOfMatchesDivision` (a Horizon of `MaxTick`
   ticks) for the reasons named; and four that were not named:
   - `TestSlotUnitRaces` (stamp and unpoint, both modes): Count at 0.5 and
     1 s reads 1 where the sequential order reads 2. A touch D pushes its
     event into the log at its own tick (its word0 CAS), and counts it in
     the register afterwards; when another touch A moves word0 into a later
     unit in between, the register counts D at A's unit ("behind the last
     touch, counted at it"), while the log keeps D at its own tick, which
     A's push of a 13-quantum gap then rolls off. The log is right in time
     and wrong against the register, and the estimator trusts both. The
     sequential order, D's touch after A's, pushes D as a gap of 0.
   - `TestCountsLastTouchWhole`: eleven touches 200 ms apart, read at the
     last one with a 200 ms window: 1, not 2. The event exactly on the
     window's edge is placed to its 156 ms quantum and counted 0.48 of an
     event; the register's uniform spread put it exactly there. The same
     cause moves `tfbench -exact`'s steady 1 s lines (periodic touches read
     at a touch's instant, the window a multiple of the period) from 1.000
     to min 0.500-1.000, max up to 1.143; their 5-60 s lines and every burst
     line are unchanged.
   - `TestResize` (1 s count error 2.2x the reference's): a resize's move
     installs the key's word0 without its log, so moved keys read as today.
   - `TestMerge` (1 s count error 3.3x the single filter's, bar 1.5x): a
     summed history's log says nothing, so the merged filter reads as
     today while the single filter it is held to reads with its log.

   The soak and the concurrent fuzzer under `-race`: below.

**Verdict: W4 fails its rule** on clause 5. The velocity result stands as a
reading of the filter (clause 2), but the arm as built answers a
deterministic race inconsistently and loses its log in resizes.

## W4b: the drip log, mended

**Mends** (written after W4's reading, as is this rule):

- an event the register counts at a later unit than its own (a caller
  whose clock lags across a unit boundary) makes the log say nothing:
  `next0` for a touch behind the last touch in an earlier unit, and the
  touch itself after its count when the count landed at a later unit than
  its tick's (the race above), by a CAS of word0's log to 0;
- a resize's move carries the log with the last touch.

**Rule (written after W4's reading, before W4b's).** W4's clauses 1-4 and
6, read again on W4b's tree; clause 5 with `TestSlotUnitRaces` and
`TestResize` required to pass, and these failures allowed, named now:
`TestConformance` (pinned Counts), `TestUnitOfMatchesDivision` (`MaxTick`),
`TestCountsLastTouchWhole` (an event exactly on the window's edge, read at
a touch's instant), and `TestMerge`'s 1 s count bar alone (summed histories'
logs say nothing; every other assertion of it must hold); the soak and the
concurrent fuzzer under `-race`, 5 minutes each, clean. And a new clause:

7. periodic traffic at random instants: a fifth W3 workload, 2,000 keys
   each touched with a fixed period log-uniform in 0.1-20 s from a random
   phase, queried as the velocity workload is (a random key and window
   every tick). The `tfdrip` filter's mean at most the default build's on
   it, and no window above 1.01x the default build's.

Readings added to W4b before its run, reported and not judged:

8. sustained traffic with bursts inside it (5/s Poisson per key, and 15
   events in 200 ms every ~10 s): the only workload read so far whose
   bursts are not followed by silence, so the only one where a field that
   says nothing (13 events in a quantum) stays so for long; both builds and
   the model;
9. target 8: `TestRow8` (batched throughput at 90% load) on both builds.

(W4, clause 5, continued: the soak under `-race`, 24 workers for 5 minutes:
50,284,465 operations, 15,087,970 counts checked, 0 low; the concurrent
fuzzer under `-race`, 102 cases in 5 minutes: 71,842,707 operations,
18,644,436 answers checked, 0 off for a key whose identity another shares.
Both pass; W4's verdict stands on the four unnamed failures above.)

(W4b, reading 9: `TestRow8` is not in the tree, only in
`go/verify/experiments/row8-batch-f298a7a.patch`; target 8 is read instead
with `BenchmarkCheckAndTouchBatch` and `BenchmarkTouchAndCount` on both
builds, 3 interleaved rounds. Noted before the run.)

## Phase 5: a lasting source for the bits (W5)

W4's arm takes its 12 bits by narrowing `MaxTick` to 2^34-1: 4.8 hours at
the default 1 µs tick, after which a call panics. W5 keeps `MaxTick` at
2^46-1 for callers' clocks and stores only 34 bits of a slot's last touch,
read against the reading call's tick (paper §8.3's direction, lab/bio3's
B10 postulate). It is split in two arms.

**W5a, one decode and one store for every stored tick** (branch
`lab/sw-w5`). Every read of a stored tick (`w0&tsMask`, the stash's, the
wait room's, buzz entries' and `Merge`'s copies) goes through `lastOf(w0,
ref)`, ref the reading call's tick (or, where a function has none, a tick
within a unit of the stored one: the unit it compares against); every write
masks the tick it stores; the sites that order raw stored ticks (the stash's
put, the eviction score, `Merge`'s clock, `estimateAt`'s re-read) order
decoded ones. `lastOf` is `w0 & tsMask` in both builds here: no behaviour
changes.

**Rule (before the reading).** W5a holds if the default build's regression
tier (`./test.sh regression`) passes and its `tfbench -exact` and answer
digest equal `verify/expected/`'s, the `tfdrip` build's `tfbench -exact`
and answer digest (Count included) equal W4b's tree's byte for byte, and
the unit suites in both builds fail exactly the tests they failed before
W5a.

**W5b, the wrap** (branch `lab/sw-w5b`, from W5a's 73bc1d1; `wrap.go`). With
`tfdrip`, `lastOf` decodes 34 stored bits against the reading call's tick
(signed: right while the two are less than 2^33 apart), `MaxTick` is 2^46-1
again for callers' clocks, and Horizon must be at most 2^31 ticks. A dead
tick must be cleared before it is 2^33 old, so: the decay cursor never skips
ahead (each step sweeps its lines and the stash's and wait room's sets that
map to them, so every line of the cursor's last lap was visited at or after
`doneAt` = (c - lines) H / lines); a step that finds `doneAt` 2^30 behind
sweeps 64 lines; and a call that finds it 2^31 behind (an idle filter's
first call) sweeps everything first, reading stored ticks against the middle
of the span they can hold (`catchUp`). A new filter's cursor and `doneAt`
start at its clock. Buzz entries are not swept: one answers only while its
slot's words match its copy, so a stale one can hold a way, never decide.
Callers' clocks must stay within 2^31 ticks of each other.

Before this rule: `TestWrapAlias` (a key touched once reads unseen 2^34 +
500 ticks later, idle, with other keys' calls, and from the stash) passes
on d59776a, and fails all three with the catch-up disabled (Count 3 a wrap
later): it detects aliasing.

**Rule (before the reading).** W5b holds if:

1. the default build's `tfbench -exact` and answer digest equal
   `verify/expected/`'s;
2. with `tfdrip`, `tfbench -exact` and the answer digest (Count included)
   equal W4b's tree's byte for byte (ticks that never wrap);
3. translation: with `tfdrip`, the 30-cell digest with every tick offset by
   K = 566,935,680,000, 1,013,612,280,000 and 1,460,288,880,000 (multiples
   of two horizons whose runs cross a multiple of 2^34 after 3,072, 1,856
   and 640 ticks) equals K = 0's byte for byte;
4. `TestWrapAlias` passes;
5. the unit suites with `tfdrip` fail no test that W4b's tree passes, but
   for `TestUnitOfMatchesDivision` (a Horizon of `MaxTick` ticks, refused
   either way) and tests that need a Horizon over 2^31 ticks, named before
   the run;
6. the soak and the concurrent fuzzer under `-race`, 5 minutes each, with
   `tfdrip`, pass.

Reported, not judged: speed of hits and counts with the per-call check
against W4b's `tfdrip` build and the default build (3 interleaved rounds);
whether the soak's and fuzzer's clocks keep within 2^31 of each other.

### W4b's reading (06940e8's tree; `lab/w4.sh` with `O=…/w4b`, outputs in `lab/w4b/`)

1. **Holds.** Default build: `tfbench -exact` and the answer digest equal
   `verify/expected/`'s.
2. **Holds.** `tfdrip`: velocity 0.0535 / 0.0477 / 0.0505 / 0.0435 / 0.0239,
   mean 0.0438, as W4.
3. **Holds.** Digest without Count identical in 30 cells.
4. **Holds.** `Upper` below the truth 0; Count more than 10% low 297.
5. **Fails.** `TestSlotUnitRaces` and `TestResize` pass now, in every mode;
   the soak (50.3 M operations) and the concurrent fuzzer under `-race` pass;
   `TestMerge` fails its 1 s bar alone (its other assertions hold); the named
   failures as named. But `TestBackgroundConcurrentCounts` failed once in the
   plain suite, and read alone it fails **36 of 60 runs with `tfdrip`, 0 of 60
   without** (round 137 key 8: Count 21 for 20 touches). Cause: a touch
   pushes its event in word0's CAS, then counts it in word1; when the count
   fails (the slot changed hands meanwhile) the touch starts over and pushes
   the same event again. The log then holds an event the register does not,
   and the estimator, taking logged events out of their buckets, counts it.
   The default build's word0 move is idempotent on a retry; the push is not.
   W4 had the same defect; its run did not happen to hit it.
6. Speed (indicative; ns/op, 3 rounds, default → `tfdrip`): CheckAndTouchHit
   194.8 → 201.0, CheckAndTouchHitHot 76.9 → 72.3, CheckAndTouchMissInsert
   1826 → 1926, TouchAndCount 208.7 → 238.9 (1.14x), VelocityCount 193.3 →
   195.5, VelocityCountHot 94.5 → 156.0 (1.65x: the decoder walks every
   bucket for every logged event).
7. **Holds.** Periodic traffic at random instants: default 0.0786 / 0.0154 /
   0.0067 / 0.0035 / 0.0017 (mean 0.0212), `tfdrip` 0.0451 / 0.0153 /
   0.0067 / 0.0035 / 0.0017 (0.0144). The event-on-the-edge loss is a
   property of reads at a touch's instant, not of periodic traffic.
8. Sustained traffic with bursts inside it: default 0.2598 / 0.0729 /
   0.0500 / 0.0361 / 0.0181 (mean 0.0874); the 12-bit model 0.0915 / … /
   0.0537; **the `tfdrip` filter 0.2536 / … / 0.0861**: the filter keeps
   almost none of the model's gain. Each burst puts 13 events in one
   quantum, the field fills with zero codes, which W4 made "says nothing",
   and at 5 events/s a silence of 12 quanta (1.9 s) that would revive it
   almost never comes.
9. Target 8, `BenchmarkCheckAndTouchBatch`: 96.0 → 97.3 ns/key (10.4 → 10.3
   M keys/s on this guest); `TouchAndCount` 205.1 → 232.2 ns.

**Verdict: W4b fails its rule** (clause 5: `TestBackgroundConcurrentCounts`),
and reading 8 shows a second defect of the design: "says nothing" is
reached by ordinary bursts and is sticky under sustained traffic.

## W4c: the drip log, mended again

**Mends** (written after W4b's reading, as is this rule):

- **A validity bit.** word0 = fp(16) | displaced | saturated | valid(1) |
  drip log(11) | last touch(34). A field of zero codes (a burst) is a
  legitimate state; "says nothing" is the valid bit clear, set by what W4b
  set it for (a count at a later unit than its event; a summed history in a
  merge; a word written without the log), and cleared by a gap of 11 quanta
  or more, which sets the field to all ones. The log is 11 bits.
- **One push per touch.** A touch that has pushed its event and starts over
  (its count failed) does not push again while the slot's last touch is at
  or after its tick; a slot whose last touch is behind its tick holds a
  history from before its push (a relocated or stashed copy carries the
  push), and gets it.
- **A shortcut.** Count reads the log only when the window cuts a bucket.

**Rule (written after W4b's reading, before W4c's).** W4b's clauses 1-4, 6, 7
and 9 as written, read on W4c's tree; clause 5 as written for W4b, and with
`TestBackgroundConcurrentCounts` failing 0 of 200 runs with `tfdrip` (and 0
of 20 under `-race`); and:

8. sustained traffic with bursts inside it, now judged: the `tfdrip`
   filter within 0.003 of the 11-bit model (unary log at unit/16 with the
   onset) at every window, and its mean at most 0.8 of the default build's.
   The tfbench clause (2) with the model now the 11-bit one: within 0.002
   at every window, mean at most 0.050 on both seeds.

(Before any W5 reading: W5a and W5b were rebased onto W4c, 0837f9f, as
`lab/sw-w5` (e7c1328, c98f6da) and `lab/sw-w5b` (c0cea9e); where their rules
name W4b's tree, they read W4c's. W5a's second commit gives `lookupStable`
the caller's clock, so `slotWords` reads both loads of word0 against it.)

### W4c's reading (0837f9f's tree; `lab/w4c.sh`, outputs in `lab/w4c/`)

1. **Holds.** Default build: `tfbench -exact` and the answer digest equal
   `verify/expected/`'s.
2. **Holds.** `tfdrip`: velocity 0.0592 / 0.0476 / 0.0505 / 0.0435 /
   0.0239, **mean 0.0450**; held-out seed 1 s 0.0580, mean 0.0444. The
   11-bit model reads the same to the fourth decimal at every window on both
   seeds: the filter is the model.
3. **Holds.** Digest without Count identical in 30 cells.
4. **Holds.** `Upper` below the truth 0 in both tests.
5. **Holds.** `TestBackgroundConcurrentCounts` 0 of 200 runs fail with
   `tfdrip`, 0 of 20 under `-race`. Suites (plain, `TF_NOCAS2=1`): only
   `TestConformance`, `TestMerge` (its 1 s bar alone: merged 0.2245 against
   single 0.0740; every other window and check as the single filter),
   `TestUnitOfMatchesDivision` and `TestCountsLastTouchWhole`; ConstantTime and
   Background the same but `TestMerge` (skipped there). `TestSlotUnitRaces`
   passes, also under `-race`; the soak and the concurrent fuzzer under
   `-race` pass.
6. Speed (ns/op, medians of 3, default → `tfdrip`): CheckAndTouchHit 189.8 →
   203.9 (1.07x), CheckAndTouchHitHot 77.4 → 71.1, CheckAndTouchMissInsert
   1743 → 1757, TouchAndCount 204.6 → 220.1 (1.08x), VelocityCount 187.1 →
   186.9, VelocityCountHot 92.4 → 95.9 (W4b's 1.65x was the decoder on
   windows that cut no bucket).
7. **Holds.** Periodic traffic: 0.0212 → 0.0149 (the model's 0.0149), no
   window above.
8. **Holds.** Sustained traffic with bursts inside it: default 0.2598 /
   0.0729 / 0.0500 / 0.0361 / 0.0181 (0.0874), `tfdrip` 0.1047 / 0.0729 /
   0.0500 / 0.0361 / 0.0181 (0.0564, 0.645 of the default's), the 11-bit
   model's exactly.
9. Target 8: `BenchmarkCheckAndTouchBatch` 88.6 → 98.8 ns/key (11.3 → 10.1 M
   keys/s on this guest, 1.11x).

`tfbench -exact`'s other differences: the steady 1 s lines at every rate
(1-100/s: min 0.500-0.990, max 1.000-1.167; exact before), the
event-on-the-edge cost W4 found, now at 100/s too (a full field of zero
codes is a valid burst, not "says nothing").

**Verdict: W4c passes its rule.** The drip log is a candidate, at 12 bits of
word0 that the default build's last touch holds: W5 is where they come from.

### W5a's reading (c98f6da's tree; `lab/w5.sh a`, outputs in `lab/w5a/`)

Default build: `tfbench -exact` and the answer digest equal
`verify/expected/`'s, and `./test.sh regression` passes. `tfdrip`:
`tfbench -exact` and the answer digest with Count equal W4c's byte for byte.
The suites: default, no failure; `tfdrip`, in each of the four modes the same
failing tests as W4c's tree (plain and `TF_NOCAS2=1`: `TestConformance`,
`TestCountsLastTouchWhole`, `TestMerge`, `TestUnitOfMatchesDivision`;
ConstantTime and Background: the same but `TestMerge`). **W5a holds.**

### W5b's reading (c0cea9e's tree; `lab/w5.sh b`, outputs in `lab/w5b/`)

1. **Holds.** Default build: `tfbench -exact` and the answer digest equal
   `verify/expected/`'s.
2. **Fails.** `tfdrip`: the answer digest with Count equals W4c's; `tfbench
   -exact` differs in three lines, the churn rows' relocation and eviction
   counts (load 1.00: relocations 148,716 → 148,685; 1.10: 379,688 →
   379,659; 1.15: evictions 4,577 → 4,600, relocations 932,499 → 931,823);
   every answer field of those rows (positives, missed, negatives, false
   seen, live at end), the velocity rows and every other line are equal.
   Cause: the deadline itself. W4c's cursor skips ahead when it falls a lap
   behind (tfbench's inline steps cannot keep pace on a 200k-key table), and
   W5b's sweeps every line it passes and the stash's sets with them, so lines
   are freed at other times and placements differ.
3. **Fails.** The 30-cell digest at K = 566,935,680,000, 1,013,612,280,000
   and 1,460,288,880,000: the 9 cells of 64 keys equal K = 0's, the 21 cells
   of 6,000 keys differ. A lockstep diagnostic (`TestWrapTranslateDiverge`,
   `TF_EXP=1`: one cell at two offsets, every slot and answer compared after
   each operation) finds the first difference at operation 52,461, a batch,
   when the two filters evict different keys: `mass` (eviction's score) reads
   the buckets newer than `now - Horizon`, clamped to tick 0 in a filter's
   first horizon (`now-min(now, f.horizon)`, `mass` and `viewMass`), and a
   register whose newest unit filled holds spilled events in its next
   bucket, which at K = 0 lies before tick 0 and is left out (mass 31), at K
   not (33). The clamp is the default build's, not the wrap's: run from a
   base of 1,200,000 ticks (past the first horizon, never wrapping) against
   the same three wrap-crossing runs, the lockstep finds no difference in any
   slot or answer in 400,000 operations each.
4. **Holds.** `TestWrapAlias` passes.
5. **Holds.** The suites with `tfdrip` fail exactly W4c's tests in every
   mode (`TestUnitOfMatchesDivision` still fails: its Horizon of `MaxTick`
   ticks is refused by the wrap's 2^31 bound).
6. **Holds.** The soak under `-race` (42,433,128 operations, 12,731,363
   counts checked, 0 low) and the concurrent fuzzer under `-race` pass.

Reported: speed, ns/op medians of 3 (default / W4c `tfdrip` / W5b
`tfdrip`): CheckAndTouchHit 189.5 / 200.5 / 199.0; CheckAndTouchHitHot 66.7 /
72.8 / 73.7; CheckAndTouchMissInsert 1744 / 1796 / 1930; TouchAndCount 204.1
/ 222.9 / 229.4; VelocityCount 186.9 / 185.2 / 199.8; VelocityCountHot 92.0 /
96.2 / 107.0; ContainsAbsent 171.6 / 165.8 / 173.9; CheckAndTouchBatch 91.0 /
97.9 / 106.6. The concurrent fuzzer's callers read one shared clock with a
lag of at most two units (a straggler up to a horizon), well within 2^31.

**Verdict: W5b fails its rule** (clauses 2 and 3 as written), with neither
failure in the wrap's decoding: clause 2's difference is the deadline's own
placement effect (no answer moves), and clause 3's reference was ill chosen
(a filter's first horizon is not translation-invariant). Side finding: in a
filter's first horizon a key whose newest unit spilled scores lighter for
eviction than it would later (`mass`'s clamp), in the default build too.

## W5c: W5b re-read

The code is W5b's (c0cea9e, with `TestWrapTranslateDiverge` added). **Rule
(written after W5b's reading, before W5c's):**

2'. `tfdrip`'s `tfbench -exact` equals W4c's in every line but the churn
   rows' relocation and eviction counts, and those within 1%;
3'. the 30-cell digest at the three K equals the digest at K = 1,200,000
   (past the first horizon, never wrapping) byte for byte;

with W5b's clauses 1 and 4-6 as read.

### W5c's reading (d4d0187's tree; outputs in `lab/w5c/`)

2'. **Holds.** W5b's `tfbench -exact` against W4c's: only the churn rows'
   relocation and eviction counts differ, by 0.02%, 0.008% and 0.07%
   (relocations) and 0.50% (evictions at load 1.15).
3'. **Holds.** The 30-cell digest at K = 566,935,680,000, 1,013,612,280,000
   and 1,460,288,880,000, whose runs cross a multiple of 2^34 after 3,072,
   1,856 and 640 ticks, equals the digest at K = 1,200,000 byte for byte, in
   every mode (plain, Background, ConstantTime, SpillMin 1, the count-min
   overflow). (K = 1,200,000 against K = 0 differs, by the first horizon's
   clamp.)

**Verdict: W5c holds.** The wrapped last touch answers as the full one: a
history crossing a 2^34 wrap mid-run answers byte for byte as the same
history far from one; a key aliased by a whole wrap reads unseen, idle or
not, from the table or the stash; the concurrent checks pass. The 12 bits
the drip log needs come out of word0 without lowering callers' `MaxTick`
(2^46-1), at Horizon <= 2^31 ticks and callers' clocks within 2^31 of each
other.

## Where the lab leaves the weakness

| | 1 s | 5 s | 15 s | 30 s | 60 s | mean (target 7: <= 0.05) |
|---|---|---|---|---|---|---|
| the filter (8b3ba44) | 0.1671 | 0.0503 | 0.0506 | 0.0435 | 0.0239 | 0.0671 |
| the drip log, W4c / W5b (`tfdrip`) | 0.0592 | 0.0476 | 0.0505 | 0.0435 | 0.0239 | **0.0450** |
| held-out seed (3, 7), the filter → `tfdrip` | 0.1663 → 0.0580 | | | | | 0.0666 → 0.0444 |

At the same 16 bytes a slot (18.8 B per key), with 12 bits of word0 that
the 46-bit last touch held: an 11-bit unary-coded log of the key's recent
gaps in quanta of unit/16 and its valid bit, read by Count with the register
and the onset; the stored last touch keeps 34 bits and wraps under a
deadline sweep. What it costs, as read here: hits 1.05x, counts 1.07x,
TouchAndCount 1.12x, batched touches 1.17x of the default build on this
guest (indicative); `tfbench -exact`'s steady 1 s lines (periodic touches
read at a touch's instant) are no longer exact; a merged history's log says
nothing until a gap; Horizon at most 2^31 ticks. Open: MAWI (the trace is
not on this host), speed on bare metal, a TLA+ model of the log beside the
slot protocol, the ports.

## W4c's verdict revised, and W4d

A review of the record found W4c's clause 5 blind: `TestBackgroundConcurrentCounts`
asks a 60 s window, which cuts no bucket, and W4c's shortcut (the log read
only when a window cuts a bucket) came in the same commit as one push per
touch, so the test no longer reads the log. Checked: with one push per touch
undone and the shortcut kept, it passes 60 of 60 runs. A probe that reads
the log itself (`TestDripPushOnce`, `drip_probe_test.go`, `tfdrip`: the same
scenario, every key's log audited against its register after each round; a
bucket may not hold more logged events than the register counts there)
finds, in 5,000 rounds (160,000 keys): 13 with one push per touch undone,
**3 on W4c as committed**. **W4c's verdict is revised: it fails clause 5.**
The path left: a touch that pushed its event and lost its count, then
finds its key back in the stash (or the wait room) and installs it by `miss`,
pushed again: `miss` had no one-push rule.

**W4d** (mend, written after that reading): `miss` takes the touch's flag;
a recalled history whose last touch is at or after the touch's tick is
installed with its log as it is.

**Rule (written after W4c's revised reading, before W4d's; a preliminary
run of the probe on W4d's tree, 0 of 320,000 keys in two runs, came before
it and is not the reading).** W4c's clauses 1-9 read again on W4d's tree,
and: `TestDripPushOnce` 0 of 160,000 keys in 5,000 rounds, and 0 in 500
rounds under `-race`.
