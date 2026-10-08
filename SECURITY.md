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
| 4 | **Reset your own count by getting evicted.** Eviction used to drop a key's history, so it came back as new. | Returns with count 1, indistinguishable from a new key | Evicted keys go to a stash and come back with their history (`Restored`). A flood big enough to also overrun the stash with heavier keys still loses it, but the key then reads `Degraded` | A3, A6 |
| 5 | **Send faster than the register holds.** The newest bucket holds 31 events per unit (Horizon/24). Beyond that, events spill to older buckets (out of short windows) or are dropped. | 100 ev/s reads 79–87% low, silently | A sticky saturated bit marks the slot. `Saturated` is set exactly when an overflow can affect the window. Otherwise `Upper` ≥ the true count | A4, A5 |
| 6 | **Hide a burst at the window's edge.** `Count` assumes the edge bucket is uniform, so a burst just inside the edge reads low. | Undetectable | `Upper` counts the edge bucket whole | A4, A5 |
| 7 | **Hash-flood one region** to force relocations and evictions there. | Same inversion as 1 | Needs the key | (follows from 1) |

Keyed hashing (1–3, 7) holds only if the key stays secret. The default seed is per filter and never leaves the process. `Config.Key` is a 128-bit SipHash-2-4 key: a PRF, the same in every process that uses it, so it can be shared and rotated. The `*At` methods take a hash from the caller and can't check it; pass them only `Hash`/`HashUint64` values of the same filter.

## Using the trust flags

`Touch`, `TouchAt`, `Count` and `CountAt` return a `Result`. A fail-closed rate limit:

```go
r := f.Touch(key, window)
deny := r.Count >= limit ||
	r.Saturated ||                 // Count is only a floor
	strict && r.Upper >= limit ||  // the edge bucket could hold the rest
	strict && r.Degraded && !r.Seen // a dropped history may have been this key's
```

For dedup, treat `r.Seen || (strict && r.Degraded)` as a duplicate.

## What is left

- **Volume still wins, but not silently.** An attacker who keeps the table and its stash full of keys heavier than theirs, about (slots + stash) × (k+1) events per horizon, still erases a k-event history. `Lost()` counts every dropped history, and for a horizon after one, every unseen key reads `Degraded`. That is table-wide, so a strict caller then turns new keys away: integrity is kept at the cost of availability. Alert on `Lost()`.
- **Saturated keys can't be certified.** A key over 31 events in one unit reads `Saturated` until that unit leaves the register, so a strict caller can only enforce limits up to 31 per unit: 31 × 24 / Horizon events/s (12.4/s at a 60 s horizon, 74/s at 10 s). Pick the Horizon to fit the limit. Keeping all 48 counter bits was a deliberate choice (`results/backlog.md`, Decided). The saturated bit came out of the timestamp instead, which now lasts 2^46 ticks (~2.2 years at 1 µs).
- **Timing.** A hit reads one cache line, a miss two (plus the stash while it holds anything), and a first touch takes a lock. Someone who can time single requests precisely can tell seen from unseen. Not addressed.
- **Restarts** forget everything, and a new random seed changes every hash. `Config.Key` keeps hashes stable, but no state is persisted yet.
