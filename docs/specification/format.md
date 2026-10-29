# Format and parameters

This is the language-neutral description an implementation must follow to be the same filter. The key words MUST, MUST NOT, SHOULD and MAY are used as in [RFC 2119](https://datatracker.ietf.org/doc/html/rfc2119), and only where a port's behaviour has to match. The reasons behind each rule are in [the paper](../paper/03-design.md). Where this page is silent, the Go reference implementation (`go/`) is normative, and the [conformance vectors](conformance.md) decide: a port is the same filter exactly when it reproduces them.

## Time

- Time is counted in **ticks** of `Resolution` (default 1 µs). The built-in clock reads ticks since the filter was built, plus 1.
- A tick MUST fit in 46 bits: `MaxTick` = 2^46 − 1. Construction MUST reject a caller's clock that already reads past `MaxTick`, and a horizon of `MaxTick` ticks or more. An insert, a realigning touch and a sweeping decay step MUST fail (Go: panic) on a tick past `MaxTick`, before taking any lock.
- The horizon in ticks MUST be at least `2 × 24` (a unit of at least 2 ticks). A **unit** is `ceil(horizon / 24)` ticks.
- A query's window `W` covers `[now − W, now]`: an event exactly `W` ago is inside it. A negative duration converts to 0 ticks.

## Hashing

- `Hash(string)` and `HashUint64(uint64)` MUST be keyed. Under a 128-bit key (`Key`) both MUST be SipHash-2-4 of the input under that key; the conformance vectors use the key `00 01 … 0f` and record both hashes as 16 hex digits. Without a key, an implementation MUST use a secret per-filter random seed (Go: `maphash`); such hashes are not reproducible across processes.
- The primary line uses hash bits 0..35; the identity bits are disjoint from them.

## Table

- The table is `lines` 64-byte-aligned lines of 4 slots, `lines = max(2, ceil(Capacity / (Load × 4)))`, `Load` defaulting to 0.85 and at most 1. A line index comes from multiply-shift indexing (no power-of-two rounding).
- A slot is two 64-bit words:

```
word0: fp(16) | displaced(1) | saturated(1) | lastTouch(46)
word1: tag(12) | epoch(4) | 7 velocity buckets (48)        or, for a hot key: tag, the hot entry's index, bucket 0 empty
```

- A key's identity is its 16-bit fingerprint and 12-bit tag (28 bits). One tag value is reserved as **busy**, for slots changing hands.
- The alternate line is `alt(l) = (x(fp) − l) mod lines`, its own inverse. The displaced bit is set while an entry lives in its alternate line; lookups MUST match it.
- The saturated bit is set when an event overflows its bucket (spills or is dropped), and cleared once no bucket holds 31 or more.

## Velocity register

- 7 buckets, 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide, with counters of 5, 5, 6, 7, 8, 8 and 9 bits. Bucket boundaries only coarsen as time passes; advancing any distance is one re-bucketing pass.
- A full bucket spills into older ones, unless the key is promoted to a hot entry.
- The 4-bit epoch is resolved against `lastTouch`'s unit.
- An estimate reads the buckets at the register's own epoch, sums those inside `W` and interpolates the one bucket `W`'s edge cuts assuming uniform density; in the newest bucket the event at `lastTouch` counts whole. The count MUST be 0 once `W < now − lastTouch` and at least 1 otherwise. `Upper` counts the cut bucket whole.

## Placement and eviction

- Primary line first, then the alternate line. If both are full, a bounded cuckoo search: iterative deepening to depth 3 (paths of up to 4 moves), a budget of 500 lines that shrinks while recent searches fail (an EWMA).
- Eviction MUST take the slot with the fewest events in the horizon, oldest on ties.
- The evicted live entry goes to the **stash**: 4-way sets keyed by the full identity, `Stash` entries (default `max(1, slots / 16)`; negative: none). A full set keeps the entries with more events; its lightest way leaves through the spill.
- A history the stash drops goes to the **overflow table** unless it has fewer than `SpillMin` events in the horizon (default 4; 1 or less keeps every history), in which case it is shed.

## Overflow table

- `Overflow` units of 16 B (default `max(64, slots × 5 / 9)`; negative: none). 64-byte buckets of 23 entries of 21 bits (14-bit fingerprint, 5-bit count saturating at 31, 2-bit Horizon/2 slice of the newest event), three entries to a word; entry 0's 21 bits hold three 7-bit floors (count and slice).
- Each history has two candidate buckets and goes to the less loaded one; a read covers both. When both are full, the lightest history (the newcomer included) leaves, and the floor its fingerprint picks rises to its count. A second drop of the same key merges into its entry.
- Before an add moves to a new slice, entries dead by then MUST be cleared, so the 2-bit slices decode unambiguously. A key's answer adds its own entries to `Count` and `Upper`, and its floors to `Upper` alone.

## Hot entries

- `Hot` entries of 64 B with 32-bit counters (default `max(16, slots / 128)`; negative: none). A key whose register would overflow (more than 31 events in its newest unit) is promoted when an entry is free; with every entry in use it saturates instead.
- An entry is demoted back to a register once a new unit starts with every bucket at most half the register's width, and reclaimed once its newest event is past the horizon.

## Modes

- **ConstantTime** (a pad duration, or `AutoPad`): every call does the same lookups whatever the key's state and returns no sooner than its pad; first touches that find both lines full wait in the stash and are placed by later calls. Which keys get evicted then differs from the default policy, so vectors with evictions do not apply ([conformance](conformance.md#conformance-vectors)).
- **Background**: first touches that find both lines full wait in a wait room (4-way sets, `max(16, slots / 512)` sets: one entry per 128 slots) and a placer places them. In the reference implementation Background has no effect when ConstantTime is on. Like ConstantTime, it changes which keys get evicted.

## Answers

Every touch and count returns `Seen`, `Count`, `Upper`, `Saturated`, `Restored` and `Degraded`, with the meanings and the contract given in [the paper, §4.2](../paper/04-operations.md#42-trust-flags). An implementation MUST keep the contract: unless `Saturated`, `Upper` is never below the true count, outside the two documented exceptions (a key floored more than once in the same floor; with the overflow table off, a key back after a lost history).

## Defaults at a glance

| Parameter | Default |
|---|---|
| `Resolution` | 1 µs |
| `Load` | 0.85 |
| unit | Horizon/24 |
| cuckoo search | depth 3, budget 500 lines |
| claim stripes | 256 |
| stash | one entry per 16 slots, 4-way |
| overflow table | 5 units of 16 B per 9 slots, 2 candidate buckets, 3 floors |
| `SpillMin` | 4 |
| hot entries | one per 128 slots, at least 16 |
| wait room | one entry per 128 slots, at least 16 sets |
| decay | one table pass per horizon, ≤ 4 lines per call; inline on 1/64 of touches, 1 line |
