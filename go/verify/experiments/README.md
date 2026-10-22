# Reverted experiments as re-runnable patches

Every `*.patch` here is a plain `git diff` against **fbe6968** (Go 1.24.7; the module was then at the repository root, as
`github.com/scooter-sensei/temporalfilter`), except
`lockfree-dup-repair-70546b2.patch` (applies to **70546b2**) and
`insert-saturated-bench-b3e9aa5.patch` (adds 378bc37's saturated-insert
benchmark to **b3e9aa5**, for the speed comparison in `verify/speed.sh`).

```sh
git clone /mnt/project-files/temporal-filter tf && cd tf && git checkout fbe6968
git apply verify/experiments/<name>.patch     # or: verify/experiments.sh, verify/ab.sh
```

The experiment code was recovered from the earlier session transcript, which
recorded the edits as Python and heredoc scripts. Each patch's notes say where
the code was ported to HEAD and how. ("HEAD" in these notes means fbe6968.)
The raw logs of the checks below were not kept; `verify/experiments.sh`
re-runs every deterministic one and `verify/run.sh` compares the result with
`verify/expected/experiments.txt`.

Each patch was applied to a fresh worktree at fbe6968 and checked with
`git apply`, `go build ./...`, `go vet ./...` and `go test -short -count=1 ./...`.
Nine patches were also run with `go test -race -short -count=1 .`
(see the race check section at the end). No speed benchmarks were run. Any
ns/op in the notes are the original session's numbers, given as context only.

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
probe count summed across workers. HEAD matches the baseline the original
session printed for the same columns.

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

This is how the original session ran its comparisons: build test binaries for
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

**Claim** (docs/RESEARCH.md §3.1): "`TestSprayCannotEvictRepeatOffender` sprays 20× capacity of one-off keys. All 50 offender keys keep their exact count of 20. The same test **fails with the old oldest-first policy**. Insert latency is unchanged (~230 ns)."

**Change:** the eviction victim score in `claim` becomes `ts` (oldest timestamp only) instead of `mass<<48 | ts`. This is the original session's sed edit.

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

**Change:** this adds the new package `internal/clockbench`: an RDTSC asm function plus the original microbenchmarks (`TimeSince`, `Nanotime` via linkname, `MaphashString`, `RDTSC`, and `MissNanotime`/`MissRDTSC`/`MissNoClock`, which pair the clock read with a random load from a 256 MB table). The filter is not touched. The original lived in a scratch module; it moved into the repo here. The 256 MB table is now allocated on first use (with `b.ResetTimer`) instead of at package init, so `go test ./...` doesn't pay for it.

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
These are identical to the original session's numbers.

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

**Porting notes:** HEAD already has the displaced bit, so `locate` keeps HEAD's displaced-bit matching and adds the spill skip. HEAD's `install` writes both words (register first), so the original's separate word1 CAS inside `place` is folded into `install`. The concurrent audit test from the session (`spill_test.go`, `TestSpillCountsNeverUnderCount`) is included, with its two-line search switched to HEAD's `findAny`. The original diff still applies cleanly at 68eee6c (`git apply --check` ok).

**Deterministic and correctness checks:**
```sh
go test -count=3 -run 'TestSpillCountsNeverUnderCount|TestDisplacedBitTracksLine' -v .
go run ./cmd/tfbench -quick
```
- The audit passes on every run with `lines exact 5883, over-counted 0`. Relocation and eviction counts vary from run to run; the line counts did not.
- The tfbench -quick row is identical to HEAD: `| 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 |`.

**Speed:** REGEX `ContainsAbsent$|CheckAndTouchMissInsert$|CheckAndTouchHit$|VelocityCount$|InsertSaturated|InsertNearFull`, T `300ms`, 6 interleaved rounds. Then REGEX `InsertSteady`, T `1000000x`, 4 rounds, and REGEX `Latency`, T `500ms`, 4 rounds (alternate-line hits waiting on one more load).

## 10. register-46.patch and register-44.patch (deterministic)

**Claim** (backlog, Decided): "46-bit register + 14-bit tag: FPR 1.8e-8 → 7e-9, vel MAE 0.081 → 0.085, per-key count cap per horizon 1000+ → ~876." and "44-bit register + 16-bit tag: FPR → 0 seen (~1e-9 expected), vel MAE 0.088, cap ~630."

**Change:** this is the session's scratch-copy transform. `regBits` becomes 46 (or 44) and `tagBits = 64 - regBits - 4`. The tag is taken from hash bits `48-tagBits..47`, and `primary` uses the bits below the tag. The epoch moves to `regBits`. Bucket widths become {5,5,6,7,7,8,8} for 46 bits and {5,5,6,7,7,7,7} for 44 bits.

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
These match the original session's rows exactly.

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

**Result:** vel MAE **0.083** against HEAD 0.081. Per window (1s/5s/15s/30s/60s): 0.234 0.058 0.054 0.046 0.024, against HEAD 0.227 0.058 0.051 0.044 0.024. The other columns are identical to HEAD. In the original session, against the older estimator, it was 0.239 0.067 0.076 0.048 0.024 against uniform 0.227 0.066 0.070 0.047 0.024.

## 12. eight-slot.patch (deterministic FN and FPR; speed for the rest)

**Claim:** "8-slot (128-byte) buckets: 0 misses at 98% load and 3x cheaper near-full inserts, but ~10% slower everywhere else. Reverted. Re-tested on DRAM (round 8): near-full inserts 2945 → 464 ns and steady inserts slightly faster, but DRAM hits 208 → 227 ns, absent keys 127 → 200 ns …, saturated inserts 514 → 793 ns, and ~2x false positives. Still not worth it."

**Change:** this is the round-8 port. `slotsPerLine = 8` (a 128-byte bucket made of two cache lines, aligned to 128 B by the existing `alignedWords`), and `warm` loads both 64-byte lines of each bucket. The original commit 4103013 also set relocDepth 4 and relocBudget 1000. Those were relocation settings under test at the time, and the round-8 port did not carry them, so they are not included here.

**Measure:** `go run ./cmd/tfbench -quick`

**Result:**
```
eight-slot | 10-08 16:12 | fbe6968+ | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.90e-08 | 100% | 18.8 | 0.081 | ...
```
- FN at 1.15x is **0.000%** (HEAD 0.051%), so the "0 misses at 98% load" part holds.
- Static FPR is **2.90e-08** against 1.80e-08, about **1.6x**. The "~2x" in the claim was stated, not measured, in round 8.

**Speed** (not `-short`; the Huge benchmarks build a 600 MB table): REGEX `Huge|CheckAndTouchHit$|CheckAndTouchHitHot$|InsertSteady|InsertSaturated|InsertNearFull|VelocityCount$|ContainsAbsent$`, T `1000000x`, 3 interleaved rounds.

## 13. lockfree-dup-repair (complete in the transcript; nondeterministic result)

**Claim:** "Lock-free duplicate repair (in-flight marker + lower index wins): cut duplicates 10x but can't be made symmetric. Replaced by the stripe lock."

The transcript holds the complete code: three edits on top of 70546b2, before the stripe lock existed. Replaying those edits on 70546b2 reproduces the diff the session printed, byte for byte. Three files:

- **lockfree-dup-repair-70546b2.patch** is the original, applied to **70546b2**, not HEAD. It has the in-flight marker (`install` zeroes word1 before publishing word0, and `find`/`dup` match fp with word1 == 0), a claim-time scan for a racing insert of the same key, `claim` returning -1 on a lost slot, and a post-claim `dup` check where the higher index yields. It also adds the session's `dup_test.go`.
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
- The original cuts duplicates about 16x by mean (23x by median). The original session saw 1889–2648 against 149–230.
- The HEAD port cuts them about 6x.
- The stripe lock at HEAD gives 0.

## 14. contention-bench.patch (speed)

**Claim:** "Hot-key contention (round 8 check, not a change): 4 goroutines on 1 key run at 10 ns/op aggregate, 16 keys at 16 ns, 16K keys at 22 ns. Same-tick touches skip the word0 CAS and full registers skip the word1 CAS, so a bot storm doesn't ping-pong the line. Touch elision isn't needed."

**Change:** this appends the session's `benchContended` (RunParallel on 1, 16 or 16K keys with a shared clock that advances every 64 ops) to bench_test.go. The benchmarks are renamed from `BenchmarkZZ*` to `BenchmarkContended1`, `BenchmarkContended16` and `BenchmarkUncontended` (16K keys).

**Measure (speed):** `go test -run '^$' -bench 'Contended|Uncontended' -cpu 4 -count 2 .`

## 15. Pure dyadic register (H/32): does registers_experiment_test.go reproduce it?

**Claim:** "Pure dyadic register (H/32, widths to 32 units): same mean MAE as the H/24 geometry but 60s error 0.040 vs 0.024."

**No.** `registers_experiment_test.go` is a standalone simulation. It has its own float model, 5 traffic shapes × 40 trials, and its own estimator. It compares linear, dyadic H/16, dyadic H/32 and Laplace registers, and it has **no H/24 geometry row**, so it cannot show H/32 against H/24. Its output for reference (`go test -count=1 -run '^TestRegisterExperiment$' -v .`, full table in `verify/expected/register-experiment.txt`):
```
mean over all traffic x windows:
  linear 6x8 (H/5)                   0.154
  dyadic 6 buckets (H/16 near now)   0.122
  dyadic 7 buckets (H/32 near now)   0.108
  laplace 3x16                       0.268
  laplace 4x12                       0.232
```
The claim came from swapping the geometry constants in register.go and running `tfbench -quick`. **dyadic-h32.patch** does exactly that, with the session's values: `geomUnits = 32`, logStep {0,0,1,2,3,4,5}, off {0,1,2,4,8,16,32}, bits {5,5,6,6,7,8,11}. It also changes one test fixture: `TestUnitOfMatchesDivision` hard-coded the smallest legal horizon as 48 µs (2×24 units), which `New` rejects at 32 units. It now uses `2 * unitsPerHorizon * time.Microsecond`, which is 48 µs at HEAD, so the test is unchanged there.

**Result (deterministic, `go run ./cmd/tfbench -quick`):**
```
dyadic-h32 | 10-08 16:12 | fbe6968+ | 0.000% | 0.000% | 0.059% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.082 | ...
vel MAE 1s/5s/15s/30s/60s: H/32 0.204 0.065 0.057 0.045 0.038 (mean 0.082) | HEAD H/24 0.227 0.058 0.051 0.044 0.024 (mean 0.081)
```
Mean MAE is the same within 0.001, and the 60 s error is **0.038 vs 0.024**. The backlog's 0.040 was measured with the older estimator (before commit a477d31). HEAD's estimator gives 0.038, the same value the session measured after that change.

## Race check

`go test -race -short -count=1 .` in the patched worktrees passed for all nine patches it was run on: cas2-cmpxchg16b, homing, spill-counts, branchless-find, eight-slot, reciprocal-clock, thp-madvise, reloc-prefetch and piecewise-linear. It was not run on the patches that fail a test on purpose, nor on the benchmark-only or geometry patches. The race detector cannot see the asm CAS in cas2-cmpxchg16b.

## Note on HEAD itself

`TestTouchRacingRelocationKeepsCounts` is slightly flaky at HEAD on this shared host. It failed in 5 of 30 standalone runs in one batch and 2 of 30 in another, losing 2 to 21 counts, and once 1116. Backlog item 16 says 0 of 20. Take this into account when a patch's verify log shows this test failing.

## Later rounds (patches against their own commits, not run by experiments.sh)

| Patch | Applies to | What it does | Where it is measured |
|---|---|---|---|
| pad-modes-and-sip13 | 150fac4 and later | sleep, adaptive and yield ConstantTime pads; SipHash-1-3 | `results/security-cost-2026-10-09.md`, claims S30, S31 |
| ct-shadow-and-prefetch-toggle | 9314d89 and later | shadow work in ConstantTime (each path also runs the other's, read-only), `TestCTProfile` (pad sweep), `TF_NOPF=1` to skip the alternate-line prefetch | `results/overflow-ct-2026-10-09.md`, claim S33 |
| no-alt-prefetch | 9314d89 and later | removes the alternate-line prefetch from `claim` (speed A/B) | claim S35 |
| spillmin4 | 9314d89 only | SpillMin default 4 instead of 1; it is commit ae0b5ad | claim S35 |
| sum-floors | 3b5a427 | ghost floors add the counts that leave instead of keeping the largest (declined) | `results/backlog.md` (Decided), claim A12 |
| trace | 4b31a89 and later | debugging trace under build tag `tftrace`: installs, evictions, relocations, abandoned moves, sweeps, register resets, stash puts and takes, spills and ghost writes go to a ring of 2^23 records keyed by identity, and the soak's first low answer dumps that key's records (`traceDump`). Without the tag it compiles away (`traceOn = false`) | claims A16 (how the 2026-10-21 soak lows were traced). Checked 2026-10-21 in a full export of 4b31a89 (with `spec/vectors`): applies, `go vet` with and without the tag, `go test -short` passes with and without it |
| ct-pad-dither-probe | 4a8aa22 | `TF_DITHER=<ns>`: a random, key-independent end for ConstantTime pads (the committed dither of the next commit, made switchable); `TF_CTDUMP=<dir>`: TestCTPads writes each mode's raw seen and unseen timings; `TestSpinPeriod` times one spin iteration and a dither draw | claim S41 |
| level-prefetch-and-thp | the commit that adds it (per-candidate batching) | `TF_PF2=1`: once the search's first level fails, load all 32 second-level lines together (slower: S42); `TF_THP=1`: tables of 8 MiB or more 2 MiB-aligned and advised huge (granted, insert classes within 3%: S43); TestInsertTail logs the table's AnonHugePages | claims S42, S43 |
| two-phase-warm | dd18c6e | batch `warm` loads every key's primary line, then the alternate line of each key its primary doesn't hold, so displaced keys don't wait on a second miss (row 8) | not yet measured (status note, Remaining goals) |
| background-stripe-placer-f298a7a | f298a7a | Background's first design: a first touch whose lines are full waits in the eviction stash, and the placer goroutine holds the key's claim stripe through its whole search; `TF_BG=1` forces it on, `TF_BG_DEPTH` sets the deepest search done inline (default 0, one move) | claim E17 |
| background-one-move-inline | the commit that adds Background | Background inserts search one step inline and wait only for longer paths | claim E18 |
| background-room-lock-per-set | the commit that adds Background | one wait-room lock per set (up to 4,096) instead of 64 striped locks | claim E19 |
| background-no-settle | the commit that adds Background | mutation: a key found in neither the table nor the wait room is taken as absent whatever moved meanwhile (the commit's move check off) | claim A19: `TestBackgroundConcurrentCounts` fails |
| background-keep-expired | the commit that adds Background | mutation: a touch that finds its key's wait-room entry expired leaves it there | claim A19: fuzz case 54 fails |
