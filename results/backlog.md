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
9. VelocityCount estimates from the register's own epoch (no re-bucketing). Cache-resident 123 → 45 ns; MAE 0.087 → 0.081.
10. CoarseClock option (cached clock, one refresher goroutine). String path 172 → 110 ns.
11. Concurrent first touches of one key no longer land in two slots (striped lock on the miss path only). Was ~12% duplicate slots and ~5% lost counts with 8 racing writers; now 0. Steady insert +~15 ns.

12. Same-unit counter increment picks the first non-full bucket with one TrailingZeros instead of a loop. Hot-key hit 44 → 32 ns.

## Tried, no gain
- One CMPXCHG16B for both slot words instead of two 8-byte CASes: 19 vs 15 ns on this Xeon. Reverted.
- Branchless 4-way fingerprint compare in find: hot hits ~10% faster, but cold lookups ~15% slower (the slot index becomes data-dependent, so the CPU can't run ahead on a predicted branch). Reverted.
- 2 MiB huge-page mmap for the table: THP was granted, latency unchanged (Go heap already benefits or TLB isn't the bottleneck here).
- 8-slot (128-byte) buckets: 0 misses at 98% load and 3x cheaper near-full inserts, but ~10% slower everywhere else. Reverted.
- Piecewise-linear density in the cut velocity bucket: slightly worse than uniform. Reverted.
- Alternate-line prefetch (plain load and asm PREFETCHT0), two-choice insert: hits got slower. Reverted.
- Lock-free duplicate repair (in-flight marker + lower index wins): cut duplicates 10x but can't be made symmetric. Replaced by the stripe lock.
- Pure dyadic register (H/32, widths to 32 units): same mean MAE as the H/24 geometry but 60s error 0.040 vs 0.024.

## Next
- Hot path: two CASes per touch (~15 ns of the ~32 ns hot hit). Option: Resolution floor that skips the word0 CAS when the timestamp would not change.
- String path: faster hashing.
- Relocation copy-then-replace can still leave a duplicate if a touch lands on the copy mid-move. Rare; needs a test at >94% load with racing writers.
- Batch variants for VelocityCount / TouchAndCount.
- Near-full (>94%) inserts cost µs; consider a per-line overflow hint so misses skip the alternate line.
