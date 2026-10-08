# Temporal filter scorecard history

Produced by `go run ./cmd/tfbench -label ...`. Capacity 200k, horizon 60s.
FN = missed keys on the churn workload at 0.5x / 1.0x / 1.5x capacity (ground truth: exact map).
FP churn = false "seen" on the churn workload at 1.0x. Static FPR = 5M never-inserted probes at capacity.
Spray = repeat offenders keeping exact counts after 20x-capacity spray. Latencies in ns/op (best of 3),
1M-key table; parallel is per-op throughput with GOMAXPROCS goroutines; str = string key + built-in clock.

| when (UTC) | rev | FN 0.5x | FN 1.0x | FN 1.5x | FP churn | static FPR | spray | hit cold | hit hot | insert | velocity | parallel | str | change |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
