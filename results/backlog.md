# Improvement loop backlog

Scorecard: `bash bench.sh "label"` (race tests + cmd/tfbench, appends to results/history.md).

## Done
1. Partial-key cuckoo relocation (alt line from fp), iterative deepening to depth 2. Missed keys at 2.5x load: 5.0% → 0%.
2. Adaptive relocation budget (failure EWMA). Overfull-table insert: 12µs → ~0.4µs.
3. 28-bit slot identity (4-bit epoch, 12-bit tag). Static FPR 2e-7 → 4e-8.
4. CheckAndTouchBatch (overlapped misses). Cold hit ~100ns → ~45-55ns/key.

5. Any-size table (multiply-shift index, self-inverse alt), default 85% load: 42 → 19 B/key with 0 misses up to 94% load.
6. Relocation depth 3 / budget 500: misses at 98% load 0.19% → 0.06%.
7. Velocity register: 7 age-graded buckets (H/24 near now), closed-form advance, spill on overflow. Velocity MAE 0.129 → 0.087; heavy hitters no longer cap at 255.
8. Register fast paths: lazy victim scoring, in-place same-unit increment, reciprocal instead of divide.

## Tried, no gain
- 2 MiB huge-page mmap for the table: THP was granted, latency unchanged (Go heap already benefits or TLB isn't the bottleneck here).
- 8-slot (128-byte) buckets: 0 misses at 98% load and 3x cheaper near-full inserts, but ~10% slower everywhere else. Reverted.
- Piecewise-linear density in the cut velocity bucket: slightly worse than uniform. Reverted.
- Pure dyadic register (H/32, widths to 32 units): same mean MAE as the H/24 geometry but 60s error 0.040 vs 0.024.

## Next
- VelocityCount is ~115 ns vs ~75 ns with the old register: skip re-bucketing when in the same unit, integer math in estimate.
- Hot path: two CASes per touch. Option: Resolution floor that skips the word0 CAS when the timestamp would not change.
- String path: cheaper clock (cached coarse clock option), faster hashing.
- Batch variants for VelocityCount / TouchAndCount.
- Near-full (>94%) inserts cost µs; consider a per-line overflow hint so misses skip the alternate line.
