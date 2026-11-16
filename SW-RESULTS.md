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
