# Improvement loop backlog

Checked on 2026-10-08: `verify/claims.md` maps each number here to a check (deterministic, replayed from git, or host speed), and marks the ones that turned out wrong. Patches for every reverted experiment are in `verify/experiments/` (diffs against 1e0ef14).

Scorecard: `bash bench.sh "label"` (race tests + cmd/tfbench, appends to results/history.md).

## Done
1. Partial-key cuckoo relocation (alt line from fp), iterative deepening to depth 2. Missed keys at 2.5x load: 5.0% → 0%.
2. Adaptive relocation budget (failure EWMA). Overfull-table insert: 12µs → ~0.4µs.
3. 28-bit slot identity (4-bit epoch, 12-bit tag). Static FPR 2e-7 → 4e-8 as first recorded (1 and 2 events in 5M/50M probes); at 1e9 probes it is 1.96e-7 → 1.40e-8 (claims X7).
4. CheckAndTouchBatch (overlapped misses). Cold hit ~100ns → ~45-55ns/key.

5. Any-size table (multiply-shift index, self-inverse alt), default 85% load: 42 → 19 B/key with 0 misses up to 94% load.
6. Relocation depth 3 / budget 500: misses at 98% load 0.19% → 0.066%. Correction (claims X6): measured against 3e9277c; the commit just before (9f2529f, depth 4 / budget 1000) missed 0.011%, so this change raised misses. Its insert-speed reason was not recorded.
7. Velocity register: 7 age-graded buckets (H/24 near now), closed-form advance, spill on overflow. Velocity MAE 0.129 → 0.087; heavy hitters no longer cap at 255.
8. Register fast paths: lazy victim scoring, in-place same-unit increment, reciprocal instead of divide.
9. VelocityCount estimates from the register's own epoch (no re-bucketing). Cache-resident 123 → 45 ns; MAE 0.087 → 0.081.
10. CoarseClock option (cached clock, one refresher goroutine). String path 172 → 110 ns.
11. Concurrent first touches of one key no longer land in two slots (striped lock on the miss path only). Was ~12% duplicate slots and ~5% lost counts with 8 racing writers (replayed: 13.5% and 8.0% in one run; it varies). Correction (claims X15): the lock left a rare duplicate (10 of 400 runs under CPU load, 1 slot each), which item 16 closed. Steady insert +~15 ns.

12. Same-unit counter increment picks the first non-full bucket with one TrailingZeros instead of a loop. Hot-key hit 44 → 32 ns.
13. Displaced bit (taken from the timestamp, now 47 bits): an entry records which of its two lines it is in, and lookups match it. A key is compared only with entries homed at its own primary line. Static FPR 3.2e-8 → 1.8e-8 (1e9 probes), no speed cost. Storing which bucket choice was used is a known way to extend a fingerprint by a bit.
14. `Ticks` clamps negative durations to 0. A negative window used to wrap to a huge one and report every key as seen.
15. `TouchAndCountBatch` and `VelocityCountBatch` (same overlapped loads as CheckAndTouchBatch). Median of 6 runs, 1M-key table: TouchAndCount 154 → 106 ns/key, VelocityCount 129 → 83 ns/key. Empty batches no longer panic.
16. Installs publish the new key's register together with its word0, and touches only update a register that still carries their tag. A touch landing mid-install or mid-relocation used to reset a hot key's register: 12 of 20 runs of the relocation race test lost counts (up to 70), now 0 of 20 (replayed: 4–8 of 40 before, 0 of 200 after; the test itself was corrected, claims X14). It also closed the residual duplicate-slot race from item 11. Saturated inserts ~10% faster (one write fewer).
17. Scorecard: `hit DRAM` column (32M-key, ~600 MB table). This host's L3 is 260 MB, so the 1M-key "cold" table (20 MB) was really L3-resident. On DRAM: hit ~207 ns, batched ~95, absent-key ~115, VelocityCount ~190 (`go test -bench Huge`). Hits are latency-bound there because the CAS stops later lookups from overlapping; batches recover that overlap.

## Tried, no gain
- Sending displaced entries home (`homing.patch`): at insert (before using the alternate line) and on hits found in the alternate line. Under churn, keys in their second line drop from 37% to 30% (inserts only) or 33% → 24% (with re-touches), but no measurable hit gain on the L3-resident table, inserts +15 ns, and hits on full tables +18 ns from failed attempts. Reverted.
- Transparent huge pages (madvise) for tables ≥ 8 MB (`thp-madvise.patch`): granted (AnonHugePages matched the table), but no clear change on the 600 MB table either (hit 206–229 vs 196–246 ns). Reverted.
- Reciprocal division for the built-in clock and `Ticks` (`reciprocal-clock.patch`): no change in the string path. The clock read itself (~38 ns in this VM, lfence/rdtscp-ordered) stops later work from overlapping, so the division was hidden.
- Raw RDTSC clock (`rdtsc-clock.patch`, benchmarks only): 19 ns vs 37 ns and doesn't block overlap (~20 ns saving on the string path), but needs TSC calibration and trust in invariant TSC across cores and VM migrations. Not done; CoarseClock already removes the clock cost.
- Issuing all eight relocation candidates' alternate-line loads before the cuckoo search (`reloc-prefetch.patch`): no measurable change in steady or saturated inserts (the 1M-key benchmark table is mostly L3-resident; misses are cheap).
- Per-line spill counts (skip the alternate line when nothing homed at the primary line is displaced; exact under a concurrent audit). Absent lookups ~10% faster, but steady inserts +15% and near-full inserts +15% from the extra counter traffic, and alternate-line hits wait on one more load. Reverted; the displaced bit it needed was kept (item 13). Patch: `results/experiments/spill-counts.diff` (against c8dbb54) and `spill-counts.patch` (ported).
- One CMPXCHG16B for both slot words instead of two 8-byte CASes (`cas2-cmpxchg16b.patch`): 19 vs 15 ns on this Xeon. Reverted.
- Branchless 4-way fingerprint compare in find (`branchless-find.patch`): hot hits ~10% faster, but cold lookups ~15% slower (the slot index becomes data-dependent, so the CPU can't run ahead on a predicted branch). Reverted.
- 2 MiB huge-page mmap for the table: THP was granted, latency unchanged. (Same experiment as `thp-madvise.patch`; the explanation offered at the time was a guess.)
- 8-slot (128-byte) buckets (`eight-slot.patch`; also commits 983b804 / 9f2529f): 0 misses at 98% load and 3x cheaper near-full inserts, but ~10% slower everywhere else. Reverted. Re-tested on DRAM (round 8): near-full inserts 2945 → 464 ns and steady inserts slightly faster, but DRAM hits 208 → 227 ns, absent keys 127 → 200 ns (a bucket is two cache lines; the adjacent-line prefetcher doesn't make the second free — a hypothesis, not measured), saturated inserts 514 → 793 ns, and more false positives (measured 29 vs 18 in 1e9 probes, 1.6x; "~2x" was not measured). Still not worth it.
- Piecewise-linear density in the cut velocity bucket (`piecewise-linear.patch`): slightly worse than uniform (vel MAE 0.0834 vs 0.0809). Reverted.
- Alternate-line prefetch (plain load and asm PREFETCHT0), two-choice insert: hits got slower. Reverted. The plain-load version is commit 40f9223 (reverted in bc2e12d); the asm PREFETCHT0 and two-choice variants weren't kept (unverifiable).
- Lock-free duplicate repair (in-flight marker + lower index wins; `lockfree-dup-repair-bc2e12d.patch`, HEAD port `lockfree-dup-repair.patch`): cut duplicates 10x but can't be made symmetric (re-run: 16x at bc2e12d, 6x for the port; racy). Replaced by the stripe lock.
- Pure dyadic register (H/32, widths to 32 units; `dyadic-h32.patch`): same mean MAE as the H/24 geometry but 60s error 0.040 vs 0.024. Re-run on today's estimator: mean 0.0817 vs 0.0809, 60s 0.038 vs 0.024.
- Hot-key contention (round 8 check, not a change; `contention-bench.patch`): 4 goroutines on 1 key run at 10 ns/op aggregate, 16 keys at 16 ns, 16K keys at 22 ns. Same-tick touches skip the word0 CAS and full registers skip the word1 CAS, so a bot storm doesn't ping-pong the line. Touch elision isn't needed.

## Decided
- 2026-10-08: Priyasis chose to keep the 48-bit counter (exact heavy-hitter counts) over trading counter bits for identity bits. Options measured:
- More identity bits from the velocity register (measured, 1e9 probes):
  - 46-bit register + 14-bit tag (`register-46.patch`): FPR 1.8e-8 → 7e-9, vel MAE 0.081 → 0.085, per-key count cap per horizon 1000+ → ~876.
  - 44-bit register + 16-bit tag (`register-44.patch`): FPR → 0 seen (~1e-9 expected), vel MAE 0.088, cap ~630.
  - Correction (claims X3): the caps were burst counts at one phase. The current register's 60 s burst reads 770–1,273 depending on phase (46-bit: 638–889; 44-bit: 508–633), and steady traffic reads within 1% only up to 12 events/s (46-bit: 12% low at 10/s; 44-bit: 18–30% low). The ordering between the options holds.
- Truncated timestamps (needs a guaranteed sweeper when idle) would free up to 7 fingerprint bits without touching counts.

## Next
- Missed keys at 97.8% load come from the bounded cuckoo search, not the layout (claims D8). A deeper or BFS search (9f2529f's depth 4 / budget 1000 missed 12 instead of 58) should cut them; measure the insert cost.
- Per-key counts undercount above ~12 events/s (claims D7): the newest bucket holds 31 events per 2.5 s unit.
- Hot path: two CASes per touch (~15 ns of the ~32 ns hot hit). Option: Resolution floor that skips the word0 CAS when the timestamp would not change.
- String path: faster hashing.
- Near-full (>94%) inserts cost µs; consider a per-line overflow hint so misses skip the alternate line.
