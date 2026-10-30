# 7. Evaluation

## 7.1 Setup

- **Accuracy and memory** come from the scorecard, `go/cmd/tfbench` (capacity 200k, horizon 60 s): a churn workload (a stream mixing new keys and repeats, run for 4 horizons, every op a CheckAndTouch with a random window) measured against an exact map of last-seen times, at three load points (1.0x, 1.1x and 1.15x capacity: 85%, 94% and 97.8% peak slot load), plus 1e9 never-inserted probes, a spray of 20x capacity one-off keys against repeat offenders, and the velocity error over 1, 5, 15, 30 and 60 s windows and 5 traffic shapes. These results are deterministic (seeded, single-threaded, unkeyed integer hash) and checked byte for byte by `go/verify/run.sh`.
- **Speed** is host-dependent and reported, not checked; each comparison is an A/B of two builds run alternately on one host ([artifact/measuring-speed.md](../artifact/measuring-speed.md)). The hosts are described in [the documentation index](../README.md#hosts). The scorecard's rows over time are in [record/scorecard-history.md](../record/scorecard-history.md); the Xeon host's reference runs are in [artifact/speed-runs.md](../artifact/speed-runs.md).

## 7.2 Accuracy and memory

- **Missed keys** [D1]: 0 at 85%, 94% and 97.8% slot load (1.15x Capacity). Before the stash, 58 of 113,040 (0.051%) at 97.8%.
- **Evictions at 97.8%** [D8, D10]: not the layout. The load threshold for 2 choices × 4 slots is 98.04%, and an exact placement oracle on the same stream places every key (0 forced drops). The filter's ~4,575 evictions come from its bounded search: the oracle's shortest paths needed 5 moves for 3,406 inserts, past the filter's 4. The stash gives every evicted key that returns its history back.
- **False positives** [D2, D3]: 0 of 723,513 negatives on the churn workload; 18 in 1e9 never-inserted probes at capacity (1.8e-8; Poisson 95% interval 1.07e-8–2.84e-8; model 1.27e-8). At 1M absent-key lookups/s, 1.8e-8 is about one false hit per minute [D16].
- **Memory** [D6, D17, A14]: 18.8 B per key at design load; 31.1 B per key under a heavy flood.
- **Velocity error** [D5]: 0.078, averaged over 1–60 s windows and 5 traffic shapes (0.214 at 1 s, 0.024 at 60 s). The original 6×8 linear register scored 0.129 on the same harness [R3]. Most of what remains is the register's information: under a uniform model of events within a bucket no estimator reading it gets below ~0.058 on this workload, and 0.05 would need about 1.25 s resolution near now [D18]. Side information a few more bits could carry doesn't close it: the offset of the newest bucket's first event reads 0.067–0.070, every bucket's 0.065, the exact times of the key's last 4 events 0.058; only the last 8 events' times (0.041) reach 0.05 [D19].
- **Spray**: 100 of 100 offenders keep exact counts after 20× capacity of one-off keys [D4].
- **Counts** [D7, A15]: exact from 10 to 5,000 events/s per key at 5–60 s windows ([3 Design](03-design.md#count-limits)).
- **Floods** [A14]: a strict limit of 20 turns away 0.12% / 0.17% / 0.29% of new keys after heavy floods of 2x / 3x / 4x the table's slots, and every offender keeps its full `Count` ([6 Security](06-security.md)).

## 7.3 Targets

Thirteen targets were set for the structure (a targets spreadsheet, `temporal-filter-targets.xlsx`, outside the repository, with a copy beside it that gives each value its source). Status on the Ryzen host:

| # | Metric | Target | Now | Status |
|---|---|---|---|---|
| 1 | Sustained per-key rate | ≥1,000/s | exact up to 5,000/s (the highest tested) [A15] | Met |
| 2 | New keys turned away at a 4x flood | ≤1% | 0.29% (alarm: `Floored()`) [A14] | Met |
| 3 | `Upper` below the truth | 0, fuzzed | 0 in floods, attack tests, soaks and 1.004e10 single-owner fuzzed operations (after f44e7dd) [A18]. Concurrent: the fuzzer's rare lows (A22) explained and fixed (A28–A30); since, 0 failing in 8.02e9 unamplified operations (60 min) and in 40 minutes with tags shared 8 ways (21 failing cases in 10 minutes before). A further kind, a hot history leaving the stash while its touch adds (A32), fixed in 6b3cb39: case 9787 0 of 12 batches of reruns (4 before). One modelled low remains, at ~6e-17 per count (A29) | Met |
| 4 | False-positive rate | ≤1e-7 | 1.8e-8 [D3] | Met |
| 5 | Memory, steady state | ≤20 B/key | 18.8 [D6] | Met |
| 6 | Memory under flood | ≤32 B/key | 31.1 [A14] | Met |
| 7 | Velocity error | ≤0.05 | 0.0779 (0.0807 at 9be2f39); this register's floor ~0.058 [D18]; with a few more bits of side information 0.058–0.070 [D19] | Gap: needs tens of bits per key |
| 8 | Batched throughput at ≥90% load | ≥10 M/s per core | 11.53 M keys/s (ccf23af; 9.0–10.2 before) [S47, S48] | Met |
| 9 | Miss-insert latency | ≤150 ns | 429 ns (steady-state insert; the Xeon host read 491 at 0597649). On the Ryzen host one DRAM round trip is 189 ns; with the table inside the cache the insert costs 340–377 ns, two-thirds of it the cuckoo search [S62] | Gap; not reachable on the Ryzen host |
| 10 | Scaling to 16 cores | ≥12x | 6 cores: uniform 4.9x, Zipf 3.4x (5.1x and 3.6x at 6 goroutines, 7.9x and 5.5x at 12, at ee5996c) [S37, S53] | Gap; 16 cores untestable on the Ryzen host |
| 11 | Unseen Touch p99.9, no ConstantTime | ≤2 µs | default 3.9–6.6 µs at 85% load, 1.3–3.1% of inserts over 2 µs; with `Background` 3.3–3.9 µs, 0.16–0.19% over 2 µs (p99 1.0–1.1 µs). The host's floor: a 0.5 µs busy loop is over 2 µs in 0.05–0.15% of calls [S43, S45]. About 60% of the rest is the placer goroutine's; with its placements moved off the caller's timing, p99.9 reads 1.9–3.0 µs, median ~2.1 [S61] | Gap on this host, at its floor |
| 12 | ConstantTime pad | ≤1 µs | 1 µs fixed (end dithered over 128 ns), or AutoPad 0.9–1.2 µs [S36, S41] | Met |
| 13 | Distinguisher | ≤52% | Touch at 1 µs with the dithered pad: 50.9, 50.4, 50.6% (51.1–52.8% in five runs before) [S41] | Met |

## 7.4 Speed

### Hits, clock and batches

On the Xeon host [S1–S4]: the built-in clock reads in 30–42 ns, `CoarseClock` in ~1.4 ns, and the string path drops from 115–121 to 73–75 ns with it (0.63x). Batching a 1M-key, L3-resident table: hit 65.3 → 42.6 ns/key (0.65x), TouchAndCount 97.9 → 62.9 (0.64x), VelocityCount 72.3 → 51.3 (0.71x). On a DRAM-sized 32M-key (~600 MB) table: hit 174, batched 78, absent 91, VelocityCount 142 ns; hits are latency-bound there (a hypothesis: the CAS stops later lookups from overlapping), and batches recover the overlap. Two CASes are about 12 of the ~27 ns hot hit [S25].

### Batches

Target 8 on the Ryzen host (`TestRow8`, four arms interleaved over five rounds; M keys/s, per-round medians) [S47]:

| Arm | M keys/s |
|---|---|
| base | 10.04 / 9.80 / 9.45 / 9.81 / 9.02 |
| two-phase warm | 8.94 / 6.82 / 8.89 / 8.67 / 8.56 |
| huge pages | 10.08 / 10.10 / 10.50 / 10.32 / 10.50 |
| both | 9.36 / 9.25 / 9.39 / 8.89 / 9.35 |

- The two-phase warm is about 9% slower: its extra pass scans every primary line again before the touches, and the keys it targets are 23.0% of the table, not the ~35% assumed. Refuted.
- Huge pages are 5% faster in batches, in every round: less than the 22% raw random-load gain (S42), and insert classes were within 3% (S43). Not adopted: the gain is host-dependent and small, and 2 MiB page faults would land on first touches.
- **Prefetch instructions** (same test, a second run): a load in the warm pass holds up retirement, and every touch's locked CAS waits for earlier loads; `PREFETCHT0` (assembly) does neither. Medians of the per-round medians: base 9.50; prefetching the primary lines 9.77 (+3%); prefetching both lines of every key 10.49 (+10%); prefetching primaries, touching the keys found there first, then prefetching the others' alternate lines and touching them last 10.93 (+15%, past 10 M/s in every round).

The committed batch (ccf23af) prefetches its chunk's primary lines, looks each key up there, prefetches the missing keys' alternate lines and touches the keys **in order** from the slot found: 9.57 → 11.53 M keys/s on a 38 MB table at 90% load (1.20x); 0.86x on a 1 MB table that fits the cache (the lookup pass costs), 1.07x at 4 MB, 1.20x at 19 MB, 1.17x at 150 MB [S48]. Other arms are in experiment E20.

### Unseen-key inserts and Background placement

Insert tail (`TF_EXP=1 go test -run TestInsertTail -v`; 2M-key table, 40k unseen keys each timed alone) [S38]: at 85% load 55% of inserts find a free slot (p50 ~0.4 µs, p99 0.8 µs) and 44–45% move an entry (p50 0.8–1.3 µs, p99 2.5–4.7 µs); p99.9 over all inserts 3.8–43 µs between runs. At 90% load 60% move; at 95%, 79% move and 0.4% evict. Loading one candidate's four next lines together cut the 90% p99 from 4.4–4.9 to 3.8–3.9 µs [S42]. At 85% load about 1% of inserts still take over 2 µs, mostly 2-move inserts (p99 3.2–3.6 µs), so a p99.9 ≤ 2 µs needs those off the call path [S43]: hence Background.

With and without `Background` (interleaved, three runs each) [S45]:

| | Background off | Background on |
|---|---|---|
| 85% load, inserts over 2 µs | 1.27 / 1.59 / 3.12% | 0.16 / 0.18 / 0.19% |
| 85% load, p99 | 2.1–2.8 µs | 1.01–1.09 µs |
| 90% load, inserts over 2 µs | 9.2–11.0% | 0.21–0.33% |
| 95% load, inserts over 2 µs | 52–54% | 16.0–16.4% |

The Ryzen host takes the CPU away ~1,070 times a second (S43): a 0.5 µs busy loop timed the same way runs past 2 µs in 0.05–0.15% of calls, and a 1 µs loop in 0.12–0.16% (`TestHostFloor`). So p99.9 ≤ 2 µs can't be shown on that host for an operation near 1 µs; with Background the code's own share over 2 µs is ~0.1% at 85% and 90% load. Costs (S46): with Background off the default paths read 0.94–1.02 of 385d67f; with it on, absent-key lookups 1.08–1.12 (the wait-room bit), the caller's inserts 0.52–0.94. What is left of the code's share is the waiting path (room write, shared counters; ~0.6 µs p50) and the placer beside it: the same placements run by hand on the caller's goroutine between timed inserts cut inserts over 2 µs from ~0.19% to ~0.12% (eight paired readings) and p50 by 30–50 ns, and leave p99.9 at 1.9–3.0 µs, median ~2.1 [S61]. Which part of the placer it is wasn't found: its memory traffic, replayed by another goroutine, read both ways; spinning through its naps made the tail worse, and leaving the shared wait count alone changed nothing measurable [E27].

### Throughput and scaling

`TF_EXP=1 go test -run TestThroughput -v`: a fresh 2M-key table at 90% load per point, 1M calls per goroutine, 1% unseen keys, M ops/s [S37]:

```
goroutines        1     2     4     6     8    12
zipf, entry lock  8.0  12.0  22.6  28.5  30.6  29.9   (500fcb7)
zipf, one CAS     9.7  15.6  26.5  33.2  41.5  44.1   (3a44c40)
uniform           5.4  10.1  18.9  26.6  29.7  30.6
```

Batched, one goroutine reaches 9.3–10.2 M keys/s and single calls 5.1–5.6 M ops/s (uniform, present keys). Under Zipf s=1.1 (heaviest key ~9% of calls) every touch of a hot key writes its entry's line, so six cores reach 3.4x one core, uniform traffic 4.9x. With the entry lock, `hotEntry.lock` and its CAS took ~37% of CPU; at 90% load with 5% unseen keys, inserts (`claimAt` and its search) took 46% [S40]. A later reading at ee5996c: Zipf 3.59x at 6 goroutines and 5.51x at 12, uniform 5.11x and 7.93x [S53]. Four goroutines on one key run at 10 ns/op aggregate, 16 keys at 16 ns, 16K keys at 22 ns (Xeon host): same-tick touches skip the word0 CAS and full registers skip the word1 CAS, so a bot storm doesn't ping-pong the line [E16, S23].

**The ceiling of per-core cells** [S53]. Under Zipf s 1.1 the top key takes ~12% of calls, and each of its touches is one CAS of its hot entry's bucket-0 word, a line every core writes. Per-core cells would spread that add, but every count of a hot key would then sum the cells (a rate limiter's `TouchAndCount` reads on every call). Before building them, the most they could give: `TestThroughput`'s scaling arm with a hot entry's add returning without its CAS (it counts nothing). The rule, written before the reading: build cells only if that ceiling is ≥ 1.25x at 12 goroutines. Reading (5 rounds alternated; medians): 1.00x at 1 goroutine, 1.17x at 2, 1.27x at 3, 1.34x at 4, 1.47x at 6, 1.30x at 8, 1.23x at 12. Uniform traffic, where nothing is promoted, 0.90–1.02. Below the bar at 12, so the cells weren't measured. The ceiling peaks at one goroutine per physical core (6) and falls on the SMT siblings, where other limits share the cost: on a host with more physical cores the case could differ. The design (experiment E23) is in [record/decisions.md](../record/decisions.md#scaling).

### ConstantTime

`ConstantTime` without a pad is 2–6× slower than the default mode [S29]; with its pads, a call costs at least its pad. Leak and cost by pad mode are in [6 Security](06-security.md#66-evidence-timing).

### What each change cost

Every change on a hot path was measured as an A/B on one host; the results are claims S5–S60 in [artifact/claims.md](../artifact/claims.md#speed-host). A few that changed the picture: velocity-aware eviction made miss-inserts 1.32x slower [S6]; the stash and trust flags made saturated inserts +42% [S27]; the overflow sketch +65%, later recovered (−31%, then −16% with the ghost table) [S29, S32, S35]; realign by CMPXCHG16B made sparse touches from every core 14–15% faster [S52]; moving the per-call fields off the counters' line removed a 1.68x slowdown of other cores' lookups [S60].

### The scorecard's speed columns

The scorecard's `hit hot` column (and `BenchmarkCheckAndTouchHitHot`) has timed refused hot-entry promotions since 964b518: its 4,096 keys come back ~4,000 times within one unit, every register fills, and with the hot entries taken each touch tries a promotion that is refused [X36]. `BenchmarkCheckAndTouchHitCached` (c0e32d1) times the cached hit. One 1M-key filter serves several of tfbench's columns in turn, so its registers fill as earlier columns' iterations add up, and how many there were depends on how fast those columns ran (not measured; [8 Limitations](08-limitations.md)).

## 7.5 Head-to-head against industry structures

What a deployer gives up or gains by putting Clepsydra in front of a per-key limit instead of the structures industry uses for it. The protocol ([artifact/head-to-head.md](../artifact/head-to-head.md)) was committed before any reading, with two amendments before any reading; the arms are published implementations run as released, or, where none exists, the published algorithm: Cloudflare's `pingora-limits` 0.9.0 (`Rate`, a count-min estimator, used as Pingora's own limiter example uses it), an exact sliding-window log per key in DashMap 6.2.1, and the ECM-sketch implemented from its paper (Papapetrou et al. 2012), which keeps the paper's ε on every range [D20]. Ryzen host; Go 1.24.7 and Rust 1.99.1.

### Floods [A41, A42, S63, S64, S65]

The flood protocol of [§6.5](06-security.md#65-evidence-floods) at N = 262,144 keys (Clepsydra: 308,408 slots, 85% load), so no table fits the cache: 1,000 offenders at a strict limit of 20 events per 60 s, then floods of 1x–16x the slots in keys of 21 events (one over the limit) and 16x in one-event keys, all within one window; one trace per flood, replayed by every arm; 5 rounds of 61,000 probes. A count-min sketch has no key count, only bytes, so `pingora-limits` gets Clepsydra's peak bytes (31.0 B per key, the overflow table allocated), and in a sizing run 8x and 64x those. In a supplement declared before its reading, the ECM-sketch ran on the same traces at the same bytes, sized for each flood on its own (which favours it), with ε_sw 0.5 and the depth that did best on the 1x flood (one row).

Strict decisions (Clepsydra's fail-closed recipe, the count-min estimate, the ECM-sketch's upper bound, the exact count). False-allow, a key at or over the limit allowed, was **0 for every arm at every flood**. False-deny, an innocent key turned away (Wilson 95% intervals in A41):

| flood | Clepsydra | `pingora-limits`, same bytes | at 8x the bytes | at 64x the bytes | ECM-sketch, same bytes | exact map | exact map's bytes per key |
|---|---|---|---|---|---|---|---|
| 1x | 0.00% | 69.41% | 0.44% | 0.00% | 47.47% | 0 | 331 |
| 2x | 0.07% | 96.99% | 4.32% | 0.00% | 92.78% | 0 | 482 |
| 4x | 0.27% | 99.98% | 24.41% | 0.04% | 99.91% | 0 | 947 |
| 8x | 98.99% | 100% | 69.12% | 0.47% | 100% | 0 | 1,877 |
| 16x | 99.89% | 100% | 96.91% | 4.25% | 100% | 0 | 3,738 |
| 16x light | 0.00% | 99.98% | 0.00% | 0.00% | 26.18% | 0 | 1,630 |

Count error (mean |estimate − truth| over all probes): Clepsydra 0.29 / 0.34 / 0.38 / 3.91 / 5.75 / 0.62, `pingora-limits` 22.2 / 58.9 / 140.9 / 316.8 / 684.2 / 34.4, the ECM-sketch 11.1 / 43.7 / 120.7 / 299.8 / 688.7 / 12.9, the exact map 0. Speed, medians of 5 rounds (Clepsydra was slower than both in every round, except the 12-thread p99.9 at 16x, where it beat the exact map in all 5):

| flood | Mops/s, 1 / 12 threads: Clepsydra | `pingora-limits` | exact map | p99.9 µs, 1 / 12 threads: Clepsydra | `pingora-limits` | exact map |
|---|---|---|---|---|---|---|
| 1x | 3.06 / 19.9 | 9.84 / 84.6 | 13.27 / 49.7 | 8.6 / 19.7 | 0.84 / 0.56 | 2.1 / 12.8 |
| 4x | 2.06 / 15.0 | 10.15 / 88.8 | 13.80 / 53.7 | 5.8 / 19.5 | 0.52 / 0.52 | 2.1 / 12.5 |
| 16x | 1.73 / 12.1 | 10.16 / 86.7 | 12.29 / 42.6 | 5.8 / 22.6 | 0.51 / 0.51 | 2.1 / 23.6 |
| 16x light | 0.49 / 2.28 | 6.28 / 45.7 | 2.01 / 4.68 | 23.6 / 230 | 0.55 / 0.72 | 7.8 / 282 |

The ECM-sketch (the reference implementation from its paper, not tuned for speed; a run of its own, so against these medians rather than round by round) ran at 9.2–10.4 Mops/s on one thread under heavy floods, and 6.7–7.2 Mops/s shared by 12 threads under one mutex, with a 12-thread p99.9 of 0.46–0.52 ms [S65]. Every flood is in [artifact/head-to-head-ryzen.md](../artifact/head-to-head-ryzen.md).

- **Decisions.** Up to 4x its slots, Clepsydra's decisions match the exact map's within 0.27% false denies, at 31 B per key against the exact map's 331–947. A count-min sketch of the same bytes turns away 69% of innocent keys at the first flood: its noise per counter is the flood's events over its width, and every key reads it. It needs 64x the bytes (1,987 B per key) to stay under 1% up to 8x. Past ~4.5x Clepsydra's overflow table overflows (§6.4) and it turns away ~99%, still failing closed; the exact map stays exact, with memory the attacker sizes (3.7 KB per key at 16x, 121x Clepsydra's).
- **The ECM-sketch** at the same bytes turns away 47% of innocent keys at the first flood and ~100% from 4x: one wide row of histograms beats the count-min estimator's two estimators of four rows up to 4x, but every key still reads the flood's traffic in its counter, as in part 2. Sized for each flood on its own, it had the benefit a deployment would not.
- **Counts.** Clepsydra's count error up to 4x is one-event keys the flood sheds (`SpillMin`: −0.89 to −0.98 per base key, never deciding wrongly at a limit of 20); offenders keep their exact `Count` to 4x and lose it from 8x, where `Upper` still covers them.
- **Speed.** Clepsydra is 3.2–5.9x slower than `pingora-limits` and 4.3–7.1x slower than the exact map on one thread, with a p99.9 of 5.8–8.6 µs against 0.5–0.8 and 2.1 µs. Two causes, both in Clepsydra: every flood key is an insert into a full table (the scorecard's saturated insert, 1,963 ns on this host), and once the overflow table holds histories every touch reads the key's own entries there, 47% of the CPU in a 16x flood [S64]. The one-event flood is all inserts: 0.49 Mops/s. These are the implementations as shipped, in two languages (§8.2 has the open problem).

### Windows at equal memory [D21]

The scorecard's velocity workload (D5) with Clepsydra's `Capacity` equal to its 2,000 keys; the ECM-sketch gets Clepsydra's 38,912 B (19.5 B per key), counted generously (1 B per counter and per level, 4 B per bucket). Its depth, ε_sw and width were tuned on the test workload over 36 configurations (the widest that fits for each depth 1–5 and ε_sw 0.01–0.5, and the paper's own split); the table gives the best by mean error, which was also the best at every window. Normalized error Σ|estimate − exact| ÷ Σ exact:

| arm | memory | 1 s | 5 s | 15 s | 30 s | 60 s | mean |
|---|---|---|---|---|---|---|---|
| Clepsydra `Count` | 38,912 B | **0.214** | **0.057** | **0.051** | **0.044** | **0.024** | **0.078** |
| ECM, best (d=1, ε_sw 0.5, w 704) | 38,721 B | 2.850 | 2.933 | 2.955 | 2.951 | 2.872 | 2.912 |
| ECM, the paper's split (d=3, w 37) | 38,484 B | 43.5 | 45.3 | 46.2 | 46.0 | 46.3 | 45.5 |
| ECM at 2x the memory | 77,733 B | 0.512 | 0.534 | 0.526 | 0.518 | 0.495 | 0.517 |
| ECM at 4x | 155,337 B | 0.112 | 0.132 | 0.133 | 0.132 | 0.122 | 0.126 |
| ECM at 8x | 311,114 B | 0.029 | 0.053 | 0.055 | 0.056 | 0.053 | 0.049 |
| ECM, no memory limit (a histogram per key) | 334,990 B of buckets | 0.0001 | 0.010 | 0.016 | 0.017 | 0.017 | 0.012 |

At equal memory Clepsydra's error is 37x lower and lower at every window (rule 5: below every configuration and every seed). Every best ECM configuration has one row: its memory buys width, because the error left is other keys' traffic in a key's counter, which no ε_sw removes. ECM reads lower at 1 s from 4x the memory and at 5 s from 8x; at 8x Clepsydra still reads lower at 30 and 60 s (15 s is inside ECM's seed range). With a histogram per key, ECM's information is better than the 48-bit register's at every window (0.012 against 0.078, at 8.6x the bytes): the register's resolution near now is the limit D18 measured, not its sharing.
