# Reverted experiments as re-runnable patches

Every `*.patch` in `go/verify/experiments/` is a plain `git diff` against **fbe6968** (Go 1.24.7; the module was then at the repository root, as
`github.com/scooter-sensei/temporalfilter`), except
`lockfree-dup-repair-70546b2.patch` (applies to **70546b2**) and
`insert-saturated-bench-b3e9aa5.patch` (adds 378bc37's saturated-insert
benchmark to **b3e9aa5**, for the speed comparison in `verify/speed.sh`).

```sh
git clone /mnt/project-files/temporal-filter tf && cd tf && git checkout fbe6968
git apply verify/experiments/<name>.patch     # or: verify/experiments.sh, verify/ab.sh
```

The experiment code was recovered from the project thread, which
recorded the edits as Python and heredoc scripts. Each patch's notes say where
the code was ported to HEAD and how. ("HEAD" in these notes means fbe6968.)
The raw logs of the checks below were not kept; `verify/experiments.sh`
re-runs every deterministic one and `verify/run.sh` compares the result with
`verify/expected/experiments.txt`.

Each patch was applied to a fresh worktree at fbe6968 and checked with
`git apply`, `go build ./...`, `go vet ./...` and `go test -short -count=1 ./...`.
Nine patches were also run with `go test -race -short -count=1 .`
(see the race check section at the end). No speed benchmarks were run. Any
ns/op in the notes are the numbers first recorded, given as context only.

## Status

| patch | applies / builds / vet | `go test -short ./...` | deterministic result |
|---|---|---|---|
| oldest-first-eviction | yes | **FAIL (intended)**: TestSprayCannotEvictRepeatOffender; also TestTouchRacingRelocationKeepsCounts (hot keys evicted) | spray test FAILs, "offender 0: count 0"; tfbench spray 0% (HEAD 100%) |
| branchless-find | yes | pass | `find` codegen: 4 SETEQ (HEAD 0) |
| cas2-cmpxchg16b | yes (amd64 asm; arm64 vet/build ok) | pass | none; speed only |
| reciprocal-clock | yes | pass (adds TestNsToTicksMatchesDivision) | none; speed only |
| rdtsc-clock | yes (new pkg `internal/clockbench`) | pass (no tests, benchmarks only) | none; speed only |
| homing | yes | pass | alt-line share 37.3% → 30.2% (steady), 33.5% → 23.9% (re-touch) |
| thp-madvise | yes | pass | AnonHugePages 307200 kB with patch, 0 kB without (on this host) |
| reloc-prefetch | yes | pass | none; speed only |
| spill-counts | yes (ported from 68eee6c) | pass | audit: 5883 lines exact, 0 over, 0 under; tfbench row identical to HEAD |
| register-46 | yes | **FAIL (intended)**: TestHeavyHitterSpill "1000 hits counted as 876" | static FPR 7.00e-09, vel MAE 0.085 |
| register-44 | yes | **FAIL (intended)**: TestHeavyHitterSpill "1000 hits counted as 627" | static FPR 0.00e+00, vel MAE 0.088 |
| piecewise-linear | yes | pass | vel MAE 0.083 (HEAD 0.081) |
| eight-slot | yes | pass | FN 1.15x 0.000% (HEAD 0.051%); static FPR 2.90e-08 (HEAD 1.80e-08) |
| lockfree-dup-repair | yes (port; stripe lock removed) | **FAIL (intended)**: TestConcurrentFirstInsertNoDuplicates (duplicates are reduced, not removed) | nondeterministic: mean 196 dup slots vs 1112 with no lock (60 runs) |
| lockfree-dup-nolock-control | yes | **FAIL (intended)**: same test | control for the row above |
| lockfree-dup-repair-70546b2 | yes, **on 70546b2** | **FAIL (intended)**: same test | original code: mean 102 vs 1592 (60 runs) |
| contention-bench | yes | pass | none; speed only |
| dyadic-h32 (extra, for item 15) | yes | pass (one test fixture made geometry-independent) | vel MAE per window 0.204 0.065 0.057 0.045 **0.038**, mean 0.082 |

Nothing was unrecoverable. Two deviations from the original code are described
at lockfree-dup-repair and spill-counts.

## HEAD reference row (`go run ./cmd/tfbench -quick`)

`-quick` skips the speed columns, so they print as 0. The accuracy columns are
deterministic: single-goroutine churn with seeded PCG, unkeyed hashes, and a
probe count summed across workers. HEAD matches the baseline its
first run printed for the same columns.

```
vel MAE  1s: 0.227
vel MAE  5s: 0.058
vel MAE 15s: 0.051
vel MAE 30s: 0.044
vel MAE 60s: 0.024
| when (UTC) | rev | FN 1.0x | FN 1.1x | FN 1.15x | FP churn 1.15x | static FPR | spray | B/key | vel MAE | hit cold | batch | hit hot | insert | insert sat | velocity | parallel | str | str coarse | change | hit DRAM |
| 10-08 16:06 | fbe6968 | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 0 | 0 | 0 | 0 | 0 | 0 | 0.0 | 0 | 0 | | 0 |
```

One `-quick` run took 13–21 s on this 4-vCPU host.

## Speed A/B recipe (for the speed claims, run later and one at a time)

This is how the comparisons were first run: build test binaries for
the base and the experiment, then interleave runs and compare the minimum and
the spread.

```sh
R=/path/to/temporal-filter
git -C $R worktree add /tmp/ab-base fbe6968
git -C $R worktree add /tmp/ab-exp  fbe6968 && git -C /tmp/ab-exp apply /path/to/out/<name>.patch
(cd /tmp/ab-base && go test -c -o /tmp/base.test .) && (cd /tmp/ab-exp && go test -c -o /tmp/exp.test .)
for r in 1 2 3 4 5 6; do for v in base exp; do
  /tmp/$v.test -test.run '^$' -test.bench '<REGEX>' -test.benchtime=<T> | grep ^Benchmark | sed "s/^/$v /"
done; done
```

For each patch, `REGEX` and `T` below are the values the original A/B used.
`go test -run '^$' -bench '<REGEX>' -benchtime <T> -count N .` in each tree,
compared with benchstat, works too.

---

## 1. oldest-first-eviction.patch (deterministic)

**Claim** (the related-work survey's §3.1, now [paper §3.3.1](../paper/03-design.md#331-velocity-aware-eviction)): "`TestSprayCannotEvictRepeatOffender` sprays 20× capacity of one-off keys. All 50 offender keys keep their exact count of 20. The same test **fails with the old oldest-first policy**. Insert latency is unchanged (~230 ns)."

**Change:** the eviction victim score in `claim` becomes `ts` (oldest timestamp only) instead of `mass<<48 | ts`. This is the edit as first made (a sed edit).

**Measure:**
```sh
go test -count=1 -run '^TestSprayCannotEvictRepeatOffender$' -v .
go run ./cmd/tfbench -quick        # spray column
```
**Result (deterministic):**
```
=== RUN   TestSprayCannotEvictRepeatOffender
    filter_test.go:322: offender 0: count 0 after spray, want 20
--- FAIL: TestSprayCannotEvictRepeatOffender (0.07s)
FAIL
```
tfbench -quick: `| 0.000% | 0.000% | 0.050% | 0.00e+00 | 1.80e-08 | 0% | 18.8 | 0.081 |`. Spray is **0%** versus 100% at HEAD. Everything else is unchanged, except FN 1.15x at 0.050% versus 0.051%.

**Side effect (not deterministic):** `TestTouchRacingRelocationKeepsCounts` also fails, because hot keys get evicted during its 98%-load fill. Over 30 standalone runs it lost counts in 30 of 30 (median 1786 lost, max 8736). HEAD lost counts in 2 of 30 (max 21). That was the test's own flaw at the time (claims X14), since fixed.

The "insert latency unchanged" part is a speed claim: REGEX `CheckAndTouchMissInsert$`, T `1s`.

## 2. branchless-find.patch (speed)

**Claim:** "Branchless 4-way fingerprint compare in find: hot hits ~10% faster, but cold lookups ~15% slower (the slot index becomes data-dependent, so the CPU can't run ahead on a predicted branch). Reverted."

**Change:** `find` builds a 4-bit match mask from four `>>47 == fpd` compares (SETEQ, no branches), then walks the set bits checking tags. It is ported to HEAD's displaced-bit compare. HEAD's `b2u` helper moves from bench_test.go to filter.go; it is the same function.

**Measure (speed):** REGEX `CheckAndTouchHit$|CheckAndTouchHitHot$|TouchAndCount$|VelocityCount$`, T `500ms`, 6–8 interleaved rounds. Hot means HitHot; cold means Hit and VelocityCount.

**Deterministic check (codegen):**
```sh
go build -gcflags=-S . 2>&1 | awk '/\(\*Filter\)\.find STEXT/{f=1;next} /STEXT/{f=0} f' | grep -c SETEQ
```
This prints 4 with the patch and 0 at HEAD.

## 3. cas2-cmpxchg16b.patch (speed)

**Claim:** "One CMPXCHG16B for both slot words instead of two 8-byte CASes: 19 vs 15 ns on this Xeon. Reverted."

**Change:** this adds `cas2` (LOCK CMPXCHG16B) and a CPUID CX16 probe in Go asm (cas2_amd64.s/.go; non-amd64 stub `hasCAS2 = false`). The hit path in `touch` then updates both words with one 16-byte CAS. Ported to HEAD: the new word0 keeps the displaced bit, and the tag check precedes the CAS. The patch also adds the original microbenchmark (cas2_bench_amd64_test.go, which keeps its original names `BenchmarkZZTwoCAS` and `BenchmarkZZCAS2`).

**Measure (speed):**
- The 19 vs 15 ns claim, within the patched tree, needs no base:
  `go test -run '^$' -bench 'ZZTwoCAS|ZZCAS2' -count 4 .`
- The hit path, base against patch: REGEX `CheckAndTouchHit$|CheckAndTouchHitHot$|CheckAndTouchParallel$|TouchAndCount$`, T `1s`, 3 rounds.

## 4. reciprocal-clock.patch (speed)

**Claim:** "Reciprocal division for the built-in clock and `Ticks`: no change in the string path. The clock read itself (~38 ns in this VM, lfence/rdtscp-ordered) stops later work from overlapping, so the division was hidden."

**Change:** the patch adds `resInv` and `nsToTicks` (multiply-high by a precomputed reciprocal, with a one-step fixup). The built-in clock and `Ticks` use them. It also adds the original exactness test `TestNsToTicksMatchesDivision`, which passes.

**Measure (speed):** REGEX `CheckAndTouchString$`, T `1s`, 4 interleaved rounds. The cost of the clock read itself is measured in rdtsc-clock below (`BenchmarkTimeSince`).

## 5. rdtsc-clock.patch (speed, benchmarks only)

**Claim:** "Raw RDTSC clock: 19 ns vs 37 ns and doesn't block overlap (~20 ns saving on the string path), but needs TSC calibration and trust in invariant TSC across cores and VM migrations. Not done; CoarseClock already removes the clock cost."

**Change:** this adds the new package `internal/clockbench`: an RDTSC asm function plus the original microbenchmarks (`TimeSince`, `Nanotime` via linkname, `MaphashString`, `RDTSC`, and `MissNanotime`/`MissRDTSC`/`MissNoClock`, which pair the clock read with a random load from a 256 MB table). The filter is not touched. The original lived in a separate, uncommitted module; it moved into the repo here. The 256 MB table is now allocated on first use (with `b.ResetTimer`) instead of at package init, so `go test ./...` doesn't pay for it.

**Measure (speed):**
```sh
go test -run '^$' -bench . -count 2 ./internal/clockbench
grep -o -w "constant_tsc\|nonstop_tsc\|tsc_known_freq\|rdtscp\|hypervisor" /proc/cpuinfo | sort | uniq -c
cat /sys/devices/system/clocksource/clocksource0/current_clocksource
```
The original numbers were RDTSC 19–20 ns against Nanotime about 37 ns, and MissRDTSC 42–44 ns against MissNanotime 64–66 ns, against MissNoClock 6.4 ns.

## 6. homing.patch (speed, plus a deterministic alt-line share)

**Claim:** "Sending displaced entries home: at insert (before using the alternate line) and on hits found in the alternate line. Under churn, keys in their second line drop from 37% to 30% (inserts only) or 33% → 24% (with re-touches), but no measurable hit gain on the L3-resident table, inserts +15 ns, and hits on full tables +18 ns from failed attempts. Reverted."

**Change:** the patch makes two moves.
- In `claim`, when the primary line is full, a displaced entry in it is sent back to its own primary line (one cuckoo step, budget 4) before the alternate line is used.
- In `touch`, a key found in its alternate line is moved home if its primary line has room (budget 1).

It also adds the original alt-share tests as `alt_fraction_test.go` (`TestZZAltFractionSteady` and `TestZZAltFractionRetouch`). Both apply to HEAD without porting changes. To test claim-time homing alone, as one of the original A/B runs did, delete the `touch()` hunk.

**Measure (deterministic):**
```sh
go test -count=1 -run 'ZZAltFraction' -v .                    # with the patch
git apply --include=alt_fraction_test.go homing.patch          # baseline: the test file only, on clean HEAD
go test -count=1 -run 'ZZAltFraction' -v .
```
**Result:**
```
patch:  steady: 200000 live, 30.2% in alt line, relocations 271784
        retouch: occupancy 0.80, 23.9% of live keys in alt line, relocations 358972, evictions 0
HEAD:   steady: 200000 live, 37.3% in alt line, relocations 233163
        retouch: occupancy 0.80, 33.5% of live keys in alt line, relocations 253334, evictions 0
```
These are identical to the numbers first recorded.

**Speed:** REGEX `HitChurned|InsertSteady|CheckAndTouchHit$`, T `1000000x`, 5 interleaved rounds.

## 7. thp-madvise.patch (speed, plus a host-dependent grant check)

**Claim:** "Transparent huge pages (madvise) for tables ≥ 8 MB: granted (AnonHugePages matched the table), but no clear change on the 600 MB table either (hit 206–229 vs 196–246 ns). Reverted."

**Change:** `alignedWords` aligns tables of 8 MB or more to 2 MiB and calls `madvise(MADV_HUGEPAGE)` on them (huge_linux.go; huge_other.go is a no-op). It also adds the original probe as `thp_probe_linux_test.go` (`TestZZTHP`). The probe is skipped under `-short` because it builds a 300 MB table.

**Measure:**
```sh
cat /sys/kernel/mm/transparent_hugepage/enabled          # was "always [madvise] never" here
go test -count=1 -run ZZTHP -v .                         # with patch; baseline: git apply --include=thp_probe_linux_test.go
```
**Result on this host:** the patch reports `Rss: 312772 kB`, `AnonHugePages: 307200 kB`, table 301 MB. The baseline reports `Rss: 312720 kB`, `AnonHugePages: 0 kB`.

**Speed** (needs about 1 GB of RAM, not `-short`): REGEX `HugeHit|HugeAbsent|HugeBatch`, T `2000000x`, 3 interleaved rounds.

## 8. reloc-prefetch.patch (speed)

**Claim:** "Issuing all eight relocation candidates' alternate-line loads before the cuckoo search: no measurable change in steady or saturated inserts (the 1M-key benchmark table is mostly L3-resident; misses are cheap)."

**Change:** in `claim`, before the iterative-deepening search, the patch loads word0 of each candidate's alternate line for all 8 candidates and sinks the results with `runtime.KeepAlive`. It applies to HEAD unchanged.

**Measure (speed):** REGEX `InsertSteady|InsertSaturated`, T `1000000x`, 5 interleaved rounds. The hugeKeys DRAM table is the setting the claim says was missing; there is no DRAM insert benchmark at HEAD.

## 9. spill-counts.patch (speed, plus a deterministic sanity row and a concurrent audit)

**Claim:** "Per-line spill counts (skip the alternate line when nothing homed at the primary line is displaced; exact under a concurrent audit). Absent lookups ~10% faster, but steady inserts +15% and near-full inserts +15% from the extra counter traffic, and alternate-line hits wait on one more load. Reverted; the displaced bit it needed was kept (item 13). Patch: `results/experiments/spill-counts.diff`." (that original diff against 68eee6c was dropped in the cleanup; git history keeps it)

**Change:** the original spill-counts diff, ported from 68eee6c to HEAD. It adds an 8-bit per-line count of displaced entries homed at that line (packed 4 per uint32). `locate` skips the alternate line when the count is 0. The counts are maintained in `claim` (free slot and eviction), `relocate` (copy, replace, withdraw, return home) and `sweepLine`, using the original rules: count before a displaced copy can exist, uncount only after it is gone, saturate at 255.

**Porting notes:** HEAD already has the displaced bit, so `locate` keeps HEAD's displaced-bit matching and adds the spill skip. HEAD's `install` writes both words (register first), so the original's separate word1 CAS inside `place` is folded into `install`. The concurrent audit test written with it (`spill_test.go`, `TestSpillCountsNeverUnderCount`) is included, with its two-line search switched to HEAD's `findAny`. The original diff still applies cleanly at 68eee6c (`git apply --check` ok).

**Deterministic and correctness checks:**
```sh
go test -count=3 -run 'TestSpillCountsNeverUnderCount|TestDisplacedBitTracksLine' -v .
go run ./cmd/tfbench -quick
```
- The audit passes on every run with `lines exact 5883, over-counted 0`. Relocation and eviction counts vary from run to run; the line counts did not.
- The tfbench -quick row is identical to HEAD: `| 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 |`.

**Speed:** REGEX `ContainsAbsent$|CheckAndTouchMissInsert$|CheckAndTouchHit$|VelocityCount$|InsertSaturated|InsertNearFull`, T `300ms`, 6 interleaved rounds. Then REGEX `InsertSteady`, T `1000000x`, 4 rounds, and REGEX `Latency`, T `500ms`, 4 rounds (alternate-line hits waiting on one more load).

## 10. register-46.patch and register-44.patch (deterministic)

**Claim** ([record/decisions.md](../record/decisions.md#decided)): "46-bit register + 14-bit tag: FPR 1.8e-8 → 7e-9, vel MAE 0.081 → 0.085, per-key count cap per horizon 1000+ → ~876." and "44-bit register + 16-bit tag: FPR → 0 seen (~1e-9 expected), vel MAE 0.088, cap ~630."

**Change:** this is the transform first applied to a scratch copy. `regBits` becomes 46 (or 44) and `tagBits = 64 - regBits - 4`. The tag is taken from hash bits `48-tagBits..47`, and `primary` uses the bits below the tag. The epoch moves to `regBits`. Bucket widths become {5,5,6,7,7,8,8} for 46 bits and {5,5,6,7,7,7,7} for 44 bits.

**Intentionally failing test:** `TestHeavyHitterSpill` fails in both, because the per-key cap drops below the test's 1000 events. That failure is the cap measurement.

**Measure:**
```sh
go run ./cmd/tfbench -quick
go test -count=1 -run '^TestHeavyHitterSpill$' -v .
```
**Result (full rows; speed columns are 0 under -quick):**
```
HEAD       | 10-08 16:06 | fbe6968  | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 0 | 0 | 0 | 0 | 0 | 0 | 0.0 | 0 | 0 | | 0 |
register-46| 10-08 16:10 | fbe6968+ | 0.000% | 0.000% | 0.055% | 0.00e+00 | 7.00e-09 | 100% | 18.8 | 0.085 | 0 | 0 | 0 | 0 | 0 | 0 | 0.0 | 0 | 0 | | 0 |
register-44| 10-08 16:10 | fbe6968+ | 0.000% | 0.000% | 0.062% | 0.00e+00 | 0.00e+00 | 100% | 18.8 | 0.088 | 0 | 0 | 0 | 0 | 0 | 0 | 0.0 | 0 | 0 | | 0 |
vel MAE 1s/5s/15s/30s/60s: HEAD 0.227 0.058 0.051 0.044 0.024 | 46: 0.227 0.058 0.053 0.057 0.031 | 44: 0.227 0.058 0.053 0.057 0.045
```
These match the rows first recorded exactly.

**Count cap:**
- TestHeavyHitterSpill reports `1000 hits counted as 876` for 46 bits and `1000 hits counted as 627` for 44 bits. HEAD passes.
- Derivation (sum of bucket maxima, plus 5000 same-instant hits read back over 60 s, from a throwaway test that was not kept; `tfbench -exact` re-measures the limits, claims D7):
  - HEAD [5 5 6 7 8 8 9]: 1273, read back as 1247 (the read-back depends on the burst's phase: 770–1,273 over 64 phases, claims D7).
  - 46 bits [5 5 6 7 7 8 8]: 889, read back as 876.
  - 44 bits [5 5 6 7 7 7 7]: 633, read back as 627.

## 11. piecewise-linear.patch (deterministic)

**Claim:** "Piecewise-linear density in the cut velocity bucket: slightly worse than uniform. Reverted."

**Change:** in `estimate`, the one bucket the window edge cuts is integrated with a piecewise-linear density instead of a uniform one. The slope comes from the neighbouring buckets' densities and is clamped so the density stays non-negative and the bucket total is preserved. This is the original `partial()` function, verbatim.

**Porting note:** the original targeted the older `estimate`, which re-bucketed to now. At HEAD, `estimate` works from the register's own epoch, so the bucket spans are now computed at that epoch. To check the restructure, running it with the uniform rule reproduced HEAD's per-window MAE exactly before `partial` was swapped in.

**Measure:** `go run ./cmd/tfbench -quick`

**Result:** vel MAE **0.083** against HEAD 0.081. Per window (1s/5s/15s/30s/60s): 0.234 0.058 0.054 0.046 0.024, against HEAD 0.227 0.058 0.051 0.044 0.024. The other columns are identical to HEAD. When first measured, against the older estimator, it was 0.239 0.067 0.076 0.048 0.024 against uniform 0.227 0.066 0.070 0.047 0.024.

## 12. eight-slot.patch (deterministic FN and FPR; speed for the rest)

**Claim:** "8-slot (128-byte) buckets: 0 misses at 98% load and 3x cheaper near-full inserts, but ~10% slower everywhere else. Reverted. Re-tested on DRAM: near-full inserts 2945 → 464 ns and steady inserts slightly faster, but DRAM hits 208 → 227 ns, absent keys 127 → 200 ns …, saturated inserts 514 → 793 ns, and ~2x false positives. Still not worth it."

**Change:** this is the port used for the DRAM re-test. `slotsPerLine = 8` (a 128-byte bucket made of two cache lines, aligned to 128 B by the existing `alignedWords`), and `warm` loads both 64-byte lines of each bucket. The original commit 4103013 also set relocDepth 4 and relocBudget 1000. Those were relocation settings under test at the time, and that port did not carry them, so they are not included here.

**Measure:** `go run ./cmd/tfbench -quick`

**Result:**
```
eight-slot | 10-08 16:12 | fbe6968+ | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.90e-08 | 100% | 18.8 | 0.081 | ...
```
- FN at 1.15x is **0.000%** (HEAD 0.051%), so the "0 misses at 98% load" part holds.
- Static FPR is **2.90e-08** against 1.80e-08, about **1.6x**. The "~2x" in the claim was stated, not measured, in the DRAM re-test.

**Speed** (not `-short`; the Huge benchmarks build a 600 MB table): REGEX `Huge|CheckAndTouchHit$|CheckAndTouchHitHot$|InsertSteady|InsertSaturated|InsertNearFull|VelocityCount$|ContainsAbsent$`, T `1000000x`, 3 interleaved rounds.

## 13. lockfree-dup-repair (complete in the project thread; nondeterministic result)

**Claim:** "Lock-free duplicate repair (in-flight marker + lower index wins): cut duplicates 10x but can't be made symmetric. Replaced by the stripe lock."

The project thread holds the complete code: three edits on top of 70546b2, before the stripe lock existed. Replaying those edits on 70546b2 reproduces the diff printed then, byte for byte. Three files:

- **lockfree-dup-repair-70546b2.patch** is the original, applied to **70546b2**, not HEAD. It has the in-flight marker (`install` zeroes word1 before publishing word0, and `find`/`dup` match fp with word1 == 0), a claim-time scan for a racing insert of the same key, `claim` returning -1 on a lost slot, and a post-claim `dup` check where the higher index yields. It also adds the `dup_test.go` of that time.
  ```sh
  git checkout 70546b2 && git apply lockfree-dup-repair-70546b2.patch
  ```
- **lockfree-dup-repair.patch** is the port to HEAD. It removes the stripe lock from `touch` and adds the same-key scan in `claim` (which gains a `tg` parameter), the `dup` helper, and the higher-index-yields step.
  - **Deviation:** there is no in-flight marker. HEAD's `install` publishes the new key's register in word1 *before* word0. So the state "fp visible, word1 not yet written" that the marker covered cannot occur, and matching `word1 == 0` would be dead code.
  - Claim returning -1 on a lost slot is already in HEAD.
- **lockfree-dup-nolock-control.patch** is HEAD with the stripe lock removed and no repair. It is the control for the 10x comparison.

**Intentionally failing test:** `TestConcurrentFirstInsertNoDuplicates` fails with all three patches. They reduce duplicates but do not remove them.

**Measure (nondeterministic; depends on scheduling):** build each variant with `go test -c`, then run it 60 times:
```sh
for r in $(seq 60); do ./x.test -test.run TestConcurrentFirstInsertNoDuplicates -test.count=1 -test.v | grep -o 'slots used [0-9]*'; done
```
**Result on this host** (duplicate slots per run, out of 20000 keys and 8 writers):
```
lf-nolock (HEAD, no lock)      runs=60 mean=1112 median=1187 p90=2493 max=2591 runs_with_dups=39
lf-port   (HEAD port)          runs=60 mean=196  median=176  p90=434  max=552  runs_with_dups=50
lf-orig-base (70546b2)         runs=60 mean=1592 median=2471 p90=2736 max=2775 runs_with_dups=48
lf-orig   (70546b2 + original) runs=60 mean=102  median=106  p90=197  max=227  runs_with_dups=53
```
- The original cuts duplicates about 16x by mean (23x by median). The first measurement saw 1889–2648 against 149–230.
- The HEAD port cuts them about 6x.
- The stripe lock at HEAD gives 0.

## 14. contention-bench.patch (speed)

**Claim:** "Hot-key contention (a check, not a change): 4 goroutines on 1 key run at 10 ns/op aggregate, 16 keys at 16 ns, 16K keys at 22 ns. Same-tick touches skip the word0 CAS and full registers skip the word1 CAS, so a bot storm doesn't ping-pong the line. Touch elision isn't needed."

**Change:** this appends the original `benchContended` (RunParallel on 1, 16 or 16K keys with a shared clock that advances every 64 ops) to bench_test.go. The benchmarks are renamed from `BenchmarkZZ*` to `BenchmarkContended1`, `BenchmarkContended16` and `BenchmarkUncontended` (16K keys).

**Measure (speed):** `go test -run '^$' -bench 'Contended|Uncontended' -cpu 4 -count 2 .`

## 15. Pure dyadic register (H/32): does the register simulation reproduce it?

**Claim:** "Pure dyadic register (H/32, widths to 32 units): same mean MAE as the H/24 geometry but 60s error 0.040 vs 0.024."

**No.** `registers_experiment_test.go` (now `go/internal/registersim/`) is a standalone simulation. It has its own float model, 5 traffic shapes × 40 trials, and its own estimator. It compares linear, dyadic H/16, dyadic H/32 and Laplace registers, and it has **no H/24 geometry row**, so it cannot show H/32 against H/24. Its output for reference (`go test -count=1 -run '^TestRegisterExperiment$' -v ./internal/registersim`, full table in `verify/expected/register-experiment.txt`):
```
mean over all traffic x windows:
  linear 6x8 (H/5)                   0.154
  dyadic 6 buckets (H/16 near now)   0.122
  dyadic 7 buckets (H/32 near now)   0.108
  laplace 3x16                       0.268
  laplace 4x12                       0.232
```
The claim came from swapping the geometry constants in register.go and running `tfbench -quick`. **dyadic-h32.patch** does exactly that, with the values first used: `geomUnits = 32`, logStep {0,0,1,2,3,4,5}, off {0,1,2,4,8,16,32}, bits {5,5,6,6,7,8,11}. It also changes one test fixture: `TestUnitOfMatchesDivision` hard-coded the smallest legal horizon as 48 µs (2×24 units), which `New` rejects at 32 units. It now uses `2 * unitsPerHorizon * time.Microsecond`, which is 48 µs at HEAD, so the test is unchanged there.

**Result (deterministic, `go run ./cmd/tfbench -quick`):**
```
dyadic-h32 | 10-08 16:12 | fbe6968+ | 0.000% | 0.000% | 0.059% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.082 | ...
vel MAE 1s/5s/15s/30s/60s: H/32 0.204 0.065 0.057 0.045 0.038 (mean 0.082) | HEAD H/24 0.227 0.058 0.051 0.044 0.024 (mean 0.081)
```
Mean MAE is the same within 0.001, and the 60 s error is **0.038 vs 0.024**. The 0.040 first recorded was measured with the older estimator (before commit a477d31). HEAD's estimator gives 0.038, the same value measured after that change.

## Race check

`go test -race -short -count=1 .` in the patched worktrees passed for all nine patches it was run on: cas2-cmpxchg16b, homing, spill-counts, branchless-find, eight-slot, reciprocal-clock, thp-madvise, reloc-prefetch and piecewise-linear. It was not run on the patches that fail a test on purpose, nor on the benchmark-only or geometry patches. The race detector cannot see the asm CAS in cas2-cmpxchg16b.

## Note on HEAD itself

`TestTouchRacingRelocationKeepsCounts` is slightly flaky at HEAD on this shared host. It failed in 5 of 30 standalone runs in one batch and 2 of 30 in another, losing 2 to 21 counts, and once 1116. Changes item 16 says 0 of 20. Take this into account when a patch's verify log shows this test failing.

## Later experiments (patches against their own commits, not run by experiments.sh)

| Patch | Applies to | What it does | Where it is measured |
|---|---|---|---|
| pad-modes-and-sip13 | 150fac4 and later | sleep, adaptive and yield ConstantTime pads; SipHash-1-3 | [paper §6.6](../paper/06-security.md#66-evidence-timing), claims S30, S31 |
| ct-shadow-and-prefetch-toggle | 9314d89 and later | shadow work in ConstantTime (each path also runs the other's, read-only), `TestCTProfile` (pad sweep), `TF_NOPF=1` to skip the alternate-line prefetch | [paper §6.6](../paper/06-security.md#66-evidence-timing), claim S33 |
| no-alt-prefetch | 9314d89 and later | removes the alternate-line prefetch from `claim` (speed A/B) | claim S35 |
| spillmin4 | 9314d89 only | SpillMin default 4 instead of 1; it is commit ae0b5ad | claim S35 |
| sum-floors | 3b5a427 | ghost floors add the counts that leave instead of keeping the largest (declined) | [record/decisions.md](../record/decisions.md#decided), claim A12 |
| trace | 9be2f39 and later | debugging trace under build tag `tftrace`: installs, evictions, relocations, abandoned moves, sweeps, register resets, stash puts and takes, spills and ghost writes go to a ring of 2^23 records keyed by identity, and the soak's first low answer dumps that key's records (`traceDump`). Without the tag it compiles away (`traceOn = false`) | claims A16 (how the soak's unflagged lows were traced). Checked in a full export of 9be2f39 (with `spec/vectors`): applies, `go vet` with and without the tag, `go test -short` passes with and without it |
| ct-pad-dither-probe | 17f14d9 | `TF_DITHER=<ns>`: a random, key-independent end for ConstantTime pads (the committed dither of the next commit, made switchable); `TF_CTDUMP=<dir>`: TestCTPads writes each mode's raw seen and unseen timings; `TestSpinPeriod` times one spin iteration and a dither draw | claim S41 |
| level-prefetch-and-thp | the commit that adds it (per-candidate batching) | `TF_PF2=1`: once the search's first level fails, load all 32 second-level lines together (slower: S42); `TF_THP=1`: tables of 8 MiB or more 2 MiB-aligned and advised huge (granted, insert classes within 3%: S43); TestInsertTail logs the table's AnonHugePages | claims S42, S43 |
| two-phase-warm | 8cd2f4e | batch `warm` loads every key's primary line, then the alternate line of each key its primary doesn't hold, so displaced keys don't wait on a second miss (row 8) | claim S47: about 9% slower (not adopted) |
| row8-batch-c4c58bb | c4c58bb | the two-phase warm switchable (`TF_WARM2=1`); tables of 8 MiB or more 2 MiB-aligned and advised huge (`TF_THP=1`); PREFETCHT0 in amd64 assembly (`prefetch_amd64.s`) with `TF_PF=1` (prefetch primaries), `2` (both lines of every key), `3` (prefetch primaries, touch keys found there first, then prefetch the rest's alternate lines and touch them last); and `TestRow8` (TestThroughput's batched line, repeated `TF_REPS` times, with the displaced share and AnonHugePages). Applies, vets and passes the batch tests in every arm | claim S47 |
| background-stripe-placer-c4c58bb | c4c58bb | Background's first design: a first touch whose lines are full waits in the eviction stash, and the placer goroutine holds the key's claim stripe through its whole search; `TF_BG=1` forces it on, `TF_BG_DEPTH` sets the deepest search done inline (default 0, one move) | claim E17 |
| background-one-move-inline | the commit that adds Background | Background inserts search one step inline and wait only for longer paths | claim E18 |
| background-room-lock-per-set | the commit that adds Background | one wait-room lock per set (up to 4,096) instead of 64 striped locks | claim E19 |
| background-no-settle | the commit that adds Background | mutation: a key found in neither the table nor the wait room is taken as absent whatever moved meanwhile (the commit's move check off) | claim A19: `TestBackgroundConcurrentCounts` fails |
| background-keep-expired | the commit that adds Background | mutation: a touch that finds its key's wait-room entry expired leaves it there | claim A19: fuzz case 54 fails |
| batch-arms | ed4e271 | `TF_BATCH` picks CheckAndTouchBatch's arm on the committed batch (ordered, 32-key chunks): `loads` (the batch before ccf23af: load every primary line, touch in order), `prim` (prefetch the primaries), `both` (prefetch both lines of every key), `split` (keys found in their primary line touched first, each key's order kept), `inter` and `interhint` (S47's arm 3, without and with the slot handed over), `orderednohint`, `orderednoalt` (the committed arm without the slot handed over, or without the alternate-line prefetch); `TF_CHUNK` sets the chunk (at most 64). `cmd/batcharms` times one arm in a program that imports the library alone. Checked: applies to an export of ed4e271, vets, `go test -short` passes | claims S48, E20 |
| trace-concurrent | 0779e1e | debugging trace for the concurrent fuzzer and the soak, under build tag `tftrace` (without it, it compiles away): touches, misses, wait-room waits, takes and placements, evictions, stash puts and takes, relocations, sweeps, spills, recalls (with the set's wait bit), hot-entry allocations, adds, demotions, reclaims and resets go to a ring of 2^24 records (~940 MB) tagged by run; a failing case of `TestFuzzConcurrent`, or the soak's first low answer, dumps the records naming its key or its wait-room set, and the set's ways. Run with `go test -tags tftrace -run TestFuzzConcurrent` and `TF_CFUZZ_CASE`. Checked: applies to 0779e1e, vets with and without the tag, `go test -short` passes with and without it (first made on 1ea1686; the sweep's record moved with 2408705) | claims A24, A25, A26 (how cases 1068, 4509 and 1426 were explained), A22 |
| cas2-count | 326a1f9 and later | experiment E21: bump's count as one LOCK CMPXCHG16B over both slot words, from word0 and the register as read (`cas2_amd64.s`, with a CPUID probe for CX16; other architectures keep `strayed`), so a count can't land in a same-tag newcomer. `TestCountIntoHeldSameTagSlotLooksAgain` skips: its premise, a count landing in another key's register, is gone. Checked: applies to exports of 326a1f9 and of the tree with A32, vets; `go test -short` passed plain, `TF_BG=1` and `TF_CT=1` | claims E21, S51 |
| trace-concurrent-6b3cb39 | 6b3cb39 (A32) and later | trace-concurrent, ported to A32, with three more records: `recall-miss` (a recall that returns nothing, and why: the stash gate, no entry of the identity in its set, or its entry expired), `stash-over` (a put that overwrites a way holding another identity, as expired or as the lightest, spilled), `over-exp` (an insert that takes a slot over an occupant it read as expired). Touches, hits and hot adds, the most frequent records, are left out unless `TF_TRACE_ALL=1`: with them, case 9787 failed once in 2,300 traced runs (with A32's spill half), against once in about 200 untraced; without them, once in 1,100 (two hunts at once). Checked: applies to 6b3cb39, vets with and without the tag, `go test -short` passes with and without it | claims A32 (how the stash's in-place overwrite was found) |
| no-evict-move-exclusion | d0ac628 and later | the eviction/move exclusion dropped (`moveCount.evicting`, the eviction's back-off while a move of the victim's stripe is under way, the victim scan's skip of such slots, the relocation's two checks; `noexcl.py`, not committed): with copies hidden until the hand-off `Relocate.tla` holds without it. On a6f7ea6: `go test -short` hung in `TestEvictionDuringMoveKeepsHistory/source` (a hook test that holds a move open while it runs an eviction, which now waits for that move instead of backing off: the test's premise is the exclusion), `TF_BG=1` and `TF_CT=1` pass; the racy tests 0, 0, 1, 0 of 300 (TestAbandonedMoveKeepsCounts: its log was lost to `replay.sh stress`'s shared directory, since fixed); the concurrent fuzzer with tags shared 8 ways 10 min 0 of 1,605 cases; soaks with tags shared 8 ways 0 unflagged; test binaries MissInsert, InsertSaturated and Hit 0.97, the rest 1.00–1.01. Not adopted (the rule, written before: every check clean) | [paper §8.2](../paper/08-limitations.md#82-open-problems) |
| placer-factors | dfca83c and later (`patch -p1` or `git apply` in `go/`) | two switchable variants of the Background placer and `TestPlacerFactors` (`TF_EXP=1`), which times unseen-key inserts at 85% load under each, interleaved by table: `HackSpin`, the placer spinning through its idle naps instead of sleeping; `HackNoCount`, callers leaving the shared wait count (`deferred`) alone, the placer finding work and deciding to exit by the wait bits (`anyWaitBit`); both; and the placer run by hand between timed inserts. The room pointer also moves off the line of the room's first lock stripe. Checked: applies to dfca83c's `go/`, vets. Neither variant recovers the hand-run placer's share; spinning made the tail much worse. Not adopted | claims E27, S61 |
| hot-add-ceiling | d0ac628 and later | a hot entry's add in its own unit returns without its CAS (counts nothing): the most per-core cells could gain. Measured: 1.23x Zipf throughput at 12 goroutines, 1.47x at 6 | claims S53 |
| hot-cells | d0ac628 to 30cd90d (later trees renamed `release` to `putFree`) | experiment E23, not measured: per-core cells for a contended hot entry's bucket 0. An add whose CAS of bucket 0 fails twice takes the lock path, which gives the entry a block of 8 one-line cells from a pool of 32 (a lock-free bitmap); later adds pick a cell at random. Each cell's word is bucket 0's protocol (count, closed bit, generation); a writer's lock closes every cell before it reads or changes anything, `load` sums them, `store` folds them into bucket 0, unlock reopens them with the next generation; the block goes back when the entry is freed or its unit advances. `cells_test.go`: 12 goroutines on one hot key across three unit boundaries read the exact count, and about 3% low with the cells left open under the lock. `HotCells.tla` (in `verify/tla/experiments/`): the protocol holds in 12 configurations, up to 11.8 M states, and each guard undone breaks an invariant ([record/decisions.md](../record/decisions.md#scaling)) | claims S53 |
| stash-dup-probe | a9eb39e and later | a probe of the stash's same-identity rule (`stashprobe.go`): after every write of a stash or wait-room entry, the other entries of its set with its identity (within a horizon of each other, or not; the written one older; in the wait room), and every get that returns an entry while a newer one of its identity sits later in the set; `TestStressSoak` and `TestFuzzConcurrent` log the counts. `stashPutAs` and `stashWait` stop at the first free, expired or own way, so an expired way of the key's identity later in the set survives a put into an earlier one; a get finds the newer first. On 5ef42da plus the working tree of then: 94.1 M writes over soaks with tags shared 8 ways (plain, Background, ConstantTime, 60 s) and the concurrent fuzzer (10 min unamplified, 10 min with tags shared 8 ways): 18,493 left a second entry of one identity in a set, every one more than a horizon older than the other; none in the wait room; no get returned an older entry | [paper §8.2](../paper/08-limitations.md#82-open-problems) (a stash model) |
| ct-dither-calibrated | a9eb39e and later (`patch -p1` in `go/`; `git apply` on the reorganized tree, whose comments it was rebased onto) | an arrangement of the working tree (b2): the first ConstantTime `New` times the pad's spin (the fastest of 8 runs of 32 iterations) and widens the dither from 128 ns to the power of two at or above three iterations where that is wider (`spin.go`; a slow clock: ~566 ns a read under qemu-user, 0.5–1.5 µs on HPET or acpi_pm); the draw `rand.Uint64() & ditherMask`. Natively 128 ns; under qemu 1.34–1.57 µs, and `TestCTPadEndIsDithered`, which fails there without it, passes. Not adopted (by its rule): placement-randomized, b2/head read 0.993–1.018 on the plain benchmarks with every interval including 1.00, but upper bounds up to 1.052, over the 1.03 written down | claims S41, S54; [record/decisions.md](../record/decisions.md#constanttime) |
| hot-path-adjacent | ade33ce to 30cd90d (later trees changed `slotWords` and `locate`; `git apply` in an exported `go/`, as `verify/lrab.sh` does) | experiment E26: `slotWords`, `locate`, `locateBoth`, `find` and `farAhead` moved to right after `touch` in `filter.go`, so the hit path's code there is contiguous (profile-guided code positioning by source order), to cut op-cache conflicts (claims S58). No logic changes: `./test.sh sanity` and `regression` pass with it. Reading M9 (16 layouts, cycles/op): placement's sd rose on HitCached, HitHot, TouchAndCount and VelocityCount, HitHot 1.049 slower, TouchAndCount and VelocityCount 0.949 and 0.962. Not adopted |
| batch-size-gate, batch-small-bench | 63b118d and later (`git apply --directory=go` from the root, or `git apply` in `go/`) | experiment E29: the three batch calls skip `warm` on tables whose words take 2 MiB or less (ConstantTime keeps it), and `BenchmarkCheckAndTouchBatchSmall` (a 1 MiB table); batch-small-bench is the benchmark alone, the base arm | claims E29 |
| overflow-line-hint | 63b118d | experiment E30: a 2-bit slice stamp per primary line of the main table in the overflow table (`lines/256` bytes), set by every add with the entries' epoch rule and cleared by `advance`'s sweep; a read skips the overflow table while its line's stamp is dead (ConstantTime always reads). Moves answers (fingerprint matches with other keys' entries) | claims E30 |
| bfs-search, bfs-search-wide | 63b118d and later | experiment E31: `claimAt`'s iterative-deepening search replaced by a read-only breadth-first search over the same pressure-scaled budget, each line scanned once, its path then run back to front by `relocate` restricted to that path (the copy, hand-off and move counts unchanged); bfs-search-wide doubles the budget (1,000) and allows depth 4 | claims E31 |
| saturation-shortcut | 63b118d and later | experiment E32: with the search-pressure EWMA at 15/16 or more, `claimAt` skips `warmAlts` and the search and goes straight to the eviction choice, but for 1 in 16 inserts (by 4 fingerprint bits), which still search; skipped inserts leave the pressure alone | claims E32 |
| doorkeeper, doorkeeper-degraded, doorkeeper-no-overwrite | 63b118d | experiment E33: 32-bit doorkeeper entries (24-bit fingerprint, 8-bit Horizon/2 slice), one 64-byte bucket per 32 slots, allocated on first use and engaged while the search pressure reads saturated; a first touch with no stash history and no entry is kept out of the table (Seen false, Count 1), its second goes in counting the first. doorkeeper-degraded adds the kept-out path's Degraded; doorkeeper-no-overwrite never overwrites (a full bucket sends the key down the normal path) and looks the entry up whenever the doorkeeper exists | claims E33 |
| median-binomial, median-mixture | 63b118d and later | experiment E34: `estimate`'s cut bucket contributes the median of Binomial(n, p), or of an even mixture of Binomial(n, p) and all-or-none, instead of n·p (both n·p above 1,000 events) | claims E34 |
| split-now-register | 63b118d and later | experiment E35, registersim only: the shipped register's geometry as a model (`aligned 7`), and a variant with bucket 0 split into two half-unit 4-bit buckets paid for by 7-bit floating-point counters in buckets 5 and 6 (`aligned 8`); `TestAlignedSteady` (evenly spaced events at 100–700 per horizon, the 60 s count's worst error) | claims E35 |
| incremental-sweep, sweep-latency-test (and -v1 of each) | 63b118d and later (also 2ca617b, unchanged; on main as b5208fb, reverted by 2cdbde9, S79) | experiment E36: `advance`'s clearing becomes a helper; during a slice every `add` sweeps the next 4 buckets of a shared cursor, and a one-slice move of the epoch sweeps what the cursor hasn't reached (a longer move still sweeps everything); `TestLabSweepLatency` (`TF_LAB=1`) times every `TouchAt` of a flooded 1M-key filter. The -v1 pair is the first reading's (all calls timed) | claims E36 |
| time-to-count | 63b118d and later | experiment E37: `TimeToCountAt(h, n, now)`, a cumulative scan of the register at its own epoch (the newest bucket's first event at the last touch, the others spread uniformly; in older buckets the expected order statistics), and `TestLabTimeToCount` (`TF_LAB=1`) on tfbench's velocity workload | claims E37 |
| snapshot | 63b118d (filter.go has changed since: port it); adopted with a versioned, checked format as d03c19d (D27) | experiment E38: `WriteTo` and `ReadFrom` of a quiescent filter into one made by `New` with the same `Config` (a magic word, the shape, the clock anchor, the table, the decay cursor and counters, the stash, the hot table, the overflow table); the built-in clock gains an atomic offset; `TestLabSnapshot` (`TF_LAB=1`) | claims E38 |
| dispbitmap-all-lookups, dispbitmap-all-lookups-v1, contains-absent-big-bench | 63b118d | experiment E41: the displacement bitmap (one bit per line, set by `install`, cleared by the sweep under per-stripe clear counts) consulted by every lookup, inserts included; -v1 reads the clear counts before the bit. contains-absent-big-bench adds `BenchmarkLabContainsAbsentBig` (a Capacity-4M table, ~75 MiB) | claims E41 |
| overflow-prefetch, overflow-prefetch-v1 | 63b118d | experiment E42: when the overflow read is certain (the overflow table live and a count wanted, or ConstantTime), the key's overflow buckets are prefetched right after hashing, and the count batches' `warm` prefetches them beside the primary lines; -v1 prefetched on the hit path too | claims E42 |
| overflow-swar-all-paths, overflow-swar-count-flag, overflow-live-lab-bench | 63b118d | experiment E43: `ghost.estimate` matches each word's three fingerprints at once (`ghMatch`) on every path; -count-flag gives one `estimate` a flag that only counts set. overflow-live-lab-bench: the overflow benchmarks with keys hashed by `mix64`, for instruction counts | claims E43 |
| siphash-scalar, siphash-scalar-v1 | 63b118d and later | experiment E44: `internal/siphash` with the finalization written into each hash, the 8-byte path specialized and the tail read in one overlapping 8-byte load; -v1 indexes the message with an unsafe 8-byte load instead of the slice loop | claims E44 |
| overflow-swar-count | 63b118d and later (also cedeefd) | experiment E45: `ghost.estimateSWAR`, the three-to-a-word fingerprint match (`ghMatch`, `addMatches`), called by `addSketch` for counts only (`count`: CountAt, VelocityCountAt, Count, VelocityCount, the count batch); touches and ContainsAt keep `estimate`; `ghost_swar_test.go` holds the two scans equal | claims E45 |
| dispbitmap-reads | 63b118d and later (also cedeefd); on main as 24ea752, reverted by cbfb6e1 | experiment E46: B1b's bitmap and its upkeep (install sets a line's bit, the sweep clears it under per-stripe clear counts, `Bytes()` counts it, `DispBitmap.tla`, `disp_bitmap_test.go`), consulted by reads only (`locateRead` in `read`); inserts keep `locateAt` | claims A43, S73, E46 |
| wide-registers | 2ca617b | experiment E48: `TestWideRegisters`, a model of register.go's aligned geometries on tfbench's velocity workload (registers of 49–62 bits, identity the rest of 90; counters uncapped, `WIDE_CAPPED=1` capped as lab E9's); test code only | claims E48 |
| local-alt | 2ca617b | experiment E49: `TF_ALT` (64, 32768 or vacuum) makes `alt` an involution inside chunks of L lines (the last full chunk takes the remainder; vacuum: four fingerprint groups with Vacuum filters' ranges); `TestLocalAltPages` counts the distinct 4 KiB pages of each cuckoo search, `TestAltChunkInvolution` checks the function. Unset, the filter is unchanged | claims E49 |
| active-keys | 2ca617b | experiment E50: `ActiveKeysAt(window, now, sampleLines)`: keys whose last touch is within the window, a full scan of the table, stash and wait room or a strided line sample, scaled; `TestActiveKeys`, `BenchmarkActiveKeys` | claims E50 |
| work-counters, work-counters-sweep | 2ca617b; -sweep on 2ca617b + incremental-sweep | experiment E51: `workAdd(kind, n)` at every line, stash set, overflow bucket, hot entry, swept word and decay line a call touches, compiled away without the build tag `tfwork`; `TestWork` reports work per call (p50 to max) on tfbench's churn, a heavy16x trace (`TF_TRACE`), E36's sweep workload and unseen inserts with and without Background. -sweep counts incremental-sweep's swept words | claims E51 |
| backyard | 2ca617b + work-counters (build tag `tfwork`) | experiment E52: `TestBackyard`, a model of Iceberg-style placement on tfbench's churn stream (two lines with no moves, then a backyard of 8-slot buckets with two choices, a per-line overflow bit), against the filter's own counters on the same stream | claims E52 |
| spilled-bit | 2ca617b | experiment E53: word0's bit 45 (lastTouch 45 bits) set at install when `recall` or the overflow table finds history for the key, always under ConstantTime; touch, count and ContainsAt skip the overflow read for a key found with it clear. `verify/tla/experiments/SpilledBit.tla` models it | claims E53 |
| touchn | 2ca617b | experiment E54: `TouchNAt(h, n, window, now)`, n events at one tick through every counting path (register, bump, promotion, realign, hot entries, the first touch), and `TestWeightedRate` against exact counts | claims E54 |
| fine-entries | 2ca617b | experiment E55: `TestFineEntries`, a model of a side table of exact recent ticks for keys whose short windows matter, sized from 0.2 to 1.2 B per key, and an entry for every key | claims E55 |
| merge-all, merge-all-recall | 2ca617b; -recall on merge-all | experiment E56: `Merge` of hot entries (views summed, re-allocated), g's stash entries (merged as slots) and g's overflow table (entries copied bucket for bucket, floors maxed, epoch advanced); `TestMergeAll` at Capacity 512. -recall adds D26's recall to its install | claims E56 |
| colocation, colocation-small-stash | 2ca617b; -small-stash on colocation | experiment E57: `TestColocation`, an attacker's keys sharing one or both of a victim's lines (found by a perfect oracle, or built), against a control of the same volume; `N17_STASH=64` runs it with a 64-entry stash | claims E57 |
| decayed-rate | 2ca617b | experiment E58: `DecayedRateAt(h, tau, now)`, an exponentially decayed event rate from the register's buckets, and `TestDecayedRate` against the exact decayed rate | claims E58 |
| fitted-cut | 2ca617b | experiment E59: `TestFittedCut`, the cut bucket's fraction fitted per context cell on training data (`N20_ADDITIVE=1`: a correction added to the uniform fraction), scored on held-out data; `N20_MAWI` names the MAWI reader's records | claims E59 |
| spilled-bit-model | 8dd007a and later (`git apply --directory=go`) | the runner for experiment E53's model: `verify/tla.sh SpilledBit` (two sizes as designed, then each of four guards undone), the runner finding a model in `verify/tla/experiments/` when `verify/tla/` has none | claims E53. At 8dd007a with `TLC_WORKERS=1`: 7,694 and 6,206 states holding, as first read; each guard undone violates TouchNotLow (530–5,034 states) |
| lab-notebook-experiments-2026-11 | 8dd007a and later (`git apply` from the root) | the notebook of lab/experiments-2026-11, `LAB-RESULTS.md`: host, gates, noise bar, and for E1–E16 the design, the rule as committed before the reading, the readings and the verdict | [below](#the-lab-branches-every-arm-and-where-it-is-recorded) |
| lab-notebook-speed-2026-11 | 8dd007a and later (`git apply` from the root) | the notebook of lab/speed-2026-11, `SPEED-RESULTS.md`: B1, B1c and S1–S7 the same way, and the passing ones written up in full | [below](#the-lab-branches-every-arm-and-where-it-is-recorded) |
| lab-notebook-next-2026-11 | 8dd007a and later (`git apply` from the root) | the notebook of lab/next-2026-11, `NEXT-RESULTS.md` (N1–N21, the gap audit's arms, M1), with its tools and raw readings under `lab/`: `duet.sh` (E47), `cg.sh` and `cgm.sh` (cachegrind), `fd.sh` (strict false denies), `flood.sh`, `gates.sh`, `pads.sh`, `n2.sh`, `a6-repro.sh`, `common.sh`, `ledger.py`; the MAWI reader `lab/mawi` (its own module: E59's `N20_MAWI` records, §7.2's light sources); `lab/readings/`, each arm's raw output. Also three arm patches with no copy above: `next-n14b-resize-evict` and `next-n16c-size-all` (their cases are now `TestResize`'s and `TestSnapshotSize`'s) and `next-n2-b1c-bytes-nobitmap` (prepared, never run) | [below](#the-lab-branches-every-arm-and-where-it-is-recorded) |

## The lab branches: every arm and where it is recorded

Four branches carried experiments after 63b118d, each arm's rule committed before its reading:

- **lab/experiments-2026-11**: E1–E16, 43 commits (93b768c–9263915) from 63b118d.
- **lab/speed-2026-11**: B1, B1c and S1–S7, 30 commits (82fbbe5–c7d55f1) from 63b118d.
- **lab/next-2026-11**: N1–N21, the gap audit's arms and M1, 59 commits (f45141c–c02a974) from 2ca617b.
- **fix/merge-own-stash**: M1's fix as a pull request (c8b6d56, 0c02e0f) from 2ca617b; it went in as 1cb3c35.

The three labs ran on a 4-vCPU KVM guest of the Xeon host's description without performance counters (ns/op and cachegrind's instruction counts; [hosts](../README.md#hosts)); what was adopted was read again on the Ryzen host before it merged. The branches were deleted once every arm below was recorded, so their commits are no longer fetched: each notebook is in a `lab-notebook-*` patch (above), each arm's code in the patch named here, or in the tree where it was adopted. Arm names (E1, S2d, N14b) are the notebooks' own; claim ids (E28–E59, D, A, S) are this repository's.

**lab/experiments-2026-11**

| Arm | Verdict | Recorded as | Code |
|---|---|---|---|
| E1 ConstantTime's dither on a `tsc` clock | not adopted: that clock needs no fix | E28 | — (ct-dither-calibrated is the fix) |
| E2 Go 1.27.3 | adopted (cedeefd) | D23 | `go.mod` |
| E3 a size gate for the batch's lookup pass | not adopted | E29; §8.2 | batch-size-gate, batch-small-bench |
| E4 a per-line overflow hint | not adopted | E30 | overflow-line-hint |
| E5a, E5b breadth-first cuckoo search | not adopted | E31 | bfs-search, bfs-search-wide |
| E6, E6b a saturation shortcut | not adopted | E32 | saturation-shortcut |
| E7, E7b, E7c a first-touch doorkeeper | not adopted | E33 | doorkeeper, doorkeeper-degraded, doorkeeper-no-overwrite |
| E8 median estimators for the cut bucket | not adopted | E34 | median-binomial, median-mixture |
| E9 finer buckets near now, approximate old ones | not adopted | E35 | split-now-register |
| E10 accuracy on real traffic (MAWI) | report | §7.2; the MAWI row of [claims' not-reproducible table](claims.md#not-reproducible) | — |
| E11, E11b an incremental overflow-table sweep | not adopted; read again on the Ryzen host, failed again | E36, S79 | incremental-sweep, sweep-latency-test (and -v1 of each) |
| E12 `TimeToCount` | not adopted | E37; §8.3 | time-to-count |
| E13 `Merge` | adopted (cedeefd) | D24 | in the tree (`merge.go`) |
| E14 snapshot and restore | not adopted (size 2.16% over `Bytes()`, bar 1%); N16's format adopted | E38, D27 | snapshot |
| E15 a SIMD fingerprint compare | unmeasurable: a bound | E39 | — |
| E16 interleaving a batch's keys (AMAC) | unmeasurable: a bound | E40 | — |

**lab/speed-2026-11**

| Arm | Verdict | Recorded as | Code |
|---|---|---|---|
| B1 the displacement bitmap on every lookup | not adopted | E41 | dispbitmap-all-lookups (and -v1), contains-absent-big-bench |
| B1c the bitmap on reads only | adopted (24ea752), reverted (cbfb6e1) and held for the user | S73, A43, E46; §8.4 | dispbitmap-reads |
| S1, S1b prefetching the overflow buckets | not adopted | E42 | overflow-prefetch (and -v1) |
| S2 SWAR overflow match on every path | not adopted | E43 | overflow-swar-all-paths, overflow-live-lab-bench |
| S2c, S2d SWAR overflow match on counts only | S2c failed its rule; S2d adopted (140823d), reverted on the Ryzen host (c305232) | S75, E45 | overflow-swar-count-flag (S2c), overflow-swar-count (S2d) |
| S3 the overflow add word by word | adopted (562c1df) | S77 | in the tree |
| S4 eviction scoring without the view | failed its rule (miss-inserts −0.08% in instructions, −0.05% in the library's own, against −3%) | S76 | — |
| S4b S4 under a rule without MissInsert (written after S4's reading) | adopted (c2c8c0f) | S76 | in the tree |
| S5 bounds checks and inlining | adopted (9946855) | S72 | in the tree |
| S6, S6b a scalar SipHash rewrite | not adopted | E44 | siphash-scalar (and -v1) |
| S7 multi-lane SipHash (AVX-512) | adopted (8269430) | S74 | in the tree (`HashUint64s`) |

**lab/next-2026-11** (and fix/merge-own-stash)

| Arm | Verdict | Recorded as | Code |
|---|---|---|---|
| N1 duet benchmarking | not adopted | E47 | `lab/duet.sh` (notebook) |
| N2 the speed candidates stacked | not run: 2ca617b had adopted them | S72–S78 | next-n2-b1c-bytes-nobitmap (notebook), never run |
| N3 the batch size gate again | not run: it needed N1's wall-clock bar | E29; §8.2 | — |
| N4 14 freed bits, counters capped | void: the capped model is not the filter | E48 | — |
| N4b the same, counters uncapped | worth building: a decision left to the user | E48; §8.4 | wide-registers |
| N5 page-local alternate lines | not adopted | E49 | local-alt |
| N6 `ActiveKeysAt` | not adopted | E50 | active-keys |
| N7 work counters; E11b in work units | counters not adopted; E11b a candidate, failed on the Ryzen host | E51, S79 | work-counters, work-counters-sweep |
| N8 backyard placement, simulated | not worth writing | E52 | backyard |
| N9 a per-key spilled bit | not adopted | E53 | spilled-bit, spilled-bit-model |
| N10 weighted touches (`TouchNAt`) | not adopted | E54 | touchn |
| N11 a conditional touch | adopted (4579f36) | A45, S81 | in the tree (`TouchIfUnder`) |
| N12 fine entries for short windows, simulated | not worth building | E55 | fine-entries |
| N13 MAWI's light sources | report | §7.2; claims' MAWI row | `lab/mawi` (notebook) |
| N14 resizing | adopted (50e9f5a) in the scope N14b and N14d set | D28, S81 | in the tree (`Resizable`) |
| N15 `Merge` of hot entries, stashes and overflow tables | not adopted | E56 | merge-all |
| N15b N15 with the stash recall | not adopted | E56 | merge-all-recall |
| N16 a checked snapshot format | adopted (d03c19d) | D27, S80 | in the tree (`WriteTo`, `ReadFrom`) |
| N17 a co-location attack | report | E57 | colocation |
| N18 a stash model in TLA+ | the premise holds; it found D24's `Merge` defect | A44 | in the tree (`StashSet.tla`) |
| N19 `DecayedRateAt` | not adopted | E58 | decayed-rate |
| N20, N20b a fitted cut fraction | not adopted | E59 | fitted-cut |
| N21 an AVX-512 overflow-table scan | not run: cachegrind can't decode AVX-512 and N1 gave no wall-clock bar | §8.3 | — |
| M1 `Merge` keeps f's stashed history | adopted (1cb3c35, from fix/merge-own-stash) | D26 | in the tree |

The gap audit re-ran N14–N20 against their specification and gave each gap that needed evidence an arm with its own rule:

| Arm | Verdict | Recorded as | Code |
|---|---|---|---|
| N14b grow and shrink into sizes that evict | narrowed N14's claim: a grow from a filter that had evicted carries its losses for a horizon (4,412 unflagged lows, all in the transition) | D28 | `TestResize`'s grow from 1,280 (next-n14b-resize-evict in the notebook) |
| N14d a shrink that evicts | holds | D28 | `TestResize`'s shrink to 1,280 |
| N16b N16's clock line, 3 runs | holds: the line is wall-clock by design | D27 | — |
| N16c the size with every section | void: the overflow table was never allocated (single-touch histories are shed under SpillMin 4) | — | next-n16c-size-all (notebook) |
| N16f N16c with SpillMin 1 | holds: +0.006% | D27 | `TestSnapshotSize` |
| N17b N17 over 10 seeds | holds | E57 | colocation |
| N17c N17 with a 64-entry stash | 19.7% of the victim's reads Degraded and 22 unflagged lows: a contract question left to the user | E57; §8.4 | colocation-small-stash |
| N18c StashSet again | holds: the holding runs' state counts identical | A44 | — |

The audit's other arms were proposed and not run; each gap is stated where its claim is:

| Arm | What it would read | Stated in |
|---|---|---|
| N14c | a lock-free `Resizable` whose move recalls the new filter's stash, fed by 4 goroutines | D28's scope; [record/decisions.md](../record/decisions.md) (resizing) |
| N16d | 100,000 structured mutations of a snapshot with its check words recomputed: refused, or restored consistent | D27 (a snapshot is trusted input) |
| N16e | a big-endian restore (`GOARCH=s390x` under qemu) | D27 |
| N17d | the draws a timing attacker needs, on a host with a usable wall-clock bar | E57 (no timing oracle measured) |
| N18b | StashSet with ConstantTime's parked keys and `putBack` | A44 (not modelled) |
| N19b | `DecayedRateAt` with hot keys read through their entries and evicted keys through the overflow table | E58 |
| N20c | the fitted cut trained on three or more trace days and more seeds | E59 |
