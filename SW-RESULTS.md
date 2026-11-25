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

### W10's reading (223ce81's tree; `lab/w10.sh`, outputs in `lab/w10/`)

`lab/w10.sh` ran end to end (15:30 to 16:11, Go 1.24.7) and exited 1, on
`cfuzz-drip`. Every run printed its summary; no run reported a data race.

1. **Holds.** On 23f03b6 (W4d's library) the reclaim leg's reclaim
   happened inside K's install (Promoted 2), K's log read 0xffe, and both
   checks flagged it: the log ("2 events logged at quantum 767 or later,
   the key had 1") and `high` at 250 ms and at 1 s (Upper 2 against 1
   event in the reach). On the head the same leg (Promoted 2) left 0xfff,
   its log was checked, and neither check flagged it. On both trees the
   control was checked and passed, and the push made by hand was flagged
   by both checks at both windows (`sight-23f03b6.txt`,
   `sight-223ce81.txt`).
2. **Fails as written: one answer over**, in the fuzzer with `tfdrip`
   (`cfuzz-drip.txt`): case 865 of 1,119, goroutine 2, a Count at a window
   of 6,576 ticks answered {Seen, Count 1, Upper 2, Degraded} for a key
   whose events are at 353,296,800 and 353,300,418, at now 353,311,113: 1
   event back to the reach's edge (353,300,366), and the overflow table's
   part read 0 before the call and after it. Read against the code below:
   a defect of the check, not of the filter. Otherwise clause 2 holds. No
   other answer was over and no log was over in any run. Every existing
   check passed; case 865's reported failure is `high`, its first (a case
   reports its first failure only).

   | run | ops | answers `high` checked | over | live keys' logs checked | log over |
   |---|---|---|---|---|---|
   | soak, `tfdrip` | 1,007,557,997 | 302,248,722 | 0 | 4,272 | 0 |
   | soak, `tfdrip`, `-race` | 51,232,745 | 15,372,055 | 0 | 4,271 | 0 |
   | soak, default | 1,237,170,693 | 371,142,658 | 0 | 0 (no log) | 0 |
   | fuzzer, `tfdrip` (1,119 cases) | 745,524,147 | 183,887,251 | **1** | 4,663,638 | 0 |
   | fuzzer, `tfdrip`, `-race` (107) | 75,636,140 | 18,675,764 | 0 | 280,587 | 0 |
   | fuzzer, `tfdrip`, straggler (1,099) | 731,712,615 | 166,542,958 | 0 | 4,034,960 | 0 |
   | fuzzer, default (1,206) | 792,496,700 | 195,483,141 | 0 | 0 (no log) | 0 |

3. **Holds.** `high`: the soak checks every count it makes; the fuzzer
   checks 95.1% to 95.2% of the answers it checks (`tfdrip`, `-race`,
   default) and 87.8% with the straggler. The rest are shared keys and the
   straggler's. Logs: the soak's 4,272 of 4,272 live keys (`tfdrip`) and
   4,271 of 4,271 (`-race`), none past the horizon. These fit the pool at
   1x capacity plus the heavy keys (at most 4,288) when the run ended; the
   default build's 16,560 fit the pool at 4x (at most 16,576). The pool's
   size cycles with wall time (`soak_test.go`). The fuzzer's 54.3%
   (4,663,638 of 8,580,869), 50.7% (280,587 of 553,731) and 53.4%
   (4,034,960 of 7,553,295), the rest saying nothing or absent. None is
   under half, so none is thin, but the fuzzer's are near half. Not
   reported by the summaries: a log over on a key whose identity another
   key of its worker shares is passed over uncounted, and the fuzzer does
   not count its keys past the horizon. The answers whose overflow part
   was taken out (a part the check cannot vouch for, below) are 22.3%,
   1.0% and 21.1% of the soak's checked answers and 21.6%, 23.9%, 21.1%
   and 21.0% of the fuzzer's.

**Reported** (`w4c-cfuzz.txt`). The head's soak and fuzzer files built and
vetted on 0837f9f (W4c's tree). In 5 minutes the fuzzer ran 1,082 cases,
717,684,556 ops, 177,009,453 answers `high`-checked and 4,590,381 logs
(3,857,316 saying nothing or absent), with no failing case. Neither check
saw W4c's double push through `miss` under the fuzzer's concurrency.
Neither has been shown to see a known double push under concurrency: the
only sight is clause 1's single-threaded probe.

**The failure, read against the code.** Count's answer from the key's slot
needs the key's last touch within a horizon of now (`count`, touch.go). The
key's last event is 10,695 ticks back, past the 10,000-tick horizon. So
that part is 0 (unless another goroutine's key shares its identity:
`sharesIdentity` looks only at the worker's own keys). Upper 2 and Count 1
came whole from `addSketch`, the overflow table, and Degraded (set there
only when the table's Count is above 0) says the table held an entry with
the key's fingerprint. That part is what the check sets aside: the table
counts a whole history while its newest event's slice (Horizon/2) is in the
window, and a floor adds to the Upper of a third of a bucket's keys
(ghost.go; `Result.Degraded`).

Case 865 was then replayed by hand (`TF_CFUZZ_CASE=865 TF_CFUZZ_RUNS=1`,
one run per process, in archives of 223ce81 outside the tree; scripts as
run from a scratch directory, the hook files, the patch and the outputs in
`lab/w10-diag/`, the test binaries left out). The scripts' `$SP` was that
scratch directory. `parts-hook.go` and `parts-hook_test.go` are the
`w10hook.go` and `w10parts_test.go` that `parts.sh` copies; `epochs-hook.go`
and `epochs-hook_test.go` are `epochs.sh`'s `w10hook3.go` and
`w10parts3_test.go`.

- **Unmodified** (`plain.sh`): 5 of 40 runs fail with `tfdrip`, 1 of 40 in
  the default build. The failures are a Count of Upper 1 against no event
  in the reach, and a fresh key's Touch of Upper 2 against its one event.
- **The part each call adds** (`parts.sh`, a hook in `addSketch`): 51 of 60
  runs fail with `tfdrip`, 48 of 60 in the default build. These are not
  rates: the hook takes a lock and formats a string on every call. In all
  99 failing answers, Upper less the part the call itself added is within
  the key's events in the reach. The excess is the overflow table's part,
  which the reads around the call saw lower (0 in most).
- **Why the reads around the call miss it** (`epochs.sh`, the table's
  epoch, `until` and both buckets' words at every read, and the shared
  clock; `tfdrip`, 29 of 40 runs fail; `classify.txt`):
  - In 28, the epoch moved on between the call's read and the read after
    it. When the failure was reported, the shared clock stood 5,040 to
    53,553 ticks past the call's now: the fuzzer moves it on by up to
    three horizons at random. Goroutines at the later clock had swept
    (`advance`) the entries or floors the call counted. At the read before
    the call the clock was 0 to 12,283 ticks past now, and that read held
    less of the part (none in 25 of the 29).
  - In the other one (`r-32`), the call's read and the read after it share
    an epoch but not the words: an add replaced an entry with the key's
    fingerprint (a key with no events) between them.
  - In none did the same words read differently. The failing parts of
    Count 0 and no Degraded are floors.

The sweep and the replace are recorded in these replays only. For the
original answer the mechanism is inferred: its part came from the table,
and the reads around it saw none. The table's part at a fixed now is not
stable while other goroutines run at later clocks. The larger of a read before the call and one after it
misses a part raised and swept within the call: the check's bracket, not
the filter's answer. Case 865 is a two-unit overflow table (full at once,
its floors rising all the time), Background, 17 goroutines. No other case
in any run failed, and case 865 (run once per run) passed in the W4c, the
straggler and the default-build runs.

Two consequences. First, the bracket errs the other way too: a part raised
after the call and read after it is taken out though the call did not
count it, which can hide a real over. So "no answer over" is loose on the
answers with an overflow part taken out (clause 3's shares). Second, in
case 865's run a log over at rest would not have been reported: a case
reports its first failure only.

**Verdict: W10 fails its rule as written, on clause 2:** one answer over,
case 865 of the fuzzer with `tfdrip`. Read against the code and three
replays, the over is the overflow table's part: the check means to set it
aside, and its reads before and after the call missed it. That is a defect
of the check, not of the filter. Clauses 1 and 3 hold: both checks see W9's
double push and a hand-made one single-threaded, and neither flags the
head. Under concurrency, no log over in any run, and no other answer over.
What W10 leaves: a `high` that takes the overflow part from the call itself
(a hook in `addSketch`, as `parts.sh` does), under its own rule; and sight
of a known double push under concurrency (W4c's, not seen in 5 minutes).

## W10b: the overflow table's part taken from the call

**Written after W10's reading**, which failed on one answer over in the
fuzzer (case 865), its excess the overflow table's part: the check took the
larger of two reads around the call, and goroutines at a jumped clock swept
or replaced the table's entries between them. That bracket can also err
the other way and hide a real over. W10 also left the checks without sight
of a known double push under concurrency, and a fuzzer case that stopped at
its first failure, so a log over behind an earlier failure went unreported.

**The library** (`go/touch.go`, both builds). `addSketch` calls
`testHookSketchPart(h, s.Upper)` when the hook is set, after the table's
estimate and before adding it to the answer: nil in the library, set only
by the tests below. Nothing else of the library changes. W10b's library
differs from 01e49a6's (W4e's) by these added lines alone: 10, one of them
blank. `lab/w10b.sh` checks that before it runs.

**The checks** (`go/countcheck_test.go`, and the soak's, the fuzzer's and
`TestDripConcurrentCounts`'s changes; both builds):

- **The part the call added.** Before a checked call, the check arms its
  key's cell in `partCells`, a table of 2^18 atomic words: the key's 48 high
  bits beside a 16-bit part, in the cell its 18 low bits pick. The hook
  writes the part into the cell by compare-and-swap only while the cell is
  armed for that key. After the call, `high` takes out the part read from
  the cell, in place of the larger of two reads around the call. The record
  takes no lock and allocates nothing. A cell another key armed meanwhile
  reads as taken over, and that answer is set apart and counted (`taken`),
  not checked.
- **A part over Upper is a failure.** A recorded part above the answer's
  Upper cannot be the call's part and would hide an over. It is counted as
  its own failure in the soak, the fuzzer and `TestDripConcurrentCounts`.
- **Every failure, every kind.** A fuzzer case goes on past a failure. It
  counts every failing answer and log by kind (`high`, `part over Upper`,
  `low`, `shared low`, `log`, and `occupancy` or `New` for the case) and
  keeps the first of each kind, formatted only then. A log over on a key
  whose identity another key of its goroutine shares is counted in the
  soak and the fuzzer, not passed over.
- **At rest under W7's scenario.** `TestDripConcurrentCounts` also reads
  each key at rest against its own events: its drip log (`logHigh`), and
  the Upper of every answer it already reads, at nine windows (`countHigh`,
  with the part as the call added it). A key whose fingerprint and tag
  another key of the round shares is counted, not failed. Its summary line
  is "W10b at rest".

The summaries read "W10b: " (the soak, the fuzzer) and "W10b at rest"
(`TestDripConcurrentCounts`). The soak and the fuzzer draw nothing new
from their random streams, so a seed runs the same case.

**Sight.**

- **The cells alone.** `TestPartCells`, with no filter, checks that a cell:
  - reads its key's record, or 0 when armed and not recorded;
  - ignores another key's record and saturates;
  - reads as taken over once another key with the same 18 low bits arms it.
  It also checks that, from 16 goroutines recording their own keys, every
  read is the goroutine's own.
- **The probe** (`lab/w10b_test.go`, copied into worktrees by
  `lab/w10b.sh`, never in the tree):
  - `TestW10bSight` is W10's probe (W9's reclaim at install, a control, the
    control pushed again by hand) with the part taken from the call.
  - `TestW10bSightOverflow` gives the check a key with a real overflow
    part, which W9's legs have none of. K's 3 events at t0 = 60,000 are
    spilled to the overflow table and K's slot freed, and K is touched
    afresh at tK = 88,900. At now = 89,000 the table's entry counts K's 3
    events whole in the 250 ms and 1 s windows, while those events are past
    the windows' reach (22,501 ticks). With the part taken out, Upper is
    K's one event at tK. The probe asserts that the record equals the part
    read from the table alone (above 0) and that neither check flags K.
    With K's touch at tK made twice and the truth holding it once, both
    checks must flag K at both windows: a real over is not hidden by the
    part.
- **Case 865.** `TF_CFUZZ_CASE=865 TF_CFUZZ_RUNS=1`, one process a run, 200
  runs per build, on the head and on 01e49a6 (W10's check, the control).
  W10's replays of the unmodified head failed 5 of 40 runs with `tfdrip`
  and 1 of 40 in the default build (`lab/w10-diag/`).
- **Under concurrency.** `TestDripConcurrentCounts`, Background, spread,
  20,000 rounds, `tfdrip`, on 0837f9f (W4c's tree, with the hook and the
  head's test files) and on the head. In this scenario W7b's reading
  counted 46 Count-high answers on 0837f9f (its clause 2), from W4c's
  double push through `miss`, and its outputs (`lab/w7b/progress.txt`)
  show those keys' logs differing from the shadow's (0xdde against
  0xeef). A round there spans about 16,000 ticks, inside the reach
  (22,501), so at rest `high` can flag only an Upper above the key's total
  events: its pass there says no more than that.

The record is sound only if a checked `TouchAt` or `CountAt` reaches
`addSketch` at most once and returns the answer it added to. Read from the
code: `count` adds the table's part last and returns; `touch` does so on
each of its three returns (the two misses and the hit), after its retries;
`touchCT` is `touch` padded.

**Before the rule** (not a reading). The code compiles in both builds on
the head, on 23f03b6 and 0837f9f (with the hook applied as a patch and the
head's test files) and on 01e49a6. `TestPartCells` was run on a scratch
clone, plain and under `-race`, and passes: it reads no filter. `lab/w10b.sh`
was tried on a scratch clone with `TestPartCells` in every test's place and
short durations. It exited 2 on a change not committed. On the stand-in run
it checked the library against 01e49a6's ("[go/touch.go]", 0 lines removed,
9 added, as the runner counts lines that are not blank), applied the hook to 23f03b6 and 0837f9f and found each tree's
change the hook's, logged "NO SUMMARY" for every judged run, and exited 1.
None of `TestW10bSight`, `TestW10bSightOverflow`, case 865,
`TestDripConcurrentCounts` or the soak and the fuzzer has run on W10b's
code. Outputs in `lab/w10b/`; about 55 minutes.

**Rule (before the reading).** W10b holds if:

1. sight, single-threaded: `TestPartCells` passes on the head. On 23f03b6
   (W4d's library, with the hook), `TestW10bSight`'s reclaim leg is flagged
   by both checks (the log, and `high` at 250 ms and at 1 s). On the head
   it is flagged by neither, its log checked. On both, the control passes
   and the push made by hand is flagged by both checks at both windows.
   `TestW10bSightOverflow` passes on both, as the probe asserts. The
   reclaim leg's flags are read from the probe's lines, which the probe
   does not assert;
2. case 865: on the head it fails in none of 200 runs in each build, every
   run passing with its "W10b: " line. On 01e49a6 (the control) it fails in
   at least one of 200 runs with `tfdrip`. If the control fails in none,
   the case no longer reaches what W10 read, and clause 2 is not judged;
3. sight under concurrency: on 0837f9f the log check at rest flags at least
   one key in 20,000 rounds. On the head, no log over, no Upper over and no
   part over Upper. Each run prints its "W10b at rest" line. The control: if
   W7b's own line on 0837f9f counts no Count-high answer, W4c's double push
   did not occur in the run, and the 0837f9f half is not judged. The log
   overs are reported beside that count;
4. the library: the default build's `tfbench -exact` and answer digest
   equal `verify/expected/`'s, and `tfdrip`'s `tfbench -exact` equals
   `lab/w4e/exact-drip.txt` byte for byte;
5. W10's runs on the head, 5 minutes each: the soak and the fuzzer with
   `tfdrip` plain and under `-race` and in the default build, and the
   fuzzer with a straggler (`tfdrip`). No answer over, no part over Upper,
   no log over. The existing checks (`low`, `shared low`, occupancy) pass as
   before. No data race, and every run prints its "W10b: " line;
6. thinness, reported without failing: as W10's clause 3, and the answers
   set apart because their record was taken over, against those checked.
   If the logs checked are under half the live keys in a `tfdrip` run, or
   the answers set apart over 0.1% of those checked, the check is thin
   there, and W10b says so.

Reported, not judged:
- the fuzzer on 23f03b6 (W4d's library, with the hook), `tfdrip`, 5
  minutes: whether either check sees W9's double push under the fuzzer's
  concurrency, where a reclaim comes from a concurrent caller (W4e's open
  leg);
- W7b's own lines on the head (W7b was read on W4d's library);
- each run's failures by kind.

A failure is read against the code before anything else: a defect of the
filter, or of the check, said as such. An answer over by one whose trace is
claims A29's residual (a same-tag hand-over just after a count) is that
documented case. If clause 1 fails, the checks as W10b has them are blind
or the probe is at fault, and W10b says nothing of clauses 2 to 6 until
that is read.

### W10b's reading (a66af40's tree; `lab/w10b.sh`, outputs in `lab/w10b/`)

`lab/w10b.sh` ran end to end (03:16 to 04:02, Go 1.24.7) and exited 0.
Its first check found the head's library different from 01e49a6's in
`go/touch.go` alone, by added lines only, and the hook applied to 23f03b6
and 0837f9f changed each tree by the hook's lines alone. Every judged run
printed its summary, and no run reported a data race.

1. **Holds.** `TestPartCells` passes on the head.
   - **On 23f03b6** (W4d's library, with the hook): the reclaim leg's
     reclaim happened inside K's install (Promoted 2), and both checks
     flagged it. The log read 0xffe ("2 events logged at quantum 767 or
     later, the key had 1"), and `high` read Upper 2 against 1 event at
     250 ms and at 1 s.
   - **On the head:** the same leg left 0xfff, its log was checked, and
     neither check flagged it.
   - **On both trees:** the control passed, and the push made by hand was
     flagged by both checks at both windows. `TestW10bSightOverflow`
     passed. With K's 3 events in the overflow table, the part the call
     added read 3 at both windows, as the table read alone does, and
     neither check flagged K. With K's touch counted twice, the log read
     0xffe and `high` read Upper 5, a part of 3, against 1 event: both
     flagged K at both windows (`sight-s-w4d.txt`, `sight-s-head.txt`).
2. **Holds.** Case 865, 200 single runs each (counts in `progress.txt`; the
   control's failing runs in `control-drip/` and `control-default/`; the
   runner keeps no passing run). On the head it failed in none, with
   `tfdrip` and in the default build, and every run passed with its
   "W10b: " line. On 01e49a6 (W10's check, the
   control) it failed in 11 of 200 with `tfdrip` (Touch Upper 2 against 1
   event 8 times; Count Upper 1 against none twice; Count Upper 2 against 1
   once) and in 11 of 200 in the default build (Touch Upper 2 against 1 six
   times, Upper 6 against 5 once; Count Upper 1 against none four times),
   W10's bracket reading the overflow table's part as 0 in every one. The
   control was met.
3. **Holds.** `TestDripConcurrentCounts`, Background, spread, 20,000 rounds
   (`conc-w4c.txt`, `conc-head.txt`).
   - **On 0837f9f:** W7b's own line counts 3 Count-high answers, so the
     control was met. The log check at rest flagged 4 of 639,915 keys' logs
     (85 saying nothing). Three of them are the keys W7b's comparison found
     Count high (round 3463 key 8, round 11922 key 22 and round 17051 key
     6, each at 2 s; logs 0x9f5, 0xe7f and 0xcef against the shadow's
     0xdf5, 0xf7f and 0xeef). The fourth (round 19123 key 1, log 0xf3f, 3
     events logged at quantum 85 or later against 2) is one the comparison
     did not count; whether its log differs from the shadow's is not in the
     outputs. No Upper over, and no part over Upper.
   - **On the head:** 639,921 logs checked (79 saying nothing), and no log
     over, no Upper over and no part over Upper in 5,760,000 answers.
     W7b's own line on the head counts no answer different from the
     shadow's (reported).
   - **Beside the earlier readings:** in this scenario on 0837f9f, W7
     counted 29 Count-high answers and W7b 46; this run counts 3, all at
     2 s (its differences by window, `conc-w4c.txt`). Why it counts fewer
     was not read. At rest, `high` checked 5,760,000 answers on each tree (873
     and 819 with a part taken out). In W7's scenario its pass says only
     that no Upper is above the key's total (the rule's note).
4. **Holds.** The default build's `tfbench -exact` and answer digest equal
   `verify/expected/`'s, and `tfdrip`'s `tfbench -exact` equals
   `lab/w4e/exact-drip.txt` byte for byte.
5. **Holds.** No answer over, no part over Upper and no log over in any of
   the seven runs. In every run the existing checks failed nothing
   (`low`, `shared low`, occupancy: 0 by kind in the fuzzer's), and every
   run printed its "W10b: " line.

   | run | ops | answers `high` checked | part taken out | over | logs checked | log over |
   |---|---|---|---|---|---|---|
   | soak, `tfdrip` | 1,009,351,308 | 302,786,102 | 69,938,979 | 0 | 5,122 | 0 |
   | soak, `tfdrip`, `-race` | 49,891,187 | 14,969,920 | 127,342 | 0 | 4,919 | 0 |
   | soak, default | 1,274,215,920 | 382,255,936 | 84,069,234 | 0 | 0 (no log) | 0 |
   | fuzzer, `tfdrip` (1,147 cases) | 760,399,970 | 187,555,842 | 40,012,757 | 0 | 4,764,718 | 0 |
   | fuzzer, `tfdrip`, `-race` (103) | 72,104,851 | 17,804,887 | 4,376,151 | 0 | 277,690 | 0 |
   | fuzzer, `tfdrip`, straggler (1,068) | 714,094,997 | 162,535,475 | 34,013,815 | 0 | 3,984,134 | 0 |
   | fuzzer, default (1,239) | 813,309,702 | 200,617,229 | 42,018,750 | 0 | 0 (no log) | 0 |

6. **Not thin.**
   - **Logs:** the logs checked are 61.2% of the soak's live keys
     (`tfdrip`; 3,254 saying nothing) and 99.0% under `-race`. The
     `-race` run also left out 5,527 keys past the horizon, where W10's
     left none. The 8,376 live keys of the `tfdrip` soak (and of the
     default one) fit the pool at 2x capacity plus the heavy keys (at most
     8,384) when the run ended; W10's runs ended at 1x. The filter holds at
     most 5,124 keys with a log (4,820 slots and a stash of 304), and the
     5,122 checked fit every key it held: the 3,254 others had left the
     table and the stash. The fuzzer's are 54.3%, 50.5% and 53.3%: over half, though
     near it in the fuzzer.
   - **Answers set apart:** the answers set apart because their record was
     taken over are 0.0004% to 0.0016% of those checked (101 to 2,513 a
     run), far under the rule's 0.1%.
   - **`high`'s share:** in the fuzzer `high` checks 95.1% to 95.2% of the
     answers it checks (`tfdrip`, `-race`, default) and 87.8% with the
     straggler. An overflow part was taken out of 0.9% to 24.6% of the
     answers `high` checked.

**Reported** (`w4d-cfuzz.txt`). The fuzzer on 23f03b6 (W4d's library, with
the hook), `tfdrip`, 5 minutes: 1,143 cases, 757,515,691 ops, 186,842,342
answers `high`-checked and 4,794,401 logs (54.7%), with no failing case.
Neither check saw W9's double push under the fuzzer's concurrency. The
fuzzer counts no restore whose entry a concurrent caller reclaimed, so
whether the path was taken is not known.

**Verdict: W10b passes its rule.** With the overflow table's part taken
from the call itself, case 865 no longer fails: none of 400 runs on the
head fail, against 22 of 400 on W10's check in the same reading.

What bears on a record that takes out too much:
- Read: the probe's overflow legs show the record equal to the table's
  part, single-threaded, in one configuration.
- Read: no part was above its answer's Upper in any run. That bounds a
  wrong record; it does not show a right one.
- Argued from the code, not read: under concurrency the hook is given the
  same `s.Upper` that `addSketch` adds, once per call, for a key only its
  owner calls.

Under concurrency, the log check at rest sees W4c's double push in W7's
Background scenario. That is the first sight of a known double push under
concurrency by these checks. It is in that scenario at rest, not in the
soak or the fuzzer.

With the hook, the default build's single-threaded `tfbench -exact` and
answer digest, and `tfdrip`'s `tfbench -exact`, are unchanged.

What W10b leaves:
- `high` under W7's scenario cannot reach past a key's total, so its
  concurrent sight is the soak's and the fuzzer's alone. Those have not
  seen a known double push: W4c's in W10's 5 minutes, and W9's in this 5
  minutes on 23f03b6.
- W4e's open legs, as before: a restore whose claim evicts; ConstantTime
  and Background reclaims; a reclaim by a concurrent caller; the wait
  room's drop.

## Phase 11: the hot restore's open legs (W11)

**Written after W10b's reading.** W4e mended the hot restore's double push
on the legs W9 probed: in plain mode, a reclaim at the install into a free
slot and in a claim that relocates. Not probed, as W4e's reading left them:

- a restore whose claim evicts;
- ConstantTime and Background, whose claims install on the call path only
  into a free slot and otherwise let the history wait (in the stash; in
  the wait room, where the placer places it);
- the wait's leg, which W4e's wording (b187156) read from the code: the
  look again recalls the waiting history, `hotGone` finds its entry gone,
  and the key starts afresh, dropping the history with no second push;
- a reclaim by a concurrent caller rather than a hook.

**The library** (`go/hot.go`, both builds). `hotAdd` calls
`testHookRestoreAdd()` when the hook is set and it counts a hot restore's
touch (`slot` nil: `miss`'s add, after the history went in or waits with the
touch's push), before the add. Nil in the library. No hook in the tree runs
between the wait and that add. W11's library differs from 8a15270's by these
added lines alone. `go/hot.go` is the same on 23f03b6 (W4d's library), and
the hook applies to it as a patch; `lab/w11.sh` checks both before it runs.

**The probe** (`go/drip_hotlegs_test.go`, `tfdrip`, in the tree).
`TestDripHotRestoreLegs` builds W4e's test's filter (one hot entry, Horizon
1 minute at 1 ms): K promoted at t0, its words in the stash, its slot freed.
L, on other lines than K's and another claim stripe, is promoted at tL,
more than a horizon past K's newest event, which reclaims K's entry. K's
restoring touch is at tK. The fill keys are on other stripes than K's,
since a hook that touches L inside K's claim must not take K's stripe. The
legs, each with a control that reclaims nothing:

1. plain, every line full of keys live at tK: the claim cannot relocate and
   evicts; L's promotion runs from `testHookVictim`;
2. ConstantTime, K's slot free: L's from `testHookInstalling`;
3. ConstantTime, K's lines full: the history waits in the stash; L's from
   `testHookRestoreAdd`. K holds its stripe, so the pads cannot place it,
   and the look again recalls it;
4. Background, K's slot free: L's from `testHookInstalling`;
5. Background, K's lines full, the placer running: from
   `testHookRestoreAdd` the test waits (up to 2 s) for the placer to place
   K, then runs L's promotion; the look again finds K in the table;
6. Background, K's lines full, no placer (`bgManual`): the look again takes
   K out of the wait room (`testHookTaken`).

K's drip log after its touch is classed against W4e's history w0: "once"
(the history with the touch's one push), "twice" (two pushes) or "fresh"
(the touch afresh, the history dropped). K is also read 100 ms after its
touch at 250 ms, 1 s and 10 s, where the truth is 1. The read at tK at 60
s is logged in full and reported only: K's events in it are its 32 at t0
and the touch, and the look again's fresh register reads about 1 in every
reclaim leg.

Each leg checks first that its path was taken, and fails with "path not
taken" otherwise:
- the hook fired and L was promoted;
- with every line full, the restore evicted;
- with K's lines full, the history waited;
- with the placer, K was placed;
- where the look again finds K in the table, it went through `bump`'s hot
  add on K's slot (`testHookHotAdding`);
- without the placer, K was taken out of the wait room.

Its assertions are W4e's library's:
- a control restores K with its entry and one push;
- legs 1, 2, 4 and 5 read once, K's slot no longer a hot pointer;
- legs 3 and 6 read fresh;
- every reclaim leg reads Count 1 at the three windows.

`TestDripHotRestoreRace` (`TF_DRIP_RACE_ROUNDS`) is the concurrent leg. Each
round builds the filter with K's slot free (plain mode), touches L 31 times
at tL and checks that L is not yet promoted and that K's entry is still
K's. Then K's touch at tK and L's 32nd touch, its promotion, run from two
goroutines, each after a random spin of up to 4,000 loop steps. A round is
classed by K's log, whether L was promoted, and whether K's touch looked
again through `bump`'s hot add, which in a round only K's look again
makes. There are three orders:
- L's reclaim first: K starts afresh;
- K's add first: its entry then holds an event too new for L to reclaim,
  and L is refused;
- L's reclaim between K's `hotGone` and its add: K looks again.

**Predictions, from the code before any run.** On 23f03b6 (W4d's library,
with the hook):
- legs 1, 2, 4 and 5 read twice: the look again finds K in the table with
  `pushed` false and pushes again;
- legs 3 and 6 read fresh: the recall finds the entry gone and starts K
  afresh, on either library;
- the race reads twice in every round where K looked again.

If leg 5 reads twice there, that is a double push on W4d's library through
the wait room and the placer, which W4e's wording of the wait's leg did not
cover; it is recorded as such. On the head every leg reads as its
assertions say, and no race round reads twice.

**Before the rule** (not a reading). The hook, the probe and the runner were
compiled in both builds on the head (and the `-race` build), and with the
hook applied on 23f03b6 (`tfdrip`). `lab/w11.sh` was tried on a scratch
clone with `TestPartCells` in every test's place. It exited 2 on a change
not committed. On the stand-in run it found the library different from
8a15270's in `go/hot.go` alone, by 7 lines that are not blank (none
removed), applied the hook to 23f03b6 and found that tree's change the
hook's, logged "NO W11 LINES" for every run, and exited 1. None of W11's
tests has run. Outputs in `lab/w11/`. The test runs are bounded (2 minutes
for the legs, 30 minutes for a race run). The race's rounds were not timed
before the rule.

**Rule (before the reading).** W11 holds if:

1. paths: on both trees every leg takes its path (no "path not taken"). A
   leg that does not is the probe's fault, and W11 says nothing of that
   leg. A hang or a timeout is read the same way;
2. sight, 23f03b6: legs 1, 2, 4 and 5 read twice, legs 3 and 6 read fresh,
   and every control reads once with K's entry. A reclaim leg among 1, 2, 4
   and 5 that reads once there has no sight: its head reading is reported,
   not judged as mended;
3. the head: every leg passes its assertions, plain and under `-race`, with
   no data race. W4e's `TestDripHotRestorePushOnce` passes beside it;
4. the race: on the head, no round reads twice in 200,000 rounds, nor in
   20,000 under `-race` with no data race. The head's pass counts only if
   at least 10 rounds looked again; with fewer, the window was not reached
   often enough and the leg is not judged. On 23f03b6, at least one round
   reads twice in 200,000. If none does, the race has no sight and the
   head's pass is reported, not judged as mended;
5. the library: the default build's `tfbench -exact` and answer digest
   equal `verify/expected/`'s, and `tfdrip`'s `tfbench -exact` equals
   `lab/w4e/exact-drip.txt` byte for byte.

Reported, not judged: each leg's counts (restored, evictions, relocations,
waited), where K is after its touch, the 60 s read; the race's classes and
their counts.

A failure is read against the code before anything else: a defect of the
library, or of the probe, said as such.

Not covered:
- the default build (W9 read its path);
- a look again that meets the placer mid-placement (the wait-room entry
  busy);
- ConstantTime's pads placing K between the wait and the look again (K's
  stripe is held throughout);
- relocation under ConstantTime and Background (their claims don't search
  on the call path);
- the hook's cost on `hotAdd`'s path, not measured here.

`TestDripHotRestoreLegs` runs in every `tfdrip` suite from W11 on.

### W11's reading (d38270e's tree; `lab/w11.sh`, outputs in `lab/w11/`)

`lab/w11.sh` ran end to end (05:36 to 05:38, Go 1.24.7) and exited 0:
every run printed its W11 lines. Its first check found the head's library
different from 8a15270's in `go/hot.go` alone, by 7 added lines and none
removed, and the hook applied to 23f03b6 changed that tree by the hook's
lines alone. The tests' own exits: the legs' test failed on 23f03b6 (where
W4d's library pushes twice, by design, and in legs 3 and 6, below) and on
the head plain and under `-race` (legs 3 and 6); the race failed on
23f03b6 and passed on the head plain and under `-race`. No run reported a
data race. W4e's `TestDripHotRestorePushOnce` ran on the head only: its
file is W4e's, not 23f03b6's.

1. **Holds.** Every leg took its path, on both trees and on the head under
   `-race`: no leg stopped on a path check, and each line's fields show it.
   - Leg 1: the hook at `testHookVictim`, Promoted 2, one eviction, the
     look again through `bump`'s hot add on K's slot.
   - Legs 2 and 4: `testHookInstalling`, Promoted 2, the look again
     through `bump`.
   - Leg 3: `testHookRestoreAdd`, Promoted 2, waited 2 (the restore's wait
     in the stash, and the look again's).
   - Leg 5: `testHookRestoreAdd`, Promoted 2, K placed by the placer (one
     relocation), waited 1, the look again through `bump`.
   - Leg 6: `testHookRestoreAdd`, Promoted 2, waited 2, K taken out of the
     wait room once.
   - The controls: none reclaimed (Promoted 1); leg 1's evicted once, leg
     3's and leg 6's waited, leg 5's waited and was placed.
2. **Fails as written, on legs 3 and 6** (`legs-w4d.txt`). On 23f03b6
   (W4d's library, with the hook):
   - legs 1, 2, 4 and 5 read twice: K's log 0xffe against one push's
     0xfff, its slot a register, its last touch tK, and 100 ms after the
     touch Count 2 at 250 ms and 1 s (1 at 10 s) against a truth of 1;
   - legs 3 and 6 read once (0xfff), where the rule says fresh;
   - every control reads once with K's entry (word1 a hot pointer to K's
     entry, Restored 1).

   Leg 5's twice is a double push on W4d's library through the wait room
   and the placer: K's history waited with the touch's push, the placer
   placed it, the reclaim came after, and the look again found K in the
   table and pushed again. b187156's wording of the wait's leg (the look
   again recalls the waiting history and starts K afresh) did not cover a
   history the placer places before the look again.
3. **Fails as written, on legs 3 and 6** (`legs-head.txt`,
   `legs-head-race.txt`). On the head, plain and under `-race`, legs 1, 2,
   4 and 5 and the six controls pass their assertions: each reclaim leg
   reads once (0xfff), its slot no longer a hot pointer, and Count 1 at the
   three windows. Legs 3 and 6 fail theirs: K's log reads 0xfff, classed
   once, where they assert fresh. The two runs' lines are the same, times
   aside, and neither reported a data race. `TestDripHotRestorePushOnce`
   passes beside it, plain and under `-race`, its four legs as W4e's
   reading read them.
4. **Holds** (`race-w4d.txt`, `race-head.txt`, `race-head-race.txt`). On
   the head no round reads twice in 200,000 (134 looked again) nor in
   20,000 under `-race` (27 looked again), with no data race; both are over
   the 10 the rule asks. On 23f03b6, 141 of 200,000 rounds read twice:
   every round that looked again, and no other.

   | run | rounds | once, L refused | once, L promoted, no look again | looked again, once | looked again, twice |
   |---|---|---|---|---|---|
   | 23f03b6 | 200,000 | 2,533 | 197,326 | 0 | 141 |
   | head | 200,000 | 2,382 | 197,484 | 134 | 0 |
   | head, `-race` | 20,000 | 9,661 | 10,312 | 27 | 0 |

5. **Holds.** The default build's `tfbench -exact` and answer digest equal
   `verify/expected/`'s, and `tfdrip`'s `tfbench -exact` equals
   `lab/w4e/exact-drip.txt` byte for byte.

**Legs 3 and 6, read against the code: a defect of the probe.**
`dripClass` names K's log once if it equals W4e's history with the touch
pushed once at tK, and tries fresh only after. At tK = t0 + horizon - 1 the
gap from the history's last touch (t0) is far past the log's 11 quanta,
and `next0` then sets the log all ones and valid, 0xfff ("no event within
the field's reach", W4c): `dripFresh`'s log. At tK, once and fresh are one
log, so no library could read fresh in this probe. A second push at tK is
still another log: `next0` at the last touch shifts a zero code in, 0xffe,
onto either. Legs 3 and 6's 0xfff on both trees says that K's log holds the
touch's event once, not whether the history was kept or dropped. Legs 1,
2, 4 and 5 and clause 4 judge twice against not twice, which this does not
touch. Neither tree was predicted to push twice in legs 3 and 6, so they
have no sight of a double push either.

From the code, not a reading: K's other fields in legs 3 and 6 fit the
fresh start b187156 worded. The look again's `miss` finds the entry gone
(`hotGone`), takes a fresh register (word1 no longer hot) with
`dripFresh(tK)`, finds no free slot and waits again (waited 2), and counts
no restore (Restored 0; a waiting history counts none either). The 60 s
read below gives K's 32 events at t0 to no reclaim leg, but on the head
legs 1, 2, 4 and 5 lose them too: `bump` starts a fresh register on the
dead pointer (`unpoint`) and keeps the log.

**Reported.**
- The read at tK at 60 s: Count 33, Upper 33 in every control on both
  trees (K's 32 events at t0 and the touch); Count 1, Upper 1 in every
  reclaim leg on both trees.
- Where K is after its touch: in the table in legs 1, 2, 4 and 5 and their
  controls; in the stash or the wait room in legs 3 and 6 and their
  controls.
- Evictions, relocations, waits: leg 1 and its control evicted once; leg 5
  relocated once (the placer's placement), its control not; legs 3 and 6
  waited twice, leg 5 and the full-lines controls once; nothing else
  evicted, relocated or waited.
- The race's classes: the order the rule names "L's reclaim first" is
  "once, L promoted, no look again", where the probe predicted fresh; its
  log, as above, is once's as well. Under `-race`, K first (L refused) took
  48% of the rounds, against 1.2% and 1.3% plain. A race run of 200,000
  rounds took 12.5 s (23f03b6) and 12.8 s (head), and 20,000 under `-race`
  6.0 s, far inside the 30-minute caps; the whole run took 2 minutes 22
  seconds.

**Verdict: W11 fails its rule as written, on clauses 2 and 3, in legs 3
and 6 alone:** they read once where the rule says fresh, on both trees.
Read against the code, that is a defect of the probe: at tK, the touch
afresh and the history with the touch pushed once leave one log, 0xfff.
Clauses 1, 4 and 5 hold, and legs 1, 2, 4 and 5 hold clauses 2 and 3.

What W11 reads of W4e's library:
- Read: a hot restore whose entry is reclaimed after `hotGone` pushes its
  touch's event once where W4d's library pushes it twice: in a restore
  whose claim evicts (plain), in ConstantTime's and Background's install
  into a free slot, and in Background's wait room when the placer places K
  before the look again. K reads Count 1 there, where W4d's read 2 at
  250 ms and 1 s.
- Read: with K's history waiting (ConstantTime's stash; Background's wait
  room with no placer), K's log holds the touch's event once on both
  libraries.
- Read: a reclaim by a concurrent caller. W4d's library pushed twice in 141
  of 200,000 rounds, every one that looked again; W4e's in none of 200,000
  (134 looked again), nor of 20,000 under `-race` (27).
- Read, on W4d's library: a double push through the wait room and the
  placer (leg 5), which b187156's wording did not cover.
- Read: with the hook, the default build's `tfbench -exact` and answer
  digest, and `tfdrip`'s `tfbench -exact`, are unchanged.

What W11 leaves:
- Whether a waiting history whose entry goes is dropped (b187156's
  wording). Legs 3 and 6 cannot tell. Nor can W9's, W4e's or W11's "once"
  at a tK a horizon past the history's last touch, which is a fresh
  start's log as well: those probes separate one push from two, not a kept
  history from a fresh start. A probe that tells them apart needs a
  history whose log at tK differs from a fresh one's, and its own rule.
- `TestDripHotRestoreLegs` as committed fails on legs 3 and 6 in every
  `tfdrip` suite that runs it on the head (it skips under `TF_CT` and
  `TF_BG`).
- Not covered, as the rule named: the default build; a look again that
  meets the placer mid-placement; ConstantTime's pads placing K between the
  wait and the look again; relocation under ConstantTime and Background;
  the hook's cost on `hotAdd`'s path.

## The record audited again (W4 to W11)

Every arm from W4 to W11 was read again against the library and the tests
as e6d31a1 has them, after W11's reading. Nothing was run and nothing was
mended: each item is from the code and the outputs already in `lab/`, not
a reading. In order of weight:

**1. Target 7's gain is read only where none of the mended paths run.**
tfbench's velocity workload (`velocityMAE`, `cmd/tfbench/accuracy.go:83-151`)
is one goroutine in plain mode, ticks in order, Horizon 60 s at 1 ms, 2,000
keys in a filter of Capacity 200,000: no eviction, stash, wait, relocation
pressure or reclaim, and promotion only in its rates' tails. Every path W4b
to W11 mended is off it, and no velocity reading (W4 to W6, W3's other
workloads, the periodic and sustained ones) had a second goroutine, a
lagging clock, ConstantTime or Background. Where those run, the log can go
dead and stay so. W4b's reading 8 found that "says nothing" erases the gain
under sustained traffic, since only a gap of 11 quanta (1.7 s here) revives
it. W4c took bursts out of its triggers; two remain, and both need a second
toucher or a lagging clock:

- a touch whose tick is in an earlier unit than the key's last touch
  (`next0`, `drip.go:50-51`);
- a count that lands in a later unit than its event's (`dripForget`,
  `touch.go:357-358`): a touch of the key into the next unit between this
  touch's push and its count.

Callers lag in ordinary use: two goroutines touching one key across a unit
boundary, a caller descheduled between its clock read and its call, a
caller passing its own timestamps, a batch (one `now` for every key,
`batch.go:12-34`). How
often a busy key meets one, and what that does to the metric, is not
measured; at tfbench's rates on the built-in clock it may be rare. W6's
fresh seeds re-draw the same generator: they read the metric's spread, not
other traffic. The workload and the metric are the user's (paper §8.4);
this says only where the readings reach.

**2. Upper reads the drip log.** `estimate` returns `Upper: max(n,
int(upper))` (`register.go:513`), and with the log `n` is `dripCount`'s,
which counts each logged event whole whatever the register holds
(`drip.go:113`). The record says the opposite: `drip.go:16` ("Upper never
reads it") and W4's design ("`Upper`, `Seen`, saturation, eviction and
every other answer read what they read today"). The `max` is main's
(8b3ba44's `register.go:508`), where `n` never exceeds `upper`. W4's clause
3 (only Count moves) was read on the single-threaded digest, where the log
never holds more than the register. The readings already show it: W10's
and W10b's sights read Upper 2 for W9's register of one event. Where the
log holds more than the register, Upper rises with Count:

- after a double push: a log defect then reaches `TouchIfUnder`'s refusals,
  which read Upper (`buzz.go:276-286`), as a false denial at the limit;
- after `unpoint`'s reset (`register.go:263`), which starts a fresh
  register and keeps the log: Upper counts the log's older events, where
  the default build reads the register alone.

Upper stays at or above the register's either way, so the contract (not
`Saturated` ⇒ `Upper` ≥ exact) is not touched. The sentence goes into the
claims and paper §8 on adoption, and needs a decision: reworded, or the
code changed.

**3. The concurrent checks have no shown power over a double push on a busy
key, and none at all on some keys.** W10's and W10b's checks are sound
bounds:

- `high` (`countHigh`, `go/countcheck_test.go:82`) allows events up to 9
  units and the lag behind the window (`countReach`, `:68`), so on a key
  with steady traffic one extra event never shows;
- `logHigh` (`:96`) sees an extra event on a dense key only while it is
  among the newest 11 codes at the case's end and the lag is 0: later
  pushes shift it out before the check at rest, and a `maxLag` of 1 or 2
  units (half the fuzzer's cases, `fuzzconc_test.go:161`) lets events 16 to
  32 quanta older stand for it.

Keys with neither check: the fuzzer's shared keys (`shared low` only,
`fuzzconc_test.go:354-379`), so no key with two touchers; the straggler's
keys (`cc` off, `:265`; skipped at rest, `:195`), and the straggler is the
caller that drives A25's path on purpose; every key under tag
amplification. W7's scenario has one owner per key at increasing ticks,
about 16 s a round (`dripcount_test.go:42-55`): no reclaim, no lag, no
second toucher. So W10b's report (W9's double push not seen in 5 minutes of
the fuzzer on 23f03b6) says little of that path. Under concurrency only
W4b's and W4c's double pushes have been seen, both in W7's scenario (W7's
shadow, W10b's log check at rest). No guard of the log has a mutation
reading: W4c's rule in the table's path, W4d's in `miss`, `missPlaced`,
`dripForget`, the cross-unit `d = 0`.

**4. Every hot-restore probe sits a horizon less a tick after the history.**
W9, W4e's test, W10's and W10b's sights and W11 restore K at tK = t0 +
horizon - 1, where `next0` sets the log all ones (`drip.go:45-46`), and K's
history is 32 touches at one tick. So:

- once and fresh are one log (W11's legs 3 and 6);
- no probe has seen a restored history's own logged events across a
  restore;
- the state W8's audit named, `unpoint`'s fresh register beside a log that
  keeps older events (where item 2 raises Upper), has not been made. It
  needs spread events in the history, tK within 11 quanta of them, and a
  reclaimer more than a horizon ahead.

W9's rule checked that its classes differ (clause 1: one push and two leave
different logs); W11 added fresh without that check, which would have shown
legs 3 and 6 before the rule.

**5. The copy paths the model leaves out are where the late defects were.**
W8's model has relocation's hand-off, eviction, and a `miss` that always
succeeds. Not modelled, and probed at one point each at most: `putBack`
(`cuckoo.go:94`); the wait in the stash and the wait room; the placer's
commit (`placeRoomKey`, `stash.go:136-173`); the pads' `placeSet`, which
stashes a waiting copy again when the table already holds the key
(`ct.go:177-178`); ConstantTime's put-back of a stale stash copy
(`touch.go:266-268`); hot entries and `missPlaced`. Both double pushes
found after W8 are there: W9's, and leg 5's on W4d's library. One more
reach of leg 5's path: in ConstantTime `touch` releases K's stripe between
attempts (`touch.go:243-246`), so under concurrency another call's pad can
place K between `missPlaced` and the look again. W11 names the pads as not
covered; a single-threaded probe cannot reach them, a concurrent one can.
From the code, the head's skip covers it as it covers leg 5.

**6. A known failure hides new ones in it.** The regression clauses compare
failing tests by name. Under `tfdrip`, `TestConformance` prints five
differing ops and then a count ("11 ops differ in all", "162 ops differ in
all": `lab/w4e/unit-plain.txt`); whether any differs in more than Count is
not in the outputs. `TestDripHotRestoreLegs` now fails legs 3 and 6 in
every `tfdrip` suite on the head, and a later failure of its other ten
subtests would read as the same failing test.

**7. Smaller.**

- Speed was read single-threaded only. A contended hot key loses word0's
  same-tick skip until its field is all zero codes: probably small, not
  measured.
- ConstantTime's timing (S41) was not read with the log; `dripCount`'s loop
  follows the key's codes.
- A25 says the reclaiming clock "can run more than a horizon ahead" of the
  next caller's; W11 reclaims at 3 units and 1 tick ahead, past the fuzzed
  two units (`reclaim`'s `dead`, `hot.go:332`).
- `TestDripHotRestoreLegs` writes hook globals that other tests' Background
  placers read; `Close` doesn't stop a placer (`filter.go:359-363`), which
  naps about 1 ms after its room drains. No test runs in parallel.

Checked and not a gap: W11's read at 60 s (Upper 1 against 33 events) is
within the fuzzer's contract, which drops events past a horizon of the
newest clock (`fuzzconc_test.go:312`), and within A25; buzz's `next0`
(`buzz.go:359`) is a prediction its trust check compares bit for bit, and a
wrong one drops the entry; the batches, `TouchIfUnder` and ConstantTime all
count through `touch`; `markSat` keeps the log; `Resizable` is serialized,
has hot entries off and carries the log; `hotAdd` without a slot is called
only by `miss`, so W11's hook fires only at a restore's add;
`TestCountsLastTouchWhole`'s failure is the one W4's reading explains (the
event on the window's edge placed in its quantum).

Open, each to have its rule before its reading (none written):

- the metric under concurrency and lag: tfbench's workload from several
  goroutines with clocks of their own, keys owned and shared, in the three
  modes, reading the metric, and the logs that say nothing apart from the
  keys absent;
- W11b: a classifier that tells fresh from once (a spread history, tK
  within 11 quanta of it, the reclaimer more than a horizon ahead), which
  also makes item 4's `unpoint` state and reads Upper there;
- a count of each touch's pushes for the fuzzer, behind a build tag: a
  touch that pushes its event twice, outside a dropped history's fresh
  start, fails, on any key, busy or not; whether W4d's push-again half is
  ever taken in Go is read on the way;
- a mutation reading of the log's guards against the soak, the fuzzer and
  W7;
- `DripPush.tla` with a hot entry, its reclaim, `missPlaced`, and a wait
  room with its placer, sized first (W8's two-toucher runs reached 43
  million states);
- Upper's sentence: reworded, or the code changed.

## Phase 12: the drip log's gain under lag and concurrency (W12)

**Written after the second audit**, whose first item found target 7's
metric read only on tfbench's workload as tfbench runs it: one goroutine,
ticks in order. Two of the log's triggers of "says nothing" need a caller
behind the key's last touch (`next0`: a tick an earlier unit behind;
`dripForget`: a count landing a unit later than its event), and the state
lasts until a gap of 11 quanta (1.7 s at Horizon 60 s). W4b's reading 8
showed what such a state does to the gain under sustained traffic. No
velocity reading has had a late event or a second goroutine. W12 reads the
metric with both. The delay model, its sizes, the order of equal arrivals,
judging the gain, and leaving load to a later arm are the user's choices.

**The probe** (`go/swlag_test.go`, `TF_EXP=1`, both builds; the library
unchanged, `lab/w12.sh` checks it against 828524f's).

- **The workload.** tfbench's velocity workload (`swWorkload`), recorded
  once per seed: its events in the generator's order, and its query of
  each tick from the first horizon on (key and window). The filter is
  tfbench's (Capacity 200,000, Horizon 60 s, Resolution 1 ms), touched by
  `CheckAndTouchAt` and read by `VelocityCountAt`, as tfbench and
  `swRecord` do.
- **Arrival.** Each event gets an arrival tick: its own tick plus a delay
  drawn from a stream of the seed and the delay alone, not the
  generator's, so the queries are tfbench's at every delay. The filter is
  given the events in order of arrival, each touched at its own tick.
  Equal arrivals go in an order drawn from the same stream, as arrival
  times finer than a tick would order them, so that a delay of 1 can put
  an event behind the one a tick after it (in tick order, two events swap
  only if they are less than J ticks apart, and J = 1 would swap none);
  with no delay they go in the generator's order. An event arriving at or
  after the workload's end is not given. The query of tick now comes
  after every event that arrived by now, and its exact count is over the
  events given by then: the metric reads the filter's error on what it was
  given, not the lateness itself.
- **Delays.** `jitter:J`: every event delayed uniformly in [0, J] ticks;
  `stall:J`: 1% of events so delayed, the rest on time; J = 1, 10, 100 and
  1,000 (ms); and `none`, the control.
- **Leg A**: one goroutine, plain mode. **Leg B**: 8 workers; each arrival
  tick's events go to the workers behind a gate, and the tick's query
  follows the gate. `owned`: key k's events always to worker k % 8, in
  arrival order, so leg B must answer as leg A does, query for query.
  `shared`: the tick's events dealt to the workers in turn, so one key's
  events of one tick can run at once in two workers. Leg B runs in plain
  mode, Background, and ConstantTime (functional, no pad, as `TF_CT=1` runs
  it), the shared keys three times (they are not replayable).
- **Overlaps** (leg B, shared): per tick and key, an event at another worker
  than the one that took the key's first event of the tick (`ovl`), and one
  of those also in another unit than that first event (`ovlu`). They depend
  on the arrivals and the dealing alone.
- **Classes.** Before each query's count the key's slot is read as Count
  reads it and classed (`swLagClass`): none (no history, or its last touch
  past the horizon), no log (the default build), says nothing (the log's
  valid bit clear) or valid. Queries, exact counts and errors are summed by
  class. `lagged` counts events given an earlier unit behind their key's
  newest given tick.
- **Seeds.** Leg A: tfbench's (9, 9) and (2026, 41) to (2026, 44), which no
  arm has read. Leg B: (9, 9).

`TestShortWindowLagClassify` reads hand-made states: a key touched once
(valid); one touched again an earlier unit behind its last touch (says
nothing), still so after a gap of 9 quanta, valid again after one of 11; a
key never touched, and one a horizon and a tick past its touch (none); one
exactly a horizon past it (live). In the default build every live key is
no log.

`lab/w12.sh` runs the classify test in both builds; leg A in both builds
(45 runs each: five seeds, nine delays); leg B in both builds (117 runs
each: nine delays, its own leg A and, in three modes, the owned keys once
and the shared keys three times); and leg B under `-race`, `tfdrip`, (9, 9),
`jitter:1000`, plain, the shared keys once (3 runs). `lab/w12.py` reads both
builds' lines and prints the tables and clauses 2 to 5. Caps: 2 minutes for
the classify test, 60 for leg A, 90 for leg B, 60 for the `-race` run.
Outputs in `lab/w12/`.

**Before the rule** (not a reading).

- The test compiles and vets in both builds, and compiles under `-race`;
  it has not run under `-race`.
- `lab/w12.py` was tried on fabricated lines, read from no filter. A set
  meant to hold read as holding. A set with two planted failures (an owned
  run differing from leg A; a seed losing its gain at `jitter:10`) failed
  clauses 3 and 5 on those alone. An empty set first read clauses 2 and 3 as
  holding; the script now reads an empty clause as "NOTHING READ" and exits
  1.
- `lab/w12.sh` was tried on a scratch clone with `TestPartCells` in each
  test's place. It exited 2 on files not committed. On the stand-in run it
  found no library file changed, logged "NO W12 LINES" for every run, and
  exited 1.
- At the user's word, two of the probe's checks ran on that clone before
  this rule, first with equal arrivals in tick order and again on the test
  as committed (after the user chose the drawn order, which leaves `none`
  as it was); both times, line for line the same:
  - `TestShortWindowLagClassify` read every state as made, in both builds.
  - The zero-delay control, leg B's run at `none` on (9, 9) in both builds
    (13 runs each, 144 to 149 s): leg A equals `swRecord` query for query;
    its window sums equal the exact files' (default 19,185 of 114,779 at
    1 s, `tfdrip` 6,798; the other windows likewise); no log says nothing;
    leg B, owned and shared, in the three modes, equals leg A query for
    query; the shared dealing put one key's events of a tick at two workers
    7,728 times, never in two units (one tick is one unit).
  - No delay has been run. The reading reruns both checks.
- Sizing from that run: about 11 s a run; the whole reading about an hour
  without the `-race` run, which was not timed.

**Rule (before the reading).** W12 holds if:

1. sight: `TestShortWindowLagClassify` passes in both builds, each with its
   "W12 classify" lines;
2. no delay: on every seed, leg A equals `swRecord` query for query in both
   builds, and no `tfdrip` query's log says nothing; on (9, 9) each window's
   error and exact sums equal `lab/w4e/exact-drip.txt`'s (`tfdrip`) and
   `verify/expected/exact.txt`'s (default);
3. the harness: leg B's owned keys equal leg A query for query in every
   mode, delay and build, and leg B's own leg A lines equal leg A's run's;
   the `-race` run reports no data race; every run prints its line (45, 117
   and 3), and every test its "W12 done:" line;
4. the shared keys are judged at a delay where `ovlu` is at least 10 (the
   dealing put one key's events of one tick at two workers in two units ten
   times);
5. the gain holds at the delays up to 10 ms (`jitter:1`, `jitter:10`,
   `stall:1`, `stall:10`): the gain, the default build's mean of the five
   windows less `tfdrip`'s, is at least half the gain with no delay:
   - on leg A, on every seed, each build's run on the same seed and delay;
   - on leg B's shared keys, in every mode, at each delay clause 4 judges,
     each build's mean over its three repeats, against the same mode's
     shared keys with no delay. If clause 4 judges none of these delays,
     clause 5 rests on leg A alone, and W12 says so.

The run counts and the seeds present are read from `progress.txt` and the
runs' files (`lab/w12.py` checks neither). A run cut by its cap, or one
that hangs, is the probe's fault, not a reading.

If clause 1 fails, the classes are not read (the dead logs' shares and
errors); the metric is. If clause 2 or 3 fails, W12 says nothing of clause
5 until the difference is read against the code: the probe's, or the
library's dependence on the order of calls at one instant (at a delay a
tick's gate holds several ticks, and a touch's `decayStep` sweeps at its
caller's tick, so in leg B a sweep at a later event's tick can clear a slot
before a lagging touch that finds it live in leg A's order; a slot past
the horizon of the later clock, which the contract lets go). If clause 5
fails at any judged point, the drip log loses more than half its
gain at a lag of 10 ms or less, and the record names where (leg, seed or
mode, model, delay). If clause 5 holds, the gain survives those lags on
this workload and says nothing of the rest (below). Target 7's status is
the user's either way.

Reported, not judged: the five windows and the mean at every delay in both
builds, beside 0.050 and the default build's; the gain at 100 ms and 1 s;
`tfdrip`'s dead logs, by queries and by exact count; the error per event of
valid and dead logs; `lagged`; leg B's shared spread over its repeats, its
answers differing from leg A's, `ovl` and `ovlu`; the time of each test.

A failure is read against the code before anything else: a defect of the
library, or of the probe, said as such.

Not covered:

- load: evictions, the stash, waits and the wait room (at Capacity 200,000
  nothing waits, so Background and ConstantTime take only their call paths;
  a later arm);
- a busy key touched by many goroutines at once: tfbench's keys stay under
  about 100 events a second, and leg B's gates run one tick's events at
  once, never two ticks';
- delays over 1 s (all of W12's are inside the fuzzed two units, 5 s);
- other Horizons and Resolutions, ConstantTime's pads, and other traffic.

### W12's reading (073d7da's tree; `lab/w12.sh`, outputs in `lab/w12/`)

`lab/w12.sh` ran end to end (07:15 to 07:57, Go 1.24.7) and exited 0. Its
first check found no library file different from 828524f's. Every run
printed its line, every test its "W12 done:" line: leg A 45 runs in each
build (the five seeds, nine delays each), leg B 117 in each ((9, 9)), the
`-race` run 3. Leg A took 23 s and 25 s a build, leg B 1,183 s and 1,179 s,
the `-race` run 69 s: under the sizing's hour, since leg A's runs took
about half a second each, not the 11 s the zero-delay control averaged
with leg B's in it. No raw file holds a panic or a data race.

1. **Holds.** `TestShortWindowLagClassify` passes in both builds with its
   seven "W12 classify" lines: with `tfdrip`, valid, says nothing, says
   nothing, valid, none, valid, none, the states as made (the log 0x000
   after the touch an earlier unit behind and after the gap of 9 quanta,
   0xfff after the gap of 11); in the default build no log for every live
   key.
2. **Holds.** With no delay, on all five seeds, leg A equals `swRecord`
   query for query in both builds, and no `tfdrip` query's log says
   nothing; on (9, 9) each window's error and exact sums equal the exact
   files' (10 runs).
3. **Holds.** Leg B's owned keys equal leg A query for query in all 54 of
   their runs (three modes, nine delays, two builds); leg B's own leg A
   lines equal leg A's run's; the `-race` run reports no data race. The
   order dependence the rule named (a sweep at a later event's tick) did
   not show in any owned run.
4. The shared keys are judged at `jitter:100` (`ovlu` 96), `jitter:1000`
   (403) and `stall:1000` (11); not at `none` (0), `jitter:1` (1),
   `jitter:10` (9), `stall:1` (0), `stall:10` (0) or `stall:100` (2). None
   of the delays clause 5 judges is among them.
5. **Holds, on leg A alone** (clause 4 judges no delay up to 10 ms on the
   shared keys). At `jitter:1`, `jitter:10`, `stall:1` and `stall:10` the
   gain is 0.9994 to 1.006 of its gain with no delay on every seed (20
   comparisons; the least at `stall:10` on (9, 9), 0.02213 against 0.02214;
   the most at `jitter:10` on (2026, 43)).

Leg A, the five seeds' range (`tables.txt` has each seed):

| delay | default | `tfdrip` | gain against none | dead logs, by queries | by exact count | lagged |
|---|---|---|---|---|---|---|
| none | 0.0668-0.0671 | 0.0447-0.0452 | 1 | 0 | 0 | 0 |
| jitter:1 | 0.0668-0.0671 | 0.0447-0.0452 | 1.00 | 0 | 0 | 0-1 |
| jitter:10 | 0.0669-0.0672 | 0.0448-0.0452 | 1.00-1.01 | 0.11-0.27% | 0.15-0.71% | 30-33 |
| jitter:100 | 0.0679-0.0682 | 0.0481-0.0494 | 0.86-0.91 | 7.85-9.17% | 20.94-24.16% | 1,587-1,791 |
| jitter:1000 | 0.1026-0.1048 | 0.0979-0.1012 | 0.17-0.21 | 34.78-35.72% | 67.34-68.88% | 59,796-60,393 |
| stall:1 | 0.0668-0.0671 | 0.0447-0.0452 | 1.00 | 0 | 0 | 0 |
| stall:10 | 0.0668-0.0671 | 0.0447-0.0452 | 1.00 | 0-0.03% | 0-0.08% | 0-3 |
| stall:100 | 0.0668-0.0671 | 0.0449-0.0453 | 0.99-1.00 | 0.33-0.58% | 1.01-1.63% | 45-60 |
| stall:1000 | 0.0671-0.0673 | 0.0502-0.0512 | 0.74-0.77 | 11.20-12.42% | 30.27-33.19% | 1,386-1,411 |

**Reported.**

- Beside 0.050: `tfdrip`'s mean stays under it at every delay up to 100 ms
  in both models (at most 0.0494, `jitter:100`), and is above it at
  `stall:1000` on all five seeds (0.0502 to 0.0512) and at `jitter:1000`
  (0.0979 to 0.1012). Target 7's status is the user's.
- The 1 s window carries it. On (9, 9), `tfdrip` 0.0592 with no delay,
  0.0783 at `jitter:100`, 0.0853 at `stall:1000`, 0.3197 at `jitter:1000`;
  the default build 0.1671, 0.1724, 0.1682, 0.3417. A delay of a second on
  every event doubles the default build's 1 s error and multiplies
  `tfdrip`'s by more than five, to within 0.022 of the default's.
- Dead logs sit on busy keys: their share by exact count is 1.4 to 3.2
  times their share by queries (about 1.9 at `jitter:1000`). At 100 ms and 1 s their error per event
  (0.017 to 0.024) reads below the valid logs' (0.039 to 0.064) for that
  reason, so the two are not compared.
- From the code, not a reading: in leg A a log goes dead only by a touch
  an earlier unit behind (one goroutine: no merge, and `dripForget`'s
  condition comes with it), so `lagged` counts the triggers. At
  `jitter:100`, 1,587 to 1,791 of them leave 8 to 9% of the queries on a
  dead log: the sticky state W4b's reading 8 found under bursts, which the
  second audit's first item named.
- Leg B's shared keys: their means are within 0.00006 of leg A's on
  (9, 9) at every delay, mode and build; they differ from leg A in at most
  123 of 240,000 answers a run (`jitter:1000`), and in none at `none`,
  `stall:1` or `stall:10`. At the judged
  delays their gain against none is leg A's (0.87 to 0.88, 0.21, 0.77).
  Plain, Background and ConstantTime read alike. On this workload the
  shared dealing adds no measurable loss to the lag's.
- The `-race` run's shared keys differ from leg A in 91 answers.

**Verdict: W12 passes its rule**, clause 5 on leg A alone: on tfbench's
workload the drip log keeps its gain over the default build when events
reach it up to 10 ms late, in both delay models, on all five seeds. The
pass reads that the trigger is rare there (`lagged` 0 to 33 a run, dead
logs at most 0.27% of the queries), not that the log withstands it.

What W12 reads beyond the rule: the gain wears with larger lags. At 100 ms
of jitter on every event it keeps 0.86 to 0.91 of itself and the mean
stays under 0.050. A stall of up to a second on 1% of events costs a
quarter of it and puts the mean just over 0.050 on every seed. A second of
jitter on every event leaves a fifth, with both builds near 0.10. The dead
logs' share rises with the loss, but how much of the loss is theirs was
not separated (not a reading). The valid logs' error per event rises too
(0.034 to 0.035 with no delay, 0.039 to 0.040 at `jitter:100`, 0.062 to
0.064 at `jitter:1000`), and from the code a touch behind its key's last touch in
the same unit is pushed as a gap of 0 at the last touch's quantum, so a
valid log can hold late events at the wrong quantum. In `stall`, where 1%
of events are late, the dead logs are the stronger candidate; in `jitter`,
where every event is late, the two can't be told apart from these outputs.
Concurrency in leg B's form (one tick's events at once, one key's at two
workers) changed nothing measurable.

What W12 leaves:
- load (evictions, the stash, waits), a later arm;
- a busy key touched by many goroutines at once, which leg B's gates and
  tfbench's rates do not make (its shared keys reached 10 cross-unit
  overlaps only at delays of 100 ms or more);
- delays over 1 s; other Horizons and Resolutions; ConstantTime's pads;
  other traffic;
- how much of the larger lags' loss is the dead logs' and how much the
  valid logs' mistimed events: both builds' errors on the same queries,
  split by `tfdrip`'s class, a new output under its own rule;
- whether a log could leave "says nothing" sooner than a gap of 11 quanta:
  a design question, not read here.

## W11b: the hot restore's fresh start told from its one push

**Written after W12's reading**, from W11's legs 3 and 6 and the second
audit's items 2 and 4. At W11's tK, a horizon less a tick after K's history,
`next0` sets the log all ones, so the touch afresh and the history with the
touch's one push were one log (0xfff). W11 could not tell whether a waiting
history whose entry goes is dropped (b187156's wording), and no probe had
made `unpoint`'s fresh register beside a log that keeps older events, where
Upper reads the log (the audit's item 2). W11b touches K within 11 quanta of
a spread history, with the reclaimer more than a horizon ahead. W11's legs 3
and 6 reading "not twice", the race leg, and judging Upper are the user's
choices.

**The probe** (`go/drip_hotfresh_test.go`, no build tag: both builds; the
library unchanged, `lab/w11b.sh` checks it against 2393104's).

- **The history.** Unit 24 (ticks 60,000 to 62,499; quanta 384 to 399, of
  156.25 ticks): 29 events at 60,320 (quantum 386), then one each at 60,640,
  60,960 and 61,420 (quanta 388, 390 and 393). The 32nd promotes K. As in
  W11, K's words go to the stash, here at 61,420, and its slot is freed; the
  fill keys are touched at 61,420, so they are live at tK. From the code,
  K's log is then 0x9b7.
- **The touch and the reclaimer.** K's touch is at tK = 62,040 (quantum
  397), 4 quanta after the history's last event, in unit 24. L is promoted at
  tL = 127,500 (unit 51), which reclaims K's entry (its newest unit, 24, is
  under 51 - 26): from a hook inside K's touch as in W11, or before K's touch
  (below). L's clock is 65,460 ticks ahead of tK, more than a horizon: the
  case A25 names (an entry is reclaimed at the reclaiming call's clock,
  "which can run more than a horizon ahead of the key's next caller's"), and
  forced here, since tL must be 27 units or more past the history's unit.
  What the filter loses of K's history there is within the contract, which
  drops events past a horizon of the newest clock (`fuzzconc_test.go:312`).
- **The classes.** K's log after its touch is classed against its stashed
  word0 w0: once (`next0(w0, tK)`), twice (`next0` of that at tK) or fresh
  (`dripFresh(tK)`); from the code 0xeef, 0xdde and 0xfff. The test derives
  them from w0 and logs them with its inputs ("W11b inputs"). It stops before
  any leg if they are not three logs or if an input is off: the history, the
  touch and the reads in one unit; the touch 1 to 10 quanta after the
  history's last event; tL reclaiming K's entry and more than a horizon ahead
  of tK; the windows' starts as below, none strictly inside a history event's
  quantum.
- **The legs.** W11's six and their controls, on this history, and three
  more where L's promotion reclaims K's entry before K's touch, K's slot
  free: 7 plain, 8 ConstantTime, 9 Background. Each of 7 to 9 checks before
  the touch that K is in the stash at tK with its entry gone (`hotGone`), and
  after it that the touch started K afresh in the table (word1 a register,
  Restored 0, no `bump` hot add): a fresh start on the filter's own path, the
  classifier's sight of fresh. Every leg checks its path as W11's do, and
  stops with "path not taken" otherwise.
- **The reads.** 100 ms after the touch (62,140), at 250 ms, 1 s, 2 s, 10 s
  and 60 s: Count and Upper, beside the events given (the history's and the
  touch's in the window), 1, 2, 33, 33 and 33. The windows start at 61,890
  (after quantum 393), 61,140 (between 390 and 393), 60,140 (inside unit 24,
  before 386), 52,140 and 2,140 (before unit 24). "Given" is the probe's
  count, not a contract's bound: the reclaimer's clock is more than a horizon
  past every one of K's events.
- **Assertions**, W4e's library's as the code reads:
  - a control restores K with its entry and one push;
  - legs 1, 2, 4 and 5 read once, K's slot a register;
  - legs 3 and 6 and 7 to 9 read fresh;
  - no reclaim leg reads Upper over the events given;
  - K's last touch is tK.

  The default build asserts the paths, the last touch and Upper.
- **The race** (`TestDripHotFreshRace`, `TF_DRIP_FRESH_ROUNDS`, `tfdrip`):
  W11's race on this history, in plain mode, its rounds classed as W11's are.
  K's add at tK leaves its entry's newest unit at 24, so L takes the entry
  whichever comes first, and W11's "K first, L refused" does not occur. Three
  orders remain:
  - L first: K starts afresh (fresh);
  - K first: its touch is counted in its entry, which L then takes (once);
  - L between `hotGone` and the add: K looks again (once).
- **W11's test.** `TestDripHotRestoreLegs`'s legs 3 and 6 now check that
  K's log holds the touch's event once (once or fresh, which at W11's tK are
  one log), not that it is fresh. W11's rule and reading are not edited.
  `TestDripHotRestoreFresh` runs in every suite of both builds from W11b on.

**Predictions, from the code before any run.**

- The head, `tfdrip`:
  - the controls read once with K's entry, Upper 33 at every window;
  - legs 1, 2, 4 and 5 read once beside `unpoint`'s register of one event.
    The log keeps the history's events at quanta 393 and 390 (and a cut
    code), and Count and Upper read 1, 2, 3, 1 and 1. At 10 s and 60 s the
    window starts before unit 24, and `estimate` answers the register of
    bucket 0 whole, without the log (`register.go:444-447`). So Upper falls
    as the window grows (3 at 2 s, 1 at 10 s), and stays between the
    default build's (1 at every window) and the events given;
  - legs 3 and 6 read fresh: the look again recalls the waiting history,
    finds its entry gone and starts K afresh (`dripFresh`); Count and Upper
    are 1 at every window;
  - legs 7 to 9 read fresh, 1 at every window.
- 23f03b6 (W4d's library, with W11's hook): legs 1, 2, 4 and 5 read twice
  (0xdde), Count and Upper 2, 3, 4, 1 and 1, over the events given at 250 ms
  and 1 s; legs 3, 6 and 7 to 9 read fresh, the controls once.
- The race: on the head, the three orders above and no round twice; on
  23f03b6, twice in every round that looks again.

**Before the rule** (not a reading).

- The probe was compiled and vetted on the head in both builds, and under
  `-race`. With W11's hook it was compiled on 23f03b6 in both builds (a
  scratch clone).
- `lab/w11b.py` was tried on seven sets of fabricated lines, read from no
  filter. A set meant to hold read as holding and exited 0; every other set
  exited 1:
  - four planted failures (a control without its entry on 23f03b6, leg 3
    once on the head, leg 1's Upper over the events given, a race round
    twice): clause 1 failed on its plant, and clauses 3 to 6 read NOT JUDGED
    by clause 1's gate, clauses 4, 5 and 6 failing as read on their plants
    alone;
  - legs 1 to 6 stopped on their paths in both of the head's `tfdrip` runs:
    clause 2 failed, clause 4 read NOTHING READ, clause 5 held only where
    judged (legs 7 to 9);
  - 23f03b6's legs file missing: clauses 1 and 2 read NOTHING READ in part,
    clause 3 NOTHING READ, clauses 4 to 6 NOT JUDGED;
  - the head's race file under `-race` missing: clause 6 read NOTHING READ in
    part;
  - a race round twice on the head, beside fewer than 10 looks again under
    `-race`: clause 6 failed;
  - an empty set read NOTHING READ on every clause.
- `lab/w11b.sh` was tried on a scratch clone with `TestPartCells` in each
  test's place. It exited 2 on files not committed. On the stand-in run it:
  - found the library 2393104's, different from 8a15270's in `go/hot.go`
    alone (7 lines added, none removed);
  - applied the hook to 23f03b6, and found that tree's change the hook's;
  - logged "NO W11b LINES" for every run ("NO W11 LINES" for W11's tests);
  - exited 1. Its joiner read NOTHING READ on clauses 1 to 6, clause 7 as
    failing (the stand-in's lines name neither test) and clause 8 as
    holding.
- At the user's word, clause 1 ran before this rule, on a scratch clone of
  the head (`tfdrip`; `TestDripHotRestoreFresh` with the six controls and
  legs 7 to 9 alone; its output is outside the lab, in
  `~/lab-sw-out/self11b.txt`):
  - the inputs as computed: K's log 0x9b7; once 0xeef, twice 0xdde, fresh
    0xfff, three logs; the windows starting in units 24, 24, 24, 20 and 0,
    none inside a history event's quantum; tL 27 units past the history's,
    65,460 ticks ahead of tK;
  - every control once with K's entry, Count 1, 2, 33, 33 and 33 (the
    events given) and Upper 33 at every window;
  - legs 7 to 9: before the touch, K in the stash at tK with its entry gone;
    after it, fresh, Count and Upper 1 at every window.

  Not run: 23f03b6, legs 1 to 6, the default build, `-race`, the race. The
  reading reruns clause 1 whole.
- Sizing, from W11's reading: its legs took seconds and a race run of
  200,000 rounds 12.5 s, so the whole run should take a few minutes. Caps:
  2 minutes for each legs run, 30 for each race run.

**Rule (before the reading).** W11b holds if:

1. the classifier: on the head and on 23f03b6 (`tfdrip`) the inputs hold
   and once, twice and fresh are three logs, legs 7 to 9 read fresh, and
   every control reads once with K's entry. If this fails, the classes have
   no sight, and W11b says nothing of clauses 3 to 6 (`lab/w11b.py` reads
   them NOT JUDGED, beside what they read);
2. paths: every leg takes its path in the four runs (23f03b6; the head,
   plain and under `-race`; the default build). A leg that does not is the
   probe's fault, and W11b says nothing of that leg. A hang or a timeout is
   read the same way;
3. sight, 23f03b6: legs 1, 2, 4 and 5 read twice, and at least one of them
   reads Upper over the events given. A leg among them that reads once there
   has no sight: its head reading is reported, not judged. With no Upper over
   the events given there, clause 5's second half is not judged;
4. the head, `tfdrip`, plain and under `-race` alike, with no data race: legs
   1, 2, 4 and 5 read once, K's slot a register; legs 3 and 6 read fresh;
5. Upper: in every reclaim leg (1 to 9), at each window, `tfdrip`'s Upper is
   at or above the default build's on the same leg and window, and at or
   below the events given;
6. the race: on the head no round reads twice in 200,000, nor in 20,000
   under `-race`, with no data race. Judged only if at least 10 rounds looked
   again in each run; on 23f03b6 at least one round of 200,000 reads twice,
   else the head's pass is reported, not judged as mended;
7. W11's `TestDripHotRestoreLegs` (legs 3 and 6 as changed) and W4e's
   `TestDripHotRestorePushOnce` pass on the head;
8. the library is 2393104's.

`lab/w11b.py` prints the clauses from the outputs; the runs and their lines
are also read from `progress.txt`.

What the readings would say:

- Legs 3 and 6:
  - fresh: a waiting history whose entry goes is dropped, as A25 claims of a
    history recalled after its entry was reclaimed and b187156 worded for
    the wait, read with a log that tells it for the first time;
  - once: the history kept, against both;
  - twice: a double push.
- Legs 7 to 9 are A25's reclaim before the restore. Fresh there reads its
  claim with the log; it is also clause 1's sight of fresh.
- Legs 1, 2, 4 and 5 once: a history placed and then reclaimed keeps its
  logged events beside `unpoint`'s fresh register (its register starts
  afresh, its log does not). That is the state the second audit's items 2
  and 4 named, and Count and Upper read the logged events there.
- Clause 5: whether Upper, reading the log in that state, stays between the
  register's answer and the events given. It does not decide Upper's
  sentence (`drip.go:16`, W4's design), which is the user's: reworded, or the
  code changed.

Reported, not judged:

- every leg's reads in both builds, the controls' Count among them;
- Upper's fall as the window grows;
- the race's classes and their counts;
- each test's time.

A failure is read against the code before anything else: a defect of the
library, or of the probe, said as such.

Not covered:

- a history over more than one unit (here the register's bucket 0 holds all
  of it with the touch), and a touch behind the history's last event (W4d's
  push-again half);
- as in W11: a look again that meets the placer mid-placement; ConstantTime's
  pads placing K; relocation under ConstantTime and Background;
- `unpoint`'s hold path (`TF_NOCAS2`): the runs take its 16-byte CAS;
- other Horizons and Resolutions, and concurrency beyond the race's two
  goroutines.

### W11b's reading (f606d88's tree; `lab/w11b.sh`, outputs in `lab/w11b/`)

`lab/w11b.sh` ran end to end (17:32:56 to 17:33:47, Go 1.24.7) and exited
0, and its joiner exited 0. Its first checks found the library 2393104's,
different from 8a15270's in `go/hot.go` alone (7 lines added, none
removed), and the hook applied to 23f03b6 changed that tree by the hook's
lines alone. Every run printed its lines: 15 leg lines in each of the four
legs runs, the race's classes in each of the three race runs, W11's lines in
its run. The tests' own exits:

- on 23f03b6 the legs' test failed on legs 1, 2, 4 and 5 alone (each read
  twice, with Upper over the events given at 250 ms and 1 s), and the race
  failed on its rounds read twice;
- on the head every test passed, plain and under `-race`, and in the default
  build.

No raw file holds a panic or a data race.

1. **Holds.** On the head and on 23f03b6 the inputs held as computed: K's
   log 0x9b7; once 0xeef, twice 0xdde, fresh 0xfff; the windows starting in
   units 24, 24, 24, 20 and 0, none inside a history event's quantum; tL 27
   units past the history's, 65,460 ticks ahead of tK. On both trees legs 7
   to 9 read fresh: K was in the stash at tK with its entry gone before the
   touch, and held a register after it, Restored 0. Every control read once
   with K's entry (word1 a hot pointer to it, Restored 1). On the head these
   ten lines (the inputs, the controls, legs 7 to 9) equal the self-check's
   line for line.
2. **Holds.** Every leg took its path in the four runs, with the same fields
   in each: no leg stopped on a path check.
   - Leg 1: the hook at `testHookVictim`, Promoted 2, one eviction, the look
     again through `bump`'s hot add on K's slot.
   - Legs 2 and 4: `testHookInstalling`, Promoted 2, the look again through
     `bump`.
   - Leg 3: `testHookRestoreAdd`, Promoted 2, waited 2.
   - Leg 5: `testHookRestoreAdd`, Promoted 2, K placed by the placer (one
     relocation), waited 1, the look again through `bump`.
   - Leg 6: `testHookRestoreAdd`, Promoted 2, waited 2, K taken out of the
     wait room once.
   - Legs 7 to 9: L promoted before the touch (Promoted 2), K in the stash
     at tK with its entry gone, no look again.
   - The controls: none reclaimed (Promoted 1); leg 1's evicted once, leg
     3's and leg 6's waited, leg 5's waited and was placed.
3. **Holds.** On 23f03b6 legs 1, 2, 4 and 5 read twice (0xdde): Count and
   Upper 2, 3, 4, 1 and 1 against 1, 2, 33, 33 and 33 given, so Upper is
   over the events given at 250 ms and 1 s in all four. Legs 3, 6 and 7 to
   9 read fresh, the controls once with K's entry.
4. **Holds.** On the head, plain and under `-race`, the two runs' lines the
   same and no data race: legs 1, 2, 4 and 5 read once (0xeef), K's slot a
   register; legs 3 and 6 read fresh (0xfff), K waiting again with a fresh
   register (waited 2, Restored 0).
5. **Holds.** In the nine reclaim legs at the five windows (45 reads),
   `tfdrip`'s Upper is at or above the default build's and at or below the
   events given:

   | legs | `tfdrip` Upper | default Upper | given |
   |---|---|---|---|
   | 1, 2, 4, 5 | 1, 2, 3, 1, 1 | 1, 1, 1, 1, 1 | 1, 2, 33, 33, 33 |
   | 3, 6, 7, 8, 9 | 1, 1, 1, 1, 1 | 1, 1, 1, 1, 1 | 1, 2, 33, 33, 33 |

   (at 250 ms, 1 s, 2 s, 10 s and 60 s, 100 ms after the touch). Count
   equals Upper at every read of every reclaim leg in both builds.
6. **Holds.** On the head no round read twice in 200,000 (130 looked again)
   nor in 20,000 under `-race` (13 looked again), with no data race; both
   are over the 10 the rule asks, the second narrowly. On 23f03b6, 124 of
   200,000 rounds read twice: every round that looked again, and no other.

   | run | rounds | fresh, no look again (L first) | once, no look again (K first) | once, looked again | twice, looked again |
   |---|---|---|---|---|---|
   | 23f03b6 | 200,000 | 197,630 | 2,246 | 0 | 124 |
   | head | 200,000 | 197,581 | 2,289 | 130 | 0 |
   | head, `-race` | 20,000 | 10,074 | 9,913 | 13 | 0 |

   L was promoted in every round: W11's "K first, L refused" did not occur.
7. **Holds.** W11's `TestDripHotRestoreLegs` and W4e's
   `TestDripHotRestorePushOnce` pass on the head, in plain mode, in the
   runner's one selection of the two. Legs 3 and 6 of the first read 0xfff,
   classed once, as at W11's reading; the changed assertion ("not twice")
   accepts it. No `tfdrip` suite has run since f606d88:
   `TestDripHotRestoreFresh`, now in every suite of both builds, has not run
   in one, nor under `TF_NOCAS2`.
8. **Holds.** The library is 2393104's.

**Reported.**

- The controls: `tfdrip`'s Count reads 1, 2, 33, 33 and 33, the events given
  at every window; the default build's reads 4, 18, 33, 33 and 33. From the
  code, not a reading: there the newest bucket's 32 other events are spread
  from its onset to the touch. Upper is 33 at every window in both.
- In legs 1, 2, 4 and 5 `tfdrip`'s Count and Upper fall as the window grows
  (3 at 2 s, 1 at 10 s), as the rule predicted from the code (not read
  beyond the answers): at 10 s and 60 s the window starts before unit 24,
  and `estimate` answers the register of bucket 0 whole, without the log.
  The default build reads 1 at every window.
- The race under `-race`: K first took half the rounds (9,913 of 20,000)
  against 1.1% plain, as W11's did (48% against 1.2%).
- Times: each legs run took hundredths of a second; a race run 13.95 s (the
  head), 14.89 s (23f03b6), 6.69 s under `-race`; the whole run 51 s.

**Verdict: W11b passes its rule.** All eight clauses hold; clause 1's gate
held none back.

What W11b reads of W4e's library:

- Read: a waiting history whose entry goes is dropped. In legs 3 and 6
  (ConstantTime's stash; Background's wait room with no placer) the look
  again starts K afresh, and K's log holds the touch's event alone. That is
  A25's claim and b187156's wording, read with a log that tells it for the
  first time. A recalled history whose entry went before the touch (legs 7
  to 9, A25's reclaim before the restore) starts afresh in all three modes,
  and so does the race's L-first order (197,581 of 200,000 plain rounds).
- Read: a history placed and then reclaimed (legs 1, 2, 4 and 5: an evicting
  claim, ConstantTime's and Background's install, the placer's placement)
  keeps its logged events beside `unpoint`'s fresh register. Its register
  starts afresh and its log does not. Count and Upper read the logged
  events there: 1, 2 and 3 at the three windows that start inside the
  history's unit (250 ms, 1 s and 2 s), where the default build reads 1 at
  each. Both builds read 1 at 10 s and 60 s, whose windows start before that
  unit.
- Read: in that state Upper stays between the default build's and the
  events given at every read. A double push (W4d's library) puts it over
  the events given (2 against 1 at 250 ms, 3 against 2 at 1 s).
- Read: in that state `tfdrip`'s Upper is not monotone in the window (3 at
  2 s, 1 at 10 s). The default build's is 1 throughout.
- Read: the race on this history pushed no event twice on the head (0 of
  220,000 rounds, 143 looked again), and 124 times in 200,000 on W4d's
  library.
- Read: W11's `TestDripHotRestoreLegs` passes on the head with legs 3 and 6
  reading "not twice", in plain mode, in the runner's selection; no
  `tfdrip` suite has run since.

What W11b leaves:

- Upper's sentence (`drip.go:16`, W4's design: "Upper never reads it"), and
  the constraint at this notebook's head ("`Upper` never below the truth;
  only `Count` may move"). W11b reads one state where, with no defect,
  Upper reads the log: 2 and 3 where the default build reads 1, between the
  register's answer and the events given. Rewording the sentence and the
  constraint, or changing the code, is the user's decision.
- Whether Upper's fall with the window in that state matters to a caller
  that reads Upper at its own window (`TouchIfUnder`'s refusals): not read.
- The second audit's item 4 for this history alone: W11b is the first
  hot-restore probe whose touch is not a horizon after its history. Its
  history sits in one unit with the touch.
- As the rule named, not covered: a history over more than one unit, and a
  touch behind its last event; a look again that meets the placer
  mid-placement; ConstantTime's pads; relocation under ConstantTime and
  Background; `unpoint`'s hold path (`TF_NOCAS2`); other Horizons and
  Resolutions; concurrency beyond the race's two goroutines.

## W13: each touch's pushes counted

**Written after W11b's reading**, from the second audit's item 3 and the
option it listed: "a count of each touch's pushes for the fuzzer, behind a
build tag". W10's and W10b's checks have no shown power over a double push on
a busy key, and none on the fuzzer's shared keys, the straggler's keys or
under tag amplification. W13 counts, in each touch's own frame, the stores of
its push into the drip log. A touch that stores its push again into a history
at or after its own tick fails: the shape of W9's double push, on any key,
busy or not. The user chose:

- failing twice alone, and not every second store but a fresh start;
- the tally in `touch`'s frame;
- the runs: the fuzzer's three legs at an hour each, and W7's Background
  spread scenario;
- the power leg on 23f03b6;
- both self-checks, and a trial of the runner at small sizes.

The rule below was written after that trial, a peek at the data ("Before
the rule").

**The instrument** (`go/push.go`, `go/push_on.go`, `go/push_off.go`; notes in
`go/touch.go`, `go/cuckoo.go` and `go/filter.go`).

- **The tally.** A local in `touch`, passed to `miss` as one new parameter.
  With the `tfpush` tag and `tfdrip` it counts. Without them it is an empty
  struct whose notes do nothing, and `pushOn` is false, so the notes compile
  out.
- **The notes.** One at each place a word0 holding the touch's push is
  stored:
  - `touch`'s CAS in the key's unit (the line W4c's gate guards): the gate
    held (a skip), `next0` left the word as it was (silent: a log that says
    nothing took the event), or the CAS stored;
  - `stamp` (a hot key into a later unit) and `realign` or `realign2U` (a key
    into a later unit), when they succeed;
  - `miss`, once `low` is placed (a claim or a wait, `ci != -1`): a fresh
    start (no history recalled, `dripFresh`), W4d's gate held (a skip), or a
    push into the recalled history; and `missPlaced`.

  Each store keeps the history's last touch before it and the library's
  `pushed` flag at the store.
- **The classes**, at the touch's return:
  - none: no store;
  - once: one store;
  - by its later stores:
    - fresh: a later store is a fresh start;
    - again: a later store into a history whose last touch is before the
      touch's tick (W4d's push-again half);
    - twice: a later store into a history whose last touch is at or after
      the touch's tick.

  Also counted:
  - behind: a touch that stored with `pushed` false after a store of its
    own (W9's mechanism);
  - bare: a touch that returned with no store and no silent note. From the
    code there are none.
- **The counts.** Each filter keeps 16 stripes of atomic counters, a
  touch's stripe chosen by its key's low bits. They are read:
  - by the fuzzer per case, by its mode;
  - by W7's scenario per round;
  - by the tests through `testHookPushDone`, a call per touch with its
    class and tally, set only by single-threaded legs and by the race.
- **The count is of stores, not of the log.**
  - A push stored into a copy that is later dropped, then skipped by the
    tick test, reads once: a lost push, the tick test's blind side (W8).
  - Copies (relocation, eviction, the placer, `putBack`, merge, resize) are
    not pushes.
  - buzz's `next0`, a prediction, stores nothing in the slot.

From the code, on the head every store is followed by the touch's return or
by its flag set:

- the table's CAS and `stamp` set `pushed`;
- `realign` and a placed `miss` return;
- `missPlaced` sets the flag.

So twice and behind have no path on the head. A twice would read the flag
wrong, the shape of W9's defect. The fuzzer's clause mostly reads reach.

**The readers.**

- W11b's legs (`TestDripHotRestoreFresh`) and race (`TestDripHotFreshRace`)
  log K's touch's push class beside its log class ("W13 leg", "W13 race").
- `TestPushPaths` (`go/push_test.go`, `tfpush`) makes three touches on one
  goroutine, each predicted:
  - realign: a key into its next unit;
  - stamp: a promoted key into its next unit;
  - a silent push: a 13th touch at one tick, where 11 zero codes fill the
    field and `next0` leaves the word as it is.

  It then reads the filter's counts of its 48 touches, the counters' sight:
  - 27 once and 21 none;
  - 21 silent;
  - 3 first stores that start a key afresh;
  - stores: 3 claims, 22 in the table, 1 realign, 1 stamp;
  - every other count 0, bare among them.
- The fuzzer (`TestFuzzConcurrent`) reads each case's counts by mode
  (plain, ConstantTime, Background). A case with a touch twice fails, kind
  "push twice". The summary line gets a W13 part.
- W7's scenario (`TestDripConcurrentCounts`) reads the counts per mode and
  shape. A touch twice is a failure.
- 23f03b6 (W4d's library) is read with:
  - W11's hook;
  - the notes ported by hand (`lab/w13-w4d.patch`). Its `touch` and `miss`
    differ in context, and it has no `missPlaced`, so there is no note for
    that;
  - the head's push files and W11b's test;
  - its own older fuzzer, with the wiring ported by hand
    (`lab/w13-w4d-fuzz.patch`, the same summary format).

**Predictions, from the code before any run.**

- **Sight.**
  - On the head:
    - the controls read once;
    - legs 1, 2, 4 and 5 read once: a store, `missPlaced`, then the table's
      skip;
    - legs 3 and 6 read fresh: a store into the wait, `missPlaced`, then a
      fresh start with the flag set, not behind;
    - legs 7 to 9 read once, a fresh start.
  - On 23f03b6:
    - legs 1, 2, 4 and 5 read twice and behind: the claim's or the wait's
      store, then the look again's table CAS into last tK with the flag
      false;
    - legs 3 and 6 read fresh and behind;
    - the rest as on the head.
  - In the race, every round's push class tells its log class:
    - log once, push once;
    - log twice, push twice;
    - log fresh, push once with a fresh start.
- **`TestPushPaths`.**
  - realign: "once: stores 1 [realign into last 10000, flag false]";
  - stamp: "once: stores 1 [stamp into last 10000, flag false]";
  - silent: "none: stores 0 [], ... silent 1";
  - the filter's counts as listed above.
- **The fuzzer, W7 and the races on the head.**
  - twice, behind and bare read 0.
  - Again needs another copy, older than the touch's push, to replace the
    copy that holds it: a `putBack` over a stash copy holding the push, or
    the wait room's put-back. If it happens at all, it is rare.
  - `missPlaced` happens only where a hot history's entry is reclaimed
    during its restore. That needs a reclaimer a horizon ahead, as the
    straggler leg has, so it is rare or absent. Only `missPlaced` marks the
    hot restore's look again (W9's path).
  - Fresh after a store needs no hot entry. A touch stores in the table,
    its `bump` fails, and the key is evicted with its history lost, folded
    or never stashed; the look again's `miss` then recalls nothing. The
    fuzzer's floods and its cases without a stash can make that.
  - The table's skips come after a store whose `bump` fails (the slot handed
    on or moved), and after `missPlaced`.
  - Silent pushes come on busy keys with a full field or a dead log.
- **The power leg on 23f03b6:** unknown. W9's double push needs a hot
  restore whose entry is reclaimed between `hotGone` and the add. W10b's log
  check saw none in 5 minutes of the fuzzer there, without the straggler.
- **The library.** The notes compile out, both builds' answers are
  unchanged, and `tfdrip,tfpush`'s exact output equals `tfdrip`'s.

**Before the rule** (not a reading).

- **Builds.** The instrument was compiled and vetted (`go vet ./...`) on the
  head in the four tag sets: none, `tfdrip`, `tfpush`, and `tfdrip,tfpush`.
  With W11's hook and the ports it was compiled on 23f03b6 (a scratch clone)
  in three: none, `tfdrip`, and `tfdrip,tfpush`.
- **The port.** The notes' changed lines are 42 on the head and 39 in the
  port. The head's alone are the `missPlaced` note's three; the port has
  none of its own.
- **The joiner.** `lab/w13.py` was tried on thirteen sets of fabricated
  lines, read from no filter:
  - a set meant to hold read as holding and exited 0;
  - a set with no twice on 23f03b6's fuzzer failed clause 6 alone and still
    exited 0 (clause 6 qualifies the verdict);
  - every other set exited 1:
    - a leg of 23f03b6 reading once: clause 1 failed;
    - the path counts off by one: clause 1 failed;
    - race rounds not all classed: clause 1 failed;
    - in each of these three, clauses 3, 4 and 6 read NOT JUDGED by the
      gate, beside what they read;
    - a touch twice in a head fuzzer leg, in W7 and in the head's race:
      clauses 3, 4 and 7 failed;
    - bare touches in one leg: clause 5 failed;
    - four library plants (an extra line in the port, `tfdrip,tfpush`'s
      exact output differing, TestConformance's lines differing, go vet
      failing in one tag set): clause 2 failed on each;
    - 23f03b6's legs file and one head fuzzer leg missing, and a mode with
      no calls: clauses 1 and 5 read NOTHING READ in part, and clauses 3,
      4 and 6 NOT JUDGED by the gate (clause 3 NOTHING READ in part as
      read);
    - the library's exact checks skipped: clause 2 read NOTHING READ in
      part;
    - 5 rounds that looked again in the head's race under `-race`: clause
      4 read HOLDS where judged, NOT JUDGED in part;
    - W4e's test failing: clause 7 failed;
    - an empty set read NOTHING READ on every clause.
- **The joiner on real lines.** Run on the second self-check's outputs, it
  parsed every leg, path and race line as the self-check printed them.
  Clause 1 failed there on the path counts alone, a line that self-check's
  `TestPushPaths` did not yet print. The runs it lacked read NOTHING READ
  in part.
- **The runner.** `lab/w13.sh` was tried on a scratch clone with
  `TestPushNames` in each test's place. It exited 2 on files not committed.
  On the stand-in run it:
  - found the library c8eab03's but for the notes' six files;
  - found the port's lines the head's but for the `missPlaced` note's
    three;
  - found go vet clean in the four tag sets;
  - applied the hook and both ports to 23f03b6;
  - logged "NO LINES" or "NO SUMMARY WITH PUSH COUNTS" for every run;
  - exited 1, and removed its worktree.

  Its joiner read NOTHING READ on every clause but two:
  - clause 2 read NOTHING READ in part (the port and go vet judged; the
    exact checks skipped by the trial's switch);
  - clause 7 failed: the stand-in's lines name neither test.
- **The self-checks.** At the user's word, two ran before this rule, on
  scratch copies of the head's tree and on 23f03b6's port. Their outputs
  are outside the lab, in `~/lab-sw-out/self13.txt` and `self13b.txt`.
  - The first ran before the bare counter and `TestPushPaths` existed:
    W11b's 15 legs, and 20,000 rounds of its race, on both trees. Every leg
    read as predicted:
    - on the head, legs 1, 2, 4 and 5 once with `missPlaced` 1 and the
      table's skip 1, and legs 3 and 6 fresh, not behind;
    - on 23f03b6, legs 1, 2, 4 and 5 twice and behind, legs 3 and 6 fresh
      and behind;
    - on both, the controls once and legs 7 to 9 once, a fresh start.

    In the race, every round's push class told its log class:

    | run | log fresh, push once afresh | log once, push once | log twice, push twice |
    |---|---|---|---|
    | the head | 19,922 | 78 | 0 |
    | 23f03b6 | 19,901 | 89 | 10 |
  - The second ran after the counter and `TestPushPaths` were added: the
    same runs, and `TestPushPaths`.
    - The legs' lines equal the first's, line for line, on both trees.
    - The three paths read as predicted on both trees.
    - The race:

      | run | log fresh, push once afresh | log once, push once | log twice, push twice |
      |---|---|---|---|
      | the head | 19,926 | 74 | 0 |
      | 23f03b6 | 19,900 | 86 | 14 |
  - Not run in either: the fuzzer, W7, `-race`, the library's answers, and
    the filter's counts. The trial below ran them.
- **The trial.** At the user's word, after the second self-check and the
  changes since, `lab/w13.sh` ran on a scratch clone of this tree (105bf22,
  a "dry" commit of it). It used the real tests at small sizes: 5,000 race
  rounds (2,000 under `-race`), 50 W7 rounds, 30 s of fuzzer a leg, and the
  library's checks whole. Its outputs are outside the lab, in
  `~/lab-sw-out/trial13/lab/w13/`. This rule was written after it, a peek at
  the data. It read:
  - the library: the exact outputs and the digest identical, and
    `tfdrip,tfpush`'s exact output equal to W4e's. `TestConformance` printed
    the same lines in both builds ("11 ops differ in all", "162 ops differ
    in all"). go vet was clean in the four tag sets;
  - sight: every leg and path, and the filter's counts of `TestPushPaths`'s
    48 touches, as predicted on both trees. Every race round's classes
    agreed, with 1 round of 5,000 twice on 23f03b6 and none on the head;
  - the fuzzer, 30 s a leg (102 to 117 cases, 2.4 to 2.6 M ops/s):
    - no touch twice, behind 0 and bare 0, in every mode;
    - `missPlaced` 0 everywhere, and again 0;
    - W4d's gate in `miss` 766 in plain mode and 165 in Background over
      the three legs, 0 in ConstantTime;
    - fresh after a store 438, 134 and 44;
    - stores by `stamp`, `realign` and the wait all taken;
  - the power leg, 30 s: no touch twice, behind 0;
  - W7, 50 rounds: no touch twice; W4d's gate in `miss` 1,152, the
    table's 302;
  - the head's races: 3 rounds looked again in each at these sizes, under
    clause 4's 10.

  The trial was not judged against this rule. The reading reruns every
  step at full size.
- **Sizing.**
  - The legs take seconds, and a race run of 200,000 rounds takes about 14
    s (W11b).
  - W7's Background spread scenario at 20,000 rounds took 45 s (W10b).
  - The library's checks take about 3 minutes (W11).
  - The four fuzzer legs take an hour each.
  - The whole run should take about 4 hours 10 minutes.
  - W10b's fuzzer ran at 2.5 M ops/s with `tfdrip`, and 2.4 with the
    straggler (5 minutes each, a66af40's tree).
  - Caps: 2 minutes for each legs run, 30 for each race run. None on W7,
    which its rounds bound, nor on the fuzzer, which `TF_CFUZZ` bounds.

**Rule (before the reading).** W13 holds if:

1. **Sight.** On 23f03b6's port and on the head (plain and under `-race`,
   with no data race):
   - every leg's push class is the predicted one, and tells the same as its
     log class: log once, push once; twice, twice; fresh, fresh or once
     with a fresh start;
   - K's touch is noted once;
   - on the head, legs 1, 2, 4 and 5 read `missPlaced` 1 and the table's
     skip 1, and legs 3 and 6 read `missPlaced` 1 and not behind;
   - on 23f03b6, the legs' test fails on legs 1, 2, 4 and 5 alone;
   - `TestPushPaths` reads its three paths, and the filter's counts of its
     48 touches, as predicted on both trees;
   - in the races (23f03b6, 200,000 rounds; the head, 200,000, and 20,000
     under `-race`), every round is classed, its push class tells its log
     class, and 23f03b6's race reads at least one round twice.

   If this fails, the count has no sight, and W13 says nothing of clauses
   3, 4 and 6 (`lab/w13.py` reads them NOT JUDGED, beside what they read).
2. **The library's answers.**
   - The library differs from c8eab03's in the notes' six files alone, and
     the port's lines are the head's but for the `missPlaced` note.
   - The default build's `tfbench -exact` and answer digest equal
     `go/verify/expected/`'s.
   - `tfdrip`'s and `tfdrip,tfpush`'s exact outputs equal W4e's
     (`lab/w4e/exact-drip.txt`).
   - `TestConformance` prints the same lines under `tfdrip,tfpush` as under
     `tfdrip`, times aside.
   - go vet is clean in the four tag sets.
3. **The fuzzer on the head.** In each of the three legs (as it is, with
   the straggler, with `TF_CFUZZ_TAGS=8`), an hour each, no touch reads
   twice in any mode, and no case has one. A mode with no touches reads
   NOTHING READ. `lab/w13.py` prints a reach line per path beside the
   clause: each gate's count over the three legs, by mode. A path whose
   gate reads 0 is not reached there: NOTHING READ for that path, not a
   pass, and the clause is never quoted without its reach lines.
4. **W7 and W11b's race.** In W7's Background spread scenario (20,000
   rounds), and in W11b's race on the head (plain and under `-race`), no
   touch reads twice. A race run is judged only if at least 10 of its
   rounds looked again, by its "W11b race" lines (W11b's condition).
5. **Every touch noted.** No touch returned with no store and no silent
   note, in the fuzzer's four legs and in W7's.
6. **Power** (this qualifies the verdict and is not part of it). 23f03b6's
   own fuzzer, older than the head's, with the wiring ported and the
   straggler, for an hour, reads at least one touch twice.
   - If it does, clause 3 is read with the count's power under that fuzzer
     shown.
   - If not, W13's verdict says that the count's power there rests on the
     sight (clause 1) and on the head's gate counts alone.
7. **The head's tests pass under `tfdrip,tfpush`:** W11b's legs and race,
   `TestPushPaths`, W11's `TestDripHotRestoreLegs`, and W4e's
   `TestDripHotRestorePushOnce`.

W13 passes if clauses 1 to 5 and 7 hold. `lab/w13.py` prints the clauses
from the outputs. The runs and their lines are also read from
`progress.txt`.

What the readings would say:

- **Clauses 3 and 4 holding:** on the paths the gates show taken, no touch
  on the head stores its push twice. That covers the fuzzer and W7, on busy
  keys, shared keys, the straggler's keys and under tag amplification.
  Behind at 0 says the `pushed` flag never missed a store there. It does
  not say the log holds every event once: a lost push, and the tick test's
  blind side, are outside the count.
- **Again's count:** whether W4d's push-again half is taken in Go. W8 found
  it never taken in the model.
- **`missPlaced`'s count:** whether the fuzzer reaches the hot restore's
  look again (W9's path) at all. Fresh's count is not that: a fresh start
  after a store needs no hot entry.
- **Clause 6 failing:** 23f03b6's fuzzer, at an hour, does not reach W9's
  double push even with the straggler. The head's pass at clause 3 then
  says nothing of that path beyond its gate counts.

Reported, not judged:

- every gate's count per leg and mode: stores by site, skips, silent, none,
  fresh, first fresh, again, behind, `missPlaced`;
- behind on the head (predicted 0);
- 23f03b6's behind in the power leg beside the head's `missPlaced`: each is
  a look again after a placed history, the reach W9's path has on its tree;
- the fuzzer's cases and rate in each leg, beside W10b's;
- other failing kinds in the fuzzer's and W7's outputs;
- `TestConformance`'s diff counts;
- each run's time.

A failure is read against the code before anything else, and called a
defect of the library or of the instrument, as the case may be.

Not covered:

- a lost push: the count is of stores, so a store into a copy that is later
  dropped and then skipped reads once. The tick test's blind side is the
  same;
- `miss`'s gate (W4d's skip) and the again class have no sight: neither was
  made by hand, and they are read only as counts;
- `TF_NOCAS2`: the hold paths of `stamp`, `realign` and `unpoint`. The runs
  take the 16-byte CAS;
- the soak (`TestStressSoak`), the fuzzer under `-race`, other Horizons and
  Resolutions;
- W4c's double push (0837f9f) in W7's scenario, which was not run with the
  count;
- the default build's speed with the notes compiled out, and `tfpush`'s
  cost beyond the fuzzer's rate.

### W13's reading (75b7b07's tree; `lab/w13.sh`, outputs in `lab/w13/`)

`lab/w13.sh` ran end to end (06:24:45 to 10:29:12, Go 1.24.7) and exited
0, and its joiner exited 0. Its first check found the library different
from c8eab03's in the notes' six files alone. The port's changed lines are
the head's but for the three of the `missPlaced` note (the head's 42, the
port's 39, none the port's alone). Every run printed its lines. The tests'
own exits:

- `TestConformance` failed in both builds on the same lines as before W13
  ("11 ops differ in all", "162 ops differ in all"), times aside;
- on 23f03b6:
  - the legs' test failed on legs 1, 2, 4 and 5 alone;
  - the race failed on its rounds read twice;
  - its fuzzer failed on "push twice" alone, in 2 cases;
- on the head every test passed: plain and under `-race`, in the fuzzer's
  three legs and in W7's scenario.

No raw file holds a panic or a data race.

1. **Holds.** Sight, on both trees.
   - On the head, plain and under `-race` (the two runs' lines the same):
     - the controls read once;
     - legs 1, 2, 4 and 5 read once, with `missPlaced` 1 and the table's
       skip 1;
     - legs 3 and 6 read fresh: the fresh start with the flag set,
       `missPlaced` 1, not behind;
     - legs 7 to 9 read once with a fresh start.

     Each leg's push class tells its log class, and K's touch is noted
     once in each.
   - On 23f03b6:
     - legs 1, 2, 4 and 5 read twice and behind: the claim's or the wait's
       store into last 61,420, then the table's into last 62,040 with the
       flag false;
     - legs 3 and 6 read fresh and behind;
     - the rest read as on the head.
   - `TestPushPaths` read its three paths as predicted on both trees. The
     filter's counts of its 48 touches were also as predicted: calls 48,
     once 27, none 21, silent 21, first fresh 3; stores table 22, claim 3,
     realign 1, stamp 1; the rest 0.
   - In the races every round is classed, and its push class tells its log
     class:

     | run | rounds | log fresh, push once afresh | log once, push once, no look again | log once, push once, looked again | log twice, push twice |
     |---|---|---|---|---|---|
     | 23f03b6 | 200,000 | 199,067 | 837 | 0 | 96 |
     | head | 200,000 | 199,087 | 814 | 99 | 0 |
     | head, `-race` | 20,000 | 10,023 | 9,948 | 29 | 0 |

     On 23f03b6 every round that looked again read twice, and no other
     round did.
2. **Holds.** The library's answers are unchanged:
   - the default build's `tfbench -exact` and digest are identical to
     `go/verify/expected/`'s;
   - `tfdrip`'s and `tfdrip,tfpush`'s exact outputs are identical to W4e's;
   - `TestConformance` prints the same lines in both builds;
   - go vet is clean (0 lines) in the four tag sets.
3. **Holds.** No touch on the head's fuzzer stored its push twice, in any
   mode of the three legs, and no case had one:

   | leg | cases | M ops/s | touches, plain | ConstantTime | Background | twice |
   |---|---|---|---|---|---|---|
   | as it is | 11,751 | 2.2 | 5,618,489,147 | 1,743,174,070 | 1,814,187,282 | 0 |
   | straggler | 11,709 | 2.1 | 5,591,306,276 | 1,734,408,421 | 1,812,933,837 | 0 |
   | tags 8 | 12,188 | 2.2 | 5,834,880,430 | 1,816,530,429 | 1,889,663,972 | 0 |

   That is 27,855,573,864 touches in all. The reach lines give each gate's
   count over the three legs, by mode; where a count is small, it is also
   given by leg (as it is, straggler, tags 8):

   | gate | plain | ConstantTime | Background |
   |---|---|---|---|
   | `missPlaced` (W9's path) | 13 (4, 7, 2) | 0 | 4 (4, 0, 0) |
   | miss's skip (W4d's gate) | 76,937 | 0 | 16,479 |
   | the table's skip (W4c's gate) | 204,906 | 30,978 | 57,124 |
   | again | 2 (1, 0, 1) | 0 | 0 |
   | fresh | 56,508 | 15,506 | 18,774 |
   | stores, stamp | 48,724,867 | 13,840,564 | 15,236,004 |
   | stores, realign | 1,914,108,899 | 587,968,310 | 624,533,567 |
   | stores, wait | 0 | 89,326,028 | 201,251,534 |
   | silent | 6,714,105,438 | 2,036,855,943 | 2,128,822,238 |
   | behind (reported) | 0 | 0 | 0 |

   A gate at 0 is not reached in that mode, and its path there is NOTHING
   READ, not a pass. That applies to:
   - `missPlaced` and miss's skip in ConstantTime;
   - again in ConstantTime and Background;
   - the wait's stores in plain.
4. **Holds.** No touch stored its push twice in either reading:
   - W7's scenario (Background, spread, 20,000 rounds): 172,800,000 touches,
     twice 0, behind 0. W7's own gates:
     - miss's skip 492,669, the table's skip 136,522, the wait's stores
       27,111,567;
     - `missPlaced`, fresh, again and the stamp's stores read 0, not
       reached there.
   - W11b's race on the head: no round read twice, plain (99 looked again)
     or under `-race` (29). Both are over the 10 the clause asks.
5. **Holds.** Bare reads 0 in every count read: the fuzzer's four legs and
   W7's.
6. **Holds** (it qualifies the verdict). 23f03b6's own fuzzer, with the
   straggler, for an hour (12,403 cases, 2.3 M ops/s), read 2 touches twice,
   in 2 cases, both plain. Both have W9's shape, as legs 1, 2, 4 and 5 have
   on 23f03b6: the claim's store, then the table's into last at or after the
   tick with the flag false, and behind.
   - Case 3313: Capacity 96, Horizon 1 min, Stash 28, Overflow 85, Hot 5.
   - Case 4456: Capacity 69, Horizon 96 ms, Stash 53, Overflow -1, Hot 6.
     Its key 0x0 is goroutine 0's own key 0 (`mix64(0)` is 0), so it is the
     straggler's.

   Behind on 23f03b6 reads plain 8, ConstantTime 2, Background 2. Each is a
   look again after a placed history. Two of those 12 read twice. From the
   counts (again 0 there, and no touch stored more than twice), the other 10
   are fresh starts.

   The head's `missPlaced` is the same reach on its tree: plain 13,
   ConstantTime 0 and Background 4 over the three legs, and 7, 0 and 0 in
   the straggler's leg. How many of the head's 17 would have read twice on
   W4d's library is not read.
7. **Holds.** On the head under `tfdrip,tfpush`, these pass:
   - W11b's legs (plain and under `-race`) and race;
   - `TestPushPaths`;
   - W11's `TestDripHotRestoreLegs`;
   - W4e's `TestDripHotRestorePushOnce`.

**Reported.**

- The fuzzer's W10b checks:
  - In the leg as it is and in the straggler's: 0 over, and no failing
    answer or log of any kind. They checked 1,916,069,726 and 1,761,592,135
    answers' Upper, and 48,795,764 and 42,888,579 keys' logs at rest.
  - In the tags leg the checks are off by design (`go/fuzzconc_test.go:45`):
    0 checked, nothing read there.
  - 23f03b6's older fuzzer has no W10b part.
- W7's other lines: no wrong answer, no log lost, and 0 over at rest (W10b,
  639,931 logs), with 783 Degraded answers set apart.
- The predictions, against the reading:
  - twice, behind and bare read 0 on the head, as predicted;
  - again was predicted rare if it happened at all, and reads 2;
  - `missPlaced` was predicted rare or absent, and put with the straggler.
    It reads 17, 8 of them in the leg as it is;
  - fresh reads 90,788 on the head against `missPlaced`'s 17. At least
    90,771 fresh starts came without the hot restore's look again, as the
    prediction said they could;
  - the power leg was predicted unknown, and reads 2.
- Rates:
  - 2.2, 2.1 and 2.2 M ops/s on the head with `tfdrip,tfpush`;
  - 2.3 on 23f03b6 with its older fuzzer;
  - W10b's were 2.5 with `tfdrip` and 2.4 with the straggler, 5 minutes
    each on a66af40's tree.

  The runs differ in length and tree, so this is not `tfpush`'s cost.
- Checked after the reading, not in the rule: on every count line, calls
  equal none + once + fresh + again + twice, and stores equal once + 2 ×
  (fresh + again + twice). No touch stored more than twice.
- Times:
  - the library's checks, 2 min 4 s;
  - the sight, 1 min 30 s; a race run took 27.6 s on 23f03b6, 28.2 s on the
    head and 22.7 s under `-race`;
  - W7, 48.3 s;
  - each fuzzer leg, 3,600 s;
  - the whole run, 4 h 4 min 27 s.

**Verdict: W13 passes its rule.** Clauses 1 to 5 and 7 hold, and clause
1's gate held none back. Clause 6 holds, so clause 3 is read with the
count's power under 23f03b6's fuzzer shown: 2 touches twice in an hour
there, both plain.

What W13 reads of W4e's library:

- Read: no touch on the head stored its push twice:
  - 0 in the fuzzer's 27,855,573,864 touches over three hours;
  - 0 in W7's 172,800,000;
  - 0 in 220,000 race rounds, 128 of which looked again.

  That holds on the paths the gates show taken, and says no more there than
  their counts.
- Read: the head's fuzzer reaches W9's path (the hot restore's look again,
  `missPlaced`) 17 times in three hours, in plain and Background, never in
  ConstantTime. None of those touches stored twice. The same reach (behind)
  on W4d's library read 12 times in the straggler's hour, and 2 of those
  read twice. The race reads the path more densely: 128 rounds looked again
  on the head with none twice, against 96 of 96 twice on 23f03b6.
- Read: behind is 0 on the head in every count, so the `pushed` flag missed
  no store there.
- Read: W4d's push-again half is taken in Go. Again reads 2, both plain:
  one in the leg as it is and one in the tags leg. W8 found it never taken
  in the model.
- Read: every touch is noted (bare 0), and no touch stored more than twice.
- Read: the notes compile out. The default build's answers, `tfdrip`'s and
  `tfdrip,tfpush`'s are unchanged.

What W13 leaves:

- Again's two touches:
  - The class has no sight, and the instrument keeps a detail only for
    twice. So it is not read which copy replaced the push, or whether the
    event ended in the log once.
  - W8's model never takes that half, while Go does. Whether the model
    lacks the copy that makes it is not read.
  - What to do with it (a probe of the again path, a look at W8's model, or
    nothing) is the user's decision.
- In ConstantTime the fuzzer reached neither W9's path (`missPlaced`) nor
  miss's skip in three hours, though W11b's legs 2 and 3 reach `missPlaced`
  by hand in that mode. Clause 3 reads NOTHING READ for those paths there.
- The power under the fuzzer rests on 2 touches, both plain. ConstantTime
  and Background show none on W4d's library.
- The tags leg's W10b checks are off by design. W13 read only its counts
  there.
- As the rule named, not covered:
  - a lost push;
  - miss's gate and the again class have no sight;
  - `TF_NOCAS2`;
  - the soak, the fuzzer under `-race`, other Horizons and Resolutions;
  - W4c's double push in W7's scenario;
  - the default build's speed with the notes compiled out, and `tfpush`'s
    cost.

## W14: the drip log's gain under load

**Written after W13**, from W12's "not covered": every reading of target
7's metric (W4 to W12) had tfbench's 2,000 velocity keys in a filter of
Capacity 200,000 that held them all, so no velocity key was evicted,
stashed, dropped, shed or kept waiting. W14 reads the metric while the
table is full, in both builds, and asks whether the drip log keeps its
gain there. The user chose:

- both legs below: the scorecard's load, and a load that reaches the
  velocity keys;
- leg 2's form, the velocity keys as the load (proposed: a flood of light
  keys would not reach them, since eviction takes the lightest key of the
  8 slots in the new key's two lines), with the hot entries pinned to
  tfbench's 1,838 rather than left at their default for the smaller table;
- the judging: leg 1 keeps at least half the gain it has with no load
  (W12's form); leg 2 does no harm. Judging 0.050 itself on leg 1, and
  half the gain on both legs, were set aside;
- a watcher in the test, the library unchanged, over counters in the
  library behind a build tag.

**What load does to a key's log** (from the code, not a reading):

- The victim is the slot of the new key's two lines with the fewest events
  in the horizon, then the oldest last touch (`cuckoo.go:209-225`,
  `register.go:361`). Most of tfbench's churn keys hold one or two events;
  every velocity key holds more but the sparse ones (about 3 a horizon).
  tfbench's churn alone evicts no key at load 1.00 and 4,577 at 1.15
  (`go/verify/expected/exact.txt`).
- An evicted history goes to the stash whole, word0 and its log with it,
  and the touch that restores it pushes its event into the log
  (`cuckoo.go:61-68`).
- A history the stash drops goes to the overflow table as a 21-bit entry
  (fingerprint, count, time slice), its log lost; one with fewer than
  SpillMin (4) events in the horizon is shed (`overflow.go:255-307`).
  Count adds the key's own entry to the slot's answer, Degraded set
  (`touch.go:29-31`, `:178-195`); a raised floor adds to Upper alone. A
  key that comes back starts a fresh slot.
- A first touch that finds both lines full waits: in the wait room under
  Background, placed by its goroutine; in the stash under ConstantTime
  (marked `deferMark`), placed by calls.

**The probe** (`go/swload_test.go`, `TF_EXP=1`, both builds; the library
unchanged, `lab/w14.sh` checks it against 495f1aa's).

- **The workload.** tfbench's velocity workload (`swLagRecord`, W12's
  record of `swWorkload`) on a seed, from one calling goroutine: at each
  tick its events (`CheckAndTouchAt` at their ticks), then the churn's
  operations of the tick, then from the first horizon on the tick's query
  (`VelocityCountAt`), whose exact count is over the key's own events.
- **Leg 1, the scorecard's load.** tfbench's filter (Capacity 200,000,
  Horizon 60 s, Resolution 1 ms) beside tfbench's churn stream at load
  1.00 (seed 2) and 1.15 (seed 4), and with no churn. The churn is
  `workload.Churn`'s generator over the workload's five horizons instead
  of four (`swLoadChurn`, a copy whose first four horizons are
  `workload.Churn`'s op for op), with tfbench's windows.
- **Leg 2, the velocity keys as the load.** The workload alone in filters
  of Capacity 1,739, 1,000 and 500 (its 2,000 keys at 1.15, 2 and 4 times
  Capacity), Horizon and Resolution as tfbench's, the stash and the
  overflow table at their defaults, the hot entries pinned to 1,838.
- **Modes.** Plain; Background (its placer makes runs vary: five repeats);
  ConstantTime (functional, no pad, as `TF_CT=1` runs it: it places
  waiting keys only in `place`'s forced step, more than one key per 16
  stash entries waiting, which reads no clock, so its runs replay). At
  Capacity 200,000 that step needs about 920 keys waiting
  (`filter.go:287-291`, `ct.go:97`), so a key can wait far longer than
  under a pad: ConstantTime's waits here are the functional mode's.
- **Seeds.** (9, 9) and (2026, 41) to (2026, 44), as W12's leg A.
- **The watcher** (`swLoadWhere`). Before each touch and each query of a
  velocity key, the key is found as `read`'s locked branch finds it
  (`touch.go`: under its claim stripe, the table read stably, then the
  stash and the wait room, again until no move of its stripe overlapped a
  miss), but the place is named: table (and its slot), stash (evicted),
  waiting (a wait-room entry, or a stash entry marked `deferMark`), or
  none (also a history past the horizon); with `tfdrip`, the log valid or
  saying nothing; and what the overflow table holds of it, read over the
  horizon: an entry of its own (Count above 0: part) or a raised floor's
  alone (Upper above Count, 0: floor). It calls no Count, touch or take.
  - Queries: errors and exact counts by class (place, part, log).
  - Touches: the class before a touch names where it restores from: the
    table (a hit), the stash, a wait, dropped (none, with a part: a fresh
    slot, the log lost), floor (none, a floor's alone), shed (none, the
    key's newest event within the horizon), or new (none, no event within
    the horizon: a new life, no loss).
  - Left: a key seen in the table, then elsewhere while its newest event
    is within the horizon (a lower bound on its evictions). Moved: a key
    seen in the table at two slots in turn (a copy of its log).
  - The filter's counters: Evictions, Relocations, Shed, Lost, Floored,
    Folded, Waited, Promoted, HotRefused.
- **Sight** (`TestShortWindowLoadWhere`): states made by hand through the
  filter's own paths in filters of Capacity 1,000, each read by the
  watcher and then checked against a sign it does not read:
  - a key in the table; one a horizon and a tick past its touch (none);
    one never touched; with `tfdrip`, one touched an earlier unit behind
    its last touch (log says nothing);
  - the table filled with keys of 2 events, V of 1 evicted by a newcomer
    on its line: stash (Evictions 1; V's next touch Restored);
  - no stash, V of 4 events among keys of 5: dropped, a part (Evictions 1,
    Folded 1, Count Degraded with its 4);
  - no stash, V of 1 among keys of 2: shed (Evictions 1, Shed 1; V's next
    touch sees no history);
  - the table filled, a new key in Background with nothing placing: wait
    room; in ConstantTime: waiting in the stash (Waiting 1 each).

  `TestShortWindowLoadSource` reads the source of each class. The floor
  class has no hand-made state.
- **The check** (`TF_W14_LEG=check`, (9, 9)): the churn at both loads
  against `workload.Churn` (the ops before four horizons equal op for op,
  no churn key a velocity key, the last op at or past four horizons); at
  leg 1's 1.15 and leg 2's Capacity 500, in plain mode and ConstantTime, a
  run with the watcher against one without (`offmism`) and, in
  ConstantTime, a second watched run (`replaymism`). It prints no metric.

`lab/w14.sh` runs: the sight (`TestShortWindowLoadWhere`,
`TestShortWindowLoadSource`) in both builds; the check in both builds; leg
1 in both builds (105 runs each: five seeds, three loads, seven runs a
seed and load: plain, ConstantTime, Background five times); leg 2 in both
builds (105 each); under `-race`, `tfdrip`, (9, 9), leg 2 at Capacity 500
in Background, once; and `lab/w14.py`, which reads both builds' lines and
prints the clauses, the reach and the reported tables. Caps: 5 minutes for
the sight, 30 for the check, 60 for each leg's run, 60 for the `-race`
run. Outputs in `lab/w14/`.

**Before the rule** (not a reading).

- The probe compiles and vets in both builds, compiles under `-race`, and
  is gofmt-clean. `TestShortWindowLoadChurn` and
  `TestShortWindowLoadSource` (no filter) passed: the churn's first four
  horizons are `workload.Churn`'s (969,657 ops at 1.00 and 1,115,315 at
  1.15, none differing), and no churn key is a velocity key.
- `lab/w14.py` was tried on fabricated lines, read from no filter: a set
  meant to hold (with a Background excess over 1.01 times inside the
  default build's spread, and leg 1 at 1.00 restoring no key from outside
  the table) held, exit 0, clause 4 noting "judged on keys the table
  kept"; a set with two planted failures (a leg 1 gain under half; a
  Background excess beyond the spread) failed clauses 4 and 5 on those
  alone; a set missing one seed's leg 2 runs read clause 3 FAILS and
  clause 5 NOTHING READ (in part); an empty set read NOTHING READ for every
  clause, exit 1.
- A fresh reviewer read the probe, the runner and the joiner before any
  run. Fixed from it: the overflow table's part read from `estimate`'s
  ok, which a raised floor also sets (now an entry of the key's own, and a
  floor class apart); clause 4's note on keys the table kept; the reported
  tables; a missing point read FAILS (now NOTHING READ, in part); the
  check's churn assertions; sight signs read from what the watcher reads;
  `lab/w4e/exact-drip.txt` added to the runner's clean-tree check.
- At the user's word, on a scratch clone (`~/lab-sw-out/dry14`, outputs
  in `~/lab-sw-out/dry14-checks`), both builds:
  - the sight read every state as made, every sign true;
  - the no-load control, leg 1 with no churn in plain mode on (9, 9),
    equals `swRecord` query for query (mism 0), and its window sums equal
    `verify/expected/exact.txt`'s and `lab/w4e/exact-drip.txt`'s (1 s:
    19,185 and 6,798 of 114,779);
  - the check: the churn as above; with the watcher off, 0 answers differ
    at both points in plain mode and ConstantTime; a second ConstantTime
    run differs in 0;
  - `lab/w14.sh` refused the clone uncommitted (exit 2); after a scratch
    commit, with `TestPartCells` in each test's place, it found no library
    file changed, logged "NO W14 LINES" at every step, read NOTHING READ
    for every clause and exited 1. Its trial showed a NOTHING READ clause 4
    or 5 printing its holding sentence; mended before this rule, and the
    legs' cap lowered from 150 to 60 minutes (the only differences from
    the scratch clone's runner and joiner; the probe is the one that ran).
  - `lab/w14.py` on those outputs (the control's files as leg 1's, no
    filter run): clause 1 holds, clause 2 reads NOTHING READ in part with
    (9, 9)'s sums equal, clause 3 fails on its run counts alone, and the
    reach and reported tables print from real lines.
- At the user's word, a quiet Background check on the same clone
  (`TF_W14_QUIET=1`: each run's identity, mismatch count and time alone;
  outputs `bg-*.txt` there), (9, 9), one repeat: leg 1 at 1.15 and leg 2
  at Capacity 500 in both builds, and the runner's `-race` point. Every
  run ended with its "W14 done:" line and passed; the `-race` run
  reported no data race.
- Not read before the rule: no loaded run printed its metric or its
  counters; Background ran once a point at leg 1's 1.15 and leg 2's 500
  alone; leg 2 ran at Capacity 500 alone; the seeds other than (9, 9) ran
  nowhere.
- Sizing from the check: a loaded leg 1 run about 5 s, a leg 2 run at
  Capacity 500 about 2 s, a run with no churn under half a second; the
  watcher adds 2 to 9% at leg 1's 1.15 and 18 to 42% at leg 2's 500
  (times to a tenth of a second). Background: 2.3 to 2.5 s at leg 1's
  1.15, 1.6 s at leg 2's 500, 25.5 s under `-race`. The reading about half
  an hour.

**Predictions** (from the code and tfbench's exact output; none judged):

- Leg 1 at 1.00: tfbench's churn alone evicts no key, so in plain mode no
  velocity key restores from outside the table, and clause 4 notes it
  judged on keys the table kept there. Background and ConstantTime park
  first touches that find both lines full; a velocity key's new life (a
  sparse key past a horizon without an event) can wait.
- Leg 1 at 1.15: a few velocity keys evicted, nearly all sparse (the
  lightest), most restored from the stash with their logs. The gain kept
  on every seed and mode, near its no-load size.
- Leg 2: both builds' errors rise as Capacity falls; every source reached
  at 1,000 and 500 (waits only in Background and ConstantTime); floors
  rare or none. `tfdrip` at or under the default build: a dead or fresh
  log falls back to the register, and the overflow table's part is the
  same in both builds. The gain shrinks with Capacity.

**Rule (before the reading).** W14 holds if:

1. sight: the sight's two tests pass in both builds, with every state's
   "W14 where" line and its sign true;
2. no load: leg 1's runs with no churn in plain mode equal `swRecord` query
   for query on every seed in both builds, and on (9, 9) each window's
   error and exact sums equal `lab/w4e/exact-drip.txt`'s (`tfdrip`) and
   `verify/expected/exact.txt`'s (default);
3. the harness: the library unchanged (the runner's step 0); the check's
   churn as above at both loads, 0 answers differing with the watcher off
   at both points in plain mode and ConstantTime, and 0 between
   ConstantTime's two runs, in both builds; 105 runs a leg in each build,
   each test with its "W14 done:" line; the `-race` run reports no data
   race;
4. the gain holds under the scorecard's load: at 1.00 and 1.15, on every
   seed and in every mode, the gain (the default build's mean of the five
   windows less `tfdrip`'s, each build's run on the same seed, load and
   mode; in Background each build's mean over its five repeats) is at
   least half the gain with no load on the same seed and mode;
5. no harm where the velocity keys are the load: at every Capacity, on
   every seed and in every mode, `tfdrip`'s mean and each of its five
   windows are at most 1.01 times the default build's. In Background
   (each build's mean over its five repeats) an excess is harm only if
   `tfdrip`'s value also exceeds the default build's by more than the
   default build's spread there (its highest repeat less its lowest): the
   placer's noise alone moves the two builds apart where the log changes
   nothing.

A point not read (a seed, load, Capacity or mode without both builds'
runs) makes its clause NOTHING READ in part, never a pass. A run cut by its
cap, or one that hangs, is the probe's fault, not a reading: the lines its
test printed before it stopped are reported, and its leg's points stand as
not read until it is mended and run under a rule of its own. Reach is
reported and bounds what clauses 4 and 5 read: per leg, point, mode and
build, how often each source was taken; a source at 0 is NOTHING READ for
that path there. Where leg 1 restores no velocity key from outside the
table and none leaves it, clause 4 says so: its gain was judged on keys
the table kept.

If clause 1 fails, the classes and the reach are not read; the metric is.
If clause 2 or 3 fails, W14 says nothing of clauses 4 and 5 until the
difference is read against the code. If clause 4 fails at a point, the
drip log loses more than half its gain under the scorecard's load there,
and the record names where (seed, load, mode). If clause 5 fails, the log
does harm where the velocity keys are the load, and the record names
where (seed, Capacity, mode, window). Target 7's status is the user's
either way. A failure is read against the code before anything else: a
defect of the library, or of the probe, said as such.

Reported, not judged: the five windows and the mean of every point in
both builds beside 0.050, and the gain at each load and Capacity; the
classes' shares and errors per event; the reach, left and moved; the
filter's counters; Background's spread over its repeats; times.

Not covered: concurrency beyond Background's placer (W12's leg B); lag
with load; loads between those read; other Horizons and Resolutions;
ConstantTime's pads; churn keys as heavy as the velocity keys; other
traffic; the floor class's sight.

### W14's reading (1e18e7f's tree; `lab/w14.sh`, outputs in `lab/w14/`)

`lab/w14.sh` ran end to end (17:44:50 to 17:55:16, Go 1.24.7) and exited 0,
and its joiner exited 0. Its first check found no library file different
from 495f1aa's. Every run printed its line: 105 a leg in each build, and
the `-race` run's one. No raw file holds a panic, a failing test or a data
race.

1. **Holds.** Sight, in both builds: the nine states read as made, every
   sign true; `TestShortWindowLoadSource` passes.
2. **Holds.** With no churn in plain mode, leg 1 equals `swRecord` query for
   query on all five seeds in both builds, and (9, 9)'s window sums equal
   the exact files'.
3. **Holds.** The harness, in both builds:
   - the churn's first four horizons equal `workload.Churn`'s at both
     loads (969,657 and 1,115,315 ops), and no churn key is a velocity key;
   - with the watcher off, 0 answers differ at both points in plain mode
     and ConstantTime, and ConstantTime's two runs differ in 0;
   - 105 runs a leg;
   - the `-race` run (leg 2 at Capacity 500, Background) reported no data
     race.
4. **Holds.** At 1.00 and 1.15, on every seed and in every mode, the gain
   is the gain with no load. Every window's error and exact sums under load
   equal no load's in both builds, so each point's gain is its seed's
   no-load gain (0.0216 to 0.0222) against a floor of half that. The joiner notes clause 4 judged on keys the
   table kept at 1.00 in plain mode: no velocity key restored from outside
   the table there, and none left it.
5. **Holds.** At Capacity 1,739, 1,000 and 500, on every seed and in every
   mode, `tfdrip`'s mean and five windows are within 1.01 times the default
   build's, but at one point. That point holds by clause 5's Background
   spread rule, with the excess taken as `tfdrip` less the default build,
   as the rule names it:
   - the point: (2026, 41), Capacity 1,000, Background, the 30 s window;
   - `tfdrip` reads 0.1964 over its repeats (0.1942 to 0.1985) against the
     default build's 0.1937 (0.1872 to 0.1984), 1.0141 times;
   - the excess, 0.0027, is inside the default build's spread there
     (0.0112).

   In plain mode and ConstantTime, where both builds place keys alike, the
   same seed's 30 s error sums at 1,000 differ by 4 events of 661,532.
   Every other point holds on the 1% alone.

**Reach** (touches by where they restored from, summed over each point's
runs: 5 a mode, Background's 25; the default build's and `tfdrip`'s, which
differ only in Background):

- Leg 1 at 1.00:
  - plain: the table alone, with 13,204 moves (relocations copying a
    velocity key's log) and none leaving;
  - Background: waits 639 and 675;
  - ConstantTime: waits 403.
- Leg 1 at 1.15:
  - plain: stash 15, left 34, moved 59,568;
  - Background: stash 71 and 68, waits 1,628 and 1,725, left 206 and 205;
  - ConstantTime: stash 9, waits 1,207, left 20.
- Leg 2 at 1,739: no eviction in any mode; waits alone (Background 11,372
  and 11,836, ConstantTime 7,274).
- Leg 2 at 1,000 and 500: every source but the floor, in every mode
  (waits in Background and ConstantTime only). Plain at 1,000: stash
  138,839, dropped 591,493, shed 72,438. At 500: stash 93,602, dropped
  2,492,180, shed 662,564.

NOTHING READ:
- the floor class everywhere: no run raised a floor (Floored 0);
- dropped and shed on leg 1 and at Capacity 1,739;
- the stash at leg 1's 1.00 and at Capacity 1,739, in every mode;
- waits in plain mode, which parks no key.

**Reported.**

- **Leg 1.** Over the seeds, at every load and mode:
  - the default build reads 0.0669 (1 s 0.1667);
  - `tfdrip` reads 0.0449 (1 s 0.0594), 0.0447 to 0.0452 a seed.

  Both sit inside W6's ranges (`tfdrip` 0.0445 to 0.0454, the default
  build 0.0661 to 0.0674). `tfdrip` is under 0.050 on every seed at every
  point.
- **Leg 1's counters, a run:**
  - at 1.00: no eviction; 220,263 relocations in plain;
  - at 1.15 in plain: 35,710 evictions, 1,430,281 relocations, 46 sheds;
  - waits in Background: 98,000 and 101,000 at 1.00, 340,000 and 345,000
    at 1.15;
  - waits in ConstantTime: 206,833 at 1.00 and 592,527 at 1.15.

  The velocity keys left the table 34 times over the five plain runs at
  1.15. The stash gave back 15 of them, and the metric is still the
  no-load metric (answers were not compared one by one).
- **Leg 2 at 1,739** (2,000 keys at 1.15 times Capacity): they fit. There is
  no eviction, and the metric reads as with no load, to the fourth decimal.
- **Leg 2 at 1,000 and 500** (2 and 4 times Capacity), over the seeds:

  | | default | `tfdrip` | gain | share of no load's gain |
  |---|---|---|---|---|
  | 1,000, mean | 0.6169 | 0.6001 | 0.0168 | 76% |
  | 1,000, 1 s window | 2.07 | 1.99 | | |
  | 500, mean | 1.0504 | 1.0426 | 0.0077 | 35% |

  The per-run counters at 1,000 and 500, in plain mode:

  | | evictions | sheds | folded | lost | floored |
  |---|---|---|---|---|---|
  | 1,000 | 161,597 | 119,117 | 14,639 | 0 | 0 |
  | 500 | 651,301 | 611,858 | 20,682 | 0 | 0 |

  Promotions fall from 521 a run to 446 and 181.

  In plain mode at 1,000 the queries split as follows, with the error per
  exact event the same in both builds within 0.002:

  | key | share of queries | error per exact event |
  |---|---|---|
  | in the table, no overflow part | 48.5% | 0.037 default, 0.036 `tfdrip` |
  | in the table, with a part | 10.5% | 0.44 |
  | in the stash | 3.6% | |
  | nowhere, with a part | 16.6% | 0.86 |
  | nowhere, no part (the history shed) | 20.7% | 1.00 |

  At 500, 68% of queries find the key nowhere. The error under overload is
  the shed and dropped histories', the same in both builds. The log's gain
  is on the keys still in a slot.

  A 1 s error above 1 means some answers overcount the short window. From
  the code, an overflow entry keeps a count and a time slice. That is not
  separated here.
- **Checked after the reading, not in the rule.** In plain mode and
  ConstantTime, where both builds place every key alike, the builds were
  compared window by window on leg 2. `tfdrip`'s error sum is above the
  default build's in 5 of 150 seed, Capacity, mode and window points, by
  1 to 14 events in 0.66 to 1.93 million: (2026, 41) at 1,000, plain, 30
  s; at 500, plain and ConstantTime, 30 s; (2026, 43) at 500, plain, 15 s;
  (2026, 44) at 500, plain, 30 s. At 500 the 1% margin (about 0.010 of the
  mean) is wider than the whole gain (0.0077), so clause 5 alone says
  little there; the paired sums say the log adds at most 14 events'
  error at any of those points.
- **Times:**
  - sight, 3 and 2 s;
  - the check, 35 and 33 s;
  - leg 1, 2 min 29 s and 2 min 37 s;
  - leg 2, 1 min 43 s and 1 min 48 s;
  - `-race`, 36 s;
  - the whole run, 10 min 26 s, against the rule's estimate of half an
    hour.
- **The predictions against the reading:**
  - Leg 1 came out as predicted:
    - at 1.00, plain mode restored nothing from outside the table, and
      Background and ConstantTime parked velocity keys' new lives;
    - at 1.15, a few velocity keys were evicted and came back from the
      stash;
    - the gain was kept, and is not only near its no-load size but equal
      to it.
  - Leg 2 came out as predicted but for one part. Predicted and read:
    - every source but the floor was reached at 1,000 and 500;
    - no floor was raised;
    - `tfdrip` read at or under the default build, but for Background's
      noise in the 15 s to 60 s windows at 1,000 and 500, and the paired
      sums' few events above;
    - the gain shrank with Capacity.

    Not as predicted: the errors rise only from 1,000. At 1,739 the keys
    fit.

**Verdict: W14 passes its rule.** Clauses 1 to 5 hold. Clause 5 holds at
one point, (2026, 41) at Capacity 1,000 in Background at 30 s, by its
Background spread rule alone.

What W14 reads of W4e's library:

- Read: the scorecard's load barely touches the velocity keys. At 1.15 in
  plain mode, 34 of their 6.5 million touches found them gone from the
  table. Under tfbench's own load, the churn stream at 1.00 and 1.15
  beside the velocity keys, every window's error sum is as without load,
  in plain mode, Background and ConstantTime, in both builds. Eviction takes the lightest keys, the churn's. The few velocity
  keys that leave come back from the stash, or from a wait, with their
  histories whole. Relocations copy their logs tens of thousands of times
  with no window's error sum moved. The drip log's gain under the
  scorecard's load is its whole gain: target 7's metric reads 0.0447 to
  0.0452 there.
- Read: where the velocity keys are the load, at 2 and 4 times Capacity,
  both builds read 0.59 to 1.07, far past 0.050, from histories shed or
  dropped to the overflow table. The log keeps 76% and 35% of its gain and
  does no harm beyond 1% in any window, but at the one Background point
  above, inside the placer's noise. At 1.15 times Capacity the keys fit,
  no key is evicted, and leg 2 reads as with no load.

What W14 leaves:

- **Paths not reached.** The floor class was not reached, since no floor
  was raised. Dropped and shed were never reached on a velocity key under
  the scorecard's load, which cannot make them.
- **Under overload the error is the filter's, not the log's.** Both builds
  are 12 to 21 times target 7's bar, from sheds and overflow parts. The 1 s
  window's overcount was not separated. Whether a filter run at 2 to 4
  times its Capacity matters is a question of the filter's sizing, not of
  the short window.
- **As the rule named, not covered:**
  - concurrency beyond Background's placer;
  - lag with load;
  - loads between those read;
  - other Horizons and Resolutions;
  - ConstantTime's pads;
  - churn keys as heavy as the velocity keys;
  - other traffic.

Target 7's status is the user's.

## W12b: the lag's loss split by the drip log's class

**Written after W14**, from W12's "What W12 leaves": how much of the larger
lags' loss is the dead logs' and how much the valid logs' mistimed events.
W12 read the gain wear with lag: at `stall:1000` (1% of events up to a
second late) 0.74 to 0.77 of it is left, and `tfdrip` reads 0.0502 to
0.0512, the one reading of target 7's metric over 0.050. The dead logs'
share of the queries rose with it (11.2 to 12.4% of them at `stall:1000`,
30 to 33% by exact count), but W12 did not separate the two. W12b reads the
same queries in both builds and splits the loss by `tfdrip`'s class at each
query. Its answer decides whether a change to the log (bringing a dead log
back sooner, or not killing it) is worth an arm of its own; that decision is
the user's. The user chose:

- the judging: whether the dead logs carry at least half of the gross loss
  at `stall:1000`, on every seed. The parts are signed (below), and the
  gross loss is the sum of those above 0, so a class that gained at the
  delay cannot lift the dead logs' share. Set aside: the net loss, under
  which a dead part over the whole loss would hold; whether `stall:1000`
  would read at most 0.050 with the dead queries' no-delay gain restored
  (it would once they carry 3.9 to 20.3% of the loss, by seed: too weak to
  judge, so reported as an estimate); the harness alone;
- the valid logs split in two by reading the slot: clean, a log whose
  field is the one its delivered events would leave delivered in order;
  off, one whose field differs.

Proposed, and taken: leg A alone (one goroutine, plain mode: it replays, so
both builds answer the same queries; W12's leg B read within 0.00006 of leg
A at every delay); the loss counted per query against the same seed's run
with no delay, window by window; the per-query records kept outside `lab/`;
how long each dead log has been dead, and whether its key is a hot entry,
reported.

**What lag does to a key's log** (from the code, not a reading):

- A dead log's query answers as the default build's: `estimate` reads the
  log only when the window cuts a bucket, the valid bit is set and the
  register's newest unit is the last touch's (`register.go:509`); else both
  builds read the same register by the same code. No code that writes word1
  reads the log, and the default build's top 12 timestamp bits, where
  `tfdrip` keeps the log, are 0 below 2^34 (`drip_off.go:9`). So at a delay
  a dead query's gain is 0: what the dead logs lose is the gain the same
  queries had with no delay.
- In leg A a log dies only by an event an earlier unit behind its key's
  last touch (`next0`'s second case, `drip.go:50-51`). `dripForget` is
  called on each such event (`touch.go:372-373`) but finds the log already
  dead (`drip.go:151`). It stays dead until an event 11 quanta or more
  after the last touch, by quantum index (`drip.go:45`): 1.56 to 1.88 s. A
  key touched more often stays dead.
- A hot key's touch goes through `next0` on word0 as a register's does
  (`touch.go:347-358`; `stamp`, `touch.go:436`), so its log dies the same
  way; but its hot entry counts a late event at the event's own unit
  (`hot.go:212-218`), so the register's reason to kill the log does not
  hold for it. Reviving a log sooner and not killing a hot key's log are
  different changes.
- An event at or behind the last touch in its unit is pushed as a gap of 0
  at the last touch's quantum (`next0`'s third case): the valid logs'
  mistiming. One in the last touch's own quantum leaves the field it would
  leave in order. A field of 0 (twelve events in one quantum) is valid; the
  class reads the valid bit alone.

**The split** (per seed and delay). Each query q (W12's: the key and window
of tick now) has in both builds the same exact count x(q) of the events
delivered by then, and its error e(q) = |answer - x(q)|, e_d in the default
build and e_t in `tfdrip`; X_w is the run's exact sum over window w's
queries. The gain is g(q) = (e_d(q) - e_t(q)) / (5 X_w); summed, it is the
default build's mean less `tfdrip`'s, as W12 reads it. The loss of q is
g_none(q) - g(q), from the same seed's run with no delay (its own X_w);
summed, it is the gain with no delay less the gain at the delay. The class
c(q) is `tfdrip`'s at the delay, read before the query's count: none, dead,
clean or off. Each class's part is its queries' loss, each taken over all
five windows; the parts are signed and sum to the loss. The gross loss is
the sum of the parts above 0; the dead logs' share is their part over it.
The dead part is the dead queries' no-delay gain, not a bound on a revival
(a revived log would answer them with its own error, better or worse, and
change later queries' logs); `tfdrip`'s mean less the dead part is reported
beside 0.050 as an estimate. All sums are integers until the joiner divides
once per class and window.

**The probe** (`go/swsplit_test.go`, `TF_EXP=1`, both builds; the library
unchanged, `lab/w12b.sh` checks it against 4a586d8's, and W12's three test
files `swlag_test.go`, `swinfo_test.go` and `velinfo_test.go` against
073d7da's, W12's; the head's library differs from 073d7da's by W13's tally
alone: `push.go`, an extra parameter of `miss`, and calls behind `pushOn`, a
constant false without `tfpush`).

- **The runs.** W12's leg A (`swLagRecord`, `swLagOrder`, `swLagKeys`,
  `swLagFilter`, the run's `deliver` and `query`) on (9, 9) and (2026, 41)
  to (2026, 44), at W12's nine delays, each build once: 45 runs a build.
  `swLagA`'s loop is copied (it has no hook after a delivery), with a read
  of the slot after each delivery and around each query. Each run prints
  W12's line unchanged (`swLagLine`), whose integer fields must equal
  `lab/w12/a-default.txt`'s and `a-drip.txt`'s, and its own `W12b run:`
  line, and writes its per-query file.
- **The class** (`classify`, one read of the slot): `swLagClass`'s, and for
  a valid log the slot's 11-bit field against the in-order field of the
  key's delivered ticks (the newest twelve pushed by `next0`'s in-order rule
  from all ones: `swSplitField`): equal is clean, else off. Whether the key
  is a hot entry is read with it. Counted at each query, all to be 0:
  `lastbad` (a valid slot whose last touch is not the key's newest
  delivered tick); `clsmism` (the class against the one W12's own `query`
  counted); `qmoved` (word0 changed across the count).
- **Dead since.** From the slot after each delivery: the arrival of the
  delivery that turned the key's log from valid to dead, kept while it
  reads dead; a dead query's age is now less that. Beside it the probe's
  own model of `next0`'s cases (`swSplitNext`); their disagreements counted
  (`modelmism`, to be 0).
- **The per-query file.** Binary, little-endian, one a run, in the
  directory `TF_W12B_Q` names: a header (magic, version, build, seed,
  delay, head, record count); a record of 16 bytes a query (key, window,
  class with the hot bit, exact count, answer, dead age in ticks); a
  trailer (each window's error and exact sums, and a CRC-32 of the
  records). A file of the wrong size, magic, version, sums or CRC is
  refused. 240,000 queries a run; 90 files, about 350 MB.
- **The join** (`TestShortWindowLagSplitJoin`, the default build, no
  filter). For each seed and delay it pairs the two builds' files and the
  same seed's two files with no delay (the none pair with itself), checks
  each header against the pair it is read as, and counts: queries whose
  key, window or exact count differ between the builds, or whose key or
  window differ from the none run's; dead queries whose answer differs from
  the default build's (`deadmism`); records classed impossibly (`clsbad`).
  It prints integers per class and window (queries, exact sum, both builds'
  error sums at the delay and with none), per hot and register, and per age
  bin ([0, 1 s), [1, 2.5 s), [2.5, 10 s), [10, 60 s), 60 s and over). A
  refused file skips its pair with an error.
- **Sight** (`TestShortWindowLagSplitSight`): states made by hand, each
  checked as it is made, in `tfdrip`: a key touched in order (clean); a
  late event in its key's last quantum (clean); one an earlier quantum
  behind in the same unit (off), still off a push later, clean once eleven
  more pushes have left the field; one an earlier unit behind (dead, dead
  since its delivery), still dead 9 quanta on, clean 11 quanta on; a key
  promoted to a hot entry (32 events in a unit: clean), touched in order
  (clean), then an earlier unit behind (dead, hot); a key past the horizon
  touched again (all ones, clean; on a filter of its own); twelve events in
  one quantum (field 0, valid, clean); forty in-order touches at random
  gaps, the field equal to the in-order field at each. A key never touched
  reads none. In the default build every touched key reads no log. No
  model or slot-check disagreement anywhere.
- **No filter** (`TestShortWindowLagSplitField`, `TestShortWindowLagSplitFile`):
  the in-order field and the dead model against hand-worked values and an
  independent fold; the file's refusals (a byte cut or added, a record's
  key, class, hot bit or age changed, a sum changed, the magic or version
  wrong, an empty file); the join on two hand-made pairs, cell by cell.

`lab/w12b.sh` runs: the sight in both builds; the runs, default then
`tfdrip`; the join; and `lab/w12b.py`, which reads the lines, computes the
parts, and prints the clauses and the reported tables. It refuses an
uncommitted tree, a library or a reused test file that differs, a revision
that does not resolve, and a scratch trial without a per-query directory of
its own (or one at `/`, `$HOME`, the clone or beneath it). Caps: 10 minutes
for the sight, 60 for each build's runs, 30 for the join. Outputs in
`lab/w12b/`; the per-query files in `~/lab-sw-out/w12b-q/`, not committed
(the runs replay).

**Before the rule** (not a reading).

- The probe compiles and vets in both builds and is gofmt-clean;
  `TestShortWindowLagSplitField` and `TestShortWindowLagSplitFile` (no
  filter) pass in both builds.
- `lab/w12b.py` was tried on fabricated lines (`~/lab-sw-out/fab12b`, read
  from no filter, W12's lines taken from `lab/w12`): a set meant to hold
  held, exit 0; planted failures (a W12 field changed, a dead answer
  differing, a seed's dead share 0.21) failed clauses 3 and 4 on those
  alone, clause 5 printing not read; a set with one class part below 0, the
  dead logs holding 0.44 of the gross loss and 1.46 of the net, failed
  clause 5 alone; a pair the join refused failed clauses 3 and 4; a line
  cut mid-write failed clause 3 by name; a missing seed read NOTHING READ
  (in part); an empty set read NOTHING READ, exit 1.
- Fresh reviewers read each piece, and then the whole, before any run.
  Fixed from them: a check of the in-order field that compared it with
  itself (now against an independent fold); a trailer of sums alone, which
  could not vouch for the classes and ages the split rests on (now a
  CRC-32); a refused pair reading as not read (now a failure); a cut line
  crashing the joiner; no exact tie between the join's per-class cells and
  W12's own per-class sums (now in clause 3); clause 5 printed beside a
  failing clause 3 or 4 (now not read); a runner trial able to clear the
  reading's per-query directory (now refused).
- At the user's word, on a scratch clone (`~/lab-sw-out/dry12b`, the three
  files equal to the lab's; outputs in `~/lab-sw-out/dry12b-checks`, the
  per-query files in `~/lab-sw-out/dry12b-q`), with no delay alone:
  - the sight, both builds: every state as made, the test passing (15
    lines ok; in `tfdrip` the two off states off, the dead ones dead since
    the delivery, the hot key dead and hot);
  - the runs with no delay, both builds, all five seeds: each equals
    `swRecord` query for query (mism 0), no query dead or off, `lastbad`,
    `modelmism`, `clsmism` and `qmoved` 0, 240,000 records; every integer
    field of W12's line equals `lab/w12`'s, and the printed means too; each
    file 3,840,166 bytes;
  - the join on those pairs: 5 pairs, every count 0;
  - `lab/w12b.sh` refused the clone uncommitted (exit 2); after a scratch
    commit there, with a pattern matching no test in each test's place, it
    logged its overrides, found no library or reused test file changed,
    logged "NO W12b LINES" at every step, read NOTHING READ for every clause
    and exited 1, its per-query directory the scratch one;
  - `lab/w12b.py` on those outputs: clauses 1 and 2 hold, 3 and 4 read
    NOTHING READ (in part), 5 NOTHING READ.
- Not read before the rule: no delayed run; no dead query outside the
  sight.
- Sizing: a run 0.3 s (default) and 0.4 s (`tfdrip`); a build's five runs
  with no delay, with `swRecord`, 8 to 9 s; the join 0.3 s for five pairs.
  The reading about two minutes.

**Predictions** (none judged):

- Clause 2 and W12's lines as read before the rule. At every delay W12's
  line reproduces field for field: the probe only reads the filter.
- Dead queries only where an event arrives an earlier unit behind its key,
  as W12's lines count them: none at `none` and `stall:1`; 0 to 62 at
  `jitter:1` and `stall:10` (some seeds none); hundreds at `jitter:10` and
  `stall:100`; 18,828 to 22,012 at `jitter:100`, 26,872 to 29,816 at
  `stall:1000`, and 83,474 to 85,728 at `jitter:1000` (of 240,000).
- From W12's own figures (an independent review's estimate, not a reading):
  99.5 to 99.8% of the loss at `stall:1000` sits in the 1 s window. A seed
  holds if its dead queries' mean no-delay gain per 1 s query is 1.01 to
  1.09 times the run's average 1 s query's: a gain like the average
  query's gives a share of 0.46 to 0.49, one in proportion to the exact
  count about 1.3 (the dead queries hold 30 to 33% of the counts on 11 to
  12% of the queries). Dead keys run about 6 events a second, where the log
  holds the last second. The lean: clause 5 holds, at about two in three;
  its weakest seeds (2026, 41) and (2026, 44). The other parts are small
  (at `jitter:10` the whole loss is -0.000025 to -0.000130), so the gross
  and net loss should differ little.
- The dead part mostly in registers, not hot entries (about 500 promotions
  a run, W14's no-load runs). Long dead ages: a log revives only after
  1.56 to 1.88 s with no event, which a key of about 6 events a second
  seldom sees, so many dead queries should fall in the 10 s and 60 s bins.

**Rule (before the reading).** W12b holds if:

1. sight: the sight passes in both builds, every one of its 15 lines reads
   as made and its done line counts them;
2. no delay: on every seed, both builds' runs with no delay equal
   `swRecord` query for query, and no `tfdrip` query is dead or off;
3. the harness: the library equal to 4a586d8's and the three reused test
   files to 073d7da's (the runner's step 0); 45 runs a build, one W12 line
   and one W12b line for each build, seed and delay, each test with its
   done line; every W12 line's integer fields equal `lab/w12`'s line for
   its build, seed and delay; `lastbad`, `modelmism`, `clsmism` and
   `qmoved` 0 and 240,000 records at every run; the join reads 45 pairs,
   refuses none, and every count of differing keys, windows and exact
   counts, and `clsbad`, is 0; X equals both builds' exact sums, and each
   error column summed over the classes equals its W12 line's error sums;
   each class's cells summed over the windows equal `tfdrip`'s W12 sums for
   that class (dead, none, and clean with off as valid), and the clean,
   off and hot dead queries equal its W12b line's; the parts sum to the
   loss within 1e-12;
4. a dead query answers as the default build's, at every seed and delay;
5. the split: at `stall:1000`, on every seed, the dead logs' part is at
   least half the gross loss (the sum of the four class parts above 0).

A point not read (a seed or delay without its lines) makes its clause
NOTHING READ in part, never a pass; a pair the join refuses fails clauses 3
and 4. Clause 4 reads NOTHING READ if `stall:1000` has no dead query on
some seed, and clause 5 if a seed's net loss there is 0 or less (no loss to
split). A run cut by its cap, or one that hangs, is the probe's fault, not
a reading: its lines are reported, and its points stand as not read until
it is mended and run under a rule of its own.

If clause 1 fails, the classes are not read; W12's lines are. If clause 2,
3 or 4 fails, W12b says nothing of clause 5 until the difference is read
against the code: where clause 3 or 4 fails the joiner prints clause 5 as
not read, and where clause 1 or 2 fails the record sets it aside. If clause 5
holds, the dead logs carry most of the one loss that puts target 7's metric
over 0.050, and a change to the dead logs is the one aimed at it; how much
it would win back is that arm's reading, not this one's, and the hot and
register split says which change (reviving sooner, or not killing a hot
key's log). If it fails, the record names the larger parts at each seed by
signed size (off, clean, none), and whether the gap-0 push (off) is the
larger. Target 7's status is the user's either way. A failure is read
against the code before anything else: a defect of the library, or of the
probe, said as such.

Reported, not judged: each class's part at every seed and delay, per
window; the net and gross loss and the dead logs' share of each; the dead
part by hot entry and register, and by age; `tfdrip`'s mean less the dead
part beside 0.050 (an estimate); each class's queries, exact sums and both
builds' errors per window (in the join's lines); times.

Not covered: concurrency (leg B); lag with load; delays over 1 s; other
Horizons and Resolutions; the revival itself, and a fix of the gap-0 push
(W12b sizes them; neither is built here).

### W12b's reading (b9dd783's tree; `lab/w12b.sh`, outputs in `lab/w12b/`)

`lab/w12b.sh` ran end to end (03:40:28 to 03:41:34, Go 1.24.7) and exited 0,
and its joiner exited 0. Its first check found no library file different
from 4a586d8's and no reused test file different from 073d7da's. Every run
printed its lines: 45 a build. The join read 45 pairs and refused none. The
per-query files, 90 of them (330 MB), are in `~/lab-sw-out/w12b-q/`, not
committed: the runs replay.

1. **Holds.** Sight, in both builds: the 15 lines read as made and the test
   passes. In `tfdrip` the two off states read off, the dead ones dead
   since their delivery, the hot key dead and hot, the rest clean; in the
   default build every touched key reads no log.
2. **Holds.** With no delay, on all five seeds, both builds equal `swRecord`
   query for query, and no `tfdrip` query is dead or off.
3. **Holds.** The harness:
   - the base unchanged;
   - 90 W12 lines and 90 W12b lines, one for each build, seed and delay;
     every W12 line's integer fields equal `lab/w12`'s, so W12's leg A
     reproduces at every delay with the probe's reads in it;
   - `lastbad`, `modelmism`, `clsmism` and `qmoved` 0, 240,000 records, at
     every run;
   - the join's 45 pairs, every count of differing keys, windows and exact
     counts 0, `clsbad` 0;
   - X and each error column equal W12's sums, each class's cells W12's
     per-class sums, and the parts sum to the loss within 1e-12.
4. **Holds.** At every pair a dead query answers as the default build's. At
   `stall:1000` the dead queries number 27,529 (9, 9), 29,816 (2026, 41),
   28,070 (2026, 42), 29,386 (2026, 43) and 26,872 (2026, 44).
5. **Holds.** At `stall:1000`, on every seed, the dead logs' part is at
   least half the gross loss: it is 0.9593 to 0.9693 of it.

**The split at `stall:1000`** (each class's part summed over the five
windows):

| seed | loss | gross | dead | off | clean | dead / gross | dead / loss |
|---|---|---|---|---|---|---|---|
| (9, 9) | 0.005024 | 0.005106 | 0.004911 | 0.000195 | -0.000082 | 0.9618 | 0.9774 |
| (2026, 41) | 0.005695 | 0.005808 | 0.005581 | 0.000227 | -0.000113 | 0.9608 | 0.9800 |
| (2026, 42) | 0.005301 | 0.005410 | 0.005244 | 0.000166 | -0.000109 | 0.9693 | 0.9893 |
| (2026, 43) | 0.005586 | 0.005667 | 0.005474 | 0.000193 | -0.000080 | 0.9659 | 0.9798 |
| (2026, 44) | 0.005290 | 0.005404 | 0.005184 | 0.000220 | -0.000114 | 0.9593 | 0.9799 |

The none class's part is 0 at every seed. The off logs (the gap-0 push)
carry 3.1 to 4.1% of the gross loss; the clean logs gain a little at the
delay. The dead part sits in the 1 s window: 99.6 to 99.8% of it.

**The dead part at `stall:1000`, by key and age:**

- Hot entries carry 4.2 to 8.1% of it (0.000235 to 0.000415); registers
  the rest.
- By how long the log had been dead at the query: under 2.5 s, 3.3 to
  5.2%; 2.5 to 10 s, 7.0 to 8.7%; 10 s and over, 86.1 to 89.3%; a minute
  and over alone, 46.4 to 51.8%.
- `tfdrip`'s mean less the dead part, an estimate and not a bound: 0.0453
  (9, 9), 0.0456 (2026, 41), 0.0451 (2026, 42), 0.0453 (2026, 43), 0.0451
  (2026, 44), each under 0.050.

**Reported, at the other delays** (`lab/w12b/tables.txt` per window):

- `jitter:100`: loss 0.0020 to 0.0031; the dead logs 0.988 to 0.997 of the
  gross loss; the estimate 0.0447 to 0.0453.
- `jitter:1000`: loss 0.0172 to 0.0179; dead 0.0145 to 0.0148, off 0.0039
  to 0.0044, clean -0.0010 to -0.0012; the dead logs 0.767 to 0.787 of the
  gross loss. `tfdrip` reads 0.098 to 0.101 there; less the dead part,
  0.083 to 0.087, still over 0.050.
- `stall:100`: loss 0.00009 to 0.00024; the dead logs 0.951 to 0.998 of the
  gross loss.
- `jitter:10`: the net loss below 0 on every seed (-0.000025 to -0.000130),
  a gross loss of 0.000015 to 0.000096, the dead logs 0.82 to 1.00 of it.
- `jitter:1`, `stall:1` and `stall:10`: every loss within 0.00003 of 0;
  dead queries 0 to 62.
- The dead queries by delay, as W12's lines count them (clause 3 ties
  them): 18,828 to 22,012 at `jitter:100`, 83,474 to 85,728 at
  `jitter:1000`, 800 to 1,390 at `stall:100`.
- Times: a run 0.3 to 0.5 s; the reading 66 s.

**Against the predictions.** W12's lines reproduced at every delay, as
predicted. Clause 5 was leaned to hold at about two in three, the share
bracketed between about 0.46 (the dead queries gaining like an average
query) and 1.3 (gaining in proportion to their exact counts); it read 0.96
to 0.97 of the gross loss, 0.98 to 0.99 of the net. The dead part lies
mostly in registers and in long-dead logs, as predicted; the gross and net
loss differ little, as predicted.

**What W12b leaves.**

- At `stall:1000` the one loss that puts target 7's metric over 0.050 is
  the dead logs': 96 to 97% of the gross loss. The gap-0 push costs 3 to
  4%.
- 86 to 89% of that part is in logs dead 10 s or more, about half in logs
  dead a minute or more. A log revives only after 1.56 to 1.88 s with no
  event, which these keys seldom see, so a change aimed at them would bring
  a log back, or not kill it, without waiting for quiet. Hot entries hold
  4 to 8% of the part: not killing a hot key's log alone reaches no more.
- The estimate (0.0451 to 0.0456) bounds nothing: a revived log answers
  those queries with its own error. Whether such a change is worth an arm
  is the user's to decide.
- At `jitter:1000` the off logs carry a fifth of the gross loss and the
  estimate stays over 0.050: a jitter of a second is beyond what bringing
  dead logs back alone would mend.
- **As the rule named, not covered:**
  - concurrency (leg B);
  - lag with load;
  - delays over 1 s;
  - other Horizons and Resolutions;
  - the revival itself, and a fix of the gap-0 push.

Target 7's status is the user's.
