# Threat model

Who attacks the filter: the client of a rate limiter, abuse check or dedup that the filter backs. They choose keys (IPs they control, accounts and cards they test, request ids) and when to send. They see the caller's decisions (allowed or blocked) and how long each one took. They don't see memory or the hash key. Running at volume costs them something: requests, accounts or IPs.

What they want, in order of how much it hurts the caller:

1. **Undercount themselves.** Read below a limit while going over it, or read as new while repeating.
2. **Frame someone else.** Push another key's count up, or make it read as already seen.
3. **Slow everyone down.** Pile work onto one lock or one region of the table.

Tags like [A1] point to `verify/claims.md`.

## Attacks, fixes and what is left

| # | Attack | Before | Now | Check |
|---|---|---|---|---|
| 1 | **Forge a victim's identity.** `HashUint64` was the SplitMix64 finalizer, a public bijection. Invert it to get ids whose hashes differ from the victim's only in bits 0–3: same line, same 28-bit identity. | 4 forged ids × 25 events: the never-touched victim reads 100 and "seen" | Hashing is keyed (`Config.Key`, else a random seed). The same ids read 0 | A1 |
| 2 | **Evict a victim from its own lines.** Forge keys with the victim's line and fingerprint but other tags, each one event heavier than the victim. | 168 events evict a 20-event key | Keyed, keys land at random: 102,753 events on a 4,818-slot table (612×). Cost grows with the table: about slots × (k+1) events within one horizon | A3 |
| 3 | **Flood one lock stripe.** Pick ids whose hashes share the top 8 bits, so every first touch queues on one of 256 locks. | 4,096 crafted ids, 1 stripe | Spread over all 256 | A2 |
| 4 | **Reset your own count by getting evicted.** Eviction used to drop a key's history, so it came back as new. | Returns with count 1, indistinguishable from a new key | Evicted keys go to a stash and come back with their history (`Restored`). A history the stash has to drop goes to the overflow sketch (row 8) | A3, A6 |
| 5 | **Send faster than the register holds.** The newest bucket holds 31 events per unit (Horizon/24). Beyond that, events spill to older buckets (out of short windows) or are dropped. | 100 ev/s reads 79–87% low, silently | A sticky saturated bit marks the slot. `Saturated` is set exactly when an overflow can affect the window. Otherwise `Upper` ≥ the true count | A4, A5 |
| 6 | **Hide a burst at the window's edge.** `Count` assumes the edge bucket is uniform, so a burst just inside the edge reads low. | Undetectable | `Upper` counts the edge bucket whole | A4, A5 |
| 7 | **Hash-flood one region** to force relocations and evictions there. | Same inversion as 1 | Needs the key | (follows from 1) |
| 8 | **Flood the table and the stash** with keys heavier than yours: about (slots + stash) × (k+1) events per horizon. | The k-event history is erased (`Lost`), and for a horizon every key the filter doesn't hold reads `Degraded`, so a strict caller turns away all new keys | Dropped histories go to an overflow count-min sketch over (key, Horizon/4 slice), never erased within the horizon: `Upper` is never below the true count. `Count` adds the sketch's estimate only where it stands out from the noise of other keys' events, and then sets `Degraded`, per key. 200 offenders × 20 events under floods of 1x–16x the table's slots: 0 lost, 0 read `Upper` below the truth, 0% of new keys `Degraded`. A strict limit of 20 turns away 0.1% of new keys at 1x (was 100%), 100% from 4x on. `Count` is not protected at that scale (*What is left*) | A3, A7 |
| 9 | **Time a request** to learn whether a key was seen (a hit reads one line, a miss two plus the stash, and a first touch inserts). | One `TouchAt` timing guesses seen vs unseen right 85–89% of the time, one `CountAt` 57–64% (table larger than the cache) | `Config.ConstantTime`: the same lookups whatever the key's state, and every call padded to a fixed duration. With 3 µs: 51% for both | S28 |

Keyed hashing (1–3, 7) holds only if the key stays secret. The default seed is per filter and never leaves the process. `Config.Key` is a 128-bit SipHash-2-4 key: a PRF, the same in every process that uses it, so it can be shared and rotated. The `*At` methods take a hash from the caller and can't check it; pass them only `Hash`/`HashUint64` values of the same filter.

## Using the trust flags

`Touch`, `TouchAt`, `Count` and `CountAt` return a `Result`. A fail-closed rate limit:

```go
r := f.Touch(key, window)
deny := r.Count >= limit ||
	r.Saturated ||               // Count is only a floor
	strict && r.Upper >= limit   // the edge bucket, or a dropped history in the overflow sketch, could hold the rest
```

`Degraded` means the key likely has a history the stash dropped: its count partly comes from the overflow sketch and may include other keys' events. It fires only when that history stands out from the sketch's noise, so in a big flood a dropped history shows only in `Upper`. For dedup, treat `r.Seen || (strict && r.Degraded)` as a duplicate; strict dedup through a big flood needs `Upper` too and then rejects most new keys. With the sketch off (`Overflow < 0`), `Degraded` keeps its old, table-wide meaning: some history was lost within a horizon and this key isn't held; strict callers then add `strict && r.Degraded && !r.Seen` to `deny`.

## What is left

- **Volume buys noise.** A flood can no longer erase a history, but every history it pushes out of the stash adds to the overflow sketch, whose noise per counter is about the dropped events in a Horizon/4 slice over its width (`Config.Overflow`, default one per 8 slots). `Count` recovers a dropped history only when it stands out by 3 standard deviations, which a 20-event history doesn't against a flood of 21-event keys: of 200 offenders, the 49 dropped at a 1x-slots flood and all 200 at 4x read `Count` 0, not `Degraded`. So `Count` and `Degraded` cover small overflows; against a big flood only `Upper` holds, and a fail-closed caller must use it. `Upper` always covers them, so strict callers stay safe, but from about 4x it covers everyone (100% collateral at a limit of 20). A wider sketch moves that point linearly; alert on `Folded()`.
- **Saturated keys can't be certified.** A key over 31 events in one unit reads `Saturated` until that unit leaves the register, so a strict caller can only enforce limits up to 31 per unit: 31 × 24 / Horizon events/s (12.4/s at a 60 s horizon, 74/s at 10 s). Pick the Horizon to fit the limit. Keeping all 48 counter bits was a deliberate choice (`results/backlog.md`, Decided). The saturated bit came out of the timestamp instead, which now lasts 2^46 ticks (~2.2 years at 1 µs).
- **Timing.** Off by default: a hit reads one line, a miss two (plus the stash and sketch while they hold anything), and a first touch inserts. `ConstantTime` closes most of it at a price: every call takes at least the pad (3 µs measured: ~0.3 M ops/s per core, spinning), and a call that runs past it still leaks. Inserts that need a cuckoo search or an eviction do (unseen `Touch` p99 4.1–4.5 µs vs 3.4 µs seen), which leaves the 51%. A larger pad shrinks that.
- **Restarts** forget everything, and a new random seed changes every hash. `Config.Key` keeps hashes stable, but no state is persisted yet.
