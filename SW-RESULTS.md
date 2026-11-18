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

## W5d: the wrap's other entry points, and its untested corners

The same review found W5c's evidence thinner than its verdict: the digest
runs span ~4,400 ticks, under one horizon, so nothing expired across a wrap;
no concurrent run met a wrap (the fuzzer's clock starts below 2^30); and
`ContinuousDecay`, `ReadFrom` and `Merge` never called the deadline check.
`ContinuousDecay` can lap the table after a suspend longer than a wrap and
advance `doneAt` past entries it decoded as young; `ReadFrom` left `doneAt`
at the new filter's own tick, so a snapshot restored a wrap later read its
keys against the wrong span. **W5c's verdict ("answers as the full one") is
narrowed to what it read: a run of under one horizon crossing a wrap.**

**W5d** (branch `lab/sw-w5b` at 9db48f9, rebased onto W4d): `ContinuousDecay`
calls the check first; `ReadFrom` sets `doneAt` and `lastNow` from the
restored cursor (the writer's own ticks); `Merge` still reads both filters
against the later of their recent ticks, and is documented as needing
filters within 2^31 ticks of each other. Tests: `TestWrapDeadlineEntries`
(a ticker that laps an 8-key table a wrap after a touch; a snapshot
restored a moment and a wrap later), `TestWrapCatchUpStall` (reported), a
fast clock for `TestWrapTranslateDiverge` (`TF_TICK_SPEED`), clock bases for
the soak and the fuzzer and the fuzzer's idle jumps past 2^31
(`TF_STRESS_CLOCK_BASE`, `TF_CFUZZ_CLOCK_BASE`, `TF_CFUZZ_JUMP=1`). Before
this rule: each subtest of `TestWrapDeadlineEntries` fails with its mend
undone (the ticker's key reads Count 3 a wrap later; the snapshot's reads
Seen) and passes with it.

**Rule (written after the review, before W5d's reading).** W5d holds if:

1. the default build's `tfbench -exact` and answer digest equal
   `verify/expected/`'s;
2. with `tfdrip`, the answer digest with Count equals W4d's, and `tfbench
   -exact` equals W4d's but for the churn rows' relocation and eviction
   counts, within 1%;
3. the 30-cell digest at the three W5b offsets equals the digest at
   1,200,000 byte for byte;
4. the lockstep with a fast clock (`TF_TICK_SPEED=30`: ~5.8 M ticks, ~100
   horizons, keys expiring and swept throughout) from a base of 1,200,000
   against offsets 17,175,600,000, 34,355,520,000, 51,535,320,000,
   85,895,040,000 and 120,254,880,000 (each crossing a multiple of 2^34
   ~4.2 M ticks in) finds no difference in any slot or answer in 400,000
   operations;
5. `TestWrapAlias` and `TestWrapDeadlineEntries` pass;
6. the suites with `tfdrip`, in four modes, fail exactly W4d's tests;
7. under `-race`, 5 minutes each: the soak with its clock from 2^34 -
   10,000,000 (it crosses 2^34 after ~3.3 minutes) and the concurrent fuzzer
   from 2^34 - 2^29 with idle jumps past 2^31 pass.

Reported, not judged: the largest call (a catch-up of a 1M-key table
after idle gaps of 2^31+1 and 3x2^31 ticks), and speed against W4d's
`tfdrip` build and the default.

### W4d's reading (b7a33a7's tree; `lab/w4d.sh`, outputs in `lab/w4d/`)

1. **Holds.** Default build: `tfbench -exact` and the answer digest equal
   `verify/expected/`'s.
2. **Holds.** `tfdrip`: 0.0592 / 0.0476 / 0.0505 / 0.0435 / 0.0239, mean
   0.0450; held-out 1 s 0.0580, mean 0.0444; the 11-bit model's to the
   fourth decimal.
3. **Holds.** Digest without Count identical in 30 cells.
4. **Holds.** `Upper` below the truth 0.
5. **Holds.** `TestDripPushOnce` 0 of 160,000 keys in 5,000 rounds, 0 of
   16,000 in 500 rounds under `-race`; `TestBackgroundConcurrentCounts` 0 of
   200 runs, 0 of 20 under `-race`; the suites as W4c's (only the named
   tests; `TestMerge` its 1 s bar alone); `TestSlotUnitRaces`, the soak and
   the concurrent fuzzer under `-race` pass.
6. Speed (ns/op medians of 3, default → `tfdrip`): CheckAndTouchHit 193.1 →
   201.8, CheckAndTouchHitHot 75.4 → 67.9, CheckAndTouchMissInsert 1760 →
   1774, TouchAndCount ~205 → ~224, VelocityCount 188.4 → 191.0,
   VelocityCountHot 92.2 → 94.0.
7. **Holds.** Periodic 0.0212 → 0.0149.
8. **Holds.** Sustained 0.0874 → 0.0564 (the model's).
9. `BenchmarkCheckAndTouchBatch` 91.4 → 98.3 ns/key (1.08x).

**Verdict: W4d passes its rule.**

### W5d's reading (9db48f9's tree; `lab/w5d.sh`, outputs in `lab/w5d/`)

1. **Holds.** Default build: `tfbench -exact` and the answer digest equal
   `verify/expected/`'s.
2. **Holds.** `tfdrip`: the answer digest with Count equals W4d's; `tfbench
   -exact` equals W4d's but for the churn rows' relocation and eviction
   counts (relocations 148,716 → 148,685, 379,688 → 379,659, 932,499 →
   931,823; evictions at load 1.15 4,577 → 4,600), within 1%.
3. **Holds.** The 30-cell digest at the three offsets equals the digest at
   1,200,000 byte for byte.
4. **Holds.** The lockstep with a fast clock finds no difference in any slot
   or answer in 400,000 operations at each of the five offsets.
5. **Holds as run:** `TestWrapAlias` and `TestWrapDeadlineEntries` pass in
   the plain mode, the only one the script ran them in.
6. **Fails.** The suites with `tfdrip` fail tests that W4d's tree passes:
   `TestTickRange` in all four modes (`ContinuousDecay` at 2^46 does not
   panic: its new `keepUp` finds the tick past the deadline, catches the
   sweep up at it and leaves the cursor past the tick's target, so
   `decayStep` returns before its check); `TestWrapDeadlineEntries/snapshot`
   under `TF_CT=1` and `TF_BG=1` (snapshots of those modes are refused: the
   test's defect, not the filter's); `TestWrapAlias/stash` under `TF_BG=1`
   (a key reads Seen, Count 1, a wrap later).
7. **Holds.** Under `-race`, the soak with its clock from 2^34 - 10,000,000
   (a tick per 20 µs: it crosses 2^34 after 200 s of its 300): 45,129,831
   operations, 13,540,427 counts checked, 0 low; the concurrent fuzzer from
   2^34 - 2^29 with idle jumps: 94 cases, 66,729,361 operations, 17,309,563
   answers checked, none failed. (Both fail only on an answer below the
   truth: see the audit below, item 3.)

Reported. The catch-up: the first call after an idle gap of 2^31 + 1 ticks,
and of 3 x 2^31, on the stall test's table (308,405 lines) took 34.5 ms and
34.6 ms. Speed, ns/op medians of 3 (default / W4d `tfdrip` / W5d `tfdrip`):
CheckAndTouchHit 194.4 / 201.4 / 207.4; CheckAndTouchHitHot 74.4 / 67.4 /
74.3; CheckAndTouchMissInsert 1833 / 1915 / 1908; TouchAndCount 207.6 /
220.0 / 225.8; VelocityCount 192.9 / 192.1 / 203.7; VelocityCountHot 101.3
/ 93.8 / 106.6; ContainsAbsent 172.1 / 172.5 / 178.2; CheckAndTouchBatch
92.5 / 97.0 / 105.7. Against the default build: hits 1.07x, counts 1.06x,
TouchAndCount 1.09x, batched touches 1.14x; the wrap over W4d: 1.03x,
1.06x, 1.03x, 1.09x (VelocityCountHot 1.14x).

**Verdict: W5d fails its rule** (clause 6). The audit below finds that two
of clause 6's failures are not W5d's alone.

## The record audited

Every verdict from W1 to W5d was read again against its evidence and its
code after W5d's reading. The audit's readings were taken on the trees as
committed, with a probe (`TestAuditTickPoison`, `TestAuditAliasSettled`)
copied into detached worktrees and never committed to one; they are in
`lab/audit/` (`audit_test.go`, `a1.sh`-`a3.sh`, `readings.txt`). They are
no arm's reading, and nothing was mended. In order of weight:

**1. A call past `MaxTick`, or one 2^31 ticks ahead of the others, empties
the filter, and can turn the deadline off, on every wrap tree; A35's
guarantee does not hold under the wrap.**
`keepUp` runs first in every call (`touch`, `read`, the batches,
`Occupancy`, and since W5d `ContinuousDecay`), before any tick check. A tick
past `MaxTick` is more than 2^31 past `doneAt`, so the call runs `catchUp`
at it: every live key is swept as expired, `doneAt` becomes that tick, and
the cursor goes a lap past its target. After that no call catches up (none
passes `doneAt` + 2^31) and no decay step sweeps (the cursor is past every
target), so no stored tick is cleared again, and each aliases a wrap later.
`TestAuditTickPoison` (a key touched 3 times at 1,000,000; one call past
`MaxTick`; a second key touched, then read a wrap and 500 ticks later): on
d4d0187 (W5b's code, the tree W5c read) and on 9db48f9, after a `TouchAt`
past `MaxTick` (which still panics, after the sweep) or a `CountAt` past it
(which does not check, as in the default build) the first key reads Count 0
for 3, and the second reads Seen, Count 1 a wrap later; on 9db48f9 a
`ContinuousDecay` past `MaxTick` does the same, without its panic. The
default build reads 3 for 3 after all three, and its panics stand. A35 (an
insert past `MaxTick` panics before taking any lock, so a caller that
recovers keeps a usable filter) and `format.md`'s MUST do not hold under the
wrap. `TestTickRange` did not see it before W5d: its filters either read
only keys inserted after the panic or had caught up to within a unit of
`MaxTick` first. A call within `MaxTick` that is more than 2^31 ticks past
`doneAt` takes the same path (9db48f9): one `CountAt` at 1,000,000 + 2^31 +
2 (35.8 minutes ahead at 1 µs) leaves the first key at Count 0 for 3; one at
1,000,000 + 2^35 also leaves the second key Seen a wrap later, the other
callers' clocks staying behind the new `doneAt`. The default build reads 3
for 3 after both. The precondition that callers' clocks stay within 2^31 of
each other is not checked, and breaking it once, with a read, empties the
table; an idle filter's first call looks the same when it arrives.

**2. The wrap's alias test fails under Background, on every wrap tree.**
`TestWrapAlias/stash` with `TF_BG=1` fails 11 of 40 runs on 9db48f9, 14 of
40 on 4e4966e (W5b's code on W4d) and 12 of 40 on d4d0187; plain and under
`TF_CT=1`, 0 of 40. A failing key reads Seen with its old count (1 or 3) a
wrap later. With the Background placer drained (`Waiting()` 0) before the
clock jumps (`TestAuditAliasSettled`), 0 of 40 on both trees: the failures
need the placer at work during the catch-up. From the code, an entry the
placer is committing is busy, which `sweepSet` skips, and lands in a line
`catchUp` may already have passed; that is not shown beyond the drained
runs. W5b's clause 4 and W5d's clause 5 ran `TestWrapAlias` in the plain
mode alone (`lab/w5.sh`, `lab/w5d.sh`), and W5b's Background suite passed
it by chance; W5c read no suites, taking W5b's clauses as read.
**W5c's verdict is narrowed again:** a key aliased by a whole wrap reads
unseen from the table and the stash, idle or not, except under Background
while the placer has keys in hand.

**3. The concurrent checks of the wrap cannot fail on its failure.** The
soak and the concurrent fuzzer fail only on an answer below the truth
(every answer check in `soak_test.go` and `fuzzconc_test.go` is Upper below
the true count; their other checks are on occupancy). A wrap that fails decodes a dead key's tick as recent, and the
answer reads high: Seen, with a count the key no longer has. W5b's clause 6
and W5d's clause 7 could not fail on it whatever their clocks met; the
fuzzer's Background cases passed while item 2's test fails one run in
three. The wrap's evidence against aliasing is sequential (`TestWrapAlias`,
`TestWrapDeadlineEntries`, the digests, the lockstep). A check that could
see it: count the answers that read Seen for a key whose events are all more
than a horizon old, with the default build on the same stream as the bar.
It is the shape of W4c's blind clause 5: a check on a path that never reads
what it judges. The same holds for the drip log: the soak and the fuzzer
assert nothing of Count, the one answer the log moves, so their passes from
W4 to W4d say only that `Upper` held; the log's concurrent evidence is
`TestSlotUnitRaces`, `TestDripPushOnce` (the log against the register, not
the answer) and the suites' racy tests, whose windows were not audited
here.

**4. The held-out seed is not held out.** W1's and W2's rules judged and
chose on both seeds (W2: the candidate with the lowest worse-seed mean goes
first), so (3, 7) took part in choosing unit/16 and the log's width; W3
read its other workloads on (9, 9) alone. 0.0450 and 0.0444 are both
in-sample: no seed that took no part in the choice has been read.

**5. The record is out of step.**

- "Where the lab leaves the weakness" (after W5c) still names W4c and W5b
  and says the wrap answers as the full one; both were revised after it. It
  is restated below.
- The patches exported for the main repository (`go/verify/experiments/`,
  not committed there) and its `experiments.md` section were made before
  W4c's revision: `drip-log.patch` is W4c's code (`miss`'s second push),
  `wrapped-last-touch.patch` is W5b's without W5d, the notebook patch ends
  at W5c's reading, and the section reports W4c as passed and W5c
  unnarrowed. `export.sh` takes `lab/sw-2026-11` for W4c's tree; it is
  W4d's now. They must be exported again before anything is recorded from
  them.
- W5d's outputs were in `~/lab-sw-out/w5d` alone; they are in `lab/w5d/`
  now (without the `.test` binaries), with the W4d outputs W5d compared
  against in `lab/w5d/w4d-ref/`.
- The costs in the summary (hits 1.05x, counts 1.07x, TouchAndCount 1.12x,
  batched touches 1.17x) are W5b's run; W5d's reads 1.07x, 1.06x, 1.09x and
  1.14x. All on the WSL2 guest, indicative.

**6. Limits the rules never weighed.**

- Horizon at most 2^31 ticks is 35.8 minutes at the default 1 µs
  Resolution: under the wrap `New` refuses any longer horizon unless the
  caller coarsens Resolution, where the default build takes up to `MaxTick`.
- Snapshots are version 3 in both builds, and nothing in the stream says
  that its word0s hold a drip log and a wrapped 34-bit tick, so by the code
  a `tfdrip` snapshot restores into a default build, and the reverse, and
  is misread. Not read.
- `TestMerge`: merged 1 s MAE 0.2245 against the single filter's 0.0740 (a
  merge drops every log). The default build's merged figure on the same test
  was not set beside it, so whether the merge is worse than the default
  build's, or only no better, is not known.
- The catch-up is one call of 34.5 ms (308,405 lines) holding `catchMu`
  against every other call that finds the deadline passed; the reclaimer the
  paper describes frees at most 4 lines a call. No bound was set for it.
- Adopting the log changes pinned outputs: the conformance vectors' Counts,
  `TestCountsLastTouchWhole`, `tfbench -exact`'s steady 1 s lines. Each is a
  claim or a specification change; the rules named them and did not weigh
  them.

**7. Never read** (open since W5c): MAWI; speed on bare metal; a TLA+ model
of the log beside the slot protocol (both double pushes, W4b's and W4c's
through `miss`, were found late by tests; push-once across a retry, the
stash, the wait room, a relocation and a resize is the kind of interleaving
in which `SlotUnit.tla` found B13's defect); the ports.

Checked and not a gap: W4d's `robust-drip.txt` prints that W3's rule holds
false because under `tfdrip` the test's "filter" row is the drip filter, so
W3's rule there sets the model against itself; the W4d reading uses the
filter rows alone.

### Where the lab leaves the weakness, restated

| | 1 s | 5 s | 15 s | 30 s | 60 s | mean (target 7: <= 0.05) |
|---|---|---|---|---|---|---|
| the filter (8b3ba44) | 0.1671 | 0.0503 | 0.0506 | 0.0435 | 0.0239 | 0.0671 |
| the drip log, W4d (`tfdrip`, `MaxTick` 2^34-1) | 0.0592 | 0.0476 | 0.0505 | 0.0435 | 0.0239 | **0.0450** |
| seed (3, 7) (in-sample), the filter → W4d | 0.1663 → 0.0580 | | | | | 0.0666 → 0.0444 |

What stands: the drip log as W4d builds it is the 11-bit model on the
filter and passes its rule, at a `MaxTick` of 2^34-1 (4.8 hours at 1 µs).
What does not: the wrap that would give it those bits without lowering
`MaxTick` (W5b-W5d). A call past `MaxTick`, or one read 2^31 ticks ahead
of the others, empties the filter, and past `MaxTick` or far enough ahead
stops the deadline (item 1); a key can alias under
Background (item 2); and the concurrent checks that passed the wrap could
not have failed on aliasing (item 3). The figures are on seeds that chose
the design (item 4).

Next, each with its rule before its reading (none run): W5e, the wrap
mended, with the tick checked before `keepUp`, the placer's commits kept out
of a catch-up, and a decision on a caller far ahead of the others (refused,
or a documented precondition); `TestWrapAlias` and `TestWrapDeadlineEntries`
in all four modes over repeated runs; and an over-count check in the soak
and the fuzzer, read first on a tree that fails item 2, so that it is shown
to see the failure. Then target 7 on fresh seeds that took no part in the
choice.

## Phase 6: the drip log on fresh seeds (W6)

The audit found that seed (3, 7) took part in choosing the design (W1's and
W2's rules judged on both seeds), so W4d's 0.0450 and 0.0444 are in-sample.
W6 reads W4d's filter, its library code unchanged, on 40 seeds that no arm
of this lab has read: (2026, 1) to (2026, 40), tfbench's velocity workload
otherwise as it is (`swWorkload`, Capacity 200,000, Resolution 1 ms, as
tfbench's and as every W4-W4d reading). `TestShortWindowFreshSeeds`
(`TF_EXP=1`, `swseeds_test.go`) replays each seed once and scores the
register with its onset, the 11-bit unary log at unit/8, unit/16 and
unit/32 with the onset, and the filter of the build it runs in; it runs
once in the default build and once with `tfdrip`.

**Rule (before the reading).** W6 holds if, on every one of the 40 seeds:

1. default build: the register-with-onset model equals the filter at every
   window (the estimator still reads the default filter, so the model
   stands for the default build in the `tfdrip` run);
2. `tfdrip`: the filter is within 0.002 of the 11-bit unary log at unit/16
   with the onset at every window;
3. `tfdrip`: target 7's metric (the mean of the five windows) is at most
   0.050;
4. `tfdrip`: no window above the default build's on the same seed (the
   register-with-onset model's) by more than 1%.

If clause 3 fails on any seed, the drip log meets target 7's bound on some
draws of tfbench's workload and not on others, and the record says so; the
workload, the metric and the target are not changed (they are the user's).

Reported, not judged: per seed, the filter's five windows and mean in both
builds; across the seeds, each window's and the mean's minimum, median and
maximum; on how many seeds unit/16 is still the best of the three quanta
(it was chosen on (9, 9) and (3, 7)).

### W6's reading (4cf2d97's tree; `lab/w6.sh`, outputs in `lab/w6/`)

1. **Holds.** Default build: the register-with-onset model equals the
   filter at every window on all 40 seeds.
2. **Holds.** `tfdrip`: the filter is within 0.002 of the 11-bit unary log
   at unit/16 with the onset at every window on all 40 seeds; their means
   are equal to the fourth decimal on every seed.
3. **Holds.** Target 7's metric with `tfdrip` reads 0.0445 to 0.0454, median
   0.0449, on the 40 seeds (the default build 0.0661 to 0.0674, median
   0.0668).
4. **Holds.** No window above the default build's by more than 1% on any
   seed.

Across the seeds, minimum / median / maximum, `tfdrip` (the default build in
brackets): 1 s 0.0581 / 0.0594 / 0.0610 (0.1638 / 0.1667 / 0.1694); 5 s
0.0470 / 0.0481 / 0.0492 (0.0493 / 0.0506 / 0.0514); 15 s 0.0490 / 0.0497 /
0.0504 (0.0491 / 0.0498 / 0.0504); 30 s 0.0427 / 0.0434 / 0.0441 and 60 s
0.0235 / 0.0239 / 0.0241, the default build's on every seed. Unit/16 is the
best of the three quanta on 40 of 40 seeds (means at unit/8 0.0462-0.0470,
at unit/32 0.0522-0.0534).

**Verdict: W6 holds.** W4d's 0.0450 on (9, 9) is a typical draw of the
workload, not a property of the seeds that chose the design: on 40 seeds
that took no part in the choice the drip log reads 0.0445-0.0454, each under
target 7's 0.050 by at least 0.0046, and unit/16 stays the best quantum on
every one. Item 4 of the audit is closed for tfbench's workload. What W6
does not read: other shapes and rates (the seeds draw the same five shapes
at the same rates; W3 read other rates, on (9, 9) alone). Target 7's status
is the user's.

## Phase 7: every answer checked both ways under concurrency (W7)

The audit (item 3) found that no concurrent check reads Count against the
truth in both directions: the soak and the fuzzer fail only on `Upper`
below the true count; `TestBackgroundConcurrentCounts` asks a 60 s window,
which cuts no bucket and so, since W4c's shortcut, never reads the log; and
`TestDripPushOnce` audits the log against the register, not the answer. The
drip log's two double pushes (W4b's on a retried count, W4c's through
`miss`) were each found late. W7 builds the missing check, shows it on the
trees that hold those two defects, and then reads W4d with it.

**The check** (`TestDripConcurrentCounts`, `dripcount_test.go`, `TF_EXP=1`,
both builds): `TestBackgroundConcurrentCounts`'s scenario, the one both
double pushes were found in. 8 workers each own 4 of 32 keys and touch each
20 times at ticks 25 apart from a shared clock, in a filter of Capacity 64
(Stash 4,096, SpillMin 1) under a flood of other keys, so the keys wait,
are evicted to the stash, come back and are moved while their owners touch
them. Three modes: plain, Background, and ConstantTime (functional, as the
concurrent fuzzer's). Two shapes: *spread*, the scenario as it is (a key's
touches ~800 ticks, ~5 quanta, apart: one or two codes in the field, never
a zero gap); and *burst*, where a key's 3rd, 8th, 13th and 18th touches are
12 touches at one tick each (11 zero codes: a full field of a valid burst;
at most ~14 events in a unit, under the register's 31). After each round a
shadow filter (Capacity 4,096, the same Horizon, Resolution, Stash, SpillMin
and mode, no Background, no flood) is given the 32 keys' events in tick
order on one goroutine, and every key is read in both at the round's end at
nine windows, 250 ms, 500 ms, 1, 2, 4, 8, 15, 30 and 60 s: Seen, Count,
Upper and Saturated must be equal.

Why equal: a key touched by one goroutine at increasing ticks has slot
words that are a function of its own events alone, wherever the filter
moves them, and an answer is a function of the words, the window and the
tick. The code's paths that make a log say nothing are, for such a key:
`next0`'s event at an earlier unit than the last touch (a lagging clock:
not reachable, ticks increase), `dripForget` (another touch of the same key
moving its last touch: not reachable, one owner), `Merge` and snapshots
(not called), and a new word0 for a history that was not recalled (reachable
only after the key's history was folded into the overflow table or lost).
So a difference is counted as *log lost* when the filter's log says nothing
where the shadow's does not, and as *wrong* otherwise, by direction (Count
high, Count low, another field). Set apart and reported: a key whose
fingerprint and tag another key of the round shares (a collision), and
every difference in a round whose filter folded or lost a history. The
check judges the state the concurrent writes left, not answers given while
touches race, and this one scenario only: the soak and the fuzzer still
assert nothing of Count. The script is `lab/w7.sh`.

**Rule (before the reading).** W7 holds if:

1. control: in the default build, 20,000 rounds in each mode and shape, no
   answer differs from the shadow's but those set apart (the shadow is a
   valid reference: without the log, every short window already reads the
   moved words);
2. sight: the check finds the two known defects, as Count high, the double
   push's signature. On W4b's tree (06940e8) with `tfdrip`, Background,
   spread, at least one Count high within 3,000 rounds
   (`TestBackgroundConcurrentCounts` failed 36 of 60 runs of 200 rounds
   there); on W4c's tree (0837f9f), the same, within 20,000 rounds (the probe
   found 3 keys in 5,000 rounds there). W4c's leg is also the evidence that
   the check reads the log on a tree with the shortcut. The test file is
   copied into detached worktrees of those commits, as the audit's probes
   were;
3. W4d's library with `tfdrip`: 20,000 rounds in each mode and shape, no
   wrong answer; and 1,000 rounds in each under `-race`, none.

If clause 1 fails, the shadow is no reference and W7 says nothing of W4d.
If clause 2 fails, the check is blind to a defect it was built for, and
clause 3's pass would not clear W4d: W7 fails. If clause 3 fails, W4d's
clause 5 is revised as W4c's was, and the failing path is diagnosed before
anything is mended. A log lost on W4d is reported and diagnosed (a path the
reading above missed) without revising W4d by itself: it costs accuracy,
not a wrong answer from the words it holds.

Reported, not judged: the differences by window, kind and direction;
Degraded answers; the waits, evictions, restores, relocations, folds and
losses each mode's rounds had (that the paths were taken); the rounds and
differences set apart.

### W7's reading (67638ea's tree; `lab/w7.sh`, outputs in `lab/w7/`)

1. **Spread: holds. Burst: fails.** Default build, 20,000 rounds a mode
   (5,760,000 answers each): spread, no answer differs in any mode (set
   apart: 628, 597 and 396 differences in 208, 195 and 75 rounds that folded
   a history; no collision); burst, plain 1,147 differ (Count low 395,
   another field 752), Background 3,454 (1,090, 2,364), ConstantTime 4,497
   (914, 3,583), none Count high, no log lost.
2. **Holds.** W4b's tree, Background, spread: 137 answers Count high in
   3,000 rounds, none low or in another field, at every window from 500 ms
   to 60 s (W4b reads the log at every window). W4c's tree: 29 Count high in
   20,000 rounds, none otherwise, at windows from 250 ms to 15 s and none at
   30 or 60 s (the shortcut: those windows cut no bucket). The check sees
   both double pushes, by their signature, and reads the log on a tree with
   the shortcut.
3. **Spread: holds. Burst: not judged.** W4d's library with `tfdrip`, 20,000
   rounds a mode: spread, no answer differs in any mode (set apart 613, 651
   and 427 in 246, 232 and 78 fold rounds); burst, plain 163 (low 50, another
   field 113), Background 343 (92, 251), ConstantTime 1,620 (255, 1,365), none
   Count high, no log lost. Under `-race`, 1,000 rounds in each mode and
   shape: no answer differs, burst included.

The paths were taken: on W4d's spread runs, 18.5 M evictions, 13.0 M
restores and 266,000 relocations (plain), 27.0 M waits (Background), 137 M
waits and 498,000 evictions (ConstantTime), 0 histories lost.

**Verdict: W7 fails its rule as written** (clause 1, the burst shape), and
by its rule says nothing of W4d under bursts. For the spread shape, the
scenario both double pushes were found in, all three clauses hold: the
check sees both known defects and finds no wrong answer on W4d in 60,000
rounds (17.3 M answers), nor in 3,000 under `-race`.

**Diagnosis of clause 1's burst failure** (after the reading; a copy of the
test that splits the differences by flag, `lab/w7/diag/`, never in the
tree; 2,000 and 10,000 rounds, which are not readings). The rule's "at most
~14 events in a unit" was wrong: the shared clock moves 25 ticks for any
worker's step, and when the workers do not interleave, a key's touches are
~100 ticks apart, not ~800, so up to three of its bursts fall in a second
(the shadow's Upper reaches 33 at 1 s). Those keys pass the register's 31
and contend for the filter's 16 hot entries, which a key wins or loses by
timing; the shadow, of Capacity 4,096, has more. Every difference carries a
flag the API gives for it, in both builds: a refused promotion answers
Saturated (Upper capped at 31), and a key whose hot entry was shed answers
from the overflow table's floors flagged Degraded (high: Seen at 250 ms for
a key last touched 425 ms before). With the shadow given the filter's 16
hot entries: in 2,000 rounds a mode, 18-676 differences with an answer
Saturated, none with neither flag but in Background, where 10,000 rounds
give 7 (`tfdrip`) and 14 (default), each printed one Degraded (all 7, and
12 of the 14: the copy prints 12). The logs are equal
in every difference printed, and no difference is a log lost. The rule's
set-apart named rounds that folded or lost a history, not a shed hot entry,
and so caught none of these.

What W7 leaves: the drip log under bursts, against a shadow, unjudged; and
the differences set apart in fold rounds (about 1% of rounds) unexamined.
A W7b would read the burst shape again with every answer either filter
flags Saturated or Degraded set apart (in place of whole fold rounds) and
the shadow given the filter's hot entries; it would be written after this
diagnosis, which has already read the burst shape that way for 2,000 to
10,000 rounds, and would have to say so.

## W7b: the burst shape, with the API's disclaimers set apart

**Written after W7's reading and its diagnosis.** That diagnosis already
read the burst shape with the differences split by flag, for 2,000 rounds
a mode in both builds, with the shadow given the filter's hot entries and
without, and for 10,000 rounds in Background: every difference it printed
had an answer flagged Saturated or Degraded. So this rule is not blind on
the burst shape; what it adds is the reading at full length, with
Degraded set apart and with the narrower set-apart below judging the fold
rounds W7 left unexamined.

**What changes in the check** (`TestDripConcurrentCounts` as W7b revises it,
test code only, no library change):

- set apart answer by answer, in place of W7's whole rounds that folded a
  history: every answer either filter flags Saturated or Degraded (a
  promotion refused or a hot entry shed under contention for the filter's
  hot entries, an answer that includes the overflow table: the API's own
  disclaimers), and every answer for a key the filter's overflow table holds
  an entry for (a folded history, its own or one that shares its identity:
  `addSketch` flags Degraded only when the overflow table adds a Count, and
  can add to Upper unflagged); still set apart, collisions, and rounds that
  lost a history outright (`Lost`, 0 in all of W7);
- both filters get the Capacity-64 filter's 16 hot entries (in W7 the
  shadow had its own capacity's, so its keys were promoted where the
  filter's were refused);
- the test reports how many answers it judged.

Everything else is W7's: the scenario, the two shapes, the three modes, the
nine windows, the shadow, the kinds of difference. The script is
`lab/w7b.sh`, outputs in `lab/w7b/`.

**Rule (before the reading).** W7b holds if:

1. control: in the default build, 20,000 rounds in each mode and shape, no
   judged answer differs from the shadow's;
2. sight, as W7's with the new set-apart: on W4b's tree (06940e8) with
   `tfdrip`, Background, at least one Count high within 3,000 rounds in each
   shape, spread and burst; on W4c's tree (0837f9f), at least one within
   20,000 rounds in the spread shape (its burst shape reported, not judged:
   its defect's rate there is unknown);
3. W4d's library with `tfdrip`: 20,000 rounds in each mode and shape, no
   judged answer wrong; and 1,000 rounds in each under `-race`, none;
4. the burst shape leaves at least half its answers judged in every mode,
   in both builds; if not, the burst shape is too thin to clear W4d, and
   W7b says so without failing on it.

If clause 1 fails, the shadow is no reference again. If clause 2 fails, the
narrower set-apart has blinded the check: W7b fails. If clause 3 fails,
W4d's clause 5 is revised and the path diagnosed before anything is
mended. A log lost on W4d is reported and diagnosed, as in W7.

Reported, not judged: the answers set apart by each reason, the judged
share per mode and shape, the differences by window, kind and direction,
and the paths taken.

### W7b's reading (6ffe62d's tree; `lab/w7b.sh`, outputs in `lab/w7b/`)

1. **Holds.** Default build, 20,000 rounds in each mode and shape: no
   judged answer differs, in any of the six; 99.9% to 100.0% of answers
   judged (set apart, flagged Saturated or Degraded: 549 to 4,001 a run; no
   unflagged answer for a key in the overflow table, no collision, no round
   that lost a history).
2. **Fails as written.** Spread: W4b's tree 102 answers Count high in 3,000
   rounds, W4c's 46 in 20,000, nothing else differing on either: holds.
   Burst on W4b's tree: 1,002 answers differ in 3,000 rounds, at windows
   from 250 ms to 8 s, and none is counted Count high: the test classes a
   difference as "log lost" before it looks at the direction, and every one
   has the filter's log saying nothing where the shadow's does not (the
   examples printed read Count high and Count low both, the count then
   coming from the register alone). The clause asked for a Count high in
   each shape. W4c's burst leg, reported: nothing differs in 20,000 rounds.
3. **Holds.** W4d's library with `tfdrip`, 20,000 rounds in each mode and
   shape: no judged answer wrong and no log lost, in any of the six (34.6 M
   answers); under `-race`, 1,000 rounds in each, none.
4. **Holds.** The burst shape's judged share is at least 99.9% in every mode
   in both builds.

**Verdict: W7b fails its rule as written** (clause 2, W4b's burst leg), not
by the failure the rule named for it: the narrower set-apart did not blind
the check, which found 1,002 differences on W4b under bursts against none in
the default build and none on W4d. From W4b's code: its log says nothing
once 13 events fall in one quantum (the defect W4b's reading 8 found), so a
burst of 12 that a retried touch pushes once more crosses it where the
shadow's 12 do not, and W4b's double push shows under bursts as a lost log
rather than as a count one high. That is read from the code and from the
log fields printed (the filter's 0, the shadow's nonzero, in every example),
not shown further. The rule took the double push's signature from the
spread shape, where it is Count high, and that signature does not carry to
a tree whose log has no valid bit.

What W7b reads of W4d: in the scenario both double pushes were found in,
in both shapes and all three modes, no judged answer differs from a
single-threaded shadow's in 120,000 rounds (34.6 M answers), nor in 6,000
under `-race`; under bursts the drip log's full field of zero codes is read
as a valid burst and answers as the shadow's. What it leaves: the check's
sight of W4c's kind of double push (through `miss`) under bursts is not
shown (W4c's burst leg found nothing in 20,000 rounds); answers flagged
Saturated or Degraded are not judged (72 to 1,531 of W4d's 5.76 M a run, under
0.03%); and the
soak and the fuzzer still assert nothing of Count.

## Phase 8: one push per touch, modelled (W8)

The drip log's two double pushes were each found late by tests (W4b's on a
retried count, W4c's through `miss`), and W7/W7b read W4d's push only in
one scenario's state at rest. The slot protocol's other interleavings were
settled by TLA+ models (`go/verify/tla/`), and `SlotUnit.tla` found B13's
defect where tests had not. W8 models the push.

**The model** (`go/verify/tla/experiments/DripPush.tla`, PlusCal, run by
`lab/w8.sh`). It is `Relocate.tla`'s protocol as committed (the "hidden"
variant, claims A23: a relocation's copy goes in with word1 busy and is
published after the hand-off, which takes src only while it holds the words
the move read; an abandoned copy is cleared; evictions and moves of K's
stripe exclude each other with counts; a miss is trusted only between
unchanged move counts, and restores K's history from the stash under K's
claim stripe), with word0 carrying the drip log as the sequence of events
pushed into it. `Touchers` owners (one or two) each touch K `Touches` times,
one at a time, touch n at tick n, so that two owners' n-th touches fall at
one tick, or with `Burst` every touch at tick 0, the tick of the event
before the run. A touch pushes its event in word0's CAS (`next0`), then
counts it in word1 (bump); a count that fails because the slot changed hands
sends the touch back to its lookup; the miss path restores K's history from
the stash and counts there. The push-once rule as each tree has it:

- `"w4d"`: a touch that pushed its event and starts over pushes it again
  only into a history whose last touch is behind its tick, in the table's
  path (`touch.go`) and in `miss` (`cuckoo.go`);
- `"w4c"` (0837f9f): the rule in the table's path only;
- `"w4b"` (06940e8): no rule; every attempt pushes;
- `"tickOnly"`, a wrong mend kept for sight: the tick test without the flag
  (no push into a history at or after the touch's tick, pushed or not),
  which loses pushes;
- `"noHandoffCheck"`, a broken protocol kept for sight: W4d's rule, with a
  hand-off that takes src whatever it holds (`Relocate.tla`'s variant), so
  a copy from before a touch's push can survive a move and the touch meets
  it (with two owners, one whose last touch the other's push has moved to
  the touch's tick).

Invariants: **PushOnce**, no copy of K's history (in a slot, hidden or not,
or the stash) holds an event twice; **CountLogged**, a touch is counted only
into a history whose log holds its event (a push skipped by the tick test is
not lost); **LogComplete**, once every process is
done, K's history holds every event of the run exactly once (a lost push
breaks it as a double one does); and `Relocate.tla`'s TouchNotLow, OneHome
and Conserved, so the moves still keep every event (its ReadNotLow cannot
fail at 0 reads and is not judged). LogComplete and Conserved are read only
where every process is done, so each configuration is also checked for
**Termination** (the translation's: every fair behavior ends with every
process done): no state from which the run cannot end, where a lost push
would go unread. NotMoved, NotEvicted and NotRepushed check reachability: a
move, an eviction, and a second push by a touch that pushed already, into a
history behind its tick (the half of W4d's rule that pushes again).

Not modelled, named now: the log's 11-bit bound, its quanta and its valid bit
(only which events the log holds, and how often); a toucher whose clock
lags the other's (the two owners touch at the same ticks: neither is ever
behind the last touch, nor in an earlier unit), and two touchers of two
touches each (not run); the wait room and its placer
(`WaitRoom.tla`'s ground), ConstantTime's stash path, `realign` and `stamp`
(a touch into a new unit, which pushes with its register by one CAS or under
a hold, and after `stamp` retries through the same rule), `dripForget`,
merges and snapshots; hot entries (promotion, `hotAdd`, `unpoint`) and
`bump`'s strayed count; `miss`'s two failures (a claim that loses its race
puts the recalled words back as they were, its push never placed; a hot
restore whose entry is reclaimed after `hotGone` fails with its push
placed, below): the model's `miss` always succeeds. A restored history stays
in the stash, as in `Relocate.tla`, and the mover and the evictor act once,
so no move or eviction of a restored copy is reached. Reads are 0
throughout: a read moves no log.

**Sizing, before the rule** (not a reading of W8): the committed
`Relocate.tla` at 2 touches and 0 reads, the base this model adds the log
to, checks in 15 s (795,231 states) with eviction and 3 s (39,699) without.
Each W8 run is capped at 30 minutes (`TLC_TIMEOUT`); a run that does not
finish is reported with its states and not judged, and a clause judged on
fewer runs says so. `lab/w8.sh small` reads every run with one toucher and
one touch (clause 1's one-toucher sight, the reported runs, clause 2's
one-touch configurations); `lab/w8.sh large` every run with one toucher and
two touches; `lab/w8.sh two` every run with two touchers of one touch each
(clause 1's two-toucher sight, clause 3, a reported run). No base sizes two
touchers: `Relocate.tla` has one. Outputs in `lab/w8/small`,
`lab/w8/large` and `lab/w8/two`.

**Rule (before the reading; revised before it, below).** W8 holds if:

1. sight: PushOnce is violated by `"w4b"` with one touch and relocation only
   (no eviction), and by `"w4c"` with one touch and eviction; LogComplete is
   violated by `"tickOnly"` with one touch in a burst, relocation only; each
   in at least one of the two scan orders (`CopyFirst`); with `"w4d"`, a
   move and an eviction are reachable (NotMoved and NotEvicted violated);
   and with two touchers of one touch each, spread, relocation only,
   CountLogged is violated by `"noHandoffCheck"` in at least one scan order
   (the second toucher reaches what it was added for: a copy from before a
   touch's push, its last touch moved to the touch's tick by the other's
   push, taken for one that holds the push);
2. W4d, one toucher: with `"w4d"`, no invariant is violated and Termination
   holds, at 1 and 2 touches, spread and burst, with and without eviction,
   in both scan orders (16 configurations, 32 runs);
3. W4d, two touchers: the same with two touchers of one touch each (8
   configurations, 16 runs).

If clause 1 fails, the model does not reach the defects it was built for,
and its passes of clauses 2 and 3 clear nothing: W8 fails. If clause 2 or 3
fails, the trace is read against the code before anything else: a trace the code can
follow revises W4d's clause 5; one it cannot is a defect of the model, said
as such. A Termination counterexample is a behavior that never ends (a
retry that loops while every other process waits or is done), read the same
way: a loop the code can follow is a livelock of W4d's protocol, and one it
cannot is the model's. Reported, not judged: `"w4c"` with relocation only
(the table path's rule should cover it); NotRepushed with `"w4d"` at 1 and 2
touches with eviction, and with two touchers with eviction;
`"noHandoffCheck"` with one toucher of one touch, relocation only:
NotRepushed spread, CountLogged in a burst, and CountLogged spread (the
contrast to clause 1's two-toucher sight: with one toucher, a copy from
before the push is behind the touch's tick); every run's distinct states.

**What a pass means.** One or two owners of the key at the same ticks, the
table's same-unit path, relocation's hidden copy and eviction to the stash,
as `Relocate.tla` has them. If NotRepushed is never violated with `"w4d"`
and is with `"noHandoffCheck"`, no path in the model hands a touch that
pushed a history from before its push (the hand-off and the eviction both
compare word0 before they take the slot), and clauses 2 and 3 then judge
only the half of W4d's rule that skips: the half that pushes again exists
for paths the model leaves out (the wait room and its put-back,
ConstantTime's stale stash copy, a toucher whose clock lags). If
`"noHandoffCheck"` violates CountLogged in a burst with one toucher and in
spread with two, and not in spread with one, the tick test's blind side is
shown both ways the model can reach it: a copy from before the push whose
last touch is at the touch's own tick, and one moved to that tick by the
other toucher's push; clause 3's pass then says that with the hand-off and
the eviction as committed no such copy reaches a touch, two touchers at one
tick included. A pass does not clear `miss`'s hot restore (below). W4c's
defect was found in a Background scenario; its path through `miss` after
an eviction is modelled, the wait room's is not.

**Revised before its reading** (no TLC run of `DripPush.tla` or of
`lab/w8.sh` preceded the revision; written after an audit of the rule, the
model and the script against W4d's code):

- `lab/w8.sh` judged what the rule did not say. A run passed unless TLC's
  log held one of three strings, so a crash, a killed run or a missing
  `java` read as a pass; any `Error:` line or a deadlock counted as a
  violation, so a sight pair could be "seen" on a TLC error; a timeout
  failed its run, where the rule leaves it unjudged; a second `small` kept
  the first's logs and numbered its runs after them. Revised: ok only on
  TLC's completion line with exit 0; a violation only of the invariant or
  property the run names; a timeout reported and not judged (a sight pair
  is seen if either run violated, not judged if both timed out); anything
  else an error, never as wanted; each phase in its own directory, cleared
  when it starts; the script refuses a model or script that differs from the
  commit and exits non-zero on a judged outcome not as wanted. The
  classification was tried on a scratch spec (ok, violation, deadlock,
  livelock, a TLC error, a timeout, a failed translation), not on
  `DripPush.tla`. `go/verify/tla.sh` on main, which the script copied, has
  the same blind spots.
- Termination added to clause 2; ReadNotLow taken out of the judged
  invariants; the two-touch NotRepushed run moved to `large`.
- `"noHandoffCheck"` added, reported: NotRepushed's silence under `"w4d"`
  said nothing while no run showed that the ghost can fire.
- The constants are checked (`ASSUME`): a misspelt variant ran as `"w4b"`.
- Found by reading W4d's code during the audit, not a reading: **`miss` can
  push a touch's event twice through a hot restore.** `miss` places a
  recalled history with `low = next0(s0, now)`, this touch's push in it
  (`cuckoo.go:59`; by `claim`, `:68`, or waiting, `:74`), and only then counts
  a hot history by `hotAdd`. If the entry was reclaimed since `hotGone` (a
  call whose clock is past the entry's newest event by a horizon, claims
  A25), `miss` returns Count -1 with the history in place (`:95-100`), and
  `touch` retries with `pushed` as it was (`touch.go:288-290`): false unless
  the table's path had pushed. The retry pushes again, in the table's path
  (`touch.go:333`) or, if the history waits, in `miss` (`cuckoo.go:59`);
  `bump` then finds the reclaimed pointer and `unpoint` starts a fresh
  register (`register.go:263`). The log holds the event twice: at windows
  that cut a bucket, Count reads one above what a single push would. Not
  shown to be above the truth: the reset register no longer counts the
  history's older events, and any `unpoint` reset keeps the log's older
  events, which Count then reads though the register does not. Neither is
  in the model (no hot entries); nothing is mended here.

**Revised again before its reading** (the second toucher, an open decision
taken before any TLC run of `DripPush.tla`):

- Two touchers: the owner is a process set of `Touchers` (1 or 2), touch n
  of each at tick n; clause 1's two-toucher sight, clause 3 and a reported
  NotRepushed run, in `lab/w8.sh two`. With one toucher the steps are those
  of the model as first revised, CountLogged's ghost aside.
- CountLogged, judged in clauses 2 and 3, and the `"noHandoffCheck"` runs
  read through it: the reported LogComplete run in a burst could not show
  the tick test's blind side, as the first revision said it would, because
  a hand-off that takes src whatever it holds also overwrites a touch
  already counted there, push and count together, which breaks LogComplete
  as well. CountLogged fails only where a count lands in a history without
  its event.
- The script was run end to end on a stub module (32, 17 and 19 runs; exit
  0 as wanted, 1 on a failing W4d run, 2 on a model that differs from the
  commit), not on `DripPush.tla`; an error's line now carries TLC's
  message.

### W8's reading (4461e7a's tree; `lab/w8.sh`, outputs in `lab/w8/`)

1. **Holds.** One toucher of one touch (`small`): PushOnce violated by
   `"w4b"`, relocation only, in both scan orders (4,139 and 3,346 states), and
   by `"w4c"` with eviction in both (73,041 and 72,585); LogComplete by
   `"tickOnly"` in a burst, relocation only, in both (4,880 and 4,682); with
   `"w4d"`, NotMoved and NotEvicted violated (1,861 and 1,376). Two touchers
   of one touch each (`two`): CountLogged violated by `"noHandoffCheck"`,
   spread, relocation only, in both scan orders (379,709 and 626,426).
2. **Holds.** `"w4d"`, one toucher, 16 configurations: no invariant violated
   and Termination holds in all 32 runs; 7,776 to 138,360 states at 1 touch,
   17,154 to 389,919 at 2, the longest run 29 s.
3. **Holds on 12 of its 16 runs; 4 not judged.** `"w4d"`, two touchers of one
   touch each: no invariant violated in any of the 8 configurations
   (1,082,007 and 1,109,538 states without eviction, 43,415,628 and
   43,908,288 with, the latter 13 minutes each), every search complete and
   none reaching a deadlock (TLC's check, on in every run); Termination holds
   in the four without eviction, and timed out at 30 minutes in the four with
   (18,989,969 to 19,791,411 states at the cap, TLC's last progress line,
   under half of what the invariant runs found), not judged.

Every phase's line reads "every judged outcome as wanted"; `two`'s adds the
four timeouts. Reported: `"w4c"` with relocation only violates nothing (7,776
states, as `"w4d"`'s); NotRepushed is never violated by `"w4d"`, at 1 touch
(137,742 states), at 2 (389,919) and with two touchers (43,415,628), each
with eviction, and is violated by `"noHandoffCheck"` with one toucher in both
scan orders (4,625 and 4,984); `"noHandoffCheck"` with one toucher,
relocation only, violates CountLogged in a burst in both scan orders (6,781
and 7,409) and not spread (9,000 and 9,081). Every `"w4d"` configuration has
as many distinct states in a burst as spread, at 1 and 2 touches and with
two touchers.

The counterexamples read state by state follow the paths their sights
name. `"w4c"` (`small` run 3): the touch pushes into src, the evictor moves
the history with its push to the stash, the count fails, and `miss` restores
it and pushes again (the stash's log 0, 1, 1). `"noHandoffCheck"` in a burst
(`small` run 13): the touch pushes into src after the mover's copy, the
hand-off takes src unchecked and publishes the copy from before the push,
whose last touch is the touch's tick (0), and the retry skips and counts
there. Two touchers (`two` runs 1 and 2): the second toucher pushes event 2
into src after the mover's copy, the hand-off takes src unchecked, the first
pushes event 1 into the published copy at tick 1, and the second, pushed and
meeting a last touch at its tick, skips and counts there: a count whose event
no copy holds. `"tickOnly"`'s sight (`small` run 5) needs no race: in a burst
the touch's first attempt is already at the history's last touch, so it
never pushes, and the count lands in src. It shows that LogComplete can fail
and that runs reach their end, not a push lost to relocation. `"w4b"`'s run
1 was read by its steps only, and the sights' other runs not past their
results.

**Verdict: W8 passes its rule**, clause 3 judged on 12 of its 16 runs: with
two touchers and eviction, Termination is not judged.

What W8 reads of W4d, in the model (one or two touchers at the same ticks,
the table's same-unit path, relocation's hidden copy and eviction to the
stash, as `Relocate.tla` has them): every touch's event is pushed once into
K's history and every count lands in a history that holds its event, and,
where Termination was judged, every run ends. The conditions "What a pass
means" names came out as written: NotRepushed is never violated by `"w4d"`
and is by `"noHandoffCheck"`, so no path in the model hands a touch that
pushed a history from before its push, and clauses 2 and 3 judged only the
half of W4d's rule that skips; the half that pushes again was never taken.
The equal state counts in a burst and spread are consistent with that: under
W4d's rule the touch's tick decides no step in the model. `"noHandoffCheck"`
violates CountLogged in a burst with one toucher and spread with two, and
not spread with one: the tick test's blind side is shown both ways the model
can reach it, and with the hand-off and the eviction as committed no such
copy reaches a touch, two touchers at one tick included.

What W8 leaves: Termination with two touchers and eviction (four runs cut at
30 minutes at under half their space), so a livelock there, under which
LogComplete and Conserved would go unread, is not excluded; the half of W4d's
rule that pushes again, whose paths the model leaves out (the wait room and
its put-back, ConstantTime's stash copy, a toucher whose clock lags); two
touchers of two touches; hot entries and `miss`'s two failures, among them the
hot restore's double push found in the audit, read from the code and neither
reached nor mended; the log's bound, quanta and valid bit; and moves or
evictions of a restored copy.

## W8b: clause 3's Termination, re-read with a longer cap

**Written after W8's reading**, which cut its four Termination runs with two
touchers and eviction (`lab/w8/two` runs 9, 11, 17 and 19) at 30 minutes,
at 18,989,969 to 19,791,411 states of the 43,415,628 and 43,908,288 the same
configurations' invariant runs found. Their logs show TLC finding about
0.8 M distinct states a minute while it keeps the behavior graph for the
liveness check (3.7 M a minute in the invariant runs), and stopping for
periodic liveness checks on the partial graph, which grew to 3 minutes at
15.8 M states. No Termination outcome of these four configurations has been
seen; nothing else of W8 is re-read.

**What changes** (`lab/w8.sh b`, outputs in `lab/w8b/`). The model
(`DripPush.tla` as 4461e7a has it, unchanged), the four configurations
(`"w4d"`, two touchers of one touch each, eviction, spread and burst, both
scan orders), the property (`PROPERTY Termination`) and the classification
are W8's. TLC runs them one after another with:

- a cap of 3 hours a run (W8's: 30 minutes);
- the liveness check made once, on the complete state space (`-lncheck
  final`), in place of the periodic checks on the partial one: a violation
  is found either way, at the end rather than as it arises;
- 8 workers and a 4 GB heap (W8's: 4 workers and the JVM's 1.7 GB);
- TLC's log written into the outputs as it runs (W8's was copied out at the
  end), so a run a VM shutdown stops keeps its log.

The runner's other phases are unchanged. The options were checked, not
read: on the self-test's scratch spec (under `-lncheck final` a livelock is
a violation and a spec that ends passes; a run cut by the cap keeps its
log), and on one configuration W8 read (one toucher of two touches,
eviction, spread, copy first: Termination holds, 389,919 states, one check
on the complete space, 23 s against W8's 29 s).

**Sizing, before the rule** (not a reading of W8b): at W8's rate without its
periodic checks, about 55 minutes to find 43.9 M states, then the final
check (about 12 s a million states in W8's periodic checks: some 9
minutes); about an hour a run.

**Rule (before the reading).** W8b holds if Termination holds in each of
the four runs. A run cut at 3 hours is reported with its states and not
judged. A run that ends in an error (an `OutOfMemoryError` included) is not
a pass, and its cause is read before anything is rerun. A counterexample is
read as W8's rule reads one: a loop the code can follow is a livelock of
W4d's protocol, and one it cannot is the model's.

If W8b holds, W8's clause 3 is judged on all 16 of its runs and holds. If a
run finds a counterexample, W8's clause 3 fails in that configuration and
W8's verdict is revised to say so. If a run is cut or ends in an error,
clause 3 stays judged on fewer runs, and W8b says how many.

Reported, not judged: each run's distinct states and time, and its
liveness check's time.

### W8b's reading (21f215b's tree; `lab/w8.sh b`, outputs in `lab/w8b/`)

**Holds.** Termination holds in all four runs, each checked once on the
complete state space, with as many states as the same configuration's
invariant run found in W8: spread, copy first, 43,415,628 states (53
minutes, its liveness check 13); spread, not, 43,908,288 (59 minutes, 18);
burst, copy first, 43,415,628 (2 hours 23 minutes, 1 hour 43); burst, not,
43,908,288 (58 minutes, 18). The phase's line reads "every judged outcome as
wanted". The third run's liveness check took eight times the first's on as
many states, and the run ended 37 minutes inside the cap; its log does not
say why. The sizing's hour a run held for the other three: the states took
40 minutes to find, not 55, and the check 13 to 18 minutes, not 9.

**Verdict: W8b passes its rule.** W8's clause 3 is judged on all 16 of its
runs and holds: with two touchers of one touch each, `"w4d"` violates no
invariant and every fair behavior ends, with and without eviction, spread
and burst, in both scan orders. Of what W8 leaves, this closes the first
item: the model has no livelock with two touchers and eviction under which
LogComplete and Conserved would go unread. The rest of that list stands,
the half of W4d's rule that pushes again and the hot restore's double push
among it.

## Phase 9: the hot restore's push, probed (W9)

**Written after W8's and W8b's readings**, from the code reading W8's
revision recorded ("Found by reading W4d's code during the audit, not a
reading"); no run has taken the path. `miss` places a recalled history with
this touch's push in it (`cuckoo.go:59`) and only then counts a hot history
by `hotAdd` (`:95`). If the entry was reclaimed after `hotGone` passed
(`:33`), `miss` returns Count -1 with the history in place, and `touch`
retries with `pushed` false (`touch.go:288-290`): the retry pushes the event
again (`touch.go:333`), and `bump`, finding the pointer no longer the key's,
starts a fresh register (`register.go:263`). The tree's own test of the
corner, `TestHotEntryReclaimedDuringRestore` (`hot_test.go:273`), reclaims
the entry from a hook inside the restore's claim and checks only that Upper
counts the touch, in the default build. W8's model has no hot entries.

**The probe** (`lab/w9_test.go`, run by `lab/w9.sh` in a detached worktree of
the lab's head, whose library is b7a33a7's, W4d's; never in the tree;
outputs in `lab/w9/`). As that test: a filter of one hot entry, Horizon 1
minute at 1 ms; K promoted by 32 touches at t0 = 60,000, its words moved to
the stash and its slot freed, as `claimAt`'s eviction does; a key L whose
lines are not K's, promoted by 32 touches at tL = t0 + horizon + 3 units,
which reclaims K's entry; K's restoring touch at tK = t0 + horizon - 1,
behind L's clock and within a horizon of K's last touch. Four legs, plain
mode:

1. control, free slot: no reclaim; K's restore takes its freed slot;
2. reclaim at install, free slot: L's touches run from `testHookInstalling`,
   inside K's install, after `hotGone` and before `hotAdd`;
3. control, full lines: K's two lines first filled with keys live until t0 +
   horizon, so the restore relocates or evicts; no reclaim;
4. reclaim in the search, full lines: L's touches run from `testHookCopied`
   or `testHookVictim`, whichever K's claim reaches first (the tree's test).

During K's touch the probe records `testHookHotAdding`, which runs only in
`bump`'s add to a hot entry: on K's slot it shows the retry's look after
`miss` returned (`miss` adds to the entry itself). After the touch it reads
K's slot: the drip log against word0's after one push (`next0` of K's
stashed word0 at tK) and after two; word1 (a hot pointer and whose entry, or
a register and its events); and Count at 250 ms, 1 s, 10 s and 30 s, where
the truth is 1 (K's other events are 59.999 s earlier). In both builds:
`tfdrip` (the log) and the default build (Count's reference; no log). Only
hooks the tree has are used; the library is unchanged. The probe was
compiled in both builds and not run; `lab/w9.sh` was tried on a scratch
clone with the tree's own test in the probe's place (both builds; exit 2
on a probe not committed, 1 when no test runs). A first try ran it on the
lab itself before the probe was there: it copied nothing, ran no test ("no
tests to run" in both builds) and left `lab/w9/`, since removed; the script
then refused nothing, and now refuses a probe that is not committed.

**Rule (before the reading).**

1. The probe's checks: in legs 2 and 4 the hook fires and L is promoted on
   K's entry (Promoted 2); in legs 1 and 3, K is restored with its entry
   (word1 a hot pointer to K's entry, Restored up by 1, no `bump` add on
   K's slot) and its log holds one push; in every leg one push and two
   leave different logs. If a check fails, the probe is at fault, and W9
   says nothing of the hot restore until the probe is mended (a mend after
   a peek, said so).
2. The reading, `tfdrip`, legs 2 and 4, each judged only if its touch
   retried through `bump`'s add on K's slot (else: path not taken,
   reported): K's log holds the touch's event once, twice, or neither. The
   audit's reading predicts twice, beside a fresh register of 1 event.

If either reclaim leg reads twice, W4d's push-once fails on the hot restore,
as the audit read it; W4d's readings and W8's stand as read (neither reached
the path), and a mend (W4e) is written with its own rule before its reading,
not here. If both take the path and read once, the audit's reading was
wrong, and why is diagnosed (not a reading). "Neither", or a path not
taken in both, settles nothing and is diagnosed. Reported, not judged:
Count at the four windows in both builds against the truth; the paths taken
(relocations, evictions, restores).

Not probed: ConstantTime and Background (their claims also install on the
call path when a slot is free, but run on paths this probe does not set up);
the wait room's leg, where the history waits with its push and the retry
pushes in `miss` (`cuckoo.go:74`, `:59`), with no hook between `hotGone`
and `hotAdd`; a reclaim by a concurrent caller (the hooks run L's touches in
K's goroutine, between two of K's steps).

### W9's reading (fb7eeb3's tree; `lab/w9.sh`, outputs in `lab/w9/`)

1. **Holds.** The probe's checks, in both builds: the controls restored K
   with its entry (word1 a hot pointer to K's entry, Restored 1, no `bump`
   add on K's slot; the free slot by its install, the full lines by one
   relocation) and, with `tfdrip`, K's log holds one push (0xfff); in the
   reclaim legs the hook fired (`testHookInstalling`; in the full lines
   `testHookCopied`, K's claim relocating once) and L was promoted on K's
   entry (Promoted 2); one push and two leave different logs (0xfff and
   0xffe).
2. **Twice, in both reclaim legs.** Each took the path: K's touch retried
   through `bump`'s add on K's slot (Restored 0: `miss` returned before
   counting the restore), and K's log reads 0xffe, the log after two pushes
   of the touch's event at tK, beside a register of 1 event (the retry's
   fresh register) and a last touch at tK. The default build takes the same
   path: the same hooks, Promoted 2, the retry, a register of 1 event.

Reported: Count is 1 at 250 ms, 1 s, 10 s and 30 s in every leg in both
builds, the truth; every read was at tK. From the code, not a reading: in
the reclaim legs K's slot holds a register whose one event is in bucket 0,
and a read at its last touch is answered from the register alone
(`register.go:441`), without the log. A later read whose window starts
inside bucket 0 reaches the log (`:509` compares the register's unit with
the last touch's, not with the read's): traced by hand at tK + 100 ms with
a 1 s window, `dripCount` takes the extra zero code for a second event and
answers 2 against a truth of 1. That read was not made; the probe neither
shows nor rules out a Count high.

**Verdict: W9 confirms the audit's finding.** On W4d's library, a touch
whose hot restore loses its entry after `hotGone` pushes its event into K's
drip log twice, at the install into a free slot and in a claim that
relocates: W4d's push-once fails on the hot restore. W4d's readings and W8's
stand as read. W4d's suite took this path with `tfdrip`
(`TestHotEntryReclaimedDuringRestore`, in `lab/w4.sh` step 5's `go test
-tags tfdrip -short ./...`) and passed: that test reads Upper alone. W8's
model has no hot entries. By W9's rule a mend (W4e) is written with its own
rule before its reading; none is made here. What W9 leaves: whether the
extra event moves Count; a restore whose claim evicts (the full lines'
leg relocated); ConstantTime, Background and the wait room's leg; a reclaim
by a concurrent caller.

**Worded again after the reading** (no rerun; from the code and
`lab/w4.sh`): the Count note first said that tK, the last tick of its unit,
left no later read in that unit where the log is read, and that every Count
was a register's; the log is read at any later read whose window starts
inside bucket 0, and the controls' slots hold hot pointers. The verdict
first said W4d's tests have no reclaim inside a restore built with
`tfdrip`. The rule's "in the default build" was wrong the same way: the
tree's test has no build tag, and W4d's reading ran it with `tfdrip`.

## W4e: the hot restore's push, mended

**Written after W9's reading**, which found the double push, and after its
note's hand trace of a later read (Count 2 against 1), which was not read.

**The mend** (library, both builds). `miss`'s return after a hot restore
whose entry was reclaimed since `hotGone` (`cuckoo.go`; the history placed,
or waiting, with this touch's push) is `missPlaced` (-2) in place of -1, and
`touch` then looks again with `pushed` set (both call sites, `touch.go`): the
look skips the push into a history at or after the touch's tick, as W4d's
rule does after the table's push. The lost race (`ci == -1`: the recalled
words put back as they were, no push placed) still returns -1 and leaves
`pushed` as it was; setting it there would skip a push at `now <= last` into
a history that does not hold it, the tick test's blind side (W8's
`"noHandoffCheck"`). Where the history waits (wait room, stash) rather than
going in, the look's `hotGone` finds the entry gone and starts the key
afresh, which does not read `pushed`. In the default build `pushed` moves
nothing (a touch at or behind the last touch leaves word0 as it is there).

**The test** (`go/drip_hot_test.go`, `tfdrip`, plain mode,
`TestDripHotRestorePushOnce`): W9's four legs as assertions. In the reclaim
legs, the reclaim happens inside K's claim and K's touch looks again
through `bump`'s hot add on K's slot; then K's log holds one push, its slot
a register, its last touch tK, and K reads Count 1, the truth, at 250 ms,
1 s and 10 s, 100 ms after its touch. The controls restore K with its entry
and one push. The mend and the test were compiled in both builds and not
run; `lab/w4e.sh`'s new steps (clauses 1 and 2) were tried on a scratch
clone with the tree's own `TestHotEntryReclaimedDuringRestore` in the
test's place (exit 2 on a tree not committed); its other steps are
`lab/w4.sh`'s and `lab/w4d.sh`'s as read. Outputs in `lab/w4e/`.

**Rule (before the reading).** W4e holds if:

1. sight: the test, copied onto W4e's parent (W4d's library, b7a33a7's),
   fails, both reclaim legs' logs holding two pushes, and their Count 100 ms
   after the touch reading 2 at 250 ms and 1 s (the hand trace's
   prediction); the controls pass. If both reclaim legs read Count 1 there,
   the Count check cannot see the defect, and clause 2's Count half is not
   judged;
2. the mend: on W4e's tree the test passes;
3. the default build is unchanged: `tfbench -exact` and the answer digest
   equal `verify/expected/`'s;
4. with `tfdrip`, `tfbench -exact` equals W4d's reading's
   (`lab/w4d/exact-drip.txt`) byte for byte: single-threaded runs never take
   the path;
5. no regression with `tfdrip`, W4d's clause 5 as read: `TestDripPushOnce`
   0 keys in 5,000 rounds and in 500 under `-race`;
   `TestBackgroundConcurrentCounts` 0 of 200 runs and 0 of 20 under `-race`;
   the suites in four modes fail only what W4d's failed (TestConformance,
   TestUnitOfMatchesDivision, TestCountsLastTouchWhole; TestMerge on its
   1 s bar alone), with `TestDripHotRestorePushOnce` passing plain and with
   `TF_NOCAS2` (skipped under `TF_CT` and `TF_BG`); `TestSlotUnitRaces`,
   the soak and the concurrent fuzzer under `-race` pass.

If clause 1 fails (the test passes on W4d's library), the test is blind,
and W4e says nothing of the mend. If clause 2 fails, the mend is wrong. If
clause 3 or 4 fails, the mend moved answers off the path; diagnosed before
anything else. Not covered, as in W9: a restore whose claim evicts;
ConstantTime, Background and the wait room's leg; a reclaim by a concurrent
caller.

### W4e's reading (ae28817's tree; `lab/w4e.sh`, outputs in `lab/w4e/`)

1. **Holds.** On the parent (23f03b6, W4d's library), the test fails both
   reclaim legs and passes both controls. In each reclaim leg the reclaim
   happened inside K's claim (`testHookInstalling`; `testHookCopied`) and
   K's touch looked again through `bump`'s hot add on K's slot; K's log
   holds two pushes (0xffe against one push's 0xfff), and 100 ms after the
   touch K reads Count 2 at 250 ms and 1 s against a truth of 1 (1 at
   10 s), as the hand trace predicted. W9's double push moves Count: one
   high at windows that start inside the touch's bucket.
2. **Holds.** On W4e's tree the test passes: both reclaim legs take the
   same path (the hook, Promoted 2, the look again through `bump`), and K's
   log holds one push (0xfff), its slot a register, its last touch tK;
   Count 1 at all three windows.
3. **Holds.** Default build: `tfbench -exact` and the answer digest equal
   `verify/expected/`'s.
4. **Holds.** With `tfdrip`, `tfbench -exact` is W4d's reading's byte for
   byte.
5. **Holds.** `TestDripPushOnce` 0 of 160,000 keys in 5,000 rounds, 0 of
   16,000 in 500 under `-race`; `TestBackgroundConcurrentCounts` 0 of 200
   runs, 0 of 20 under `-race`; the suites fail what W4d's failed and no
   more: TestConformance, TestUnitOfMatchesDivision and
   TestCountsLastTouchWhole in every mode, TestMerge plain and with
   `TF_NOCAS2` on its 1 s bar alone (merged count error 50,202 over 1.5
   times the single filter's 16,537, W4d's numbers); `TestDripHotRestorePushOnce`
   is not among the failures plain or with `TF_NOCAS2`, where it runs (the
   suites' output lists failures only); `TestSlotUnitRaces` plain and under
   `-race`, and the soak and the concurrent fuzzer under `-race` (5 minutes
   each), pass.

**Verdict: W4e passes its rule.** A hot restore whose entry is reclaimed
after `hotGone` pushes its touch's event once, and K's Count read after it
is the truth where W4d's read one high. Of what W4d's reading measured, W4e
re-read clause 1 (the default build), the `tfbench -exact` part of clause
2 and clause 5, and none moved. W4d's clauses 3, 4 and 6 to 9 (the digest
without Count, Upper, speed, the periodic and sustained workloads, batched
throughput) were not re-read as clauses; that single-threaded runs never
take the path is an argument, not a reading. What W4e leaves, as W9 does: a
restore whose claim evicts; ConstantTime and Background; the wait room's
leg (below); a reclaim by a concurrent caller.

**The wait room's leg, worded again** (from the code, not a reading). W8's
audit note and W9's "Not probed" said that when the restored history waits
for a slot rather than going in, the retry pushes the event again in
`miss` (`cuckoo.go:59`). It does not: the retry recalls the waiting entry,
whose word1 is the dead pointer, `hotGone` finds the entry gone
(`cuckoo.go:33`), and `miss` starts the key afresh (`dripFresh`), as W4e's
mend paragraph says. That leg drops the waiting history rather than pushing
twice (claims A25: the restoring touch "went nowhere"); neither the drop nor
the absence of a second push is probed.

## Phase 10: Count and the drip log checked under concurrency (W10)

**Written after W4e's reading.** W4d's suite ran W9's path with `tfdrip` and
passed: nothing it runs asserts the drip log, and nothing under concurrency
asserts that an answer is not too high. The soak (`TestStressSoak`) and the
concurrent fuzzer (`TestFuzzConcurrent`) check one direction, Upper never
below a key's true count; the single-threaded fuzzer (`TestFuzzUpper`) also
checks the other (its `high`: Upper, the overflow table's part aside, counts
no event from further back than the window reaches by `reach`, 9 units and
the lag).

**The checks** (`go/countcheck_test.go`, and the soak's and the fuzzer's
changes; both builds), on keys with one writer (a goroutine's own keys; not
the fuzzer's shared keys, nor a straggler's), and off with tag amplification
(`TF_STRESS_TAGS`, `TF_CFUZZ_TAGS`), where a same-tag hand-over is made
common, which reads one high (claims A29):

- **high**, on every Count the soak makes and every Touch and Count the
  fuzzer checks: Upper, less the overflow table's part (the larger of two
  reads, before and after the call, since other goroutines fold histories
  meanwhile), is at most the key's events back to `now - min(window,
  horizon) - reach`. Count is never above Upper, so this bounds Count too;
  a Count one high shows only where Upper then reaches past those events, so
  a bucket of spread events can hide it. An answer over for a key whose
  identity another key of its goroutine shares is counted, not failed, as in
  `TestFuzzUpper`.
- **log**, at rest (the soak's end; the end of each fuzzer case), with
  `tfdrip`, for every key whose last event is within a horizon: taking the
  log's events newest first (the last touch, then one per code), the k
  newest lie at some quantum q or later, and the key must have had at least
  k events that could be logged there (`quantum(t + lag) >= q`: a touch
  behind the last touch pushes at the last touch's quantum). The reference
  is the key's own events, not its register: a register reset (`unpoint`)
  keeps the log's older events while the register restarts at 1. A log that
  says nothing, or a key with no history, is not checked, and how many is
  reported.

The workers keep their events past the checks' reach (the soak trimmed them
at a horizon). Nothing new is drawn from the workers' random streams, so a
seed runs the same case.

**Sight.** `lab/w10_test.go` (copied into worktrees by `lab/w10.sh`, never in
the tree): W9's restore with the reclaim at install, a control, and the
control with the touch's event pushed a second time by hand; both checks
100 ms after the touch, against K's events. The checks were compiled on the
head, on W4d's tree (23f03b6) and on W4c's (0837f9f), in both builds, and not
run; `lab/w10.sh` was tried on a scratch clone with stand-in tests (exit 2 on
a tree not committed, 1 when a run prints no summary). Outputs in
`lab/w10/`.

**Rule (before the reading).** W10 holds if:

1. sight: on 23f03b6 (W4d's library) both checks flag the reclaim leg, and on
   the head neither does; on both, the control passes and the second push
   made by hand is flagged by both (the probe's own assertions);
2. on the head, the soak and the fuzzer, 5 minutes each, with `tfdrip` plain
   and under `-race` and in the default build, and the fuzzer with a
   straggler (`tfdrip`): no answer over and no log over; every existing
   check passing as before; no data race; every run printing its summary;
3. thinness: the answers `high` checks and the live keys' logs checked are
   reported against those left out; if fewer than half the live keys' logs
   are checked in a `tfdrip` run, the log check is thin there, and W10 says
   so without failing on it.

Reported, not judged: the head's soak and fuzzer files on W4c's tree
(0837f9f), `tfdrip`, the fuzzer for 5 minutes: whether either check finds
W4c's double push through `miss` under the fuzzer's concurrency (its rate
there unknown).

A failure is read against the code before anything else: a defect of the
filter, or of the check, said as such. An answer over by one whose trace is
claims A29's residual (a same-tag hand-over just after a count) is that
documented case.
