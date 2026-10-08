# Temporal filter scorecard history

Produced by `go run ./cmd/tfbench -label ...`. Capacity 200k, horizon 60s.
FN = missed keys on the churn workload at 1.0x / 2.0x / 2.5x the configured Capacity (the table is sized for ~38% slot load at 1.0x, ~95% at 2.5x; ground truth: exact map). Rows before 13:06 measured 0.5x / 1.0x / 1.5x.
FP churn = false "seen" on the churn workload at 1.0x. Static FPR = never-inserted probes at capacity (5M probes before 13:16, 50M after).
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table; insert = steady state (keys expire as fast as they arrive), insert sat = every line full (rows before 13:10 measured only the saturated case, in the insert column); parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock.

| when (UTC) | rev | FN 1.0x | FN 2.0x | FN 2.5x | FP churn 2.5x | static FPR | spray | hit cold | hit hot | insert | insert sat | velocity | parallel | str | change |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-08 13:04 | 8381fd2 | 0.004% | 0.013% | 0.278% | 0.00e+00 | 4.00e-07 | 100% | 97 | 31 | 260 | - | 107 | 19.1 | 201 | baseline: v2 velocity-aware eviction |
| 10-08 13:09 | 597ee63 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.00e-07 | 100% | 100 | 27 | 12372 | - | 125 | 18.3 | 230 | cuckoo relocation depth 2 (alt line from fp) |
| 10-08 13:13 | 126bd87 | 0.000% | 0.000% | 0.000% | 0.00e+00 | 2.00e-07 | 100% | 99 | 30 | 218 | 384 | 132 | 19.0 | 207 | adaptive cuckoo budget: deep search only while it succeeds |
