# Temporal filter scorecard history

Produced by `go run ./cmd/tfbench -label ...`. Capacity 200k, horizon 60s.
FN a/b/c = missed keys on the churn workload at multiples of the configured Capacity (ground truth: exact map). The load points changed as sizing changed:
- before 13:06: 0.5x / 1.0x / 1.5x of Capacity, table sized at ~38% slot load for 1.0x (41.9 B/key)
- 13:06 to 13:25: 1.0x / 2.0x / 2.5x (38% / 76% / 95% slot load, 41.9 B/key)
- from 13:26: 1.0x / 1.1x / 1.15x with the table sized for 85% load at 1.0x (85% / 94% / 98% slot load, ~19 B/key)

B/key = table bytes per Capacity key. vel MAE = VelocityCount error: per window (1,5,15,30,60s) sum|est-exact|/sum exact, averaged over windows; 5 traffic shapes (from 13:36; the 13:34 row pooled all windows).
FP churn = false "seen" on the churn workload at 1.0x. Static FPR = never-inserted probes at capacity (5M probes before 13:16, 50M after).
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table; batch = CheckAndTouchBatch per key (rows from 13:20); insert = steady state (keys expire as fast as they arrive), insert sat = every line full (rows before 13:10 measured only the saturated case, in the insert column); parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock.

| when (UTC) | rev | FN a | FN b | FN c | FP churn | static FPR | spray | B/key | vel MAE | hit cold | batch | hit hot | insert | insert sat | velocity | parallel | str | change |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-08 13:04 | 8381fd2 | 0.004% | 0.013% | 0.278% | 0.00e+00 | 4.00e-07 | 100% | 41.9 | - | 97 | - | 31 | 260 | - | 107 | 19.1 | 201 | baseline: v2 velocity-aware eviction |
| 10-08 13:09 | 597ee63 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.00e-07 | 100% | 41.9 | - | 100 | - | 27 | 12372 | - | 125 | 18.3 | 230 | cuckoo relocation depth 2 (alt line from fp) |
| 10-08 13:13 | 126bd87 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.00e-07 | 100% | 41.9 | - | 99 | - | 30 | 218 | 384 | 132 | 19.0 | 207 | adaptive cuckoo budget: deep search only while it succeeds |
| 10-08 13:16 | 4b5850b | 0.000% | 0.000% | 0.000% | 0.00e+00 | 4.00e-08 | 100% | 41.9 | - | 95 | - | 25 | 205 | 394 | 103 | 18.1 | 260 | 28-bit identity (12-bit tag, 4-bit epoch) |
| 10-08 13:21 | 151815d | 0.000% | 0.000% | 0.000% | 0.00e+00 | 4.00e-08 | 100% | 41.9 | - | 101 | 45 | 31 | 219 | 373 | 108 | 19.2 | 201 | batched API (memory-level parallelism) |
| 10-08 13:26 | 3e9277c | 0.000% | 0.000% | 0.191% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | - | 92 | 59 | 30 | 228 | 425 | 76 | 22.0 | 170 | 85% target load, any-size table (-55% memory) |
| 10-08 13:31 | 983b804 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 6.00e-08 | 100% | 18.8 | - | 97 | 60 | 35 | 286 | 727 | 84 | 22.2 | 183 | 8-slot buckets (2 adjacent lines) — reverted: wins only above 94% load, ~10% slower elsewhere |
| 10-08 13:34 | acea1cf | 0.000% | 0.000% | 0.066% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.038 | 76 | 55 | 35 | 196 | 427 | 74 | 20.3 | 212 | velocity MAE column; reloc depth 3/budget 500 |
| 10-08 13:42 | ba0677c | 0.000% | 0.000% | 0.058% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.087 | 120 | 94 | 74 | 523 | 687 | 111 | 34.4 | 318 | age-graded velocity register (H/24 near now), spill on overflow |
| 10-08 13:46 | d5648a8 | 0.000% | 0.000% | 0.058% | 0.00e+00 | 4.00e-08 | 100% | 18.8 | 0.087 | 84 | 60 | 43 | 188 | 620 | 115 | 24.9 | 215 | register fast paths (lazy victim score, packed increment, no divides) |
