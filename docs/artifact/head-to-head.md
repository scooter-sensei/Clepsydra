# Head-to-head against industry structures

The protocol below was written and committed before any reading was taken ([method](method.md): the rule before the reading). Results go to [paper §7.5](../paper/07-evaluation.md) and the claims register; this file says what was compared, how, and how each result was to be judged.

## The question

What does a deployer give up or gain by putting Clepsydra in front of a per-key limit instead of the structures used for this in industry?

1. **Part 1, floods.** Clepsydra, an exact sharded map and a count-min estimator, on one host, under the same traffic, at the same key count, under floods of 1x to 16x the table's slots: bytes per key, throughput, p99.9 latency, count error, and false-allow and false-deny rates under a strict limit.
2. **Part 2, windows.** Clepsydra and the ECM-sketch at equal memory: count error in windows of 1 to 60 s.

## The arms

Every competitor is a published industry implementation, run as released, or, where none exists, the published algorithm implemented exactly. No single-author library is used.

| Arm | What runs | Version | Language |
|---|---|---|---|
| **Clepsydra** | this repository, default `Config` (keyed `maphash`, no `Key`, no `ConstantTime`, no `Background`), `Capacity` = the key count, `Horizon` 60 s, `Resolution` 1 ms | this tree | Go 1.24.7 |
| **Count-min estimator** | Cloudflare's `pingora-limits` crate, `Rate` (two `Estimator` count-min sketches, current and previous interval, lock-free `AtomicIsize` cells, one `ahash` per row, standard updates), used as Pingora's own rate-limiter example uses it: `observe(&key, 1)` per request, interval = the limit's window (60 s) | 0.9.0 (crates.io), `ahash` 0.8.12 | Rust 1.99.1 |
| **Exact sharded map** | a sliding-window log per key (the timestamps of its events in the horizon, `VecDeque<u32>` of ms) in `dashmap::DashMap` (the standard sharded concurrent map for Rust; default shard count and hasher) | 6.2.1 (crates.io) | Rust 1.99.1 |
| **ECM-sketch** (part 2) | Papapetrou, Garofalakis and Deligiannakis, PVLDB 5(10), 2012: a count-min array whose counters are exponential histograms (Datar et al. 2002) over a time-based window; a point query for range r ≤ the window takes the minimum over rows of each histogram's estimate for r (§4.1). No industry implementation exists; the only open one is a single-author library, so it is implemented from the paper in `go/internal/ecm` and checked against exact counts first | the paper | Go 1.24.7 |

Builds: Go's default build; Rust's default `--release` profile (no LTO, no `target-cpu`), linked with `zig cc` on the Ryzen host (no system C toolchain there). Both runners take the same trace file.

## Part 1: floods

**Traffic.** The flood protocol of [paper §6.5](../paper/06-security.md) (claims A14), at a key count large enough that no table fits the host's cache: N = 262,144 keys (Clepsydra's `Capacity`; S = its slot count). Within one 60 s horizon, at 1 ms ticks:

1. **Base:** ⌊0.85·S⌋ ≈ N keys with one event each, spread evenly over ticks 1–5,000.
2. **Offenders:** 1,000 keys with 20 events each (the limit), offender i's at tick 5,001 + i.
3. **Flood:** m·S keys with w events each in one tick, spread evenly over ticks 6,001–36,000 (30 s). Heavy floods: w = 21 (one over the limit, so each outranks an offender for eviction), m = 1, 2, 4, 8, 16. Light flood: w = 1, m = 16.
4. **Probes at tick 36,001:** all 1,000 offenders (true count 20), 20,000 flood keys (w), 20,000 base keys (1), 20,000 keys never sent (0).

Every event is inside the window at the probe, so a key's true count is its number of events. Keys are random 64-bit ids (seeded PCG); each arm hashes them with its own hasher. A generator (`go/cmd/h2h gen`) writes each experiment once as a trace file (records of key, tick, count; then the probes and their truths); every runner replays the same file, and its SHA-256 is recorded with the results.

**The operation.** Each event is one check-and-record call, the call a limiter makes per request: Clepsydra `TouchAt(HashUint64(key), 60 s, tick)`; `pingora-limits` `observe(&key, 1)`; the exact map appends the tick to the key's log, drops entries older than the horizon and counts the window. Probes are read-only: Clepsydra `CountAt`; `pingora-limits` `rate_with` (the previous and current intervals' counts); the exact map's log.

**The strict limit.** L = 20 events per 60 s: a key whose window already holds L events is denied its next request.

- Clepsydra denies by the fail-closed recipe ([paper §4](../paper/04-operations.md#using-the-trust-flags)): `Count ≥ L || Saturated || Upper ≥ L`.
- `pingora-limits` denies when previous + current interval counts ≥ L (both intervals whole: it cannot read below the window; in this traffic the previous interval is empty).
- The exact map denies when its count ≥ L.

**Metrics.**

- **False-allow rate:** keys at or over the limit (offenders; heavy-flood keys) whose next request is allowed.
- **False-deny rate:** keys under the limit (base keys, keys never sent, light-flood keys) that are denied.
- **Count error:** mean |estimate − truth| per probe class; estimates are Clepsydra's `Count`, Cloudflare's proportional estimate (previous × (1 − elapsed fraction) + current), the exact count.
- **Bytes per key:** bytes held after the flood ÷ N. Clepsydra: `Bytes()` (every array it allocated); Rust arms: a counting global allocator's live bytes, enabled only in the accuracy pass.
- **Throughput:** ops/s replaying the whole trace, one thread and 12 threads (the host's 12 hardware threads; records split by a hash of the key, so each key's events stay in order on one thread).
- **Latency:** every op timed by RDTSCP (Go: a 5-instruction assembly function; Rust: the `__rdtscp` intrinsic), calibrated against `CLOCK_MONOTONIC`; a log-linear histogram (64 sub-buckets per power of two, ≤1.6% bucket width, the same code in both languages); p50, p99, p99.9, one thread and 12 threads. Each runner also reports the p99.9 of an empty timed region (the timer's own floor).

**Sizing.** Equal key count: each arm serves the same N keys. Clepsydra is sized by `Capacity`; the exact map needs no size. A count-min sketch has no key count, only bytes, so `pingora-limits` gets **Clepsydra's peak bytes** (after the 16x heavy flood, ghost table allocated): 4 rows (its default) and slots = ⌊peak ÷ (2 estimators × 4 rows × 8 B)⌋, so it never holds fewer bytes than Clepsydra. A sensitivity run gives it 8x and 64x those bytes (accuracy only).

**Passes.** Per experiment and arm, five passes, each on a fresh structure: accuracy (replay, probes, bytes; untimed), throughput at 1 and at 12 threads (untimed), latency at 1 and at 12 threads (every op timed). Five rounds; the arms' order rotates each round (Clepsydra first, then the next arm first, …). Nothing else runs on the host during a round. `pingora-limits` reads the wall clock itself: each pass must finish within its 60 s interval, or the pass is void (the runner checks).

## Part 2: windows

**Traffic.** The scorecard's velocity workload (claim D5: steady, bursty, ramp, sparse and on/off keys over 5 horizons of 60 s; after the first horizon, one random key queried every ms for a window of 1, 5, 15, 30 or 60 s), with Clepsydra's `Capacity` equal to the key count: 2,000 keys (the scorecard's own number). Ground truth: exact event times. Metric: D5's normalized error per window, Σ|estimate − exact| ÷ Σ exact, and its mean over the five windows.

**Equal memory.** The ECM-sketch's budget is Clepsydra's `Bytes()` at the end of the run. Its memory is counted generously: 1 B per counter (its level count), 1 B per level in use (its bucket count) and 4 B per histogram bucket (a 32-bit timestamp; the bucket's size is implied by its level), at its peak over the run (Go's slice headers and pointers not counted). *Amended before any reading:* the protocol as first committed charged nothing for an empty counter, so the width search could have grown the array to about one counter per key, a per-key histogram rather than a sketch; every counter now costs its 1 B. Its parameters are tuned on the test workload: for depth d ∈ {1, …, 5} and ε_sw ∈ {0.01, 0.02, 0.05, 0.1, 0.2, 0.3, 0.5}, the widest w whose peak fits the budget (binary search), then every such configuration is scored, as is the paper's own split (ε_sw = ε_cm = √(1+ε) − 1, d = ⌈ln(1/δ)⌉, δ = 0.05). Five hash seeds for the best configuration. A second reading gives ECM 2x, 4x and 8x the budget.

**Estimates.** Clepsydra: `VelocityCountAt` (its `Count`). ECM: the paper's point estimate (minimum over rows of each histogram's sum of buckets inside the range plus half the oldest bucket that the range cuts, §3). Both upper bounds are reported beside: Clepsydra's `Upper`, and the histograms' sum of every bucket the range reaches.

## Supplement: the ECM-sketch under the floods

Declared after part 1's and part 2's readings, before this one: the study's goal names the ECM-sketch among the structures to run under the same floods, and part 1 did not include it.

- **Arm:** `go/internal/ecm` in `h2h run -arm ecm`, on part 1's six traces. Op: `Add(key, tick)` then the point query for the 60 s window, as a limiter checks and records. Probes: the point estimate (count error) and the strict decision **upper bound ≥ L** (its histograms' upper bound is never below the truth, so it fails closed like the other arms; the point estimate can read below).
- **Shape:** ε_sw = 0.5 (part 2's best; the coarsest histograms leave the most bytes for width). Depth d ∈ {1, 2, 3, 4}: the one with the lowest false-deny on the 1x heavy flood (seed 1), ties to the smaller d.
- **Memory:** for each trace, the widest sketch whose peak `Bytes` on that trace fits Clepsydra's peak bytes (8,139,136 B; binary search, seed 1). Sized per flood, which favours the sketch: a deployment must size for its worst flood.
- **Passes:** acc, t1 and lat1 as part 1's; t12 and lat12 with one mutex around the sketch (it is not safe for concurrent use; a mutex is how a deployer would share it). Five rounds, hash seed = the round. Rules 1–4 as above (rule 1: its upper bound never below the truth); its speed was read in a run of its own, so it is compared with part 1's medians, not round by round.

## Rules, written before the readings

1. **Validity first.** Before any reading: the exact map must match the generator's truth on every probe; `pingora-limits` must never read below the truth (a count-min property); the ECM histograms must stay within ε_sw of the exact count on every range of a randomized stream (`go/internal/ecm` tests); Clepsydra's `Upper` must never read below the truth for keys with at least `SpillMin` (4) events. A failing arm is fixed or dropped before readings, and the failure is recorded. *Amended before any reading:* the rule as first committed asked this of every key. A validation run (N = 4,096, two rounds, accuracy passes only) found 190,559 of 732,000 Clepsydra probes with `Upper` below the truth, every one a key with fewer than 4 events: floods evict one-event keys and the stash sheds histories under `SpillMin`, as [paper §6.4](../paper/06-security.md#64-what-is-left) documents (*Shed light histories*). The report counts both groups.
2. **Rates** (false-allow, false-deny) pool all five rounds' probes and carry a Wilson 95% interval. One arm is said to have the lower rate at a flood only when the two intervals do not overlap.
3. **Throughput and p99.9** are medians over the five rounds, with min–max. One arm is said to be faster at a flood only when it is faster in every round.
4. **Bytes per key** are reported as measured; they are deterministic except the exact map's (allocator growth).
5. **Part 2.** Clepsydra is said to have the lower error at a window only when its error is below the lowest of every ECM configuration that fits the budget at that window (the best per window, not one configuration for all) and below all five seeds of the best configuration. The same rule decides the 2x–8x readings.
6. **No re-runs to change an answer.** A void pass (the wall-clock interval, a host disturbance named before the reading) is re-run and the void run recorded; nothing else is re-run.

## The readings

The results are in [paper §7.5](../paper/07-evaluation.md#75-head-to-head-against-industry-structures) and claims A41, S63, S64 and D21; what was run, and what went wrong on the way, is recorded here.

- **Part 1** ran on the Ryzen host as `verify/h2h.sh` (commit 073f9e0): 5 rounds, 450 passes and 60 sizing passes, no void pass, 56 minutes. The full report, with the trace hashes, toolchains and sizing, is [head-to-head-ryzen.md](head-to-head-ryzen.md); every pass's line is [head-to-head-ryzen.jsonl](head-to-head-ryzen.jsonl). The rule-3 table (rounds in which Clepsydra was faster) was added to `h2h report` after the run and computed from the same lines.
- **Validity.** Rule 1 as amended held: 0 probes of keys with at least 4 events read `Upper` below the truth in any arm; Clepsydra's 477,756 low probes are all keys with fewer (shed); the exact map matched all 1,830,000 probes; `pingora-limits` never read below the truth.
- **Part 2, a void first run.** The first run of `h2h ecm` read 1.0000 for every ECM configuration at every window: every estimate was 0. The harness passed `Sketch.Count` straight to the replay, which calls `(key, window, now)`, where `Count` takes `(key, now, range)`; with the time and the range swapped every range was empty. The package's own checks (rule 1, D20) call `Count` correctly and passed; nothing checked the wiring. The harness now queries `Count(key, now, window)` and first runs a **wiring check**: an ECM-sketch with no memory limit must read within its ε_sw (0.05) at every window on this workload, or the run stops (it reads 0.0001–0.0170; the swapped call fails it). The void output was kept outside the repository; the reading is the second run, and a third, identical apart from the wiring line's byte count, is `verify/expected/h2h-ecm.txt`.
- **Not run:** part 2 at 20,000 keys (the ratio of memory to keys is what sets a sketch's collisions, and it is the same at any key count of equal bytes per key; not checked).

## What this does not measure

- Distributed use: `pingora-limits` and DashMap are in-process, as Clepsydra is; Redis-backed limiters (network round trips) are a different comparison.
- Windows other than 60 s for the count-min estimator: `Rate` has one interval per instance, so a deployer needs one instance per window; part 2's per-query windows are compared with ECM only.
- Cross-language cost: the Rust arms are compiled by rustc and Clepsydra by Go's compiler. A difference in speed is the implementations as shipped, not the data structures alone.
