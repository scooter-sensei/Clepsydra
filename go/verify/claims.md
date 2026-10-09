# Claims and how each is checked

Every claim made about this filter (docs/DESIGN.md, docs/RESEARCH.md, results/, and the project thread) is listed here with the command that checks it, the result, and a status:

- **checked**: deterministic; `verify/run.sh` re-computes it and requires a byte-for-byte match with `verify/expected/`.
- **replayed**: re-derived from the git history by `verify/replay.sh` (old commits, or today's harness on old library code).
- **derived**: follows from the code or from arithmetic shown here.
- **host**: a speed figure. It depends on the machine; `verify/speed.md` has the reference runs and `verify/ab.sh` repeats any comparison. Compare ratios, not absolute numbers.
- **corrected**: an earlier claim that the checks showed to be wrong; the corrected statement is given.
- **unverifiable**: no code or data was kept to re-check it. Treat as anecdote.

Run everything: `verify/isolated.sh` (fresh clone from a git bundle, official Go 1.24.7 image pinned by digest, `--network none`, empty caches), or `verify/run.sh` in any clean checkout. Deterministic results hold for linux/amd64 with Go 1.24.7; other architectures weren't tested. The last runs (Docker and unshare, all checks pass) are recorded in `verify/isolated-run.txt`.

## Deterministic results (checked)

| ID | Claim | Check | Result |
|---|---|---|---|
| D1 | Missed keys on the churn workload: 0 at 1.0x, 1.1x and 1.15x Capacity (0.051% at 1.15x before the eviction stash, 2d4682b) | `go run ./cmd/tfbench -exact` | 0/99,075, 0/108,572, 0/113,040 positives missed (58/113,040 at c5fa7bf) |
| D2 | No false "seen" on the churn workload | same | 0 of 628,235 / 691,508 / 723,513 negatives |
| D3 | Static FPR 1.8e-8 at capacity | same | 18 in 1e9 never-inserted probes; exact Poisson 95% interval 1.07e-8–2.84e-8 |
| D4 | Spray can't evict repeat offenders | same; `TestSprayCannotEvictRepeatOffender` | 100 of 100 offenders keep exact counts after 20× capacity of one-off keys (test: 50 of 50) |
| D5 | Velocity error 0.081 (mean over 1, 5, 15, 30, 60s windows) | same | per window 0.2273, 0.0583, 0.0514, 0.0436, 0.0239; mean 0.0809. Exact counts are over `[now − W, now]`, the window the filter answers for |
| D6 | 18.8 B per key at the default 85% load, plus 0.44 B for the stash and, once a history is dropped, 4.7 B for the overflow sketch (7.1 B at dd30cbc, from the start) | same; stash and sketch: derived (`New`, `sketchShape.bytes`) | 235,296 slots × 16 B / 200,000 keys; stash 3,676 entries × 24 B / 200,000; sketch 58,824 columns × 4 rows × 4 B / 200,000 |
| D7 | Per-key count limits | same (`burst` and `steady` lines) | Steady traffic, reported/exact over the last horizon of 3: within 1% up to 12 events/s at 15–60 s windows (1.000–1.006) and within 2% at 5 s (0.984–1.000); 13/s 0.94–0.99; 20/s 0.62–0.75; 100/s 0.12–0.22. A 1 s window reads 0.5–1.0 even at 1/s (it is under one 2.5 s unit). A burst in one tick, over 64 phases of the bucket cycle: reads at most 43 / 125 / 380 / 1,273 in 1 / 5 / 15 / 60 s windows, at least 14 / 66 / 192 / 770. A 10-event burst reads 5–10 in a 1 s window |
| D8 | The 1.15x misses are not forced by the layout | `go test -run TestPlacementLimit -v` | exact oracle (same lines, same 2 candidates, unbounded BFS) drops 0 of 731,739 inserts, peak slot load 0.9780; the filter evicts 4,575 times (4,577 with the stash, which then gives every returning key back: 0 missed). The oracle's shortest paths needed 5 moves for 3,406 inserts; the filter's search allows 4 |
| D9 | The oracle is right | `go test -run TestOracleMatchesMaxFlow -v` | on small overloaded tables it agrees with from-scratch max bipartite b-matching on 12,941 feasible and 2,933 infeasible inserts; every drop carries a checked Hall-violation certificate |
| D10 | Load threshold for 2 choices × 4 slots is 98.04% | `go test -run TestLoadThreshold -v` | 0.98037, solved from the Cain–Sanders–Wormald / Fernholz–Ramachandran equation; matches Walzer, [arXiv 2307.00644](https://arxiv.org/abs/2307.00644) §1.1.1 (0.89701, 0.95915, 0.98037 for 2, 3, 4 slots) |
| D11 | The displaced bit halves the entries an absent lookup compares | `go test -run TestComparedEntries -v` | 3.3998 vs 6.7989 entries per lookup (1e6 probes), expected FPR 1.267e-8 vs 2.533e-8; 18.9% of keys sit in their alternate line at 85% load |
| D12 | Register prototypes (docs/RESEARCH.md §3.2–3.3) | `go test -run TestRegisterExperiment -v` | table in `verify/expected/register-experiment.txt`: means linear 0.154, dyadic-7 0.108, Laplace 3×16 0.268 |
| D13 | Tests pass, including under the race detector | `go vet ./...`, `go test ./...`, `go test -race -short ./...` | pass |
| D14 | The racy regression tests hold at HEAD: no duplicate slots, no lost counts under relocation or eviction to the stash | `verify/replay.sh stress 300` (4 runs at a time; `verify/run.sh` runs this) | 0 of 300 failures each, four tests (the first three also 0 of 1,000 each in a separate run) |
| D15 | Hits skip the word0 CAS within one tick, and a full register skips the word1 CAS | derived: `touch` (`if now > last && !CAS`), `bump` (`nw == old`) | code |
| D16 | 2^46 µs ≈ 2.23 years (47 bits / 4.46 years before the saturated bit took one); 1.8e-8 FPR ≈ one false hit per minute at 1M absent-key lookups/s | derived: arithmetic | 7.04e13 µs = 2.23 y; 1.8e-8 × 1e6 × 60 = 1.08 |

## Attacks (checked; docs/SECURITY.md)

`verify/run.sh` runs these tests and compares their output with `verify/expected/attacks.txt`. The workloads are seeded and single-threaded; the keyed hash uses a random seed but only appears where any seed gives the same result.

| ID | Claim | Check | Result |
|---|---|---|---|
| A1 | The unkeyed integer hash let an attacker forge a victim's identity; the keyed one doesn't | `go test -run TestAttackForgedIdentity -v` | 4 ids built by inverting SplitMix64, 25 events each: never-touched victim reads 100 and seen (unkeyed), 0 and unseen (keyed) |
| A2 | …and pile first touches on one lock stripe | `go test -run TestAttackStripeFlood -v` | 4,096 crafted ids: 1 stripe unkeyed, 256 keyed |
| A3 | Evicting a 20-event key: 168 attacker events against the unkeyed hash, ~10^5 keyed; the stash restores it; a flood that also overruns the stash leaves it in the overflow table, which returns its full count | `go test -run TestAttackEvictionReset -v` | targeted: 168 events; offender returns with count 1 (no stash) or 21 of 21, Restored (stash). Random spray: 87,045 (no stash) / 102,753 (stash) events on 4,818 slots. Offender returns: count 1, Degraded, lost 3,568 (stash only); count 21, Upper 21, Degraded, lost 0, folded 66, the rest shed (stash + ghost table, the default since 9314d89). At 5ac63d5 (count-min sketch): count 20, Upper 21. At dd30cbc (16-bit sketch, no SpillMin): count 1, Upper 25, not Degraded |
| A4 | A key is never undercounted silently: if not Saturated, Upper ≥ the exact count | `go test -run TestAttackHeavyHitter -v` | steady and bursty senders at 1–500 ev/s, 1–60 s windows: 0 of 74,319 queries (713 not Saturated); Saturated on 0% at 1 ev/s, 93% at 12/s, ~100% from 31/s |
| A5 | Same contract, random senders (0.2–60 ev/s, silences, bursts up to 200 in one tick), horizons 10 s and 60 s | `go test -run TestUpperBoundRandom -v` | 0 of 187,219 queries (51,095 not Saturated); there Upper exceeds the exact count by 18.3% of events. Before the saturated bit, a density heuristic missed 689 |
| A6 | Concurrent evictions and restores lose no counts | `TestStashConcurrentEvictions` (in D14's stress) | 32 hot keys, 8 workers, 76-slot table flooded with heavier keys (~600 evictions per round): every key ends at its exact count. 3,000 rounds clean at 2d4682b; the first stash version lost counts in ~1 of 10 runs (a stash copy withdrawn after another eviction had replaced it), fixed before commit |
| A7 | Floods don't erase histories, and only keys with a dropped history pay for it | `go test -run TestAttackFloodCollateral -v` | 4,818-slot table at 85%, 200 offenders × 20 events, then 1x / 2x / 4x / 16x slots of keys × 21 events, or 16x slots of one-event keys, each flood within 30 s. Overflow off: 51 / 200 / 200 / 200 / 0 offenders lost, every new key Degraded. Ghost table (default since 9314d89, SpillMin 4): 0 lost, 0 offenders with Upper below 20; offenders short of 20: 0 / 5 / 200 / 200 / 0; of 20,000 new keys 0.0–0.2% Degraded (fingerprint matches), and a strict limit of 20 (Degraded or Upper > 20) turns away 0.0% / 1.8% / 100% / 100% / 0.0% (mean Upper 0 / 0.4 / 21 / 21 / 0). At 5ac63d5 (count-min sketch; no 2x row): 0% / – / 100% / 100% / 0%, mean Upper 0 / – / 124 / 254 / 0, and the 48 offenders short after 1x read Count 18 on average. Correction: at dd30cbc the floods advanced 1 ms per key, so the 16x flood ran 77 s and pushed the offenders' own events out of the 60 s horizon; that row's numbers were not about evicted histories |
| A8 | ConstantTime mode keeps every functional property | `TF_CT=1 go test ./...` and `TF_CT=1 go test -race -short` (`verify/run.sh` check `constant-time`) | all tests pass with every filter in ConstantTime (no pad). The first version livelocked under `-race` (a retry held its claim stripe while another goroutine's claim TryLocked it); fixed before commit |
| A9 | Sketch shape: 8-bit counters, 4 rows, Horizon/2 slices, one column per 4 slots (4.7 B/key, allocated on the first spill) matches or beats the dd30cbc shape (16-bit, Horizon/4, one per 8 slots, 7.1 B/key from the start) at every flood size. Superseded as the default by the ghost table (A11) | `TF_EXP=1 go test -run TestSketchShapes -v` (`results/security-cost-2026-10-09.md`) | 11 shapes × 6 floods (the 3x column since 9314d89). Offenders recovered at a 1x heavy flood: 180 vs 152 of 200; strict collateral at 2x: 89.7% vs 99.8%; equal elsewhere. 0 offenders below the truth in every cell |
| A10 | Shedding histories under 4 events (`SpillMin`) stops cheap floods from adding sketch noise. With the ghost table it matters little for collateral, but spilling every history makes inserts into a full table ~30% slower (S35), so the default stays 4 | `TF_EXP=1 go test -run TestSpillThreshold -v`; A7's last row | count-min sketch, strict collateral after 16x slots of 1-, 2- and 4-event keys: 100% / 100% / 100% with threshold 1; 0% / 0% / 100% with 4; 0% / 0% / 0% with 8. Ghost table: 0.1% / 0.1% / 0.1% with threshold 1, 0.0–0.1% with 2–8; offenders recovered at 1x and 2x heavy: 200 and 195 at every threshold |
| A11 | The ghost table (one 21-bit entry per dropped history) beats the count-min sketch in the same bytes: at 4.7 B/key (one 16 B unit per 4 slots, allocated on the first spill) a 2x heavy flood leaves 195 of 200 offenders' Count and turns away 1.9% of new keys under a strict limit, where the sketch kept 0 and turned away 89.7%. Half the bytes still beats the sketch at 2x. The cost: 0.1–0.2% of new keys read Degraded through fingerprint matches (sketch: 0%) | `TF_EXP=1 go test -run TestSketchShapes -v` rows "ghost /8, /4, /2"; A7 | per flood heavy 1x / 2x / 3x / 4x, light 16x / 64x (recovered, strict collateral): ghost /4: 200 0.1% / 195 1.9% / 18 89.2% / 1 100% / 200 0.1% / 200 0.1%. Sketch 8b r4 s2 /4 (same bytes, spill threshold 1): 180 0.0% / 0 89.7% / 0 99.7% / 0 100% / 200 100% / 200 100%. Ghost /8 (2.4 B/key): 2x 23 77.8%. Ghost /2 (9.4 B/key): 3x 188 2.5%, 4x 89 43.8%. 0 offenders below the truth in every cell; tests and `-race` pass with it as the default |

## Replays of recorded results (replayed)

| ID | Claim | Check | Result |
|---|---|---|---|
| R1 | Every results/history.md row's deterministic columns | `verify/replay.sh history` | each row's commit re-run with its own harness: all 18 rows match on every column that harness printed |
| R2 | Six bugs found and fixed | `verify/replay.sh fixes` | each regression test fails with the bug and passes with the fix (two runs): duplicate slots, 70546b2 10–12 of 20 → f31512c 0 of 20; 593bd1b still 7–10 of 400 → f31512c 0 of 400. Lost counts in the relocation test, d1db914 32–39 of 200 → d7fb2b5 0 of 200. Abandoned moves (cold keys off the hot tags), 07e2d35 20 of 20 runs (5–6 of 300 rounds each) → 4a037a5 0 of 20. Same-tag hand-over, 4a037a5 34 of 200 runs (about 1 in 1,600 rounds) → d7fb2b5 0 of 200 (60,000 rounds). Negative windows (a01e3dc) and the empty-batch panic (d1db914): deterministic |
| R3 | Before → after numbers in results/backlog.md | `verify/replay.sh harness` (today's accuracy harness on each library commit; expected output `verify/expected/harness.txt`) | item 1: 5.016% → 0% missed at 2.5x (b3e9aa5 harness on a1a7c69 vs b3e9aa5). Item 3: 1.96e-7 → 1.40e-8 at 1e9 probes (378bc37 → ea66fe6). Item 5: 41.9 → 18.8 B/key. Item 6: 0.191% → 0.066% (4bde715 → 7fbc51a), see X6. Item 7: 0.1288 → 0.0868. Item 9: 0.0868 → 0.0809. Item 13: 32 → 18 false positives in 1e9 (68eee6c → 99c054a) |

## Reverted experiments (patches)

Each "tried" item in results/backlog.md is a patch against the verification commit in `verify/experiments/`, applied with `git apply`. `verify/experiments.sh` applies each one and records its deterministic effect (`verify/expected/experiments.txt`); speed effects are host-dependent (`verify/ab.sh HEAD HEAD+verify/experiments/<name>.patch <bench>`).

| ID | Claim | Patch | Result | Status |
|---|---|---|---|---|
| E1 | Oldest-first eviction fails the spray test (RESEARCH §3.1) | oldest-first-eviction | `TestSprayCannotEvictRepeatOffender` fails ("offender 0: count 0 after spray"); scorecard spray 0 of 100 | checked |
| E2 | Branchless find: hot hits ~10% faster, cold lookups ~15% slower | branchless-find | `find` compiles to 4 SETEQ (0 without the patch); speed in `verify/speed.md` | checked / host |
| E3 | One CMPXCHG16B: 19 vs 15 ns for two CASes | cas2-cmpxchg16b | `verify/speed.md` | host |
| E4 | Reciprocal clock division: no change on the string path | reciprocal-clock | exactness test passes; speed in `verify/speed.md` | host |
| E5 | Raw RDTSC: 19 vs 37 ns | rdtsc-clock | benchmarks in `internal/clockbench`: `go test -run '^$' -bench . ./internal/clockbench` after applying; not in speed.md | host |
| E6 | Homing: keys in their second line 37% → 30% (steady), 33% → 24% (re-touch) | homing | 37.3% → 30.2% and 33.5% → 23.9% | checked; speed host |
| E7 | THP madvise granted, no clear gain | thp-madvise | AnonHugePages 307,200 kB with the patch, 0 without (host-dependent). Speed: 11–18% faster on the DRAM table in the re-run, so "no clear gain" doesn't hold here (S18, X22) | host |
| E8 | Relocation prefetch: no change | reloc-prefetch | `verify/speed.md` | host |
| E9 | Spill counts exact under a concurrent audit; absent lookups faster, inserts slower | spill-counts | audit passes; accuracy identical to the base; speed in `verify/speed.md` | checked / host |
| E10 | 46-bit register: FPR 7e-9, MAE 0.085, cap ~876 | register-46 | 7 in 1e9 (95%: 2.8e-9–1.4e-8); MAE 0.0852; FN 62 at 1.15x; 60 s burst 638–889; steady 10/s reads 12% low | checked |
| E11 | 44-bit register: no FP in 1e9, MAE 0.088, cap ~630 | register-44 | 0 in 1e9 (95% upper bound 3.7e-9); MAE 0.0879; FN 70; 60 s burst 508–633; steady 10/s reads 18–30% low | checked |
| E12 | Piecewise-linear edge estimate slightly worse | piecewise-linear | MAE 0.0834 vs 0.0809 | checked |
| E13 | 8-slot buckets: 0 misses at 98%, ~2x false positives | eight-slot | 0 missed at 1.15x; 29 vs 18 false positives in 1e9 (1.6x; "~2x" was never measured) | checked; speed host |
| E14 | Lock-free duplicate repair cut duplicates ~10x | lockfree-dup-repair-70546b2, lockfree-dup-repair, lockfree-dup-nolock-control | racy: mean duplicate slots over 60 runs 102 vs 1,592 without it at 70546b2 (16x); HEAD port 196 vs 1,112 (6x). See `verify/experiments/README.md` | re-run, not in expected/ |
| E15 | Pure dyadic H/32: same mean, 60 s error 0.040 vs 0.024 | dyadic-h32 | mean 0.0817 vs 0.0809; 60 s 0.0377 vs 0.0239 (the 0.040 was with the older estimator) | checked |
| E16 | Hot-key contention: 10 / 16 / 22 ns per op for 1 / 16 / 16K keys | contention-bench | `verify/speed.md` | host |

The alternate-line prefetch is commits 6c1f5dd (added) and 70546b2 (reverted); `verify/speed.md` compares them.

## Speed (host)

Measured by `verify/speed.sh` on one host (4-vCPU Xeon @ 2.1 GHz, 260 MB L3, Go 1.24.7); every block, with its command, is in `verify/speed.md`. Each A/B runs both builds alternately (8 rounds; 3–6 for the DRAM table) and compares medians. Same-build A/B pairs differ by up to 3%, so smaller ratios are noise. A claim **holds** if the measured direction and rough size match.

| ID | Claim (source) | Measured (median ns/op, B/A) | Verdict |
|---|---|---|---|
| S1 | Built-in clock ~38–42 ns, CoarseClock ~1.4 ns (filter.go) | 31.6 vs 1.4 | holds; the built-in read was 30–42 ns across runs |
| S2 | CoarseClock string path 172 → 110 ns (backlog 10) | scorecard `str` 115–121 vs `str coarse` 73–75 (0.63x) | holds (claimed 0.64x) |
| S3 | Batching: cold hit ~100 → 45–55 ns/key (backlog 4); TouchAndCount 154 → 106, VelocityCount 129 → 83 (backlog 15) | hit 65.3 → 42.6 (0.65x); TouchAndCount 97.9 → 62.9 (0.64x); VelocityCount 72.3 → 51.3 (0.71x) | holds; single hits are faster today, so batching saves less than the first figure |
| S4 | DRAM table: hit ~207, batched ~95, absent ~115, VelocityCount ~190 ns (backlog 17) | 174, 78, 91, 142 | same shape (batch/hit 0.45 vs 0.46); 15–25% lower in this run |
| S5 | Adaptive budget: overfull insert 12 µs → ~0.4 µs (backlog 2) | 3,729 → 304 (0.08x) | direction holds; 12x faster here, not 30x |
| S6 | Velocity-aware eviction leaves insert latency unchanged (RESEARCH §3.1) | 83.0 → 109.5 (1.32x) | **does not hold** (X19) |
| S7 | Striped lock: steady insert +~15 ns (backlog 11) | 154.9 → 174.1 (+19 ns); miss-insert +21–23 ns; hits 1.01 | holds |
| S8 | TrailingZeros increment: hot hit 44 → 32 ns (backlog 12) | 34.0 → 26.1 (0.77x) | holds (claimed 0.73x) |
| S9 | Displaced bit: no speed cost (backlog 13) | hits 1.00–1.02, insert 1.02, VelocityCount 1.06 | holds within 6% |
| S10 | Register published with key: saturated inserts ~10% faster (backlog 16) | 386.8 → 394.2 (1.02); steady 0.97 | **not reproduced** (X21) |
| S11 | Estimate without re-bucketing: VelocityCount 123 → 45 ns (backlog 9) | 88.2 → 71.1 (0.81x) | **does not hold** (X20) |
| S12 | Depth 3 / budget 500 instead of 4 / 1000: insert speed (backlog 6; the reason was never recorded, X6) | miss-insert 0.88x, near-full 0.67x, saturated 0.93x | measured now: inserts 7–33% faster, for 75 instead of 12 misses at 1.15x |
| S13 | Count-race fixes (4a037a5, d7fb2b5) | six benchmarks 1.00–1.03 | no change beyond noise |
| S14 | Branchless find: hot hits ~10% faster, cold lookups ~15% slower (E2) | hit 0.87, hot hit 0.80, VelocityCount 1.18 | holds |
| S15 | One CMPXCHG16B: 19 vs 15 ns for two CASes (E3) | 14.1 vs 11.8 (1.19x); on the hit path 1.04, hot 1.11 | holds |
| S16 | Reciprocal clock division: no change on the string path (E4) | 1.00 | holds |
| S17 | Homing: no hit gain, inserts +15 ns, hits on full tables +18 ns (E6) | steady insert +22 ns (1.14); hit +3.6 ns (1.06); churned-table hit 0.99 | holds for inserts and hit gain; the full-table hit cost was not re-measured separately |
| S18 | THP madvise: no clear change on the 600 MB table (E7, backlog) | hit 0.82 / 0.84, absent 0.87 / 0.89, batch 0.93 / 0.89 (3 and 6 rounds) | **does not hold here** (X22) |
| S19 | Relocation prefetch: no change (E8) | steady 1.06, saturated 0.99 | holds within 6% |
| S20 | Spill counts: absent lookups faster, inserts slower (E9) | absent 0.90, steady 1.09, near-full 1.09, hit 0.98 | holds |
| S21 | Alternate-line prefetch made hits slower (6c1f5dd, reverted in 70546b2) | hit 1.02, miss-insert 1.00 | **not reproduced** (X23) |
| S22 | 8-slot buckets: near-full 2,945 → 464, DRAM hit 208 → 227, absent 127 → 200, saturated 514 → 793 ns, steady slightly faster (E13) | 0.15, 1.09, 1.51, 1.58, 0.93 | holds (claimed 0.16, 1.09, 1.57, 1.54) |
| S23 | Contention: 10 / 16 / 22 ns per op for 1 / 16 / 16K keys (E16) | 7.2 / 12.6 / 19.3 | ordering holds; lower in this run |
| S24 | Raw RDTSC 19 vs 37 ns (E5) | not re-run (benchmarks live in the patch's `internal/clockbench`) | unchecked |
| S25 | Two CASes are ~15 ns of the ~32 ns hot hit (backlog, Next) | two-CAS microbenchmark 11.8 ns; hot hit 26–28 ns | roughly: ~12 of ~27 ns |
| S26 | Keyed hashing costs +3 ns per integer key (seed) or +15 ns (SipHash, `Config.Key`) | `go test -bench 'Hash(Uint64|String)'`: integer 2.9 unkeyed, 5.9–6.3 maphash, 16.8–20.6 SipHash; strings of 11 / 21 / 33 B 6.5–8.5 maphash, 18.6 / 22.8–26.7 / 28.6–35.3 SipHash | holds |
| S28 | `ConstantTime` hides whether a key was seen: one timing guesses right 85–89% (Touch) and 57–64% (Count) without it, 51% with a 3 µs pad | `TF_TIMING=1 go test -run TestTimingLeak -v` (`verify/run.sh --speed`, check `timing`): 2M-key table at 85%, 20k seen / 20k unseen probes in random order. two runs. KS D: Touch 0.70–0.79 → 0.015–0.018, Count 0.14–0.27 → 0.011–0.015. Residual: unseen inserts that run past the pad (Touch p99 4.1–4.5 µs unseen vs 3.4 µs seen) | holds |
| S29 | Overflow sketch and ConstantTime plumbing (dd30cbc vs 5f57f59), default mode: saturated inserts +65% (each spills a history and reads the sketch), other paths within ~10%. ConstantTime without a pad: 2–6× slower | `verify/ab.sh 5f57f59 dd30cbc …` 6 rounds: InsertSaturated 1.65, HugeAbsent 1.17, VelocityCount 1.11, InsertSteady 1.10, HitHot 1.09, HugeHit 1.09, InsertNearFull 1.07, VelocityCountHot 1.06, Hit 1.02, ContainsAbsent 1.02, TouchAndCount 1.02, Parallel 0.86. `TF_CT=1` (3 rounds): HitHot 6.19, ContainsAbsent 5.98, VelocityCountHot 4.39, HugeAbsent 3.91, Hit 3.35, InsertSteady 2.97, TouchAndCount 2.89, HugeHit 2.12, InsertSaturated 1.46 | holds |
| S30 | Timing pads: spinning 3 µs is the cheapest that works; sleeping, adaptive (doubling) and yielding pads lose | `verify/experiments/pad-modes-and-sip13.patch`, then `TF_TIMING=1 go test -run 'TestTimingModes|TestTimingThroughput' -v` (`results/security-cost-2026-10-09.md`) | spin 3 µs: KS D 0.014–0.023, 3.3 µs CPU per call. Sleep 50/200 µs: never under ~1.1 ms, 64–70 µs CPU per call. Adaptive from 1 µs: pad climbed to 128–512 µs. Yield 3 µs with 16 goroutines on 4 cores: 1.31 vs 1.10 M ops/s but p50 11 µs and D 0.063 | holds |
| S31 | SipHash-1-3 would cut keyed integer hashing by a third | same patch, `TF_EXP=1 go test -bench SipVariants` | 9.6–10.6 ns vs 15.2–15.8 ns (2-4) | holds (not adopted) |
| S32 | 2026-10-09 round (lazy 8-bit sketch, SpillMin) vs 08c8ea0: saturated inserts 31% faster, the rest within noise | `verify/ab.sh 08c8ea0 150fac4 …` 6 rounds (`verify/speed.md`): InsertSaturated 0.69, HugeAbsent 0.93, TouchAndCount 0.96, VelocityCount 0.95, VelocityCountHot 0.95, Parallel 0.95, InsertSteady 0.99, Hit 0.99, ContainsAbsent 1.01, HitHot 1.02, HugeHit 1.02, InsertNearFull 1.08 | holds |
| S33 | `ConstantTime` `Count` pads to its own tracked p99.5 (capped at `ConstantTime`): 3.4 → 1.05 µs per call at the same leak; `Touch` keeps its fixed pad. Shadow work and a fixed 1 µs pad don't beat it | `TF_CTEXP=1 go test -run TestCTPads -v` (`verify/run.sh --speed`, check `ct-pads`); shadow sweep: `ct-shadow-and-prefetch-toggle.patch`, `TestCTProfile` | fresh 2M-key table at 85% per row, 80k calls to settle, 40k timed. Fixed 3 µs: Count D 0.005, 3,385 ns/op; tracked (default): D 0.011, 1,052 ns/op, pad 793 ns; Touch D 0.012–0.026 at 3 µs either way. AutoPad: Touch pad 3.0 µs (3–4.9 µs in other runs), D 0.010. Fixed 1 µs: Touch D 0.17. Shadow work, unpadded: Count D 0.31 → 0.07–0.10, Touch 0.43 → 0.09–0.13; with Count's tracked pad D 0.005 vs 0.007–0.012 | holds (one host) |
| S34 | Under load the tracked Count pad doubles ConstantTime Count throughput and leaks less | `TF_CTEXP=1 go test -run TestCTParallel -v` | 16 goroutines on 4 cores plus one probe: fixed 3 µs 1.05–1.06 M ops/s, probe D 0.050–0.060; tracked 2.19–2.56 M ops/s, D 0.015–0.024 (two runs) | holds (one host) |
| S35 | This round (ghost table, Count pad, alternate-line prefetch, ae0b5ad) vs 5ac63d5: saturated inserts 16% faster, the rest within noise. Spilling every history (SpillMin 1) would make them 30% slower; without the prefetch near-full and saturated inserts are 9–10% slower | `verify/ab.sh 5ac63d5 ae0b5ad …` 6 rounds (`verify/speed.md`); `verify/ab.sh 9314d89 9314d89+verify/experiments/spillmin4.patch` and `…+no-alt-prefetch.patch` 5 rounds | InsertSaturated 0.84, TouchAndCount 1.02, InsertSteady 1.01, Hit 1.01, ContainsAbsent 1.03, InsertNearFull 1.00, VelocityCount 1.01, HitHot 1.06, VelocityCountHot 1.02, HugeHit 1.02, Parallel 1.03, HugeAbsent 1.09 (min 0.97). 9314d89 (SpillMin 1) vs 5ac63d5: InsertSaturated 1.30. SpillMin 4 vs 1: 0.73. No prefetch vs prefetch: InsertNearFull 1.09, InsertSaturated 1.10, InsertSteady 0.97 | holds (one host) |
| S27 | Stash, saturated bit and Result (2d4682b vs c5fa7bf): saturated inserts +42%, hot VelocityCount +12%, the rest within ~10% | `verify/ab.sh c5fa7bf 2d4682b …` (`verify/speed.md`): InsertSaturated 1.42, VelocityCountHot 1.12, CheckAndTouchString 1.09, InsertSteady 1.06, HitHot 1.06, Parallel 1.06, InsertNearFull 1.05, Hit 1.04, ContainsAbsent 1.02, TouchAndCount 1.01, VelocityCount 0.99 | holds |

## Corrected claims

| ID | Was | Now | Evidence |
|---|---|---|---|
| X1 | "Two-choice, 4-slot tables saturate at ~97.7% load, so the 98% misses can't be removed with this layout" (history legend, DESIGN, thread round 5) | The threshold is 98.04%, and an exact placement exists for every insert of the 1.15x stream (peak 97.8%). The misses come from the bounded search; depth 4 / budget 1000 (6aa1c1b) missed 12 instead of 58 | D8, D9, D10, R3 |
| X2 | "Heavy hitters keep their counts (up to 2,617 per horizon)" (DESIGN, a test comment) | The register holds at most 1,273 events, and heavy keys read low well before that: see D7 and X16 | D7 |
| X3 | Decision card: "counts stay exact past 1,000 hits per key per horizon" with the current register | Only for a burst at a favourable phase; steady traffic is within 1% only up to ~12 events/s (720 per minute). The alternatives' caps (876, 630) were measured the same way; on both patterns they are still lower than the current register's, so the choice's ordering holds | D7, E10, E11 |
| X4 | "0.06% missed at 98%" (DESIGN) | 0.051% (58 of 113,040) at 1.15x | D1 |
| X5 | "lock-free filter", "safe for concurrent use without locks" (DESIGN, filter.go) | Hits are lock-free; a key's first touch takes a striped lock | filter.go `touch` |
| X6 | "Relocation depth 3 / budget 500: misses at 98% load 0.19% → 0.06%" (backlog item 6) | True against 4bde715, but the commit just before it (6aa1c1b: depth 4 / budget 1000) missed 0.011%; the change raised misses from 12 to 75. Its insert-speed benefit wasn't recorded; measured now: inserts 7–33% faster (S12) | R3, S12 |
| X7 | "Static FPR 2e-7 → 4e-8" (backlog item 3, thread round 1) | Those were 1 and 2 events in 5M and 50M probes. At 1e9 probes: 1.96e-7 → 1.40e-8 | R3 |
| X8 | `VelocityCount` "re-buckets to now" (DESIGN) | It reads the buckets at the register's own epoch, no re-bucketing (since a477d31) | register.go `estimate` |
| X9 | 1s error ranges in RESEARCH §3.2 (linear 0.38–0.75, dyadic 0.17–0.35) | 0.02–0.75 and 0.02–0.35 (the sparse shape was left out) | D12 |
| X10 | Laplace register "2× the linear register's error" | 1.7× (0.268 vs 0.154) | D12 |
| X11 | Truncated timestamps would allow "6 slots per line" (RESEARCH §3.4) | 15 bits freed per slot; 6 slots would need ≤ 85 bits each, a slot needs ~113 | arithmetic |
| X12 | Outdated text: 48-bit timestamps / 8.9 years, `Horizon/32` units, "255 per sub-window", "32 B per key at 50% load", "FP churn at 1.0x", "misses at zero up to ~0.95" | 47 bits / 4.46 years, `Horizon/24`, D7, D6, FP churn is at the highest load point, D1 | code, D1, D6, D7 |
| X13 | "~5% of counts lost" with 8 racing first touches (backlog item 11, thread round 3) | 8.0% in the replayed run (147,186 of 160,000 counted); duplicates 13.5% (2,695 of 20,000). The number varies per run | R2 report |
| X14 | `TestTouchRacingRelocationKeepsCounts` as the evidence that relocation races lose no counts (backlog item 16, thread round 6: "12 of 20 runs lost counts, now 0 of 20") | The test was flawed: under CPU load it failed at HEAD too (12 of 40 runs), with no race involved. Hot keys still at one event could be evicted at 98% load, and on fast runs keys passed 762 events and VelocityCount interpolated the oldest bucket. Fixed test: 3 initial touches per hot key, all touches at a bucket-aligned tick, ≤1,203 events per key. It fails often at d1db914 (38 of 200) and rarely after f31512c (1 lost count in 1 of 300 runs at 07e2d35): see X17 | R2, D14 |
| X15 | "0 duplicates across 50 runs" after the striped lock (593bd1b; thread round 3) | 593bd1b still produced 1 duplicate slot in ~2.5% of runs under CPU load (10 of 400). f31512c's install order closed it: 0 of 400 | R2 |
| X16 | Count limits stated as "exact past 1,000 per horizon" or "1,273 per burst" | Steady traffic above ~12 events/s per key reads low in every window; bursts read low in windows shorter than the horizon; even 60 s windows read 770–1,273 for a 1,273-event burst depending on phase | D7 |
| X17 | No counts lost to relocation races after f31512c (backlog item 16) | Two rarer causes remained. (1) A move abandoned because a touch reached the source first dropped the copy's own counts: 2% of `TestAbandonedMoveKeepsCounts` rounds with distinct tags. Fixed in 4a037a5. (2) A touch could count into the slot's next occupant when it had the same 12-bit tag: about 1 in 1,600 rounds with shared tags. Fixed in d7fb2b5. Both were found from the 1-in-300 stress failure; the distinct-tags run (0 failures in 24,000 rounds vs 4 with shared tags at the time) isolated the second | R2, D14 |
| X18 | D7 as first written today: "within 1% up to 12/s" | The check counted `(now − W, now]` while the filter counts `[now − W, now]`, so low rates read one event high (1.2x at 1/s in 5 s) and contradicted the claim. The check now uses the filter's window; the claim holds | D7, 7302e5b |
| X19 | Velocity-aware eviction left insert latency unchanged, ~230 ns (RESEARCH §3.1) | Miss-insert went from 83 to 110 ns (1.32x) on the same benchmark | S6 |
| X20 | Estimating without re-bucketing cut cache-resident VelocityCount from 123 to 45 ns (backlog 9) | 88 → 71 ns (0.81x). The scorecard's velocity column didn't show 123 → 45 either (111, 115, 106 ns over 7293e5d, 0a00063, a477d31) | S11 |
| X21 | Publishing the register with its key made saturated inserts ~10% faster (backlog 16) | 387 → 394 ns (1.02): no measurable change | S10 |
| X22 | THP madvise: no clear change on the 600 MB table (backlog, tried) | 11–18% faster hits, absent lookups and batches on the DRAM table in two runs (3 and 6 rounds; THP mode `madvise`). Host-dependent; worth re-testing | S18 |
| X23 | The alternate-line prefetch made hits slower (70546b2) | No measurable change: hit 1.02, miss-insert 1.00 | S21 |
| X24 | Commit bed5c07: "Keep steady-insert benchmark" | The commit removed `BenchmarkInsertSteady`; 99c054a added it back. `verify/experiments/insert-steady-bench-70546b2.patch` restores it for S7 | git show bed5c07 |

## Not reproducible

| Claim | Where | Why |
|---|---|---|
| "pprof puts 71% of the time on that single line load" | thread, first prototype | profile not kept; re-profiling 564d865 would give a new number, not this one |
| "Missed keys: 0.09% of keys were dropped when the table was full" | thread, first prototype | the test setup wasn't recorded |
| "Absent keys skip the second line read (64% of misses at 85% load)" | thread, round 4 status | how it was computed wasn't recorded |
| Asm PREFETCHT0 and "two-choice insert" variants of the alternate-line prefetch | backlog, thread round 3 | code not kept (the plain-load version is 6c1f5dd) |
| Mechanism explanations: the clock read "stops later work from overlapping", hits on DRAM are "latency-bound because the CAS stops later lookups from overlapping", "the adjacent-line prefetcher doesn't make the second [line] free", "Go heap already benefits or TLB isn't the bottleneck" | backlog | hypotheses offered to explain measurements; never tested directly |
| Literature summaries and originality judgments | docs/RESEARCH.md §1–2 | check them against the linked sources; the search wasn't exhaustive |
