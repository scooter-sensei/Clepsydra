# Sliding-Window Probabilistic Register (Temporal Filter)

A fixed-memory filter answering **"seen key X in the last W?"** and **"how many times in the last W?"** for any `W ≤ Horizon`, chosen per query. The window is `[now − W, now]`: an event exactly `W` ago is inside it. Hits are lock-free (CAS on the slot's two words); a key's first touch takes one of 256 striped locks.

**Checking these claims.** Tags like [D1] point to `go/verify/claims.md`, which gives the command, the expected result and its status for every claim. `go/verify/isolated.sh` re-runs all deterministic checks in a fresh, offline container and compares them byte for byte with `go/verify/expected/`. Speed figures are reported, not checked: rounds up to 2026-10-09 ran on a 4-vCPU Xeon @ 2.1 GHz with 260 MB L3; the 2026-10-21 round ran on a Ryzen 5 4600H under WSL2 (6 cores, 4 MB L3 per CCX; a random DRAM access 189 ns with 4 KB pages and 147 ns with huge pages, about 4 misses in flight with 4 KB pages and 7 with huge pages; the CPU is taken away ~1,070 times a second for 15 µs at the median) [S42, S43]. Compare ratios measured on one host, not absolute numbers across hosts.

Measured history of every change is in `go/results/history.md`; ideas done, tried and queued are in `go/results/backlog.md`; round reports are in `go/results/`.

## Layout

The table is any number of 64-byte-aligned lines (multiply-shift indexing, no power-of-two rounding). Each line holds 4 slots of 2 words:

```
word0: fp(16) | displaced(1) | saturated(1) | lastTouch(46)   freshness, exact to 1 tick (default 1µs: 2^46 µs ≈ 2.23 years)
word1: tag(12) | epoch(4) | 7 velocity buckets (48)  age-graded counter, or the index of a hot entry
```

- **Identity** = 16-bit `fp` + 12-bit `tag` = 28 bits, both in the same line (one tag value is reserved for slots changing hands). The primary line uses hash bits 0..35, disjoint from the identity bits.
- **Displaced bit**: set when an entry lives in its alternate line, flipped whenever it moves. Lookups match it, so a key is only compared with entries homed at its own primary line: 3.40 entries per absent-key lookup instead of 6.80 at 85% load [D11]. That halves the expected false-positive rate (1.27e-8 vs 2.53e-8); measured 18 vs 32 false positives in 1e9 probes [R3].
- **Saturated bit**: set when an event overflows its velocity bucket (spills to an older one or is dropped), cleared once no bucket holds 31 or more. With hot entries (below) a register overflows only if every hot entry is in use. See *Trust flags*.
- **Sizing**: `Config.Load` (default 0.85) is the slot occupancy at `Capacity`: 18.8 B per key [D6]. Everything else is allocated on first use [D17], so a filter that never evicts holds only its table: the eviction stash (1.8 B per key, on the first eviction), the overflow table (10.5 B per key, on the first history the stash drops) and hot entries (0.6 B per key, on the first promotion); with `Background`, the wait room (0.2 B per key, on the first wait). Under a heavy flood a filter holds 31.1 B per key [A14]. `Bytes()` reports what a filter holds now.
- **Placement**: primary line first, then the alternate line `alt(l) = (x(fp) − l) mod lines`, which is its own inverse, so stored entries can move without their key. At 85% load, 18.9% of keys sit in their alternate line [D11], and 44% of new keys find both lines full and need a move [S38].
  - If both lines are full, a **bounded cuckoo search** (iterative deepening, depth 3, so paths of up to 4 moves, budget 500 lines) moves an entry to its alternate line. Each line of the first level is loaded before the search, so their misses overlap.
  - The search budget shrinks while recent searches fail (an EWMA), so an overfull table degrades to cheap eviction.
  - Eviction takes the **lowest-velocity** slot (oldest on ties), so a spray of fresh keys can't flush a repeat offender [D4, E1].
  - The evicted entry goes to the **stash**: 4-way sets, one entry per 16 slots by default (`Config.Stash`), keyed by the entry's full identity. A first touch that misses the table looks there while it may hold a live entry, and takes the history back. A full set keeps the entries with more events. The victim is stashed under its own claim stripe, so its next first touch finds it in the table or in the stash, never in between [A6].
  - **Moves and evictions of the same claim stripe exclude each other.** A relocation counts its move from the moment its copy appears to the end of the hand-off; an eviction counts itself, then checks for moves; a relocation counts itself, then checks for evictions (a Dekker handshake on sequentially consistent counters). An eviction never takes the source or the copy of an entry being moved, which would split the key's history between the table and the stash, and a lookup that misses during a move looks again [R2: torn lookups, split histories].
  - **Background** (`Config.Background`, off by default) keeps the search off the call path. A first touch whose two lines are full doesn't search: the key waits in the **wait room** (4-way sets keyed like the stash, one entry per 128 slots, 0.2 B per slot, allocated on the first wait), where its touches and counts find it, and a goroutine places it by path or eviction as above. The goroutine runs while keys wait and exits after ~1 ms without any; it never reads the clock (a placement uses the key's last touch). If more than one key per 16 wait-room entries waits, first touches search inline again [S45, S46].
    - A lookup that misses the table reads one bit of its wait-room set (set while the set holds a waiting key).
    - The placer searches without the key's claim stripe, so it blocks no first touch. It commits the key at the install: it marks the room entry busy (a recall of the key waits that out), installs the entry's current words and removes the entry. The commit counts as a move of the key's stripe, so a lookup, or a first touch, that misses the table and then the room around it looks again; a key a touch took out of the room meanwhile is that touch's to place [A19].
  - A history the stash drops goes to the **overflow ghost table** (`go/ghost.go`), allocated on the first drop [A11, A14]:
    - One 21-bit entry per history (14-bit fingerprint, 5-bit event count saturating at 31, 2-bit Horizon/2 slice of its newest event), three to a word, 23 entries per 64-byte bucket.
    - Each history has **two candidate buckets** and goes to the less loaded one; a read covers both. Buckets fill evenly: no history leaves to a floor with the table 93% full, where one bucket per history had floored histories at 64% full. Reading two buckets doubles fingerprint matches (~0.3% of keys once the table is full).
    - When both buckets are full, the lightest history (the newcomer included) leaves, and one of its bucket's **three floors** rises to its count. The floors are 7 bits each (count and slice), packed into entry 0's 21 bits; a history's fingerprint picks its floor, so a raised floor reaches a third of the bucket's keys. `Floored()` counts these: it stays 0 below the table's capacity.
    - A second drop of the same key merges into its entry. A key's answer adds its own entries to `Count` and `Upper` and its floors to `Upper` alone, so `Upper` never reads below the truth (one exception: a key floored more than once in the same floor, docs/SECURITY.md) and other keys' histories add nothing unless a fingerprint matches. All of a history's events count while its newest one is in the window: `Upper` stays a bound, `Count` reads high for windows shorter than the horizon.
    - Default size: 5 units of 16 B per 9 slots (`Config.Overflow`), room for 3.2 histories per slot: a heavy flood of 4x the table's slots raises no floor [A14].
    - Time: before an add moves to a new slice, entries dead by then are cleared (the whole table, once per Horizon/2), so the 2-bit slices decode unambiguously. The epoch covers the newest event of the history being added even when the adding caller's clock is behind it, and a write commits only while the epoch it decoded against is current [R2: ghost epoch races]. Reads take two cache lines.
    - Histories with fewer than 4 events (`Config.SpillMin`) are shed instead: cheap floods can't fill the table, and inserts into a full table stay cheap [A10, S35].
    - It replaced a count-min sketch of the same bytes (still in `go/overflow.go` for the experiments): under a 2x heavy flood the sketch's noise reached every key [A9, A11]. A returning key starts a fresh slot, so table and overflow never count the same event, and overflow answers can't make a key look heavier for eviction [A7].
- **Hot entries** (`go/hot.go`): a key whose register would overflow (more than 31 events in its newest unit, 12.4/s at a 60 s horizon) is promoted: its 7 buckets move to a hot entry (64 B, 32-bit counters) in a side table of one per 128 slots (`Config.Hot`), and word1 keeps the key's tag with the entry's index in the register bits and bucket 0 empty, which no register has. Counts stay exact at any realistic rate [A15]. A touch in the entry's own unit adds with one CAS on bucket 0's word; a writer (new unit, demotion, reclaim) closes that word and reopens it with a new generation, so no CAS lands across a change of owner or unit. An entry is demoted back to a register once a new unit starts with every bucket at most half the register's width, and reclaimed once its newest event is past the horizon. With every entry in use a key saturates as before (`HotRefused`).
- **Velocity register** (`go/register.go`): 7 buckets that are 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide, where a unit is `Horizon/24`. Resolution is fine near now and coarse further back.
  - Bucket boundaries only coarsen as time passes, so advancing any distance is one re-bucketing pass with no per-tick work and no sweep.
  - Counter widths grow with bucket width (5,5,6,7,8,8,9 bits). A full bucket **spills** into older ones, unless the key is promoted to a hot entry.
  - **Count limits** [D7, A15]. With hot entries (the default), steady traffic reads 1.000 of the exact count at 5–60 s windows from 10 to 5,000 events/s per key, and bursts of up to 2,000 events in one tick read exactly in windows of 5 s and more. Windows shorter than one unit (2.5 s at a 60 s horizon) are approximate at any rate: a 10-event burst reads as 5–10 in a 1 s window. Without hot entries (`Hot < 0`, or every entry in use), the register alone holds 31 events per unit: steady traffic reads within 1% up to 12 events/s (15–60 s windows; within 2% at 5 s) and then `Saturated`, at 13/s 0.94–0.99 of the truth, at 100/s 0.12–0.22.
  - The 4-bit epoch is resolved against `lastTouch`'s unit.

## Operations

| Op | What it does | Memory |
|---|---|---|
| `CheckAndTouch(key, W) bool` | CAS `lastTouch` to now, returns `now − lastTouch ≤ W`, counts the event | 1 line on a primary hit |
| `CheckAndTouchBatch(hs, W, now, out)` | Same for many keys; issues every key's line load first so cache misses overlap | see speed table |
| `VelocityCount(key, W) int` | Reads the buckets as laid out at the register's own epoch (no re-bucketing), sums those inside W and interpolates the one W's edge cuts, assuming uniform density; in the newest bucket the event at lastTouch counts whole and only the others are spread | 1 line |
| `TouchAndCount(key, W)` | Both in one pass | 1 line |
| `TouchAndCountBatch`, `VelocityCountBatch` | Batched like CheckAndTouchBatch | see speed table |
| `ContainsAt(h, W, now)` | Read-only freshness check | 1 line |
| `Touch(key, W) Result`, `Count(key, W) Result` | TouchAndCount / VelocityCount plus the trust flags below | 1 line |
| `ContinuousDecay()` | Bounded slot reclaimer, paced by the clock (one table pass per horizon, ≤ 4 lines per call; inline on 1/64 of touches, 1 line) | — |

`*At(hash, windowTicks, nowTicks)` variants take a pre-hashed key and a caller-cached clock. `Hash` and `HashUint64` are keyed: a random per-filter seed, or SipHash-2-4 under `Config.Key` (SECURITY.md). Counters for operators: `Evictions`, `Relocations`, `Restored`, `Folded`, `Shed`, `Floored`, `Lost`, `Promoted`, `HotRefused`, `Demoted`, `HotInUse`, `Waited`, `Waiting`, `Bytes`.

## Trust flags

`Result` says how far an answer can be trusted, so a rate limiter or abuse check can fail closed (SECURITY.md has the recipe):

- **`Upper`** counts the bucket the window's edge cuts as wholly inside, and the overflow table's floors the key reads, so it is never below the true count unless `Saturated` [A4, A5], with one exception: a key that left more than one history in the same overflow floor can read low by all but its largest (SECURITY.md, "Repeatedly floored keys") [A12]. This needs the overflow table (the default): with `Overflow < 0` a history the stash drops is lost, and a key that comes back afterwards reads low without a flag until the lost events leave the window; `Lost()` is then the only signal [A18].
- **`Saturated`**: the slot's saturated bit is set and a bucket the window reaches holds at least 31 events. Why that is exact: an overflowing bucket holds at least 31 and still covers the overflowing event's time, and bucket counts only grow until they age out. For a hot entry: windows reaching back to the unit it was promoted in, if the register it came from had overflowed. `Count` and `Upper` are then floors.
- **`Restored`**: the key came back from the stash.
- **`Degraded`**: part of the count comes from the overflow table: the key likely had a history the stash dropped (`Folded()` counts them), and the count may be another key's whose fingerprint matches (~0.3% of keys while the table is full) [A7, A14]. With the overflow off, the old table-wide meaning: the key wasn't found and some history was lost within the last horizon (`Lost()`).

**`ConstantTime`** (off by default) makes every call do the same lookups whatever the key's state (both lines, the stash set, the overflow buckets, under the key's stripe) and pads it, so its time doesn't say whether the key was seen [S28, S36]:

- A first touch that finds both lines full doesn't search on the call path: the key **waits in the stash** (every ConstantTime call reads its stash set anyway), marked by the displaced bit of the stash entry's word0, which is otherwise clear. Touches and counts find it there with the same work as in the table. A waiting key whose stash set is full is inserted at once, so nothing is dropped.
- **Calls place waiting keys before their own work**, in the part of the pad their own work's running estimate leaves: as many steps as fit a step's running mean cost. A step takes the next stash set with a waiting key (a bit per set) and inserts its keys as a plain first touch would: free slot, path or eviction. How much a call places depends on the stash, not on the caller's key. If more than one key per 16 stash entries waits, every call places one set regardless, so calls then run longer for every key alike. `Waited()` and `Waiting()` report it.
- `Touch`'s pad is `ConstantTime`; `Count`'s, and with `AutoPad` `Touch`'s too, is a running estimate of its own work's 99.5th percentile (one sampled call in 4 moves it up 100 ns if it ran past, down 0.5 ns if not), capped at the pad. Every pad ends at a random point over 128 ns: the spin reads the clock every ~37 ns, and a pad ending at a fixed time would end at a phase of those reads that follows the call's own work [S41]. Tables allocated mid-operation (stash, overflow, hot entries) are prefaulted, so no call takes their page faults.

## Why it avoids the classic problems

- **No boundary artifacts in freshness.** `lastTouch` is exact, so `W` is checked to the tick (`TestExactBoundary`).
- **Bounded reclaim work.** Expiry is arithmetic on every access, so answers never depend on the reclaimer. The reclaimer frees at most 4 lines per call (1 when run inline) — a property of the code (`decayStep`), not a measured latency distribution.
- **Velocity is gated by exact facts** (`estimate` in `go/register.go`).
  - The count is 0 once `W < now − lastTouch`, and at least 1 otherwise.
  - The newest non-empty bucket's events end at `lastTouch`.

## Current numbers

**Deterministic** (checked by `go/verify/run.sh`; capacity 200k, horizon 60s, `go/cmd/tfbench`):

- **Missed keys** [D1]: 0 at 85%, 94% and 97.8% slot load (1.15x Capacity). Before the stash, 58 of 113,040 (0.051%) at 97.8%.
- **Evictions at 97.8%** [D8, D10]: not the layout. The load threshold for 2 choices × 4 slots is 98.04%, and an exact placement oracle on the same stream places every key (0 forced drops). The filter's ~4,575 evictions come from its bounded search: the oracle's shortest paths needed 5 moves for 3,406 inserts, past the filter's 4. The stash gives every evicted key that returns its history back.
- **False positives** [D2, D3]: 0 of 723,513 negatives on the churn workload; 18 in 1e9 never-inserted probes at capacity (1.8e-8; Poisson 95% interval 1.07e-8–2.84e-8; model 1.27e-8).
- **Memory** [D6, D17, A14]: 18.8 B per key at design load; 31.1 B per key under a heavy flood.
- **Velocity error** [D5]: 0.078, averaged over 1–60s windows and 5 traffic shapes (0.214 at 1 s, 0.024 at 60 s). The original 6×8 linear register scored 0.129 on the same harness [R3]. Most of what remains is the register's information: under a uniform model of events within a bucket no estimator reading it gets below ~0.058 on this workload, and 0.05 would need about 1.25 s resolution near now [D18].
- **Floods** [A14]: a strict limit of 20 turns away 0.12% / 0.17% / 0.29% of new keys after heavy floods of 2x / 3x / 4x the table's slots, and every offender keeps its full `Count`.

**Speed** (one host per round; `go/verify/speed.md` has the runs, `go/verify/ab.sh` repeats them; compare ratios, not absolute numbers): see `go/verify/speed.md` and, for the 2026-10-21 round, `go/results/round-2026-10-21.md`.

## Known limits

- Two CASes per touch, one per word (a single 16-byte CAS was slower on the reference host [E3]). Hits within one tick skip the word0 CAS; a hot entry's touch is one CAS on its bucket-0 word.
- **New keys at design load often need a move.** At 85% load 44% of first touches find both lines full; the move costs a round of 8 parallel misses plus the hand-off (~1 µs median on a DRAM-bound table, p99 2.4–3.5 µs, p99.9 several µs) [S38]. With `Background` the move runs in the placer goroutine: at 85% load 0.16–0.19% of unseen-key inserts take over 2 µs instead of 1.3–3.1%, and p99 is 1.0–1.1 µs instead of 2.1–2.8 µs (90%: 0.21–0.33% instead of 9–11%), against 0.05–0.15% for a 0.5 µs busy loop on the same host. A miss reads one more bit (absent-key lookups 1.08–1.12x) [S45, S46]. On that host the p99.9 stays above 2 µs, because the host itself interrupts ~0.1% of microsecond-long calls [S43]. Above ~94% slot load the bounded search starts evicting.
- **Hot keys under contention.** Every touch of a hot key writes its entry's line: under Zipf traffic (heaviest key ~9% of calls) six cores reach 3.4x one core, uniform traffic 4.9x [S37].
- First touches of a key take a striped lock (256 stripes) so racing writers can't create two slots [R2]. Hits stay lock-free.
- **Counts under concurrency** [R2, D14]. A slot changes hands with word1 set to a reserved "busy" tag before word0 changes, and a touch re-reads word0 after word1 before its CAS, so a touch never counts into another key's register unless the two share all 28 identity bits. A move abandoned because a touch reached the source first adds the copy's counts back to the entry; evictions and moves of one stripe exclude each other (Layout).
- **Floods past the overflow table.** From about 4.5x the table's slots in heavy keys the overflow table's buckets overflow: the floors cover the keys sharing them, strict limits turn new keys away (96% at 6x), and the offenders' `Count` is lost; `Upper` still covers them. `Floored()` is the alarm [A14]. Without `ConstantTime`, timing reveals seen vs unseen. See SECURITY.md.
