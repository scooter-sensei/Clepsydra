# 3. Design

This section describes the structure's layout and mechanisms. [4 Operations and guarantees](04-operations.md) gives the operations and what each answer promises, [5 Concurrency](05-concurrency.md) the slot protocol under concurrent callers, and [specification/format.md](../specification/format.md) the parameters an implementation must match.

## 3.1 Table and slot layout

The table is any number of 64-byte-aligned lines (multiply-shift indexing, no power-of-two rounding). Each line holds 4 slots of 2 words:

```
word0: fp(16) | displaced(1) | saturated(1) | lastTouch(46)   freshness, exact to 1 tick (default 1µs: 2^46 µs ≈ 2.23 years)
word1: tag(12) | epoch(4) | 7 velocity buckets (48)  age-graded counter, or the index of a hot entry
```

- **Identity** = 16-bit `fp` + 12-bit `tag` = 28 bits, both in the same line (one tag value is reserved for slots changing hands). The primary line uses hash bits 0..35, disjoint from the identity bits.
- **Displaced bit**: set when an entry lives in its alternate line, flipped whenever it moves. Lookups match it, so a key is only compared with entries homed at its own primary line: 3.40 entries per absent-key lookup instead of 6.80 at 85% load [D11]. That halves the expected false-positive rate (1.27e-8 vs 2.53e-8); measured 18 vs 32 false positives in 1e9 probes [R3]. Storing which bucket choice was used is a known way to extend a fingerprint by a bit. The bit was taken from the timestamp (then 47 bits).
- **Saturated bit**: set when an event overflows its velocity bucket (spills to an older one or is dropped), cleared once no bucket holds 31 or more. With hot entries (§3.6) a register overflows only if every hot entry is in use. See [trust flags](04-operations.md#42-trust-flags). It took one more timestamp bit (47 → 46 bits, ~2.2 years at 1 µs) [D16].
- **Ticks** count from the filter's own start, not an epoch; a slot keeps 46 bits of them (`MaxTick`, [4 Operations](04-operations.md#ticks-and-the-46-bit-range)).

## 3.2 Sizing and memory

`Config.Load` (default 0.85) is the slot occupancy at `Capacity`: 18.8 B per key [D6]. Everything else is allocated on first use [D17], so a filter that never evicts holds only its table:

| Part | Size | Allocated |
|---|---|---|
| the table | 16 B per slot; 18.8 B per key at 85% load | in `New` |
| eviction stash | 1.8 B per key (one 24 B entry per 16 slots) | on the first eviction (in ConstantTime, the first insert that waits) |
| overflow table | 10.5 B per key (5 units of 16 B per 9 slots) | on the first history the stash drops |
| hot entries | 0.6 B per key (one 64 B entry per 128 slots) | on the first promotion |
| wait room (with `Background`) | 0.2 B per key (one entry per 128 slots) | on the first wait |

Under a heavy flood a filter holds 31.1 B per key [A14]. `Bytes()` reports what a filter holds now. Tables allocated mid-operation (stash, overflow, hot entries) are prefaulted, so no call takes their page faults.

## 3.3 Placement

A key's **primary line** comes from its hash; its **alternate line** is `alt(l) = (x(fp) − l) mod lines`, which is its own inverse, so stored entries can move without their key. At 85% load, 18.9% of keys sit in their alternate line [D11], and 44% of new keys find both lines full and need a move [S38].

- If both lines are full, a **bounded cuckoo search** (iterative deepening, depth 3, so paths of up to 4 moves, budget 500 lines) moves an entry to its alternate line. Each line of the first level is loaded before the search, so their misses overlap; at the second level the search loads one candidate's four next lines together before trying them, which is about what the Ryzen host keeps in flight [S42].
- The search budget shrinks while recent searches fail (an EWMA), so an overfull table degrades to cheap eviction.
- Eviction takes the **lowest-velocity** slot (oldest on ties), so a spray of fresh keys can't flush a repeat offender [D4, E1] (§3.3.1).
- The evicted entry goes to the **stash**: 4-way sets, one entry per 16 slots by default (`Config.Stash`), keyed by the entry's full identity. A first touch that misses the table looks there while it may hold a live entry, and takes the history back. A full set keeps the entries with more events. The victim is stashed under its own claim stripe, so its next first touch finds it in the table or in the stash, never in between [A6].
- **A relocation's copy is hidden until the hand-off.** The copy goes in with word1 busy, so no lookup finds it and no touch counts in it. The hand-off takes the source only if its words are still those copied, and only then is the copy's word1 published, so a key is never live in two slots. A visible copy let a touch that had found the source count there while reads found the copy, and let a copy of words read before the key's eviction come back as a second history [A23]. If the source changed, the move is abandoned and the copy, which counted nothing, is cleared. A relocation counts its move from before its copy goes in to the copy's publication or clearing, and a lookup that misses during a move looks again (between the hand-off and the publication the key is in no slot a lookup matches).
- **Moves and evictions of the same tag stripe exclude each other**, so neither does work the other undoes: an eviction counts itself, then checks for moves; a relocation counts itself, then checks for evictions (a Dekker handshake on sequentially consistent counters). An eviction never takes the source of an entry being moved, nor a busy slot. Move counts are striped by the low byte of the 12-bit tag, the tag of the word1 a hold's CAS takes, so they belong to whoever the slot held when the CAS succeeded; claim stripes (the first-touch locks) stay by fingerprint [A28]. While copies were visible the exclusion also kept a key's history from splitting between the table and the stash; with hidden copies it saves the moves' work, not histories [R2: torn lookups, split histories; A23].

Evictions at 97.8% load are not forced by the layout: the load threshold for 2 choices × 4 slots is 98.04%, and an exact placement oracle on the same stream places every key. The filter's evictions come from its bounded search (the oracle's shortest paths needed 5 moves for 3,406 inserts, past the filter's 4), and the stash gives every evicted key that returns its history back [D8, D9, D10].

### 3.3.1 Velocity-aware eviction

**The problem.** With oldest-first eviction, an attacker can spray fresh keys to overflow the table and **evict their own repeat key**: a fraud-evasion vector none of the prior structures discuss.

**The policy** (implemented in `go/filter.go`). When both candidate lines are full, evict the slot with the fewest events in the horizon, oldest on ties. One-off keys churn among themselves.

**Evidence.** `TestSprayCannotEvictRepeatOffender` sprays 20× capacity of one-off keys. All 50 offender keys keep their exact count of 20. The same test **fails with oldest-first eviction** (`go/verify/experiments/oldest-first-eviction.patch`) [E1]. Insert latency was first reported as unchanged; re-measured, miss-insert went from 83 to 110 ns (`verify/ab.sh 564d865 eeb5521 'CheckAndTouchMissInsert$'`) [S6, X19].

## 3.4 Background placement

`Config.Background` (off by default) keeps the cuckoo search off the call path.

- A first touch whose two lines are full doesn't search: the key waits in the **wait room** (4-way sets keyed like the stash, one entry per 128 slots, 0.2 B per slot, allocated on the first wait), where its touches and counts find it, and a goroutine places it by path or eviction as in §3.3. The goroutine runs while keys wait and exits after ~1 ms without any; it never reads the clock (a placement uses the key's last touch). If more than one key per 16 wait-room entries waits, first touches search inline again [S45, S46].
- A lookup that misses the table reads one bit of its wait-room set (set while the set holds a waiting key).
- The placer searches without the key's claim stripe, so it blocks no first touch. It commits the key at the install: it marks the room entry busy (a recall of the key waits that out), installs the entry's current words and removes the entry. The commit counts as a move of the key's stripe, so a lookup, or a first touch, that misses the table and then the room around it looks again; a key a touch took out of the room meanwhile is that touch's to place [A19].
- A touch that finds its key's room entry expired (under a lagging clock) removes it, so the placer can't install it beside a fresh one [A19]. A key's wait never takes a wait-room way the placer is committing [A24].

**Why.** At 85% load 44% of unseen-key inserts find both lines full. A move costs dependent DRAM misses (~189 ns each, about four in flight on the Ryzen host), and the deeper ones make the tail: 1.3–3.1% of inserts take over 2 µs [S45]. With Background, inserts over 2 µs at 85% load fall to 0.16–0.19% and p99 to 1.0–1.1 µs, at the cost of one more bit per miss (absent-key lookups 1.08–1.12x) [S45, S46]; the measurements are in [7 Evaluation](07-evaluation.md#unseen-key-inserts-and-background-placement). Its earlier designs are in [record/decisions.md](../record/decisions.md#background-placement).

Background stays off by default: it starts a goroutine, and its placements aren't deterministic, so the harness and the conformance vectors keep it off. Whether it becomes the default is the user's decision ([8 Limitations](08-limitations.md)).

## 3.5 The overflow table

A history the stash drops goes to the **overflow ghost table** (`go/ghost.go`), allocated on the first drop [A11, A14]:

- One 21-bit entry per history (14-bit fingerprint, 5-bit event count saturating at 31, 2-bit Horizon/2 slice of its newest event), three to a word, 23 entries per 64-byte bucket.
- Each history has **two candidate buckets** and goes to the less loaded one; a read covers both. Buckets fill evenly: no history leaves to a floor with the table 93% full, where one bucket per history had floored histories at 64% full. Reading two buckets doubles fingerprint matches (~0.3% of keys once the table is full).
- When both buckets are full, the lightest history (the newcomer included) leaves, and one of its bucket's **three floors** rises to its count. The floors are 7 bits each (count and slice), packed into entry 0's 21 bits; a history's fingerprint picks its floor, so a raised floor reaches a third of the bucket's keys. `Floored()` counts these: it stays 0 below the table's capacity.
- A second drop of the same key merges into its entry. A key's answer adds its own entries to `Count` and `Upper` and its floors to `Upper` alone, so `Upper` never reads below the truth (one exception: a key floored more than once in the same floor, [6 Security](06-security.md#64-what-is-left)) and other keys' histories add nothing unless a fingerprint matches. All of a history's events count while its newest one is in the window: `Upper` stays a bound, `Count` reads high for windows shorter than the horizon.
- Default size: 5 units of 16 B per 9 slots (`Config.Overflow`; 8.9 B per slot once allocated), room for 3.2 histories per slot: a heavy flood of 4x the table's slots raises no floor [A14].
- **Time.** Before an add moves to a new slice, entries dead by then are cleared (the whole table, once per Horizon/2), so the 2-bit slices decode unambiguously. The epoch covers the newest event of the history being added even when the adding caller's clock is behind it, and a write commits only while the epoch it decoded against is current [R2: ghost epoch races]. A read decodes its lines against the epoch they were loaded under, and loads them again if an add moved it meanwhile: decoded against the new epoch, an entry the move's sweep cleared after the load read four slices younger, a history dead for a horizon as live [A34]. Reads take two cache lines.
- Histories with fewer than 4 events (`Config.SpillMin`) are shed instead: cheap floods can't fill the table, and inserts into a full table stay cheap [A10, S35].
- It replaced a count-min sketch of the same bytes (still in `go/overflow.go` for the experiments): under a 2x heavy flood the sketch's noise reached every key [A9, A11]. A returning key starts a fresh slot, so table and overflow never count the same event, and overflow answers can't make a key look heavier for eviction [A7].

How the table's shape was chosen, and the sketches it replaced, is in [6 Security](06-security.md#65-evidence-floods) and [record/decisions.md](../record/decisions.md#overflow).

## 3.6 Hot entries

A key whose register would overflow (more than 31 events in its newest unit, 12.4/s at a 60 s horizon) is **promoted** (`go/hot.go`):

- Its 7 buckets move to a hot entry (64 B, 32-bit counters) in a side table of one per 128 slots (`Config.Hot`; at least 16), and word1 keeps the key's tag with the entry's index in the register bits and bucket 0 empty, which no register has. Counts stay exact at any realistic rate [A15].
- A touch in the entry's own unit adds with one CAS on bucket 0's word (count, closed bit, generation); a writer (new unit, demotion, reclaim) closes that word and reopens it with a new generation, so no CAS lands across a change of owner or unit [A31].
- An entry is demoted back to a register once a new unit starts with every bucket at most half the register's width, and reclaimed once its newest event is past the horizon.
- A touch counts in an entry by its key's identity, not through the slot, so a history that leaves the stash takes its entry with it: the spill closes the entry and spills its last state, and a touch that found the key's slot before then fails its add and looks again. The stash's lightest way leaves through the spill too, even with no events in the horizon, which is how a hot history weighs while its touch has moved word0 and not yet added [A32].
- With every entry in use a key saturates as before (`HotRefused`).

## 3.7 The velocity register

`go/register.go`: 7 buckets that are 1, 1, 1–2, 2–4, 4–8, 8 and 8 units wide, where a unit is `Horizon/24`. Resolution is fine near now and coarse further back.

- Bucket boundaries only coarsen as time passes, so advancing any distance is one re-bucketing pass with no per-tick work and no sweep.
- Counter widths grow with bucket width (5, 5, 6, 7, 8, 8, 9 bits). A full bucket **spills** into older ones, unless the key is promoted to a hot entry.
- The 4-bit epoch is resolved against `lastTouch`'s unit.
- Same-unit increments pick the first non-full bucket with one TrailingZeros instead of a loop (hot-key hit 44 → 32 ns as first recorded; 34.0 → 26.1 re-measured) [S8].
- A touch 8 or more units past its register's unit realigns it: every touch of a key touched less often than every 8 units (20 s at a 60 s horizon) moves both words together ([5 Concurrency](05-concurrency.md#realign)).

### Count limits

[D7, A15] With hot entries (the default), steady traffic reads 1.000 of the exact count at 5–60 s windows from 10 to 5,000 events/s per key, and bursts of up to 2,000 events in one tick read exactly in windows of 5 s and more. Windows shorter than one unit (2.5 s at a 60 s horizon) are approximate at any rate: a 10-event burst reads as 5–10 in a 1 s window. Without hot entries (`Hot < 0`, or every entry in use), the register alone holds 31 events per unit: steady traffic reads within 1% up to 12 events/s (15–60 s windows; within 2% at 5 s) and then `Saturated`, at 13/s 0.94–0.99 of the truth, at 100/s 0.12–0.22. A register alone holds at most 1,273 events per key in a burst and 762 per horizon for steady traffic (exact up to about 725) [D7]; the v1 linear layout held 255 per sub-window.

### Estimating a count

`VelocityCount` reads the buckets as laid out at the register's own epoch (no re-bucketing), sums those inside W and interpolates the one W's edge cuts, assuming uniform density; in the newest bucket the event at `lastTouch` counts whole and only the others are spread. The estimate is gated by exact facts (`estimate` in `go/register.go`):

- The count is 0 once `W < now − lastTouch`, and at least 1 otherwise.
- The newest non-empty bucket's events end at `lastTouch`.

### The register: alternatives explored

**Dyadic carry-shift register** (prototyped, `go/internal/registersim/`). The same 48 bits, but the buckets get wider with age: 1, 1, 1–2, 2–4, … units.

- When the unit index gains `m` trailing zeros, buckets `0..m` shift up one place and bucket `m+1` merges into `m+2`.
- Counter widths grow with bucket size (5, 5, 6, 6, 7, 8, 11), so large old buckets don't saturate.
- It is Hokusai's time aggregation, moved **inside one key's 48-bit word**. Seven buckets give H/32 resolution near *now*, versus H/5 for the linear register.

Normalized mean absolute error on 5 traffic shapes × 40 trials, H = 60 s (ranges span the shapes; `go test -run TestRegisterExperiment -v ./internal/registersim` prints the full table) [D12]:

| Register | 1s | 5s | 15s | 30s | 60s | mean |
|---|---|---|---|---|---|---|
| linear 6×8 (v1, since replaced) | 0.02–0.75 | 0.09–0.41 | 0.03–0.17 | 0.01–0.10 | 0.01–0.05 | 0.154 |
| **dyadic, 7 buckets** | **0.02–0.35** | **0.05–0.15** | 0.04–0.16 | 0.03–0.15 | 0.02–0.14 | **0.108** |
| Laplace 3×16 | 0.02–0.76 | 0.06–0.28 | 0.15–0.33 | 0.04–0.19 | 0.27–0.38 | 0.268 |

(Corrected: the 1 s ranges for linear and dyadic had left out the sparse shape's low error [X9].) The same harness also prints dyadic with 6 buckets (H/16 near now) at 0.122 and Laplace 4×12 at 0.232.

The dyadic register roughly halves short-window error (mean over shapes at 1 s: 0.38 → 0.20; at 5 s: 0.21 → 0.09), which is where fraud velocity rules live ("5 cards in 10 s"). It loses on 60 s windows (0.030 → 0.071).

**Laplace register** (prototyped; negative result). It stores *k* exponentially decayed sums (log-encoded, 16 bits each). Decay is pure clock arithmetic, with no epochs or shifts, so it matches "decay proportional to clock ticks" most literally. For any query W, it fits weights by least squares so that Σ aⱼ·e^(−age/τⱼ) ≈ 1[age ≤ W]. It loses: with 3–4 exponentials you can't make a sharp box, so the mean error is 1.7× the linear register's (0.268 vs 0.154; corrected from "2×") [D12, X10]. It is still a good fit if you want EWMA rates (recency-weighted velocity) rather than hard windows.

**What shipped.** An age-graded variant of the dyadic register with `Horizon/24` units and counter widths 5, 5, 6, 7, 8, 8, 9 (`go/register.go`), which scores 0.078 on the scorecard's velocity metric against the linear register's 0.129 [D5, R3] (0.081 until the newest bucket's last event counted whole, commit eed54e6; before that the 1 s window dominated the error, 0.227 of a 0.0807 mean). A pure dyadic H/32 geometry has the same mean but a worse 60 s error (0.038 vs 0.024) [E15]; a piecewise-linear density in the cut bucket is slightly worse than uniform (0.0834 vs 0.0809) [E12]. Most of what remains is the register's information: under a uniform model of events within a bucket no estimator reading it gets below ~0.058 on this workload, and 0.05 would need about 1.25 s resolution near now [D18].

## 3.8 Constant-time mode

**`ConstantTime`** (off by default) makes every call do the same lookups whatever the key's state (both lines, the stash set, the overflow buckets, under the key's stripe) and pads it, so its time doesn't say whether the key was seen [S28, S36]:

- A first touch that finds both lines full doesn't search on the call path: the key **waits in the stash** (every ConstantTime call reads its stash set anyway), marked by the displaced bit of the stash entry's word0, which is otherwise clear. Touches and counts find it there with the same work as in the table. A waiting key whose stash set is full is inserted at once, so nothing is dropped. A key that only waited for a slot is not `Restored` when its next touch installs it.
- **Calls place waiting keys before their own work**, in the part of the pad their own work's running estimate leaves: as many steps as fit a step's running mean cost. A step takes the next stash set with a waiting key (a bit per set) and inserts its keys as a plain first touch would: free slot, path or eviction. How much a call places depends on the stash, not on the caller's key. If more than one key per 16 stash entries waits, every call places one set regardless, so calls then run longer for every key alike. `Waited()` and `Waiting()` report it.
- `Touch`'s pad is `ConstantTime`; `Count`'s, and with `AutoPad` `Touch`'s too, is a running estimate of its own work's 99.5th percentile (one sampled call in 4 moves it up 100 ns if it ran past, down 0.5 ns if not), capped at the pad. Every pad ends at a random point over 128 ns: the spin reads the clock every ~37 ns, and a pad ending at a fixed time would end at a phase of those reads that follows the call's own work [S41].
- A batch's warm-up runs outside the calls' pads; in ConstantTime it prefetches both lines of every key and looks nothing up, so it doesn't depend on the keys' state either (outside ConstantTime it looks each key up in its primary line, for speed).

What this costs and how much it hides is in [6 Security](06-security.md#66-evidence-timing).

## 3.9 Reclaiming slots

Expiry is arithmetic on every access, so answers never depend on the reclaimer. `ContinuousDecay()` is a bounded slot reclaimer, paced by the clock: one table pass per horizon, at most 4 lines per call; it also runs inline on 1/64 of touches, 1 line at a time. Freeing at most 4 lines per call (1 when run inline) is a property of the code (`decayStep`), not a measured latency distribution. The sweep takes an expired slot as an install does, word1 busy first, its hold counted as a move [A26].
