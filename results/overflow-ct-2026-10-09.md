# Overflow and ConstantTime round (2026-10-09, second)

Two open problems from `security-cost-2026-10-09.md`, with one constraint: **no extra cost** (memory, insert or lookup time).

1. **Heavy floods of 2x the table's slots or more** saturated the overflow count-min sketch. Its noise per counter grew past any useful limit, so a strict limit of 20 turned away 88–90% of new keys at 2x, and offenders' `Count` was lost.
2. **`ConstantTime` cost** at least a 3 µs spin per call, `Count` included.

## Adopted

| Change | Security | Cost | Check |
|---|---|---|---|
| **Ghost overflow table** instead of the count-min sketch, in the same bytes: one 21-bit entry per dropped history (14-bit fingerprint, 5-bit count, 2-bit slice), 23 entries + a floor per 64 B bucket. A full bucket evicts its lightest history into the bucket's floor, which `Upper` adds, so `Upper` stays a bound | 2x heavy flood: strict collateral **1.8%** (sketch 88–90%), offenders' `Count` kept for 195 of 200 (sketch 0). The point where collateral climbs moves from ~1.5x to ~3x | same 4.7 B/key under attack, 0 otherwise. Reads one cache line instead of four. 0.1–0.2% of new keys read `Degraded` through fingerprint matches while the table is full (sketch: 0%) | A7, A11 |
| **`Count` pads to its own tracked p99.5** (capped at `ConstantTime`). A sampled call moves the estimate up 100 ns if it ran past, down 0.5 ns if not. `Touch` keeps the fixed pad; `AutoPad` tracks both | KS D 0.011 (vs 0.005 at a fixed 3 µs; the 95% noise floor at 20k samples is ~0.014). Under 16 goroutines: D 0.015 vs 0.060 | `Count` 1.05 µs per call instead of 3.4 (−69%); 2.19 vs 1.06 M ops/s under load | S33, S34 |
| **Alternate lines loaded together** before the cuckoo search (the earlier `reloc-prefetch.patch` idea) | none | removing it made near-full and saturated inserts 9–10% slower in this round's A/B (5 rounds); the earlier round measured no change, so treat it as small | S35 |

`SpillMin` stays at 4. With the ghost table it hardly affects collateral (A10), but spilling every history made inserts into a full table 30% slower (S35).

## Tried, not adopted

| Idea | Result | Why not |
|---|---|---|
| Ghost table with `SpillMin` 1 (keep every history, so dedup of one-event keys survives floods) | collateral 0.1% at 1x and on light floods (vs 0.0%); saturated inserts 1.30x | the insert cost breaks the constraint. It is one setting (`SpillMin: 1`) for callers who need it |
| Wider ghost table, slots/2 (9.4 B/key under attack) | 3x flood: 188 recovered, 2.5% collateral; 4x: 44% | it is only a setting (`Overflow`); the default keeps the attack-time memory |
| Narrower ghost table, slots/8 (2.4 B/key) | 2x: 77.8% collateral, still better than the full-size sketch | the default size keeps 2x floods cheap |
| Probabilistic `Upper` from the sketch (count-mean-min + z·σ instead of the row minimum) | not run: at a 2x flood σ ≈ 40 events per counter, so the bound sits near 200 for a limit of 20 | noise from heavy lumps can't be subtracted; only per-key memory (the ghost table) removes it |
| Sub-bucket floors (several floors per bucket, by fingerprint bits) | not run: they cost entries, and past 3x every floor rises anyway | no gain where floors matter |
| **Shadow work**: each `ConstantTime` path also runs the other's work read-only (an unseen `Count` estimates a neighbour's register; a seen `Touch` walks the insert search without writing) | unpadded D: `Count` 0.31 → 0.07–0.10, `Touch` 0.43 → 0.09–0.13. With `Count`'s tracked pad: D 0.005 vs 0.007–0.012, about the same cost. For `Touch` the walk doubles the seen side's work and its p99 rose to 4–7 µs, so a 1.5 µs pad still leaked D 0.035–0.08 | `Count`'s tracked pad gets the same leak without it; for `Touch` it costs more than the fixed pad. Patch: `ct-shadow-and-prefetch-toggle.patch` |
| `AutoPad` for `Touch` too | settles at 3–5 µs depending on the run, D 0.009–0.014 | no cheaper than a fixed 3 µs on this host; kept as an option (no per-host number to pick) |
| Fixed 1 µs pad for both | `Count` D 0.012–0.018, `Touch` D 0.14–0.17 | `Touch`'s inserts run past it |
| Deferring evictions and relocations off the call path (insert into the stash, relocate later) | not run: a key waiting in the stash would make its next touch take the slow path, which is itself a seen-key signal, and it needs a background worker | the extra machinery and its leak outweigh the saving |

Patches: `verify/experiments/ct-shadow-and-prefetch-toggle.patch` (shadow work, `TestCTProfile`, `TF_NOPF=1` disables the prefetch), `no-alt-prefetch.patch`, `spillmin4.patch` (against e0a1e21; it is commit 49df095).

## Data

Overflow. 4,818 slots at 85%, 200 offenders × 20 events, then the flood, all within 30 s. Per flood: offenders with `Count` ≥ 18 | new keys `Degraded` | new keys turned away by a strict limit of 20. Spill threshold 1 (everything spilled). `TF_EXP=1 go test -run TestSketchShapes -v`.

```
                        heavy 1x          | 2x                | 3x                | 4x                | light 16x         | 64x
8b  r4 s2 /4 (sketch)  4.7 B/key | 180 0.0% 0.0% |   0 0.0% 89.7% |   0 0.0% 99.7% |  0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
ghost /8               2.4 B/key | 200 0.1% 0.1% |  23 0.2% 77.8% |   1 0.1% 100.0%|  1 0.2% 100.0% | 200 0.1%   0.1% | 200 0.2%   0.2%
ghost /4 (new)         4.7 B/key | 200 0.1% 0.1% | 195 0.2%  1.9% |  18 0.2% 89.2% |  1 0.2% 100.0% | 200 0.1%   0.1% | 200 0.1%   0.1%
ghost /2               9.4 B/key | 200 0.0% 0.0% | 200 0.1%  0.1% | 188 0.1%  2.5% | 89 0.2%  43.8% | 200 0.2%   0.2% | 200 0.2%   0.2%
```

With the defaults (`SpillMin` 4), `TestAttackFloodCollateral`: strict collateral 0.0% / 1.8% / 100% / 100% / 0.0% at 1x / 2x / 4x / 16x heavy and 16x light; offenders short 0 / 5 / 200 / 200 / 0.

ConstantTime. Fresh 2M-key table at 85% per row, 80k untimed calls to settle, then 40k timed, seen and unseen mixed. `TF_CTEXP=1 go test -run 'TestCTPads|TestCTParallel' -v`.

```
off              | Count D 0.254 (62.7%) p50   222/  246,   346 ns/op | Touch D 0.705 (85.3%) p50   648/  300,   702 ns/op
fixed 1us        | Count D 0.012 (50.6%) p50  1093/ 1094,  1208 ns/op pad 1000ns | Touch D 0.172 (58.6%) p50  1102/ 1098,  1309 ns/op pad 1000ns
fixed 3us        | Count D 0.005 (50.3%) p50  3126/ 3126,  3385 ns/op pad 3000ns | Touch D 0.012 (50.6%) p50  3094/ 3094,  3310 ns/op pad 3000ns
3us, Count auto  | Count D 0.011 (50.5%) p50   933/  935,  1052 ns/op pad  793ns | Touch D 0.026 (51.3%) p50  3103/ 3102,  3383 ns/op pad 3000ns
AutoPad          | Count D 0.012 (50.6%) p50  1294/ 1290,  1467 ns/op pad 1410ns | Touch D 0.010 (50.5%) p50  2990/ 2987,  3192 ns/op pad 3040ns
p50 is unseen/seen; ns/op is the mean wall time per call on one goroutine
fixed 3us         1.06 M ops/s; probe p50 unseen    3.1 us seen    3.1 us; KS D 0.060 (53.0%); Count pad 3000 ns
3us, auto Count   2.19 M ops/s; probe p50 unseen    1.6 us seen    1.6 us; KS D 0.015 (50.7%); Count pad 1041 ns
```

Speed, `verify/ab.sh 0651be5 49df095` (6 rounds, B/A of medians):

```
A = 0651be5
B = 49df095
benchmark (ns/op)                       A min   A median      B min   B median     B/A
BenchmarkTouchAndCount-4                210.0      235.6      229.7      241.2    1.02
BenchmarkInsertSteady-4                 400.9      441.6      429.2      445.6    1.01
BenchmarkHugeAbsent-4                   165.1      177.5      159.6      193.8    1.09
BenchmarkCheckAndTouchHit-4             180.4      199.8      190.3      201.4    1.01
BenchmarkContainsAbsent-4               107.7      116.4      103.5      119.4    1.03
BenchmarkInsertNearFull-4              3541.0     3881.0     3383.0     3881.0    1.00
BenchmarkVelocityCount-4                178.0      193.2      178.4      194.8    1.01
BenchmarkInsertSaturated-4             1095.0     1266.0      941.8     1067.5    0.84
BenchmarkCheckAndTouchHitHot-4           39.5       40.3       39.8       42.8    1.06
BenchmarkVelocityCountHot-4              58.1       62.5       58.0       63.6    1.02
BenchmarkHugeHit-4                      285.5      315.4      298.1      321.1    1.02
BenchmarkCheckAndTouchParallel-4         62.7       72.4       63.7       74.4    1.03
```

## Still open

- **Floods of 3x the slots or more** still overflow the ghost table's buckets. Their floors then cover every key's `Upper`, and the offenders' `Count` is gone. Past that point a flood carries more histories than the bytes can name, so the only lever left is size (`Overflow`).
- **`Touch` under `ConstantTime`** still spins ~3 µs, set by inserts that need a cuckoo search.
- **`Count`'s pad needs a warm-up.** It starts at the cap and settles within ~100k calls.
