# Temporal Filter: prior art, originality, new directions

Written at the first prototype (commits a5292e3–2a526a8) and corrected on 2026-10-08 during the verification pass; corrections are marked. Measured claims carry tags that `verify/claims.md` maps to a check. Literature summaries in §1–2 come from the linked sources as read then: check them there.

## 1. What already exists

| Work | Window | Time stored as | Counts? | Closest to our… |
|---|---|---|---|---|
| Timing Bloom Filter (TBF), Zhang & Guan 2008 | fixed | a timestamp per cell | no | freshness check |
| SWBF (cited as [33] in SWAMP) | fixed | **fingerprint + timestamp in a cuckoo table**; stale entries are treated as empty and cleaned periodically | no | **word0, nearly the same idea** |
| Age-Partitioned BF ([arXiv 2001.03147](https://arxiv.org/abs/2001.03147)) | fixed (count-based) | generations of slices; 2–4 cache lines per op | no | — |
| Time-limited BF ([arXiv 2306.06742](https://arxiv.org/abs/2306.06742), Redis module) | fixed time span | one timestamp per slice | no | — |
| SWAMP ([arXiv 1712.01779](https://arxiv.org/abs/1712.01779)) | fixed, **last W items** | cyclic fingerprint buffer plus TinyTable | yes, exact within window | VelocityCount, but count-based |
| Sliding Sketches, KDD 2020 ([PKU](https://mod.icst.pku.edu.cn/english/news/publications/504258.htm)) | fixed | "time zones" over sketch cells | yes (sketch) | — |
| Hokusai ([arXiv 1210.4891](https://arxiv.org/abs/1210.4891)) | dyadic, per-sketch | Count-Min sketches over power-of-two time spans | yes | the dyadic register in §3.2 |
| Exponential Histograms (Datar et al.) | fixed | buckets that merge by count | yes, per stream | — |
| Learned Cuckoo Filter, variable-size windows ([PACMMOD 2023](https://dl.acm.org/doi/10.1145/3626758)) | **variable** | unclear (full text returned 403) | ? | the per-query window; **closest competitor I couldn't fully verify** |
| Contention-adaptive probabilistic counters (Dice, Lev, Moir, SPAA 2013) | n/a | n/a | yes | hot-key write contention |
| TinyLFU admission (Einziger et al.) | n/a | frequency sketch | yes | §3.1 eviction policy |

## 2. How original is v1?

These judgments are relative to the sources in §1, found in a non-exhaustive search; they aren't proof that nothing similar exists.

- **Not new:** storing a fingerprint and timestamp per slot in a bucketed or cuckoo table (SWBF, TBF), and expiring entries lazily by comparing ages. Our `word0` is this, so we shouldn't claim the freshness half as novel.
- **New, as far as I found:**
  1. A **per-key velocity register in the same cache line as an exact last-touch time**, updated lock-free with CAS. Every prior structure above either has no counts, keeps them in a separate table (SWAMP's TinyTable), or uses global sketches (Hokusai, Sliding Sketches).
  2. **The window is a query argument for both membership and count.** Everything verifiable above fixes the window when the structure is built. The Learned Cuckoo Filter is the possible exception.
  3. **Gating the estimate with lastTouch.** The count is exactly 0 once `W < now − lastTouch` and at least 1 otherwise, and the newest bucket's events are known to end at lastTouch. This removes the worst boundary errors for free.
  4. **A clock-paced, bounded reclaimer** (`ContinuousDecay`). It makes one table pass per horizon and claims at most 4 lines per step via CAS. Redis's active expiry samples randomly and the published filters sweep in batches; neither is paced by the clock.

## 3. New directions explored this round

### 3.1 Velocity-aware eviction (implemented, in `filter.go`)

**Problem I found:** with oldest-first eviction, an attacker can spray fresh keys to overflow the table and **evict their own repeat key**. That is a fraud-evasion vector none of the papers above discuss.

**Change:** when both candidate lines are full, evict the slot with the fewest events in the horizon, oldest on ties. One-off keys churn among themselves.

**Evidence:** `TestSprayCannotEvictRepeatOffender` sprays 20× capacity of one-off keys. All 50 offender keys keep their exact count of 20. The same test **fails with oldest-first eviction** (`verify/experiments/oldest-first-eviction.patch`) [E1]. Insert latency was reported as unchanged at the time; re-measured, miss-insert went from 83 to 110 ns (`verify/ab.sh a5292e3 2a526a8 'CheckAndTouchMissInsert$'`; claims S6, X19).

TinyLFU uses frequency for cache *admission*. Using it as an anti-evasion eviction policy in a temporal filter appears to be new.

### 3.2 Dyadic carry-shift register (prototyped, `registers_experiment_test.go`)

This uses the same 48 bits, but the buckets get wider with age: 1, 1, 1–2, 2–4, … units.

- When the unit index gains `m` trailing zeros, buckets `0..m` shift up one place and bucket `m+1` merges into `m+2`.
- Counter widths grow with bucket size (5,5,6,6,7,8,11), so large old buckets don't saturate.
- It is Hokusai's time aggregation, moved **inside one key's 48-bit word**. Seven buckets give H/32 resolution near *now*, versus H/5 for the linear register.

**Results:** normalized mean absolute error on 5 traffic shapes × 40 trials, with H = 60s.

| Register | 1s | 5s | 15s | 30s | 60s | mean |
|---|---|---|---|---|---|---|
| linear 6×8 (v1, since replaced) | 0.02–0.75 | 0.09–0.41 | 0.03–0.17 | 0.01–0.10 | 0.01–0.05 | 0.154 |
| **dyadic, 7 buckets** | **0.02–0.35** | **0.05–0.15** | 0.04–0.16 | 0.03–0.15 | 0.02–0.14 | **0.108** |
| Laplace 3×16 (§3.3) | 0.02–0.76 | 0.06–0.28 | 0.15–0.33 | 0.04–0.19 | 0.27–0.38 | 0.268 |

Ranges span the 5 traffic shapes; `go test -run TestRegisterExperiment -v` prints the full table [D12]. (Corrected: the 1s ranges for linear and dyadic previously left out the sparse shape's low error.)

The dyadic register roughly halves short-window error (mean over shapes at 1s: 0.38 → 0.20; at 5s: 0.21 → 0.09), which is where fraud velocity rules live ("5 cards in 10s"). It loses on 60s windows (0.030 → 0.071).

**What shipped:** an age-graded variant of this register with `Horizon/24` units and counter widths 5,5,6,7,8,8,9 (`register.go`), which scores 0.081 on the scorecard's velocity metric against the linear register's 0.129 [D5, R3].

### 3.3 Laplace register (prototyped; negative result)

This stores *k* exponentially decayed sums (log-encoded, 16 bits each). Decay is pure clock arithmetic, with no epochs or shifts, so it matches "decay proportional to clock ticks" most literally.

For any query W, it fits weights by least squares so that Σ aⱼ·e^(−age/τⱼ) ≈ 1[age ≤ W]. I found no prior use of this "window synthesis from a decay bank" idea.

It loses: with 3–4 exponentials you can't make a sharp box, so the mean error is 1.7× the linear register's (0.268 vs 0.154; corrected from "2×") [D12]. It is still a good fit if you want EWMA rates (recency-weighted velocity) rather than hard windows.

### 3.4 Proposed, not built

- **Reclaimer as a correctness guarantee, which frees bits.** If every line is provably visited once per horizon, timestamps can be truncated to 32 bits (ms) without wraparound aliasing. That frees 15 bits per slot (the timestamp is now 47 bits), enough for a wider fingerprint. (Corrected: an earlier version said this allowed 6 slots per line; 6 slots need ≤ 85 bits each, and a slot would still need about 113.) Caveat: the guarantee needs a ticker when traffic is idle, because the inline 1/64 pacing alone isn't enough.
- **`TimeToCount(key, n)`.** The inverse query: "how long did the last n events take?" It detects bursts without choosing a window. It is a cumulative scan over the register in the same line.
- **Hot-key touch elision.** Skip the word0 CAS when `now − last < δ`, and use Dice-style probabilistic increments past a threshold. This cuts cache-line ping-pong for heavy hitters (the attackers). It is prior-art technique, just a new application.
- **Mergeable shards.** Merge two filters with max(ts) plus aligned register sums, for multi-node aggregation. This is standard mergeable-summary theory, not new.

## 4. Limitations to keep in view

- **Keyed hashing is required for adversarial keys.** `HashUint64` is an unkeyed mixer, so an attacker who controls IDs could aim at one line. Use `Hash` (seeded maphash) or a keyed SipHash for user-controlled keys.
- **No resizing.** Capacity is fixed at construction.
- **Count saturation.** Per key, 1,273 events for a burst and 762 per horizon for steady traffic (exact up to about 725) [D7]. (Was 255 per sub-window in the v1 linear layout.)
- **Memory.** 16 B per slot, 18.8 B per key at the default 85% load [D6]. Bloom-style APBF and TLBF use a few bits per key but give no counts and no per-query window.
- **Single process.** The clock is monotonic per process. Cross-node use needs synchronized clocks or the merge path.
- **Go limits.** No manual prefetch intrinsic and no SIMD fingerprint compare. The batch APIs overlap misses with plain loads instead.
- **Accuracy on the Learned Cuckoo Filter is unverified.** I couldn't read its full text, so the originality claim for per-query windows rests on the works I could read.
