# 1. Introduction

## 1.1 The problem

Rate limiters, abuse and fraud checks, and deduplication all ask the same two questions about a stream of keyed events (IPs, accounts, cards, request ids):

- **Freshness:** was key X seen in the last W?
- **Velocity:** how many times was key X seen in the last W?

The useful window differs by rule ("5 cards in 10 s", "1,000 requests a minute"), so it should be a query argument, not a construction parameter. The structure must fit in fixed memory, answer in about one memory access, stay correct under concurrent callers, and hold up against the very clients it is limiting: they choose keys and timing, and they gain by reading low.

## 1.2 Query model

- A filter is built with a **capacity** (the expected number of distinct keys live within the horizon) and a **horizon** (the largest window any query may use).
- Time is counted in **ticks** (default 1 µs) from the filter's own start.
- A query names a key and a window `W ≤ Horizon`. The window is `[now − W, now]`: an event exactly `W` ago is inside it.
- `CheckAndTouch(key, W)` records an event and says whether the key was seen within `W` before it. `VelocityCount(key, W)` estimates the key's events within `W`. `Touch` and `Count` return both with trust flags ([4 Operations and guarantees](04-operations.md)).

## 1.3 The approach

The table is any number of 64-byte lines, each holding 4 slots of two 64-bit words ([3 Design](03-design.md)):

- **word0** holds a 16-bit fingerprint, two flag bits and the key's **exact** last-touch time (46 bits).
- **word1** holds a 12-bit tag, a 4-bit epoch and the **velocity register**: 7 buckets that are 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide (a unit is `Horizon/24`), so resolution is fine near now and coarse further back.

Because the exact timestamp and the counter of a key share one line, a hit reads one line and updates it with a CAS per word. A key lives in one of two candidate lines (a bounded cuckoo search moves entries to make room). When the table is full, eviction takes the slot with the fewest events, so a spray of fresh keys can't flush a repeat offender. What leaves the table is not forgotten within the horizon: an evicted key's history waits in a **stash**, a history the stash drops goes to an **overflow table** of 21-bit entries, and a key too heavy for its register moves to a **hot entry** of 32-bit counters.

## 1.4 Contributions

Judged against the prior art in [2 Related work](02-related-work.md), from a search that was not exhaustive:

1. **A per-key velocity register in the same cache line as an exact last-touch time**, updated lock-free with CAS. The prior structures found either keep no counts, keep them in a separate table, or use global sketches.
2. **The window is a query argument for both membership and count.** Every verifiable prior structure fixes the window when it is built (the Learned Cuckoo Filter is the possible exception).
3. **The estimate is gated by the exact last touch.** The count is exactly 0 once `W < now − lastTouch` and at least 1 otherwise, and the newest bucket's events are known to end at `lastTouch`, which removes the worst boundary errors for free.
4. **A clock-paced, bounded reclaimer** (`ContinuousDecay`): one table pass per horizon, at most 4 lines per step.
5. **Velocity-aware eviction as an anti-evasion policy** (evict the slot with the fewest events in the horizon), which appears to be new; TinyLFU uses frequency for cache admission, not for this.
6. **A fail-closed contract on every answer**: unless `Saturated`, `Upper` is never below the true count, outside two documented exceptions, and the contract is fuzzed, soaked and model-checked ([4](04-operations.md), [5](05-concurrency.md)).

Storing a fingerprint and a timestamp per slot and expiring entries lazily is **not** new (SWBF, TBF); the freshness half is not claimed as novel.

## 1.5 Results in brief

On the scorecard's churn workload (capacity 200k, horizon 60 s), deterministic and checked byte for byte:

- **Missed keys:** 0 at 85%, 94% and 97.8% slot load (1.15x capacity) [D1].
- **False positives:** 0 of 723,513 negatives on the churn workload; 18 in 1e9 never-inserted probes (1.8e-8) [D2, D3].
- **Memory:** 18.8 B per key at design load; 31.1 B per key under a heavy flood [D6, D17, A14].
- **Velocity error:** 0.078 averaged over 1–60 s windows and 5 traffic shapes, against a floor of ~0.058 for any estimator reading this register [D5, D18].
- **Counts** stay exact from 10 to 5,000 events/s per key [A15]; a 4x heavy flood turns away 0.29% of new keys under a strict limit, and every offender keeps its full count [A14].

Speed, contention and timing results are in [7 Evaluation](07-evaluation.md) and [6 Security](06-security.md).

## 1.6 Organization

[2](02-related-work.md) places the structure among existing filters and sketches. [3](03-design.md) describes the layout and its mechanisms, [4](04-operations.md) the operations and what each answer guarantees, [5](05-concurrency.md) the lock-free slot protocol and the races it closes, and [6](06-security.md) the threat model. [7](07-evaluation.md) reports accuracy, memory and speed against 13 targets, and [8](08-limitations.md) lists what is left.
