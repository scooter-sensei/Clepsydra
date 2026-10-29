# 2. Related work

The survey below was written at the first prototype (commits 564d865–eeb5521) and corrected during the verification pass; corrections are marked and recorded as claims X9–X11. The literature summaries come from the linked sources as read then: check them there. The search was not exhaustive, so the originality judgments in §2.2 are relative to these sources, not proof that nothing similar exists (claims register, *Not reproducible*).

## 2.1 What already exists

| Work | Window | Time stored as | Counts? | Closest to Clepsydra's… |
|---|---|---|---|---|
| Timing Bloom Filter (TBF), Zhang & Guan 2008 | fixed | a timestamp per cell | no | freshness check |
| SWBF (cited as [33] in SWAMP) | fixed | **fingerprint + timestamp in a cuckoo table**; stale entries are treated as empty and cleaned periodically | no | **word0, nearly the same idea** |
| Age-Partitioned BF ([arXiv 2001.03147](https://arxiv.org/abs/2001.03147)) | fixed (count-based) | generations of slices; 2–4 cache lines per op | no | — |
| Time-limited BF ([arXiv 2306.06742](https://arxiv.org/abs/2306.06742), Redis module) | fixed time span | one timestamp per slice | no | — |
| SWAMP ([arXiv 1712.01779](https://arxiv.org/abs/1712.01779)) | fixed, **last W items** | cyclic fingerprint buffer plus TinyTable | yes, exact within window | VelocityCount, but count-based |
| Sliding Sketches, KDD 2020 ([PKU](https://mod.icst.pku.edu.cn/english/news/publications/504258.htm)) | fixed | "time zones" over sketch cells | yes (sketch) | — |
| Hokusai ([arXiv 1210.4891](https://arxiv.org/abs/1210.4891)) | dyadic, per-sketch | Count-Min sketches over power-of-two time spans | yes | the dyadic register ([3 Design](03-design.md#the-register-alternatives-explored)) |
| Exponential Histograms (Datar et al.) | fixed | buckets that merge by count | yes, per stream | — |
| Learned Cuckoo Filter, variable-size windows ([PACMMOD 2023](https://dl.acm.org/doi/10.1145/3626758)) | **variable** | unclear (full text returned 403) | ? | the per-query window; **the closest competitor, not fully verified** |
| Contention-adaptive probabilistic counters (Dice, Lev, Moir, SPAA 2013) | n/a | n/a | yes | hot-key write contention |
| TinyLFU admission (Einziger et al.) | n/a | frequency sketch | yes | velocity-aware eviction |

## 2.2 What is new

- **Not new:** storing a fingerprint and timestamp per slot in a bucketed or cuckoo table (SWBF, TBF), and expiring entries lazily by comparing ages. `word0` is this, so the freshness half is not claimed as novel.
- **New, as far as the search found:**
  1. A **per-key velocity register in the same cache line as an exact last-touch time**, updated lock-free with CAS. Every prior structure above either has no counts, keeps them in a separate table (SWAMP's TinyTable), or uses global sketches (Hokusai, Sliding Sketches).
  2. **The window is a query argument for both membership and count.** Everything verifiable above fixes the window when the structure is built. The Learned Cuckoo Filter is the possible exception; its full text could not be read, so this claim rests on the works that could.
  3. **Gating the estimate with lastTouch.** The count is exactly 0 once `W < now − lastTouch` and at least 1 otherwise, and the newest bucket's events are known to end at lastTouch. This removes the worst boundary errors for free.
  4. **A clock-paced, bounded reclaimer** (`ContinuousDecay`). It makes one table pass per horizon and claims at most 4 lines per step via CAS. Redis's active expiry samples randomly and the published filters sweep in batches; neither is paced by the clock.
  5. **Velocity-aware eviction against evasion.** With oldest-first eviction an attacker can spray fresh keys to overflow the table and evict their own repeat key, a fraud-evasion vector none of the papers above discuss. TinyLFU uses frequency for cache *admission*; using it as an anti-evasion eviction policy in a temporal filter appears to be new ([3 Design](03-design.md#331-velocity-aware-eviction)).
  6. **Window synthesis from a decay bank** (the Laplace register, a negative result): fitting weights so that Σ aⱼ·e^(−age/τⱼ) ≈ 1[age ≤ W] for any query W. No prior use of this idea was found.

## 2.3 Further prior art used in the design and its evaluation

| Work | Where it is used |
|---|---|
| Cain–Sanders–Wormald and Fernholz–Ramachandran load thresholds; Walzer, [arXiv 2307.00644](https://arxiv.org/abs/2307.00644) §1.1.1 (0.89701, 0.95915, 0.98037 for 2, 3, 4 slots) | the 98.04% load threshold of 2 choices × 4 slots [D10] |
| Storing which bucket choice was used (a known way to extend a fingerprint by a bit) | the displaced bit ([3 Design](03-design.md)) |
| SALSA's counter merging | the escalating sketch idea, not run ([record/decisions.md](../record/decisions.md)) |
| Resiliency of Sketch-based Security, [arXiv 2503.11777](https://arxiv.org/abs/2503.11777) (LSB sharing, late merging of saturated counters) | considered for the overflow sketch, not run |
| Cold Filter, [Zhou et al., SIGMOD 2018](https://minlanyu.seas.harvard.edu/writeup/sigmod18.pdf) (a two-layer prefilter keeping cold items out of the counting sketch) | `SpillMin` is the cheap version of its idea |
| Adaptive attacks on sketches, [Cohen et al., ICML 2022](https://arxiv.org/abs/2202.13736) | why sketch positions are keyed ([6 Security](06-security.md)) |
| Timing pads that double on every overrun, [Askarov, Zhang, Myers, CCS 2010](https://www.cs.cornell.edu/andru/papers/timing.html) | the adaptive ConstantTime pad, tried and rejected |
| SipHash-1-3 (as Rust's HashMap uses) | measured against SipHash-2-4 [S31] |
| MemC3's optimistic cuckoo hashing (Fan, Andersen, Kaminsky, NSDI 2013): lock-free reads validated with version counters striped by key, one writer under a global lock | why move counts are striped by word1's tag ([5 Concurrency](05-concurrency.md)) |
| LongAdder's striped counters; per-core private aggregates for heavy hitters in the parallel-aggregation literature | per-core cells for hot entries (experiment E23, not built into the library) |
| Code layout as a source of measurement bias large enough to reverse conclusions (Mytkowicz, Diwan, Hauswirth and Sweeney, ASPLOS 2009); randomized layout as its remedy (Curtsinger and Berger, Stabilizer, ASPLOS 2013) | `verify/lrab.sh`, a coarse compile-time version: pad points, a fixed set of layouts per reading ([artifact/measuring-speed.md](../artifact/measuring-speed.md)) |
| 64-byte function alignment to lower microbenchmark variance (LLVM libc, D124958, 2022; GCC's `-falign-functions=64`) | experiment E24 [S57] |
| Profile-guided code positioning (Pettis and Hansen, PLDI 1990); Go's hot-block alignment under PGO (Go 1.23 release notes; `PCALIGNMAX`, CL 577935) | experiments E25 and E26 [S57] |
| Zen 2 counter definitions (Linux `tools/perf/pmu-events/arch/x86/amdzen2`); the op cache's size, 4K ops, twice Zen's 2K ("Zen 2: The AMD 7nm Energy-Efficient High-Performance x86-64 Microprocessor Core", ISSCC 2020, paper 2.1) | the op-cache model of S58 (its sets and ways are the model's inference; AMD's Family 17h Models 30h+ optimization guide, 56305, could not be reached from the host) |
| LLVM's scheduling notes (LOCK CMPXCHG16B ~32–38 cycles against ~17–20 for LOCK CMPXCHG; no measured Zen 2 figure found) | the prediction for experiment E21 [S51] |
