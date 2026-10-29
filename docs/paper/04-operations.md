# 4. Operations and guarantees

## 4.1 Operations

| Op | What it does | Memory |
|---|---|---|
| `CheckAndTouch(key, W) bool` | CAS `lastTouch` to now, returns `now − lastTouch ≤ W`, counts the event | 1 line on a primary hit |
| `CheckAndTouchBatch(hs, W, now, out)` | Same for many keys, in order, 32 at a time: prefetches every key's primary line so the misses overlap, looks each key up there, prefetches the alternate lines of the keys it didn't find, then touches each key starting from the slot it found | see [7 Evaluation](07-evaluation.md#batches) |
| `VelocityCount(key, W) int` | Reads the buckets as laid out at the register's own epoch (no re-bucketing), sums those inside W and interpolates the one W's edge cuts, assuming uniform density; in the newest bucket the event at lastTouch counts whole and only the others are spread | 1 line |
| `TouchAndCount(key, W)` | Both in one pass | 1 line |
| `TouchAndCountBatch`, `VelocityCountBatch` | Batched like CheckAndTouchBatch (empty batches are allowed) | see [7 Evaluation](07-evaluation.md#batches) |
| `ContainsAt(h, W, now)` | Read-only freshness check | 1 line |
| `Touch(key, W) Result`, `Count(key, W) Result` | TouchAndCount / VelocityCount plus the trust flags below | 1 line |
| `ContinuousDecay()` | Bounded slot reclaimer, paced by the clock (one table pass per horizon, ≤ 4 lines per call; inline on 1/64 of touches, 1 line) | — |

`*At(hash, windowTicks, nowTicks)` variants take a pre-hashed key and a caller-cached clock (`TouchAt`, `CountAt`, `CheckAndTouchAt`, `TouchAndCountAt`, `VelocityCountAt`). The `*At` methods take a hash from the caller and can't check it: pass them only `Hash`/`HashUint64` values of the same filter.

### Ticks and the 46-bit range

Ticks count from the filter's own start, not an epoch: a slot keeps 46 bits of them (`MaxTick` = 2^46−1: 2.2 years of the built-in clock at 1 µs, 19.5 hours at 1 ns). A later tick ran into the slot's flag and fingerprint bits: a caller passing Unix microseconds got every key back unseen, a rate limit failing open. `New` rejects a `Clock` already past it and a horizon of `MaxTick` ticks, and an insert, a realigning touch and a sweeping decay step panic past it, before taking any lock, so a caller that recovers keeps a usable filter; hits and counts don't check, so their paths stay as measured, and a clock that passes `MaxTick` after its keys went in reads them wrong until the next insert panics [A35]. `Ticks` clamps negative durations to 0: a negative window used to wrap to a huge one and report every key as seen.

### Hashing

`Hash` and `HashUint64` are keyed: a random per-filter seed, or SipHash-2-4 under `Config.Key` ([6 Security](06-security.md)). Keyed hashing costs +3 ns per integer key with the seed and +15 ns with SipHash [S26].

### Counters for operators

`Evictions`, `Relocations`, `Restored`, `Folded`, `Shed`, `Floored`, `Lost`, `Promoted`, `HotRefused`, `Demoted`, `HotInUse`, `Waited`, `Waiting`, `Bytes`; also `Occupancy(now)` and `Slots()`.

### Configuration

| Field | Default | Meaning |
|---|---|---|
| `Capacity` | — | expected number of distinct keys live within `Horizon` |
| `Horizon` | — | the largest window any query may use |
| `Resolution` | 1 µs | the clock tick |
| `Load` | 0.85 | target slot occupancy when `Capacity` keys are live; lower values trade memory for headroom above `Capacity` |
| `Clock` | monotonic time since `New` | returns the current tick; must be monotonic and stay at most `MaxTick` |
| `CoarseClock` | off | the built-in clock becomes a cached value refreshed by one goroutine: an atomic load instead of a clock read (~1.4 vs 30–42 ns [S1]), accurate to `CoarseClock`; `Close` stops it |
| `Key` | a random maphash seed | a 128-bit SipHash-2-4 key: a PRF, the same in every process that uses it |
| `Stash` | one 24 B entry per 16 slots | eviction stash entries; negative: none |
| `Overflow` | 5 units of 16 B per 9 slots | overflow table size; negative: none (dropped histories are then lost) |
| `SpillMin` | 4 | dropped histories with fewer events are shed; 1 or less keeps every history |
| `Hot` | one 64 B entry per 128 slots, at least 16 | hot entries; negative: none |
| `ConstantTime` | off | a pad duration, or `AutoPad` ([3 Design](03-design.md#38-constant-time-mode)) |
| `Background` | off | cuckoo searches off the call path ([3 Design](03-design.md#34-background-placement)) |

## 4.2 Trust flags

`Result` says how far an answer can be trusted, so a rate limiter or abuse check can fail closed:

- **`Seen`**: the key was touched within the window (before this touch). **`Count`** estimates the key's events within the window (including this touch, for `Touch`); it interpolates the one bucket the window's edge cuts, so it can read a little high or low.
- **`Upper`** counts the bucket the window's edge cuts as wholly inside, and the overflow table's floors the key reads, so it is never below the true count unless `Saturated` [A4, A5], with one exception: a key that left more than one history in the same overflow floor can read low by all but its largest ([6 Security](06-security.md#64-what-is-left), "Repeatedly floored keys") [A12]. This needs the overflow table (the default): with `Overflow < 0` a history the stash drops is lost, and a key that comes back afterwards reads low without a flag until the lost events leave the window; `Lost()` is then the only signal [A18, X32].
- **`Saturated`**: the slot's saturated bit is set and a bucket the window reaches holds at least 31 events. Why that is exact: an overflowing bucket holds at least 31 and still covers the overflowing event's time, and bucket counts only grow until they age out. For a hot entry: windows reaching back to the unit it was promoted in, if the register it came from had overflowed. `Count` and `Upper` are then floors.
- **`Restored`**: the key came back from the stash.
- **`Degraded`**: part of the count comes from the overflow table: the key likely had a history the stash dropped (`Folded()` counts them), and the count may be another key's whose fingerprint matches (~0.3% of keys while the table is full) [A7, A14]. With the overflow off, the old table-wide meaning: the key wasn't found and some history was lost within the last horizon (`Lost()`).

The contract (not `Saturated` ⇒ `Upper` ≥ exact) held with 0 exceptions in 261,538 attack and random queries when the flags were introduced; a density heuristic tried first missed 689 of 187,219 [A4, A5]. Today it is fuzzed over 1e10 operations, soaked under concurrency and model-checked ([5 Concurrency](05-concurrency.md), [artifact/reproducing.md](../artifact/reproducing.md)) [A18, A22, A36].

### Using the trust flags

`Touch`, `TouchAt`, `Count` and `CountAt` return a `Result`. A fail-closed rate limit:

```go
r := f.Touch(key, window)
deny := r.Count >= limit ||
	r.Saturated ||               // Count is only a floor
	strict && r.Upper >= limit   // the edge bucket, or a dropped history in the overflow table, could hold the rest
```

`Degraded` means the key likely has a history the stash dropped: its count partly comes from the overflow table and, about 0.3% of the time while that table is full, from another key whose fingerprint matches. For dedup, treat `r.Seen || (strict && r.Degraded)` as a duplicate. Histories under `SpillMin` (4) events are shed when the stash drops them, so a key seen 1–3 times can read new again after a flood; `SpillMin: 1` keeps them too (dedup then survives floods until the overflow table itself overflows, and inserts into a full table cost ~30% more, S35). With the overflow off (`Overflow < 0`), `Degraded` keeps its old, table-wide meaning: some history was lost within a horizon and this key isn't held; strict callers then add `strict && r.Degraded && !r.Seen` to `deny`. A key whose history was lost and that has come back is held again, so it reads low without `Degraded` until the lost events leave the window: with the overflow off, `Upper` is a bound only while `Lost()` hasn't moved within a horizon (found by the fuzzer, A18).

## 4.3 Guarantees by construction

- **No boundary artifacts in freshness.** `lastTouch` is exact, so `W` is checked to the tick (`TestExactBoundary`).
- **Bounded reclaim work.** Expiry is arithmetic on every access, so answers never depend on the reclaimer. The reclaimer frees at most 4 lines per call (1 when run inline): a property of the code (`decayStep`), not a measured latency distribution.
- **Velocity is gated by exact facts** (`estimate` in `go/register.go`): the count is 0 once `W < now − lastTouch`, and at least 1 otherwise; the newest non-empty bucket's events end at `lastTouch`.
- **Cheap repeated hits.** Hits within one tick skip the word0 CAS, and a full register skips the word1 CAS [D15].
- **No double counting between tiers.** A returning key starts a fresh slot, so table and overflow never count the same event, and overflow answers can't make a key look heavier for eviction [A7].
