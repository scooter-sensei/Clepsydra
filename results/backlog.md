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
13. Displaced bit (taken from the timestamp, now 47 bits): an entry records which of its two lines it is in, and lookups match it. A key is compared only with entries homed at its own primary line. Static FPR 3.2e-8 → 1.8e-8 (1e9 probes), no speed cost. Storing which bucket choice was used is a known way to extend a fingerprint by a bit.
14. `Ticks` clamps negative durations to 0. A negative window used to wrap to a huge one and report every key as seen.
15. `TouchAndCountBatch` and `VelocityCountBatch` (same overlapped loads as CheckAndTouchBatch). Median of 6 runs, 1M-key table: TouchAndCount 154 → 106 ns/key, VelocityCount 129 → 83 ns/key. Empty batches no longer panic.
16. Installs publish the new key's register together with its word0, and touches only update a register that still carries their tag. A touch landing mid-install or mid-relocation used to reset a hot key's register: 12 of 20 runs of the relocation race test lost counts (up to 70), now 0 of 20. Saturated inserts ~10% faster (one write fewer).
17. Scorecard: `hit DRAM` column (32M-key, ~600 MB table). This host's L3 is 260 MB, so the 1M-key "cold" table (20 MB) was really L3-resident. On DRAM: hit ~207 ns, batched ~95, absent-key ~115, VelocityCount ~190 (`go test -bench Huge`). Hits are latency-bound there because the CAS stops later lookups from overlapping; batches recover that overlap.

## Tried, no gain
- Sending displaced entries home: at insert (before using the alternate line) and on hits found in the alternate line. Under churn, keys in their second line drop from 37% to 30% (inserts only) or 33% → 24% (with re-touches), but no measurable hit gain on the L3-resident table, inserts +15 ns, and hits on full tables +18 ns from failed attempts. Reverted.
- Transparent huge pages (madvise) for tables ≥ 8 MB: granted (AnonHugePages matched the table), but no clear change on the 600 MB table either (hit 206–229 vs 196–246 ns). Reverted.
- Reciprocal division for the built-in clock and `Ticks`: no change in the string path. The clock read itself (~38 ns in this VM, lfence/rdtscp-ordered) stops later work from overlapping, so the division was hidden.
- Raw RDTSC clock: 19 ns vs 37 ns and doesn't block overlap (~20 ns saving on the string path), but needs TSC calibration and trust in invariant TSC across cores and VM migrations. Not done; CoarseClock already removes the clock cost.
- Issuing all eight relocation candidates' alternate-line loads before the cuckoo search: no measurable change in steady or saturated inserts (the 1M-key benchmark table is mostly L3-resident; misses are cheap).
- Per-line spill counts (skip the alternate line when nothing homed at the primary line is displaced; exact under a concurrent audit). Absent lookups ~10% faster, but steady inserts +15% and near-full inserts +15% from the extra counter traffic, and alternate-line hits wait on one more load. Reverted; the displaced bit it needed was kept (item 13). Patch: `results/experiments/spill-counts.diff`.
- One CMPXCHG16B for both slot words instead of two 8-byte CASes: 19 vs 15 ns on this Xeon. Reverted.
- Branchless 4-way fingerprint compare in find: hot hits ~10% faster, but cold lookups ~15% slower (the slot index becomes data-dependent, so the CPU can't run ahead on a predicted branch). Reverted.
- 2 MiB huge-page mmap for the table: THP was granted, latency unchanged (Go heap already benefits or TLB isn't the bottleneck here).
- 8-slot (128-byte) buckets: 0 misses at 98% load and 3x cheaper near-full inserts, but ~10% slower everywhere else. Reverted.
- Piecewise-linear density in the cut velocity bucket: slightly worse than uniform. Reverted.
- Alternate-line prefetch (plain load and asm PREFETCHT0), two-choice insert: hits got slower. Reverted.
- Lock-free duplicate repair (in-flight marker + lower index wins): cut duplicates 10x but can't be made symmetric. Replaced by the stripe lock.
- Pure dyadic register (H/32, widths to 32 units): same mean MAE as the H/24 geometry but 60s error 0.040 vs 0.024.

## Decided
- 2026-10-08: Priyasis chose to keep the 48-bit counter (exact heavy-hitter counts) over trading counter bits for identity bits. Options measured:
- More identity bits from the velocity register (measured, 1e9 probes):
  - 46-bit register + 14-bit tag: FPR 1.8e-8 → 7e-9, vel MAE 0.081 → 0.085, per-key count cap per horizon 1000+ → ~876.
  - 44-bit register + 16-bit tag: FPR → 0 seen (~1e-9 expected), vel MAE 0.088, cap ~630.
- Truncated timestamps (needs a guaranteed sweeper when idle) would free up to 7 fingerprint bits without touching counts.

## Next
- Hot path: two CASes per touch (~15 ns of the ~32 ns hot hit). Option: Resolution floor that skips the word0 CAS when the timestamp would not change.
- String path: faster hashing.
- Near-full (>94%) inserts cost µs; consider a per-line overflow hint so misses skip the alternate line.
