# Temporal filter scorecard history

Produced by `go run ./cmd/tfbench -label ...`. Capacity 200k, horizon 60s.
FN = missed keys on the churn workload at 1.0x / 2.0x / 2.5x the configured Capacity (the table is sized for ~38% slot load at 1.0x, ~95% at 2.5x; ground truth: exact map). Rows before 13:06 measured 0.5x / 1.0x / 1.5x.
FP churn = false "seen" on the churn workload at 1.0x. Static FPR = 5M never-inserted probes at capacity.
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table; parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock.

| when (UTC) | rev | FN 1.0x | FN 2.0x | FN 2.5x | FP churn 2.5x | static FPR | spray | hit cold | hit hot | insert | velocity | parallel | str | change |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 10-08 13:04 | 8381fd2 | 0.004% | 0.013% | 0.278% | 0.00e+00 | 4.00e-07 | 100% | 97 | 31 | 260 | 107 | 19.1 | 201 | baseline: v2 velocity-aware eviction |
