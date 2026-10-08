# Sliding-Window Probabilistic Register (Temporal Filter)

A fixed-memory, lock-free filter answering **"seen key X in the last W?"** and **"how many times in the last W?"** for any `W ≤ Horizon`, chosen per query.

## Layout: everything for a key lives in one 64-byte cache line

The table is an array of 64-byte-aligned lines, each holding 4 slots of 2 words:

```
word0: fp(16) | lastTouch(48)            freshness, exact to 1 tick (default 1µs, ~8.9y range)
word1: tag(8) | epoch(8) | c0 c1 c2 c3 c4 c5 (6 × 8-bit)   velocity shift register
```

- **Identity** = 16-bit `fp` + 8-bit `tag` = 24-bit fingerprint, both in the same line, so no extra memory traffic.
- **Placement**: primary line from the hash; overflow to one alternate line only when the primary has 4 live keys. Inserts prefer empty or expired slots, then evict the lowest-velocity of the 8 candidates (oldest on ties), so spraying fresh keys cannot flush a repeat offender.
- **Velocity**: `c0` counts the current sub-window (`Horizon/5`), `c5` the oldest. Advancing time by `d` sub-windows is `ctr << 8d`.

## Operations

| Op | What it does | Memory |
|---|---|---|
| `CheckAndTouch(key, W) bool` | CAS `lastTouch` to now, returns `now − lastTouch ≤ W`, bumps `c0` | 1 line on hit (primary), 2 on insert |
| `VelocityCount(key, W) int` | Shifts the register to now and sums the sub-windows inside W, interpolating the oldest partial one | 1 line |
| `TouchAndCount(key, W)` | Both in one pass | 1 line |
| `ContainsAt(h, W, now)` | Read-only freshness check | 1 line |
| `ContinuousDecay()` | Bounded slot reclaimer, described below | ≤ 4 lines/step |

`*At(hash, windowTicks, nowTicks)` variants take a pre-hashed key and a caller-cached clock for hot paths.

## Why it avoids the classic problems

- **No boundary artifacts in freshness.** `lastTouch` is an exact timestamp, so `W` is checked to the tick (tested at `age == W` and `W+1`). Rotating Bloom generations can't do this.
- **No sweep spikes.** Expiry is arithmetic: every access compares `now` with `lastTouch` and shifts the register by elapsed sub-windows. Correctness never waits on a sweep.
- **ContinuousDecay** only reclaims slots so occupancy stays honest. A shared cursor advances at `lines × elapsed / Horizon`, which is one table pass per horizon, paced by the clock. Callers claim at most 1–4 lines per step via CAS. It runs inline on 1/64 of operations (gated by key hash, so the cursor's cache line stays cold) and never blocks a reader. If it falls behind by more than a lap, it skips ahead safely.
- **Velocity without per-event timestamps.** 48 bits per key. `lastTouch` sharpens the estimate: it is exactly 0 once `W < now − lastTouch`, at least 1 otherwise, and `c0`'s events are known to lie at or before `lastTouch`.

## Accuracy (from `filter_test.go`)

- **Freshness FPR**: about `slots-probed × 2⁻²⁴ ≈ 2e-7`. Measured **0 / 1,000,000** probes at full capacity.
- **False negatives** (evictions at capacity, ~38% slot load): **86 / 100,000 (0.09%)**. These only happen when both candidate lines are full of live keys, so oversize `Capacity` if FN matters more than memory.
- **Velocity error**: at most the count in the one sub-window straddling the window's trailing edge (property-tested on Poisson and bursty traffic for W = 1s–60s). Resolution is `Horizon/5`. If you need 1s precision on a 60s horizon, run a second filter with a short horizon.
- **Saturation**: 255 per sub-window (1,275 per horizon). Beyond that the key is already an outlier.
- **Concurrency**: no lost updates on hits (CAS loops). Tested under `-race` with 8 writers.

## Performance

Measured on a 4-vCPU Xeon @ 2.1GHz with 1M live keys (32 MiB table, cold cache). Full output is in `bench.txt`.

| Benchmark | ns/op | allocs |
|---|---|---|
| CheckAndTouch hit, cache-resident | **~35** | 0 |
| CheckAndTouch hit, cold table | ~130 | 0 |
| CheckAndTouch hit, 4 goroutines (throughput) | **~26** | 0 |
| CheckAndTouch miss/insert | ~230 | 0 |
| VelocityCount | ~125 | 0 |
| TouchAndCount | ~185 | 0 |
| String key + built-in clock | ~245 | 0 |
| Baseline: mutex + map, 1 goroutine | ~175 | 0 |
| Baseline: mutex + map, 4 goroutines | ~280 | 0 |

pprof on the cold hit path shows 71% in `find`, which is the single DRAM miss on the line, plus 13% in `touch` (the word0 CAS) and 10% in `bump`. The operation is memory-bound on exactly one line, as designed. On top of that the filter uses fixed memory (16 B/slot, sized for ~50% load), tracks velocity, and scales with cores. The mutex baseline has none of those.

## Known limits / next steps

1. **Insert touches 2 lines.** An overflow-hint bit per line would let most misses skip the alternate line.
2. **Hot-path cost is two locked CASes.** Skipping the word0 CAS when `now − lastTouch` is under some resolution floor would roughly halve the cache-resident cost.
3. **Duplicate-insert race.** Concurrent *first* inserts of the same key can land in two slots, which splits counts. This is rare and bounded.
4. **Sub-window precision.** Below `Horizon/5` the count is interpolated, not exact.

## Run

```
go test -race ./...
go test -run '^$' -bench . -benchmem
go test -run '^$' -bench CheckAndTouchHit$ -cpuprofile cpu.prof && go tool pprof -top cpu.prof
```
