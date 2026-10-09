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
| 4 | **Reset your own count by getting evicted.** Eviction used to drop a key's history, so it came back as new. | Returns with count 1, indistinguishable from a new key | Evicted keys go to a stash and come back with their history (`Restored`). A history the stash has to drop goes to the overflow table (row 8) | A3, A6 |
| 5 | **Send faster than the register holds.** The newest bucket holds 31 events per unit (Horizon/24). Beyond that, events spill to older buckets (out of short windows) or are dropped. | 100 ev/s reads 79–87% low, silently | A sticky saturated bit marks the slot. `Saturated` is set exactly when an overflow can affect the window. Otherwise `Upper` ≥ the true count | A4, A5 |
| 6 | **Hide a burst at the window's edge.** `Count` assumes the edge bucket is uniform, so a burst just inside the edge reads low. | Undetectable | `Upper` counts the edge bucket whole | A4, A5 |
| 7 | **Hash-flood one region** to force relocations and evictions there. | Same inversion as 1 | Needs the key | (follows from 1) |
| 8 | **Flood the table and the stash** with keys heavier than yours: about (slots + stash) × (k+1) events per horizon. | The k-event history is erased (`Lost`), and for a horizon every key the filter doesn't hold reads `Degraded`, so a strict caller turns away all new keys | Dropped histories go to an overflow ghost table: one 21-bit entry per history (fingerprint, count, Horizon/2 slice), never erased within the horizon. A key's answer adds only its own entry (and sets `Degraded`), so other keys' histories add nothing unless a 14-bit fingerprint matches. When a bucket overflows, its lightest history leaves and the bucket's floor rises to that count, which every key in the bucket adds to `Upper`: `Upper` is never below the true count. 200 offenders × 20 events: 0 lost and 0 read `Upper` below the truth under floods of 1x–16x the table's slots, and every offender reads its full `Count` after a 1x flood, 195 after 2x. Histories under 4 events are shed (`SpillMin`), so floods of cheap keys can't fill it. A strict limit of 20 turns away 0% of new keys after a 1x heavy flood or a 16x flood of one-event keys, 1.8% after 2x (the count-min sketch before it: 88–90%), 89% at 3x and 100% at 4x (*What is left*) | A3, A7, A11 |
| 9 | **Time a request** to learn whether a key was seen (a hit reads one line, a miss two plus the stash, and a first touch inserts). | One `TouchAt` timing guesses seen vs unseen right 85–89% of the time, one `CountAt` 57–64% (table larger than the cache) | `Config.ConstantTime`: the same lookups whatever the key's state, and every call padded. `Touch` waits out ConstantTime (3 µs: 51%). `Count`, whose paths differ by tens of ns rather than an insert search, waits out a running estimate of its own 99.5th-percentile latency (~0.7–1 µs here): 50–51% at a third of the cost | S28, S33 |

Keyed hashing (1–3, 7) holds only if the key stays secret. The default seed is per filter and never leaves the process. `Config.Key` is a 128-bit SipHash-2-4 key: a PRF, the same in every process that uses it, so it can be shared and rotated. The `*At` methods take a hash from the caller and can't check it; pass them only `Hash`/`HashUint64` values of the same filter.

## Using the trust flags

`Touch`, `TouchAt`, `Count` and `CountAt` return a `Result`. A fail-closed rate limit:

```go
r := f.Touch(key, window)
deny := r.Count >= limit ||
	r.Saturated ||               // Count is only a floor
	strict && r.Upper >= limit   // the edge bucket, or a dropped history in the overflow table, could hold the rest
```

`Degraded` means the key likely has a history the stash dropped: its count partly comes from the overflow table and, about 0.1% of the time while that table is full, from another key whose fingerprint matches. For dedup, treat `r.Seen || (strict && r.Degraded)` as a duplicate. Histories under `SpillMin` (4) events are shed when the stash drops them, so a key seen 1–3 times can read new again after a flood; `SpillMin: 1` keeps them too (dedup then survives floods until the overflow table itself overflows, and inserts into a full table cost ~30% more, S35). With the overflow off (`Overflow < 0`), `Degraded` keeps its old, table-wide meaning: some history was lost within a horizon and this key isn't held; strict callers then add `strict && r.Degraded && !r.Seen` to `deny`.

## What is left

- **Volume past the overflow table.** A flood can no longer erase a history, and up to about 2x the table's slots it no longer costs other keys anything: the ghost table (`Config.Overflow`, default one unit per 4 slots, 4 B per slot once allocated) holds 1.5 histories per slot. Past that its buckets overflow, their floors rise to the flood's weight, and `Upper` covers every key in them: at a 3x flood of 21-event keys a strict limit of 20 turns away 89% of new keys, at 4x 100%, and the offenders' `Count` is gone (their entries were the lightest, so they left first; `Upper` still covers them). `Overflow: slots/2` (9.4 B per slot under attack) moves the point to ~3x (2.5% at 3x, 44% at 4x). Alert on `Folded()`.
- **Saturated keys can't be certified.** A key over 31 events in one unit reads `Saturated` until that unit leaves the register, so a strict caller can only enforce limits up to 31 per unit: 31 × 24 / Horizon events/s (12.4/s at a 60 s horizon, 74/s at 10 s). Pick the Horizon to fit the limit. Keeping all 48 counter bits was a deliberate choice (`results/backlog.md`, Decided). The saturated bit came out of the timestamp instead, which now lasts 2^46 ticks (~2.2 years at 1 µs).
- **Fingerprint matches.** While the overflow table is full, about 0.1–0.2% of keys that never spilled match some entry's 14-bit fingerprint and read that entry's count, flagged `Degraded`. The count-min sketch it replaced flagged 0% (it hid matches under its noise threshold, and with them most real dropped histories).
- **Shed light histories.** A key with fewer than `SpillMin` (4) events in the horizon loses them if it is evicted and the stash drops it (`Shed()` counts these). Getting there still takes about a table's worth of heavier keys per eviction. `SpillMin: 1` keeps everything at ~30% more cost per insert into a full table (A10, S35).
- **Timing.** Off by default: a hit reads one line, a miss two (plus the stash and overflow table while they hold anything), and a first touch inserts. `ConstantTime` closes most of it at a price: every call spins at least its pad. `Touch` needs ~3 µs (~0.3 M ops/s per core), because inserts that need a cuckoo search or an eviction run past anything shorter (unseen `Touch` p99 2.6–3 µs vs 0.8 µs seen), and those still leak. `Count`'s tracked pad settles near 0.7–1 µs. `AutoPad` tracks `Touch`'s pad too, which settles at 3–5 µs: no cheaper than a fixed 3 µs, but no number to pick per host.
- **Restarts** forget everything, and a new random seed changes every hash. `Config.Key` keeps hashes stable, but no state is persisted yet.
