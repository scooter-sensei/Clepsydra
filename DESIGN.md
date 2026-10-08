# Sliding-Window Probabilistic Register (Temporal Filter)

A fixed-memory filter answering **"seen key X in the last W?"** and **"how many times in the last W?"** for any `W ≤ Horizon`, chosen per query. Hits are lock-free (CAS on the slot's two words); a key's first touch takes one of 256 striped locks.

**Checking these claims.** Tags like [D1] point to `verify/claims.md`, which gives the command, the expected result and its status for every claim. `verify/isolated.sh` re-runs all deterministic checks in a fresh, offline container and compares them byte for byte with `verify/expected/`. Speed figures are from one host (4-vCPU Xeon @ 2.1 GHz, 260 MB L3) and are reported, not checked.

Measured history of every change is in `results/history.md`; ideas done, tried and queued are in `results/backlog.md`.

## Layout

The table is any number of 64-byte-aligned lines (multiply-shift indexing, no power-of-two rounding). Each line holds 4 slots of 2 words:

```
word0: fp(16) | displaced(1) | lastTouch(47)        freshness, exact to 1 tick (default 1µs: 2^47 µs ≈ 4.46 years)
word1: tag(12) | epoch(4) | 7 velocity buckets (48)  age-graded counter
```

- **Identity** = 16-bit `fp` + 12-bit `tag` = 28 bits, both in the same line. The primary line uses hash bits 0..35, disjoint from the identity bits.
- **Displaced bit**: set when an entry lives in its alternate line, flipped whenever it moves. Lookups match it, so a key is only compared with entries homed at its own primary line: 3.40 entries per absent-key lookup instead of 6.80 at 85% load [D11]. That halves the expected false-positive rate (1.27e-8 vs 2.53e-8); measured 18 vs 32 false positives in 1e9 probes [R3].
- **Sizing**: `Config.Load` (default 0.85) is the slot occupancy at `Capacity`: 18.8 B per key [D6].
- **Placement**: primary line first, then the alternate line `alt(l) = (x(fp) − l) mod lines`, which is its own inverse, so stored entries can move without their key. At 85% load, 18.9% of keys sit in their alternate line [D11].
  - If both lines are full, a **bounded cuckoo search** (iterative deepening, depth 3, so paths of up to 4 moves, budget 500 lines) moves an entry to its alternate line.
  - The search budget shrinks while recent searches fail (an EWMA), so an overfull table degrades to cheap eviction.
  - Eviction takes the **lowest-velocity** slot (oldest on ties), so a spray of fresh keys can't flush a repeat offender [D4, E1].
- **Velocity register** (`register.go`): 7 buckets that are 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide, where a unit is `Horizon/24`. Resolution is fine near now and coarse further back.
  - Bucket boundaries only coarsen as time passes, so advancing any distance is one re-bucketing pass with no per-tick work and no sweep.
  - Counter widths grow with bucket width (5,5,6,7,8,8,9 bits). A full bucket **spills** into older ones.
  - **Count limits** [D7]: the newest bucket holds 31 events per unit (2.5 s at a 60 s horizon). Overflow spills into older buckets, which shorter windows don't cover and which age out sooner, so heavy traffic is undercounted:
    - Steady traffic reads within 1% up to 12 events/s per key (15–60 s windows). At 13/s it reads 2–6% low, at 20/s 25–37% low, at 100/s 79–88% low.
    - A burst in one tick reads at most 43 in a 1 s window, 125 in 5 s, 380 in 15 s and 1,273 in 60 s, and as little as 14, 66, 192 and 770, depending on where it falls in the 8-unit bucket cycle.
    - Windows shorter than one unit are approximate at any rate: a 10-event burst reads as 5–10 in a 1 s window.
  - The 4-bit epoch is resolved against `lastTouch`'s unit.

## Operations

| Op | What it does | Memory |
|---|---|---|
| `CheckAndTouch(key, W) bool` | CAS `lastTouch` to now, returns `now − lastTouch ≤ W`, counts the event | 1 line on a primary hit |
| `CheckAndTouchBatch(hs, W, now, out)` | Same for many keys; issues every key's line load first so cache misses overlap | see speed table |
| `VelocityCount(key, W) int` | Reads the buckets as laid out at the register's own epoch (no re-bucketing), sums those inside W and interpolates the one W's edge cuts, assuming uniform density | 1 line |
| `TouchAndCount(key, W)` | Both in one pass | 1 line |
| `TouchAndCountBatch`, `VelocityCountBatch` | Batched like CheckAndTouchBatch | see speed table |
| `ContainsAt(h, W, now)` | Read-only freshness check | 1 line |
| `ContinuousDecay()` | Bounded slot reclaimer, paced by the clock (one table pass per horizon, ≤ 4 lines per call; inline on 1/64 of touches, 1 line) | — |

`*At(hash, windowTicks, nowTicks)` variants take a pre-hashed key and a caller-cached clock.

## Why it avoids the classic problems

- **No boundary artifacts in freshness.** `lastTouch` is exact, so `W` is checked to the tick (`TestExactBoundary`).
- **Bounded reclaim work.** Expiry is arithmetic on every access, so answers never depend on the reclaimer. The reclaimer frees at most 4 lines per call (1 when run inline) — a property of the code (`decayStep`), not a measured latency distribution.
- **Velocity is gated by exact facts** (`estimate` in `register.go`).
  - The count is 0 once `W < now − lastTouch`, and at least 1 otherwise.
  - The newest non-empty bucket's events end at `lastTouch`.

## Current numbers

**Deterministic** (checked by `verify/run.sh`; capacity 200k, horizon 60s, `cmd/tfbench`):

- **Missed keys** [D1]: 0 at 85% and 94% slot load; 58 of 113,040 (0.051%) at 1.15x Capacity, where slot load peaks at 97.8%.
- **Why there are misses at 97.8%** [D8, D10]: not the layout. The load threshold for 2 choices × 4 slots is 98.04%, and an exact placement oracle on the same stream places every key (0 forced drops). The filter's 4,575 evictions come from its bounded search: the oracle's shortest paths needed 5 moves for 3,406 inserts, past the filter's 4. Depth 4 / budget 1000 (commit 9f2529f) missed 12 [R3].
- **False positives** [D2, D3]: 0 of 723,513 negatives on the churn workload; 18 in 1e9 never-inserted probes at capacity (1.8e-8; Poisson 95% interval 1.07e-8–2.84e-8; model 1.27e-8).
- **Memory** [D6]: 18.8 B per key.
- **Velocity error** [D5]: 0.081, averaged over 1–60s windows and 5 traffic shapes. The original 6×8 linear register scored 0.129 on the same harness [R3].

**Speed** (one host; `verify/speed.md` has the runs, `verify/ab.sh` repeats them; compare ratios, not absolute numbers): see `verify/speed.md`.

## Known limits

- Two CASes per touch, one per word (a single 16-byte CAS was slower on the reference host [E3]).
- Above ~94% slot load, inserts need cuckoo paths and the bounded search starts evicting (above). A deeper search would cut those misses at some insert cost; it hasn't been re-measured.
- Per-key counts undercount above ~12 events/s sustained, and bursts beyond the newest bucket's 31 events read low in short windows [D7]. Wider low buckets would raise this at the cost of identity or timestamp bits.
- First touches of a key take a striped lock (256 stripes) so racing writers can't create two slots [R2]. Hits stay lock-free.
- `HashUint64` is unkeyed. Use `Hash` (seeded) for attacker-controlled keys.
