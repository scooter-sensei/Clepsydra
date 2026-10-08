# Sliding-Window Probabilistic Register (Temporal Filter)

A fixed-memory, lock-free filter answering **"seen key X in the last W?"** and **"how many times in the last W?"** for any `W ≤ Horizon`, chosen per query.

Measured history of every change is in `results/history.md`; ideas done, tried and queued are in `results/backlog.md`.

## Layout

The table is any number of 64-byte-aligned lines (multiply-shift indexing, no power-of-two rounding). Each line holds 4 slots of 2 words:

```
word0: fp(16) | displaced(1) | lastTouch(47)        freshness, exact to 1 tick (default 1µs, ~4.4 years)
word1: tag(12) | epoch(4) | 7 velocity buckets (48)  age-graded counter
```

- **Identity** = 16-bit `fp` + 12-bit `tag` = 28 bits, both in the same line. The primary line uses hash bits 0..35, disjoint from the identity bits.
- **Displaced bit**: set when an entry lives in its alternate line, flipped whenever it moves. Lookups match it, so a key is only compared with entries homed at its own primary line. This halves false positives for free.
- **Sizing**: `Config.Load` (default 0.85) is the slot occupancy at `Capacity`, which comes to ~19 B per key.
- **Placement**: primary line first, then the alternate line `alt(l) = (x(fp) − l) mod lines`, which is its own inverse, so stored entries can move without their key.
  - If both lines are full, a **bounded cuckoo search** (iterative deepening, depth 3) moves an entry to its alternate line.
  - The search budget shrinks while recent searches fail (an EWMA), so an overfull table degrades to cheap eviction.
  - Eviction takes the **lowest-velocity** slot (oldest on ties), so a spray of fresh keys can't flush a repeat offender.
- **Velocity register** (`register.go`): 7 buckets that are 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide, where a unit is `Horizon/24`. Resolution is fine near now and coarse further back.
  - Bucket boundaries only coarsen as time passes, so advancing any distance is one re-bucketing pass with no per-tick work and no sweep.
  - Counter widths grow with bucket width (5,5,6,7,8,8,9 bits). A full bucket **spills** into older ones, so heavy hitters keep their counts (up to 2,617 per horizon) instead of capping.
  - The 4-bit epoch is resolved against `lastTouch`'s unit.

## Operations

| Op | What it does | Memory |
|---|---|---|
| `CheckAndTouch(key, W) bool` | CAS `lastTouch` to now, returns `now − lastTouch ≤ W`, counts the event | 1 line on a primary hit |
| `CheckAndTouchBatch(hs, W, now, out)` | Same for many keys; issues every key's line load first so cache misses overlap | ~half the per-key time |
| `VelocityCount(key, W) int` | Re-buckets to now, sums buckets inside W, interpolates the one the edge cuts | 1 line |
| `TouchAndCount(key, W)` | Both in one pass | 1 line |
| `ContainsAt(h, W, now)` | Read-only freshness check | 1 line |
| `ContinuousDecay()` | Bounded slot reclaimer, paced by the clock (one table pass per horizon, ≤ 4 lines per step) | — |

`*At(hash, windowTicks, nowTicks)` variants take a pre-hashed key and a caller-cached clock.

## Why it avoids the classic problems

- **No boundary artifacts in freshness.** `lastTouch` is exact, so `W` is checked to the tick.
- **No sweep spikes.** Expiry and decay are arithmetic on every access. The reclaimer only frees slots, a few lines at a time, inline on 1/64 of operations.
- **Velocity is gated by exact facts.**
  - The count is 0 once `W < now − lastTouch`, and at least 1 otherwise.
  - The newest non-empty bucket's events end at `lastTouch`.

## Current numbers

These come from the latest row of `results/history.md`: capacity 200k, horizon 60s, 4-vCPU Xeon @ 2.1GHz, latencies on a 1M-key table.

- **Missed keys:** 0 at 85% and 94% slot load, 0.06% at 98%.
- **False positives:** 1.8e-8 on 1e9 probes of never-inserted keys at capacity, and 0 on the churn workload.
- **Memory:** 18.8 B per key.
- **Velocity error:** 0.081, averaged over 1–60s windows and 5 traffic shapes. The original 6×8 linear register scored 0.129.
- **Latency:**
  - hit, cold table: ~85 ns
  - batched: ~60 ns per key
  - hit, cache-resident: ~30 ns
  - steady-state insert: ~200 ns
  - VelocityCount: ~90-100 ns cold, ~45 ns cache-resident
  - string key with CoarseClock: ~115 ns
  - parallel hit: ~25 ns per op

## Known limits

- Two locked CASes per touch, one per word. This is the floor of the cache-resident hit cost.
- Inserts above ~94% slot load need cuckoo paths and cost µs. Two-choice, 4-slot tables saturate at ~97.7% load, and above that some keys are evicted (0.05% missed at 98%). Size `Capacity` and `Load` for your live-key count.
- First touches of a key take a striped lock (256 stripes) so racing writers can't create two slots. Hits stay lock-free.
- `HashUint64` is unkeyed. Use `Hash` (seeded) for attacker-controlled keys.
