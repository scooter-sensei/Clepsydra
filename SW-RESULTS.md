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
