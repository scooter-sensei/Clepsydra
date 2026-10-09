# Sliding-Window Probabilistic Register (Temporal Filter)

A fixed-memory filter answering **"seen key X in the last W?"** and **"how many times in the last W?"** for any `W ≤ Horizon`, chosen per query. The window is `[now − W, now]`: an event exactly `W` ago is inside it. Hits are lock-free (CAS on the slot's two words); a key's first touch takes one of 256 striped locks.

**Checking these claims.** Tags like [D1] point to `go/verify/claims.md`, which gives the command, the expected result and its status for every claim. `go/verify/isolated.sh` re-runs all deterministic checks in a fresh, offline container and compares them byte for byte with `go/verify/expected/`. Speed figures are from one host (4-vCPU Xeon @ 2.1 GHz, 260 MB L3) and are reported, not checked.

Measured history of every change is in `go/results/history.md`; ideas done, tried and queued are in `go/results/backlog.md`.

## Layout

The table is any number of 64-byte-aligned lines (multiply-shift indexing, no power-of-two rounding). Each line holds 4 slots of 2 words:

```
word0: fp(16) | displaced(1) | saturated(1) | lastTouch(46)   freshness, exact to 1 tick (default 1µs: 2^46 µs ≈ 2.23 years)
word1: tag(12) | epoch(4) | 7 velocity buckets (48)  age-graded counter
```

- **Identity** = 16-bit `fp` + 12-bit `tag` = 28 bits, both in the same line (one tag value is reserved for slots changing hands). The primary line uses hash bits 0..35, disjoint from the identity bits.
- **Displaced bit**: set when an entry lives in its alternate line, flipped whenever it moves. Lookups match it, so a key is only compared with entries homed at its own primary line: 3.40 entries per absent-key lookup instead of 6.80 at 85% load [D11]. That halves the expected false-positive rate (1.27e-8 vs 2.53e-8); measured 18 vs 32 false positives in 1e9 probes [R3].
- **Saturated bit**: set when an event overflows its velocity bucket (spills to an older one or is dropped), cleared once no bucket holds 31 or more. See *Trust flags*.
- **Sizing**: `Config.Load` (default 0.85) is the slot occupancy at `Capacity`: 18.8 B per key [D6], plus 3.5 B for the eviction stash and, once a history has been dropped, 4.7 B for the overflow table.
- **Placement**: primary line first, then the alternate line `alt(l) = (x(fp) − l) mod lines`, which is its own inverse, so stored entries can move without their key. At 85% load, 18.9% of keys sit in their alternate line [D11].
  - If both lines are full, a **bounded cuckoo search** (iterative deepening, depth 3, so paths of up to 4 moves, budget 500 lines) moves an entry to its alternate line.
  - The search budget shrinks while recent searches fail (an EWMA), so an overfull table degrades to cheap eviction.
  - Eviction takes the **lowest-velocity** slot (oldest on ties), so a spray of fresh keys can't flush a repeat offender [D4, E1].
  - The evicted entry goes to the **stash**: 4-way sets, one entry per 8 slots by default (`Config.Stash`), keyed by the entry's full identity. A first touch that misses the table looks there while it may hold a live entry, and takes the history back. A full set keeps the entries with more events. The victim is stashed under its own claim stripe, so its next first touch finds it in the table or in the stash, never in between [A6].
  - A history the stash drops goes to the **overflow ghost table** (`go/ghost.go`): one 21-bit entry per history (14-bit fingerprint, 5-bit event count saturating at 31, 2-bit Horizon/2 slice of its newest event), three to a word, 23 entries and a floor per 64-byte bucket, one unit of 16 B per 4 slots (`Config.Overflow`, 1.5 entries per slot), allocated on the first drop [A11]. A second drop of the same key merges into its entry. A full bucket keeps its heaviest histories: the lightest (the newcomer included) leaves, and the bucket's floor rises to its count. A key's answer adds its own entries to `Count` and `Upper` and the floor to `Upper` alone, so `Upper` never reads below the truth (one exception: a key floored more than once in a flood, docs/SECURITY.md) and other keys' histories add nothing unless a fingerprint matches. All of a history's events count while its newest one is in the window: `Upper` stays a bound, `Count` reads high for windows shorter than the horizon. Before an add moves to a new slice, entries dead by then are cleared (the whole table, once per Horizon/2, like the sketch's ring), so the 2-bit slices decode unambiguously. Reads take one cache line. Histories with fewer than 4 events (`Config.SpillMin`) are shed instead: cheap floods can't fill the table, and inserts into a full table stay as cheap as with the sketch [A10, S35]. It replaced a count-min sketch of the same bytes (4 rows × 4 slices of 8-bit counters, count-mean-min with a 3σ noise threshold), still in `go/overflow.go` for the experiments: under a 2x heavy flood the sketch's noise reached every key [A9, A11]. A returning key starts a fresh slot, so table and overflow never count the same event, and overflow answers can't make a key look heavier for eviction [A7].
- **Velocity register** (`go/register.go`): 7 buckets that are 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide, where a unit is `Horizon/24`. Resolution is fine near now and coarse further back.
  - Bucket boundaries only coarsen as time passes, so advancing any distance is one re-bucketing pass with no per-tick work and no sweep.
  - Counter widths grow with bucket width (5,5,6,7,8,8,9 bits). A full bucket **spills** into older ones.
  - **Count limits** [D7]: the newest bucket holds 31 events per unit (2.5 s at a 60 s horizon). Overflow spills into older buckets, which shorter windows don't cover and which age out sooner, so heavy traffic is undercounted:
    - Steady traffic reads within 1% up to 12 events/s per key (15–60 s windows; within 2% at 5 s). At 13/s it reads 3–5% low, at 20/s 25–37% low, at 100/s 79–87% low.
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
| `Touch(key, W) Result`, `Count(key, W) Result` | TouchAndCount / VelocityCount plus the trust flags below | 1 line |
| `ContinuousDecay()` | Bounded slot reclaimer, paced by the clock (one table pass per horizon, ≤ 4 lines per call; inline on 1/64 of touches, 1 line) | — |

`*At(hash, windowTicks, nowTicks)` variants take a pre-hashed key and a caller-cached clock. `Hash` and `HashUint64` are keyed: a random per-filter seed, or SipHash-2-4 under `Config.Key` (SECURITY.md).

## Trust flags

`Result` says how far an answer can be trusted, so a rate limiter or abuse check can fail closed (SECURITY.md has the recipe):

- **`Upper`** counts the bucket the window's edge cuts as wholly inside, so it is never below the true count unless `Saturated` [A4, A5].
- **`Saturated`**: the slot's saturated bit is set and a bucket the window reaches holds at least 31 events. Why that is exact: an overflowing bucket holds at least 31 and still covers the overflowing event's time, and bucket counts only grow until they age out. `Count` and `Upper` are then floors.
- **`Restored`**: the key came back from the stash.
- **`Degraded`**: part of the count comes from the overflow table: the key likely had a history the stash dropped (`Folded()` counts them), and the count may be another key's whose fingerprint matches (~0.1% of keys while the table is full) [A7, A11]. With the overflow off, the old table-wide meaning: the key wasn't found and some history was lost within the last horizon (`Lost()`).

**`ConstantTime`** (off by default) makes every call do the same lookups whatever the key's state (both lines, the stash set, the overflow bucket, under the key's stripe) and pads it, so its time doesn't say whether the key was seen [S28]. `Touch` waits out the configured pad. `Count`'s two paths differ by tens of nanoseconds, not an insert search, so it waits out a running estimate of its own 99.5th-percentile latency (one sampled call in 4 moves it up 100 ns if it ran past, down 0.5 ns if not), capped at the pad; `AutoPad` estimates `Touch`'s pad the same way [S33, S34].

## Why it avoids the classic problems

- **No boundary artifacts in freshness.** `lastTouch` is exact, so `W` is checked to the tick (`TestExactBoundary`).
- **Bounded reclaim work.** Expiry is arithmetic on every access, so answers never depend on the reclaimer. The reclaimer frees at most 4 lines per call (1 when run inline) — a property of the code (`decayStep`), not a measured latency distribution.
- **Velocity is gated by exact facts** (`estimate` in `go/register.go`).
  - The count is 0 once `W < now − lastTouch`, and at least 1 otherwise.
  - The newest non-empty bucket's events end at `lastTouch`.

## Current numbers

**Deterministic** (checked by `go/verify/run.sh`; capacity 200k, horizon 60s, `go/cmd/tfbench`):

- **Missed keys** [D1]: 0 at 85%, 94% and 97.8% slot load (1.15x Capacity). Before the stash, 58 of 113,040 (0.051%) at 97.8%.
- **Evictions at 97.8%** [D8, D10]: not the layout. The load threshold for 2 choices × 4 slots is 98.04%, and an exact placement oracle on the same stream places every key (0 forced drops). The filter's ~4,575 evictions come from its bounded search: the oracle's shortest paths needed 5 moves for 3,406 inserts, past the filter's 4. The stash now gives every evicted key that returns its history back.
- **False positives** [D2, D3]: 0 of 723,513 negatives on the churn workload; 18 in 1e9 never-inserted probes at capacity (1.8e-8; Poisson 95% interval 1.07e-8–2.84e-8; model 1.27e-8).
- **Memory** [D6]: 18.8 B per key.
- **Velocity error** [D5]: 0.081, averaged over 1–60s windows and 5 traffic shapes. The original 6×8 linear register scored 0.129 on the same harness [R3].

**Speed** (one host; `go/verify/speed.md` has the runs, `go/verify/ab.sh` repeats them; compare ratios, not absolute numbers): see `go/verify/speed.md`.

## Known limits

- Two CASes per touch, one per word (a single 16-byte CAS was slower on the reference host [E3]).
- Above ~94% slot load, inserts need cuckoo paths and the bounded search starts evicting (above). A deeper search would cut those misses at some insert cost; it hasn't been re-measured.
- Per-key counts undercount above ~12 events/s sustained, and bursts beyond the newest bucket's 31 events read low in short windows [D7]. Wider low buckets would raise this at the cost of identity or timestamp bits.
- First touches of a key take a striped lock (256 stripes) so racing writers can't create two slots [R2]. Hits stay lock-free.
- **Counts under concurrency** [R2, D14]. A slot changes hands with word1 set to a reserved "busy" tag before word0 changes, and a touch re-reads word0 after word1 before its CAS, so a touch never counts into another key's register unless the two share all 28 identity bits. A move abandoned because a touch reached the source first adds the copy's counts back to the entry.
- Strict callers can certify limits only up to 31 events per unit (`Saturated`). A flood no longer erases histories, but from about 3x the table's slots the overflow table's buckets overflow: their floors cover every key's `Upper`, and the offenders' `Count` is lost [A11]. Without `ConstantTime`, timing reveals seen vs unseen. See SECURITY.md.
