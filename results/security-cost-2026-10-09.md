# Security and cost round (2026-10-09)

Starting point: 55c82ed..a96dc28. The overflow sketch stopped floods from erasing histories, but it cost 7.1 B per key from the start, saturated inserts ran 65% slower, and `ConstantTime` burned at least 3 µs of CPU per call. Two weaknesses were left. A cheap flood of one-event keys made a strict limit turn away every new key. A heavy flood hid dropped histories from `Count`.

## Adopted

| Change | Security | Cost | Check |
|---|---|---|---|
| **8-bit counters, Horizon/2 slices**: 4 rows × 4 slices × 1 B, so a column's ring fits one word | Same or better than the old shape at every flood size, because two thirds of the memory buys twice the columns | 4.7 B/key instead of 7.1 (−34%). One load per row instead of two | A9 |
| **Lazy allocation**: the sketch is created on the first spill | none | 0 B/key until a history is dropped. Normal load never drops one (D1: 0 missed at 1.15x) | A9 |
| **`SpillMin` (default 4)**: a dropped history with fewer than 4 events in the horizon is shed (`Shed()`), not spilled | A 16x flood of one-event keys turns away 0% of new keys under a strict limit, was 100%. At a 1x heavy flood, evicted 20-event offenders read `Count` 18 on average, was 0 | fewer sketch writes: in the 1x flood, 102 spills instead of 3,693 | A10 |

The cost of `SpillMin`: a key with at most 3 events in the horizon loses them when it is evicted and the stash drops it. Getting that to happen still takes about a table's worth of heavier keys (A3), so it is no shortcut for an attacker. Dedup callers lose those keys' "seen" after such a flood. Before this change those keys didn't read `Seen` either: a one-event history never cleared the sketch's noise floor, so only `Upper` showed it.

## Tried, not adopted

| Idea (source) | Result | Why not |
|---|---|---|
| 16-bit counters, Horizon/4 slices (the previous default) | at 7.1 B/key: 152 of 200 offenders recovered at a 1x heavy flood, 99.8% strict collateral at 2x | the new shape does better on both (180, 89.7%) at 4.7 B/key |
| 3 rows instead of 4 | 16-bit at 5.3 B/key: same as 4 rows at 1x, slightly worse Degraded false-positive rate (0.4%) | the 8-bit 4-row shape is smaller and better |
| Wider sketch, slots/2 (9.4 B/key once allocated) | 200 of 200 offenders recovered at 1x, 49% collateral at 2x heavy (was ~90%) | it is only a setting: `Overflow: slots/2`. The default stays at slots/4 to keep the attack-time memory small |
| Spill threshold 2 or 8 | 2 stops only one-event floods. 8 also stops 2- and 4-event floods, but a key can then lose 7 events per eviction | 4 balances the two; it is configurable |
| Escalating sketch: allocate a 2× wider layer when the noise passes a bound (an idea in the spirit of SALSA's counter merging) | not run. Old layers keep their noise until they age out, so for the same peak memory it can't beat allocating the full width at once | lazy allocation already gives the "pay only under attack" part |
| LSB sharing / late merging of saturated counters ([Resiliency of Sketch-based Security](https://arxiv.org/abs/2503.11777)) | not run. It targets pollution that saturates small counters; our 8-bit counters saturate only in floods past 4x, where the noise has already swamped the counts | no gain where it matters |
| Cold Filter's two-layer prefilter ([Zhou et al., SIGMOD 2018](https://minlanyu.seas.harvard.edu/writeup/sigmod18.pdf)) | `SpillMin` is the cheap version of its idea: keep cold items out of the counting sketch | a second counting layer would add memory and writes for little more than `SpillMin` gives |
| **Timing: sleep to the pad** instead of spinning | `time.Sleep` here never woke in under ~1.1 ms, and every wake-up cost ~64 µs of CPU | slower and costlier than spinning 3 µs |
| **Timing: adaptive pad** that doubles on every overrun and halves after quiet runs ([Askarov, Zhang, Myers, CCS 2010](https://www.cs.cornell.edu/andru/papers/timing.html)) | the same leak as a fixed pad, but one GC pause pushed the pad to 128–512 µs and it stayed there | a fixed pad is cheaper and predictable |
| **Timing: yield while padded** (`runtime.Gosched`) | 16 goroutines on 4 cores: 1.31 M ops/s vs 1.10 spinning, but each call's median latency went from 3.1 µs to 11 µs, and KS D rose from 0.031 to 0.063 | a little more throughput for 3.5× the latency and twice the leak |
| Timing: spin 10 µs | KS D 0.011–0.016 vs 0.014–0.023 at 3 µs | 3.3× the CPU for a marginal gain |
| SipHash-1-3 for `Config.Key` (as Rust's HashMap uses) | 10.1 ns vs 15.6 ns per integer key (−35%) | 2-4 is the standard PRF, and the default seeded hash already costs 6 ns. Worth revisiting if keyed throughput matters |

The timing variants and SipHash-1-3 are kept as `verify/experiments/pad-modes-and-sip13.patch`. `secexp_test.go` (`TF_EXP=1`) re-runs the sketch tables below.

## Data

Sketch shapes. 4,818 slots at 85%, then 200 offenders × 20 events, then the flood. Each flood fits within 30 s of the 60 s horizon. Per flood: offenders with `Count` ≥ 18 | new keys `Degraded` | new keys turned away by a strict limit of 20 (`Degraded` or `Upper` > 20). Spill threshold 1 (spill everything).

```
16b r4 s4 /8 (old)   7.1 B/key | 152 0.0% 0.0% |  0 0.0% 99.8% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
16b r4 s4 /16        3.5 B/key | 149 0.0% 0.6% |  0 0.0% 100.0%| 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
16b r4 s4 /4        14.1 B/key | 180 0.0% 0.0% |  0 0.0% 89.7% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
8b  r4 s4 /4         9.4 B/key | 180 0.0% 0.0% |  0 0.0% 89.7% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
8b  r4 s4 /2        18.8 B/key | 200 0.0% 0.0% |  0 0.0% 49.4% | 0 0.0% 97.0%  | 200 0.0% 64.1%  | 200 0.0% 100.0%
16b r3 s4 /8         5.3 B/key | 153 0.0% 0.4% |  0 0.0% 99.8% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
8b  r3 s4 /4         7.1 B/key | 177 0.0% 0.0% |  0 0.0% 92.7% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
16b r4 s2 /8         4.7 B/key | 152 0.0% 0.0% |  0 0.0% 99.8% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
8b  r4 s2 /4 (new)   4.7 B/key | 180 0.0% 0.0% |  0 0.0% 89.7% | 0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
8b  r4 s2 /2         9.4 B/key | 200 0.0% 0.0% |  0 0.0% 49.4% | 0 0.0% 99.0%  | 200 0.0% 81.3%  | 200 0.0% 100.0%
floods: heavy (21-event keys) 1x, 2x, 4x slots; light (1-event keys) 16x, 64x
```

Spill threshold. Per flood: offenders recovered | strict collateral.

```
               spillMin | heavy 1x   | heavy 2x   | w1 16x     | w2 16x     | w4 16x     | w8 8x
16b r4 s4 /8   1        | 152   0.0% |   0  99.8% | 200 100.0% | 200 100.0% | 200 100.0% | 200 100.0%
16b r4 s4 /8   4        | 149   0.0% |   0  99.6% | 200   0.0% | 200   0.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    1        | 180   0.0% |   0  89.7% | 200 100.0% | 200 100.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    2        | 200   0.0% |   0  88.0% | 200   0.0% | 200 100.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    4        | 200   0.0% |   0  88.0% | 200   0.0% | 200   0.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    8        | 200   0.0% |   0  88.0% | 200   0.0% | 200   0.0% | 200   0.0% | 200 100.0%
```

Timing pads: a 2M-key table at 85%, 20,000 probes, half seen. "Right" is the best single-timing guess, (1+D)/2.

```
spin 3us          | Touch D 0.023 (51.1%) p50    3.1 us cpu   3.31 us/op | Count D 0.014 (50.7%) p50    3.1 us cpu  3.54 us/op
spin 10us         | Touch D 0.016 (50.8%) p50   10.1 us cpu  10.83 us/op | Count D 0.011 (50.5%) p50   10.1 us cpu 10.63 us/op
adaptive from 1us | Touch D 0.015 (50.7%) p50  512.2 us cpu 462.74 us/op | Count D 0.014 (50.7%) p50  128.1 us cpu 111.56 us/op
sleep 50us        | Touch D 0.012 (50.6%) p50 1108.9 us cpu  63.53 us/op | Count D 0.020 (51.0%) p50 1109.4 us cpu 64.00 us/op
sleep 200us       | Touch D 0.035 (51.8%) p50 1116.9 us cpu  69.91 us/op | Count D 0.013 (50.7%) p50 1115.3 us cpu 66.81 us/op

16 goroutines, 4 cores, plus one probing goroutine:
none        7.77 M ops/s; probe p50 unseen  0.6 us seen  0.3 us; KS D 0.719 (85.9%)
spin 3us    1.10 M ops/s; probe p50 unseen  3.1 us seen  3.1 us; KS D 0.031 (51.5%)
yield 3us   1.31 M ops/s; probe p50 unseen 11.0 us seen 10.6 us; KS D 0.063 (53.1%)
yield 10us  0.91 M ops/s; probe p50 unseen 14.9 us seen 15.1 us; KS D 0.026 (51.3%)
```

## Still open

- **Heavy floods of 2x slots or more** saturate any sketch of affordable size. `Upper` stays safe, but strict limits then turn away everyone. A wider `Overflow` moves that point linearly. Adaptive attacks need about as many queries as the sketch has counters ([Cohen et al., ICML 2022](https://arxiv.org/abs/2202.13736)). Here the attacker sees only decisions and the sketch positions are keyed, so we found nothing to fix.
- **`ConstantTime` still costs a CPU spin per call.** The fixed spin is the cheapest pad measured. Cutting the slow inserts that set the pad's floor would be the next lever.
