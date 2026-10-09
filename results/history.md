# Temporal filter scorecard history

Produced by `go run ./cmd/tfbench -label ...`. Capacity 200k, horizon 60s.
FN a/b/c = missed keys on the churn workload at multiples of the configured Capacity (ground truth: exact map). The load points changed as sizing changed:
- before 13:06: 0.5x / 1.0x / 1.5x of Capacity, table sized at ~38% slot load for 1.0x (41.9 B/key)
- 13:06 to 13:25: 1.0x / 2.0x / 2.5x (38% / 76% / 95% slot load, 41.9 B/key)
- from 13:26: 1.0x / 1.1x / 1.15x with the table sized for 85% load at 1.0x (85% / 94% / 97.8% peak slot load, ~19 B/key)
- Correction (10-08 verification): an earlier note here said 2-choice, 4-slot tables saturate at ~97.7%, so FN c couldn't reach 0. That was wrong. The threshold is 98.04%, and an exact placement oracle places every key of the 1.15x stream, so FN c comes from the bounded cuckoo search, not the layout (verify/claims.md D8, D10).

B/key = table bytes per Capacity key. vel MAE = VelocityCount error: per window (1,5,15,30,60s) sum|est-exact|/sum exact, averaged over windows; 5 traffic shapes (from 13:36; the 13:34 row pooled all windows).
FP churn = false "seen" on the churn workload at the highest load point (FN c's; 0.5x in the first row). Static FPR = never-inserted probes at capacity (5M probes before 13:16, 50M after, 1e9 from 14:46; the 5M and 50M values are 1 or 2 events, so treat them as rough).
Re-checking: `verify/replay.sh history` re-runs each row's commit with its own harness and compares the deterministic columns (FN, FP churn, static FPR, spray, B/key, vel MAE). Rows marked `+` were measured on an uncommitted tree and are checked against the next commit. Harnesses before 13:26 didn't print B/key; `verify/replay.sh harness` shows 41.9 for those commits.
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table (20 MB, which fits this host's 260 MB L3, so "cold" means L3); hit DRAM = hit on a 32M-key (~600 MB) table, every lookup a DRAM miss (last column, from 15:39); batch = CheckAndTouchBatch per key (rows from 13:20); insert = steady state (keys expire as fast as they arrive), insert sat = every line full (rows before 13:10 measured only the saturated case, in the insert column); parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock; str coarse = same with CoarseClock (100µs cached clock), from 13:54. Before 13:57, str ran on a table the insert benchmarks had filled with future-dated keys, so it measured the eviction path; from 13:57 it uses a fresh 1M-key table.

| when (UTC) | rev | FN a | FN b | FN c | FP churn | static FPR | spray | B/key | vel MAE | hit cold | batch | hit hot | insert | insert sat | velocity | parallel | str | str coarse | change | hit DRAM |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-08 13:04 | 8381fd2 | 0.004% | 0.013% | 0.278% | 0.00e+00 | 4.00e-07 | 100% | 41.9 | - | 97 | - | 31 | 260 | - | 107 | 19.1 | 201 | - | baseline: v2 velocity-aware eviction |
| 10-08 13:09 | 597ee63 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.00e-07 | 100% | 41.9 | - | 100 | - | 27 | 12372 | - | 125 | 18.3 | 230 | - | cuckoo relocation depth 2 (alt line from fp) |
| 10-08 13:13 | 126bd87 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.00e-07 | 100% | 41.9 | - | 99 | - | 30 | 218 | 384 | 132 | 19.0 | 207 | - | adaptive cuckoo budget: deep search only while it succeeds |
| 10-08 13:16 | 4b5850b | 0.000% | 0.000% | 0.000% | 0.00e+00 | 4.00e-08 | 100% | 41.9 | - | 95 | - | 25 | 205 | 394 | 103 | 18.1 | 260 | - | 28-bit identity (12-bit tag, 4-bit epoch) |
| 10-08 13:21 | 151815d | 0.000% | 0.000% | 0.000% | 0.00e+00 | 4.00e-08 | 100% | 41.9 | - | 101 | 45 | 31 | 219 | 373 | 108 | 19.2 | 201 | - | batched API (memory-level parallelism) |
| 10-08 13:26 | 3e9277c | 0.000% | 0.000% | 0.191% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | - | 92 | 59 | 30 | 228 | 425 | 76 | 22.0 | 170 | - | 85% target load, any-size table (-55% memory) |
| 10-08 13:31 | 983b804 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 6.00e-08 | 100% | 18.8 | - | 97 | 60 | 35 | 286 | 727 | 84 | 22.2 | 183 | - | 8-slot buckets (2 adjacent lines) — reverted: wins only above 94% load, ~10% slower elsewhere |
| 10-08 13:34 | acea1cf | 0.000% | 0.000% | 0.066% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.038 | 76 | 55 | 35 | 196 | 427 | 74 | 20.3 | 212 | - | velocity MAE column; reloc depth 3/budget 500 |
| 10-08 13:42 | ba0677c | 0.000% | 0.000% | 0.058% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.087 | 120 | 94 | 74 | 523 | 687 | 111 | 34.4 | 318 | - | age-graded velocity register (H/24 near now), spill on overflow |
| 10-08 13:46 | d5648a8 | 0.000% | 0.000% | 0.058% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.087 | 84 | 60 | 43 | 188 | 620 | 115 | 24.9 | 215 | - | register fast paths (lazy victim score, packed increment, no divides) |
| 10-08 13:52 | 24f133b | 0.000% | 0.000% | 0.051% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.081 | 101 | 62 | 40 | 216 | 466 | 106 | 25.6 | 263 | - | estimate without re-bucketing; mask bucket starts |
| 10-08 13:54 | ade47fd | 0.000% | 0.000% | 0.051% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.081 | 86 | 53 | 46 | 202 | 590 | 98 | 25.7 | 263 | 78 | CoarseClock option (cached clock) |
| 10-08 14:10 | bc2e12d+ | 0.000% | 0.000% | 0.051% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.081 | 80 | 60 | 44 | 223 | 463 | 102 | 27.4 | 179 | 117 | no duplicate slots on concurrent first insert (striped claim lock) |
| 10-08 14:26 | a8a2848+ | 0.000% | 0.000% | 0.051% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.081 | 89 | 60 | 32 | 220 | 500 | 111 | 27.8 | 214 | 123 | branchless first-free-bucket increment (hot keys) |

Note 14:26: the host was noisy this round (cold-path benchmarks varied ±20% between back-to-back runs of the same binary). Compare cold columns across rows with care. The hot-hit gain held in 8 interleaved A/B runs (min 45 → 34 ns).
| 10-08 14:46 | c8dbb54+ | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 85 | 61 | 29 | 197 | 465 | 96 | 23.3 | 221 | 119 | displaced bit: match only entries homed at the key's primary line |
| 10-08 15:12 | f07b04e+ | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 90 | 58 | 35 | 222 | 483 | 93 | 21.9 | 196 | 122 | publish register with its key on install; bumps check the tag (no lost counts under relocation races); batched count APIs |
| 10-08 15:39 | 2c3342b+ | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 74 | 49 | 31 | 193 | 452 | 89 | 21.6 | 144 | 89 | scorecard: add hit DRAM (32M-key table); no filter change | 207 |
| 10-08 17:20 | d96e45d | 0.000% | 0.000% | 0.051% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 65 | 39 | 26 | 162 | 397 | 72 | 18.9 | 120 | 75 | abandoned moves fold counts back; busy hand-over; harness counts window edges inclusively | 176 |
| 10-08 20:14 | 1a89bb4 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 79 | 56 | 37 | 238 | 710 | 95 | 22.0 | 174 | 113 | security: keyed hash, eviction stash, saturated bit | 205 |
| 10-09 04:14 | e4cc7dc | 0.000% | 0.000% | 0.000% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 157 | 80 | 38 | 382 | 1590 | 174 | 53.8 | 251 | 173 | overflow sketch (no erased histories, per-key Degraded), ConstantTime mode | 270 |
| 10-09 05:38 | 43bbd6d | 0.000% | 0.000% | 0.000% | 0.00e+00 | 1.80e-08 | 100% | 18.8 | 0.081 | 163 | 85 | 37 | 394 | 1011 | 168 | 46.5 | 264 | 216 | lazy 8-bit overflow sketch, SpillMin 4 | 263 |
