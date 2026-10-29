# 6. Security

## 6.1 Threat model

Who attacks the filter: the client of a rate limiter, abuse check or dedup that the filter backs. They choose keys (IPs they control, accounts and cards they test, request ids) and when to send. They see the caller's decisions (allowed or blocked) and how long each one took. They don't see memory or the hash key. Running at volume costs them something: requests, accounts or IPs.

What they want, in order of how much it hurts the caller:

1. **Undercount themselves.** Read below a limit while going over it, or read as new while repeating.
2. **Frame someone else.** Push another key's count up, or make it read as already seen.
3. **Slow everyone down.** Pile work onto one lock or one region of the table.

## 6.2 Attacks, fixes and what is left

| # | Attack | Before | Now | Check |
|---|---|---|---|---|
| 1 | **Forge a victim's identity.** `HashUint64` was the SplitMix64 finalizer, a public bijection. Invert it to get ids whose hashes differ from the victim's only in bits 0–3: same line, same 28-bit identity. | 4 forged ids × 25 events: the never-touched victim reads 100 and "seen" | Hashing is keyed (`Config.Key`, else a random seed). The same ids read 0 | A1 |
| 2 | **Evict a victim from its own lines.** Forge keys with the victim's line and fingerprint but other tags, each one event heavier than the victim. | 168 events evict a 20-event key | Keyed, keys land at random: 102,753 events on a 4,818-slot table (612×). Cost grows with the table: about slots × (k+1) events within one horizon | A3 |
| 3 | **Flood one lock stripe.** Pick ids whose hashes share the top 8 bits, so every first touch queues on one of 256 locks. | 4,096 crafted ids, 1 stripe | Spread over all 256 | A2 |
| 4 | **Reset your own count by getting evicted.** Eviction used to drop a key's history, so it came back as new. | Returns with count 1, indistinguishable from a new key | Evicted keys go to a stash and come back with their history (`Restored`). A history the stash has to drop goes to the overflow table (row 8) | A3, A6 |
| 5 | **Send faster than the register holds.** The newest bucket holds 31 events per unit (Horizon/24). Beyond that, events spilled to older buckets (out of short windows) or were dropped. | 100 ev/s read 79–87% low, silently; then a sticky saturated bit marked it (`Saturated`), so strict callers could certify only 31 events per unit | A key past its register's capacity is promoted to a hot entry with 32-bit counters: steady traffic of 10–5,000 events/s reads its exact count at 5–60 s windows, never `Saturated`. With every hot entry in use (`HotRefused`), the saturated bit applies as before, and `Saturated` is set exactly when an overflow can affect the window. Otherwise `Upper` ≥ the true count | A4, A5, A15 |
| 6 | **Hide a burst at the window's edge.** `Count` assumes the edge bucket is uniform, so a burst just inside the edge reads low. | Undetectable | `Upper` counts the edge bucket whole | A4, A5 |
| 7 | **Hash-flood one region** to force relocations and evictions there. | Same inversion as 1 | Needs the key | (follows from 1) |
| 8 | **Flood the table and the stash** with keys heavier than yours: about (slots + stash) × (k+1) events per horizon. | The k-event history was erased (`Lost`), and for a horizon every key the filter didn't hold read `Degraded`. With the first ghost table (one bucket per history, slots/4): strict collateral 1.8% at 2x, 89% at 3x, 100% at 4x | Dropped histories go to the overflow ghost table: one 21-bit entry per history in the less loaded of two buckets, never erased within the horizon. A key's answer adds only its own entries (and sets `Degraded`), so other keys' histories add nothing unless a 14-bit fingerprint matches. When both buckets are full, the lightest history leaves and one of its bucket's three floors (picked by fingerprint) rises to that count, which the keys sharing that floor add to `Upper`: `Upper` is never below the true count (except *Repeatedly floored keys*). 200 offenders × 20 events: 0 lost and 0 read `Upper` below the truth under floods of 1x–16x the table's slots, and every offender reads its full `Count` up to 4x. Histories under 4 events are shed (`SpillMin`), so floods of cheap keys can't fill it. A strict limit of 20 turns away 0.0% of new keys after a 1x heavy flood or a 16x flood of one-event keys, 0.12% after 2x, 0.17% after 3x, 0.29% after 4x, then 96% after 6x and 99% after 8x (§6.4) | A3, A7, A11, A14 |
| 9 | **Time a request** to learn whether a key was seen (a hit reads one line, a miss two plus the stash, and a first touch inserts). | One `TouchAt` timing guesses seen vs unseen right 85–97% of the time, one `CountAt` 57–91% (table larger than the cache; the higher figures on the Ryzen host) | `Config.ConstantTime`: the same lookups whatever the key's state, every call padded, and no insert searches on the call path: a new key that finds both lines full waits in the stash (where every call looks) and calls place waiting keys before their own work. `Touch` at a 1 µs pad: 51.1–52.8% (two runs; 61.4% before the waiting stash, which needed 3 µs for 51.8%); `AutoPad` settles near 1.0 µs at 50.6%. `Count` waits out a running estimate of its own work's 99.5th percentile (~0.7–0.9 µs): 50.4–50.8% | S28, S33, S36 |

## 6.3 Keyed hashing

Keyed hashing (attacks 1–3, 7) holds only if the key stays secret. The default seed is per filter and never leaves the process. `Config.Key` is a 128-bit SipHash-2-4 key: a PRF, the same in every process that uses it, so it can be shared and rotated. The `*At` methods take a hash from the caller and can't check it; pass them only `Hash`/`HashUint64` values of the same filter. Before keying, `HashUint64` was an unkeyed mixer, so an attacker who controls IDs could aim at one line.

Adaptive attacks on sketches need about as many queries as the sketch has counters (Cohen et al., ICML 2022). Here the attacker sees only decisions and the sketch positions are keyed, so nothing was found to fix.

How callers should read the trust flags to fail closed (the rate-limit recipe, dedup, `SpillMin`, the overflow off) is in [4 Operations and guarantees](04-operations.md#using-the-trust-flags).

## 6.4 What is left

- **Volume past the overflow table.** A flood can no longer erase a history, and up to 4x the table's slots in heavy keys it costs other keys almost nothing: the ghost table (`Config.Overflow`, default 5 units per 9 slots, 8.9 B per slot once allocated) holds 3.2 histories per slot, in the less loaded of two buckets. Past about 4.5x its buckets overflow, their floors rise to the flood's weight, and `Upper` covers the keys sharing them: at a 6x flood of 21-event keys a strict limit of 20 turns away 96% of new keys, at 8x 99%, and the offenders' `Count` is gone (their entries were the lightest, so they left first; `Upper` still covers them). Three floors per bucket narrow the damage only while overflow is partial (one bucket per history at slots/2, 4x: 56% → 29%). `Overflow` moves the point; `Floored()` is the alarm (it stays 0 below capacity). Past that point a flood carries more histories than the bytes can name, so the only lever left is size.
- **Hot entries run out.** A key over 31 events in one unit is promoted to an exact hot entry (one per 128 slots by default). If every entry is in use, a key saturates as before and reads `Saturated` until that unit leaves the register, so a strict caller can enforce only 31 events per unit for it: 31 × 24 / Horizon events/s (12.4/s at a 60 s horizon). `HotRefused` is the alarm; `Config.Hot` sizes the table. Entries are reclaimed once their newest event is past the horizon.
- **Fingerprint matches.** While the overflow table is full, about 0.3% of keys that never spilled match some entry's 14-bit fingerprint in one of their two buckets and read that entry's count, flagged `Degraded`. The count-min sketch it replaced flagged 0% (it hid matches under its noise threshold, and with them most real dropped histories).
- **Repeatedly floored keys.** A full overflow bucket keeps the largest history that left to each floor, which `Upper` adds. A key that leaves a history there, returns, and is pushed out to the same floor again can read low by all but its largest floored history, unflagged. It takes a flood past the overflow table, timed around the key's own returns. Summing floors instead removes it, but a 16x flood of light keys then turns away 100% of new keys under a strict limit (0.1% now), so it was declined ([record/decisions.md](../record/decisions.md#decided)). The stress soak (`go/test.sh stress`) checks that every low answer has a live floor in its bucket.
- **Shed light histories.** A key with fewer than `SpillMin` (4) events in the horizon loses them if it is evicted and the stash drops it (`Shed()` counts these). Getting there still takes about a table's worth of heavier keys per eviction, so it is no shortcut for an attacker. `SpillMin: 1` keeps everything at ~30% more cost per insert into a full table (A10, S35). Dedup callers lose those keys' "seen" after such a flood; with the count-min sketch, a one-event history never cleared the sketch's noise floor either, so only `Upper` showed it.
- **Timing.** Off by default: a hit reads one line, a miss two (plus the stash and overflow table while they hold anything), and a first touch inserts, with a cuckoo move for 44% of new keys at 85% load (S38). `ConstantTime` closes most of it at a price: every call spins at least its pad (1 µs suffices for `Touch` on the Ryzen host, where a DRAM access costs ~150–200 ns). A pad that ended at a fixed time leaked through the phase of the spin's clock reads (~37 ns apart here): `Count` at a fixed 1 µs pad gave D 0.019–0.041, visible only at ~10 ns timing resolution; every pad now ends at a random point over 128 ns (64 ns per call on average), which brings it to 0.007–0.014 [S41]. The dither needs clock reads cheaper than its span: on a clock that costs more per read (qemu-user: ~566 ns; some VMs' HPET or acpi_pm clock sources), the pad's end follows the read's phase again (`TestCTPadEndIsDithered` fails under qemu). What still shows: calls that run past their pad. Under sustained insert pressure (more than one waiting key per 16 stash entries) every call places one stash set past its pad, for every key alike: that tells an observer the table is under insert load, not which keys were seen. A waiting key whose stash set is full is inserted at once, which takes longer; that happens only while placement falls behind. A batch's warm-up runs outside the calls' pads; in ConstantTime it prefetches both lines of every key and looks nothing up, so it doesn't depend on the keys' state either (outside ConstantTime it looks each key up in its primary line, for speed). `Count`'s tracked pad needs a warm-up: it starts at the cap and settles within ~100k calls.
- **Restarts** forget everything, and a new random seed changes every hash. `Config.Key` keeps hashes stable, but no state is persisted yet.

## 6.5 Evidence: floods

All flood tables use a 4,818-slot table at 85% load, 200 offenders × 20 events, then the flood, each flood within 30 s of the 60 s horizon. "Strict collateral" is the share of new keys a strict limit of 20 turns away (`Degraded` or `Upper` > 20).

**Overflow sketches** (`TF_EXP=1 go test -run TestSketchShapes -v`; per flood: offenders with `Count` ≥ 18 | new keys `Degraded` | strict collateral; spill threshold 1, spill everything) [A9]:

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

**Spill threshold** (per flood: offenders recovered | strict collateral) [A10]:

```
               spillMin | heavy 1x   | heavy 2x   | w1 16x     | w2 16x     | w4 16x     | w8 8x
16b r4 s4 /8   1        | 152   0.0% |   0  99.8% | 200 100.0% | 200 100.0% | 200 100.0% | 200 100.0%
16b r4 s4 /8   4        | 149   0.0% |   0  99.6% | 200   0.0% | 200   0.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    1        | 180   0.0% |   0  89.7% | 200 100.0% | 200 100.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    2        | 200   0.0% |   0  88.0% | 200   0.0% | 200 100.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    4        | 200   0.0% |   0  88.0% | 200   0.0% | 200   0.0% | 200 100.0% | 200 100.0%
8b r4 s2 /4    8        | 200   0.0% |   0  88.0% | 200   0.0% | 200   0.0% | 200   0.0% | 200 100.0%
```

With `SpillMin` 4, in the 1x flood, 102 spills went to the sketch instead of 3,693.

**The ghost table against the sketch** (`TF_EXP=1 go test -run TestSketchShapes -v`; one bucket per history and one floor, the layout before 3603076) [A11]:

```
                        heavy 1x          | 2x                | 3x                | 4x                | light 16x         | 64x
8b  r4 s2 /4 (sketch)  4.7 B/key | 180 0.0% 0.0% |   0 0.0% 89.7% |   0 0.0% 99.7% |  0 0.0% 100.0% | 200 0.0% 100.0% | 200 0.0% 100.0%
ghost /8               2.4 B/key | 200 0.1% 0.1% |  23 0.2% 77.8% |   1 0.1% 100.0%|  1 0.2% 100.0% | 200 0.1%   0.1% | 200 0.2%   0.2%
ghost /4 (new)         4.7 B/key | 200 0.1% 0.1% | 195 0.2%  1.9% |  18 0.2% 89.2% |  1 0.2% 100.0% | 200 0.1%   0.1% | 200 0.1%   0.1%
ghost /2               9.4 B/key | 200 0.0% 0.0% | 200 0.1%  0.1% | 188 0.1%  2.5% | 89 0.2%  43.8% | 200 0.2%   0.2% | 200 0.2%   0.2%
```

With the defaults of that layout (`SpillMin` 4), `TestAttackFloodCollateral`: strict collateral 0.0% / 1.8% / 100% / 100% / 0.0% at 1x / 2x / 4x / 16x heavy and 16x light; offenders short 0 / 5 / 200 / 200 / 0. The point where collateral climbs moved from ~1.5x (sketch) to ~3x (ghost table).

**Flood sizing** (`TF_EXP=1 go test -run TestFloodSizing -v`, `go/verify/expected/flood-sizing.txt`; new keys turned away after a heavy flood of 21 events per key) [A14]:

```
                              2x      3x      4x      6x      8x     B/key
one bucket /4, 1 floor, s/8   1.80%  89.17%  100%    100%    100%    27.1   (17c55c8)
one bucket /4, 3 floors       0.99%  78.59%  99.79%  100%    100%    27.1
two buckets /2, 1 floor       0.11%   0.20%  40.81%  99.64%  99.91%  31.8
two buckets /2, 3 floors      0.11%   0.20%  19.96%  97.30%  99.25%  31.8
two /1.8, 3 floors, s/16      0.12%   0.17%   0.29%  96.33%  98.95%  31.1   (default)
```

At the defaults no floor rises up to 4x, so the three floors change nothing there (two /1.8 at 6x: 99.3% → 96.0% turned away, both past the cliff). With less overflow memory they halve the cliff (two /2 at 4x: 40.8% → 20.0%; one bucket /4 at 3x: 89.2% → 78.6%). They are kept: they cost no entries (they live in entry 0's place) and widen the margin for smaller `Overflow` settings.

## 6.6 Evidence: timing

D is the Kolmogorov–Smirnov distance between the seen and unseen timing distributions; a one-sample guess is right (1 + D)/2 of the time.

**Pad modes** (Xeon host; a 2M-key table at 85%, 20,000 probes, half seen; "right" is the best single-timing guess) [S30]:

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

**Count's tracked pad** (Xeon host; `TF_CTEXP=1 go test -run 'TestCTPads|TestCTParallel' -v`; a fresh 2M-key table at 85% per row, 80k untimed calls to settle, then 40k timed, seen and unseen mixed; p50 is unseen/seen; ns/op is the mean wall time per call on one goroutine) [S33, S34]:

```
off              | Count D 0.254 (62.7%) p50   222/  246,   346 ns/op | Touch D 0.705 (85.3%) p50   648/  300,   702 ns/op
fixed 1us        | Count D 0.012 (50.6%) p50  1093/ 1094,  1208 ns/op pad 1000ns | Touch D 0.172 (58.6%) p50  1102/ 1098,  1309 ns/op pad 1000ns
fixed 3us        | Count D 0.005 (50.3%) p50  3126/ 3126,  3385 ns/op pad 3000ns | Touch D 0.012 (50.6%) p50  3094/ 3094,  3310 ns/op pad 3000ns
3us, Count auto  | Count D 0.011 (50.5%) p50   933/  935,  1052 ns/op pad  793ns | Touch D 0.026 (51.3%) p50  3103/ 3102,  3383 ns/op pad 3000ns
AutoPad          | Count D 0.012 (50.6%) p50  1294/ 1290,  1467 ns/op pad 1410ns | Touch D 0.010 (50.5%) p50  2990/ 2987,  3192 ns/op pad 3040ns
fixed 3us         1.06 M ops/s; probe p50 unseen    3.1 us seen    3.1 us; KS D 0.060 (53.0%); Count pad 3000 ns
3us, auto Count   2.19 M ops/s; probe p50 unseen    1.6 us seen    1.6 us; KS D 0.015 (50.7%); Count pad 1041 ns
```

The tracked pad gives KS D 0.011 against 0.005 at a fixed 3 µs, where the 95% noise floor at 20k samples is ~0.014; under 16 goroutines D 0.015 against 0.060; and `Count` takes 1.05 µs per call instead of 3.4 (−69%), 2.19 against 1.06 M ops/s under load. `AutoPad` for `Touch` settled at 3–5 µs depending on the run (D 0.009–0.014): no cheaper than a fixed 3 µs on that host, so it is kept as an option.

**The waiting stash** (Ryzen host; `TestCTPads`, three runs at 9be2f39 on an idle host, same method) [S36]:

```
                 run 1                          run 2                          run 3
                 Count          Touch           Count          Touch           Count          Touch
off              D 0.657 82.8%  D 0.757 87.9%   D 0.698 84.9%  D 0.953 97.6%   D 0.337 66.9%  D 0.910 95.5%
fixed 1us        D 0.035 51.8%  D 0.030 51.5%   D 0.046 52.3%  D 0.017 50.8%   D 0.040 52.0%  D 0.022 51.1%
fixed 3us        D 0.044 52.2%  D 0.021 51.0%   D 0.039 51.9%  D 0.008 50.4%   D 0.047 52.3%  D 0.013 50.7%
3us, Count auto  D 0.010 50.5%  D 0.012 50.6%   D 0.015 50.7%  D 0.009 50.4%   D 0.012 50.6%  D 0.021 51.1%
AutoPad          D 0.018 50.9%  D 0.019 51.0%   D 0.027 51.3%  D 0.016 50.8%   D 0.016 50.8%  D 0.015 50.7%
AutoPad settles at (Count / Touch): 599 / 1135 ns, 541 / 1177 ns, 918 / 905 ns
```

A fixed pad leaked more on `Count` than the tracked pad (D 0.035–0.047 against 0.010–0.015), with equal medians. The tracked pad moves on every sampled call; a fixed one doesn't. The hypothesis that the spin loop's exit phase follows the work's duration modulo one clock read, so that a few nanoseconds of random dither on the fixed pad would remove it, was then tested and confirmed: **the dither** [S41]. Raw samples show seen and unseen calls ending in four 10 ns clock steps across one 37 ns spin period, in different proportions; a random end over 128 ns brings `Count` at a fixed 1 µs pad from D 0.019–0.041 to 0.007–0.014, and `Touch` at 1 µs reads 50.4–50.9%. A ConstantTime call at the 1 µs pad takes 1.5–1.9 µs on average under a 50%-unseen load (forced placement).
