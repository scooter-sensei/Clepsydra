# Claims and how each is checked

Every claim made about this filter (DESIGN.md, RESEARCH.md, results/, and the project thread) is listed here with the command that checks it, the result, and a status:

- **checked**: deterministic; `verify/run.sh` re-computes it and requires a byte-for-byte match with `verify/expected/`.
- **replayed**: re-derived from the git history by `verify/replay.sh` (old commits, or today's harness on old library code).
- **derived**: follows from the code or from arithmetic shown here.
- **host**: a speed figure. It depends on the machine; `verify/speed.md` has the reference runs and `verify/ab.sh` repeats any comparison. Compare ratios, not absolute numbers.
- **corrected**: an earlier claim that the checks showed to be wrong; the corrected statement is given.
- **unverifiable**: no code or data was kept to re-check it. Treat as anecdote.

Run everything: `verify/isolated.sh` (fresh clone from a git bundle, official Go 1.24.7 image pinned by digest, `--network none`, empty caches), or `verify/run.sh` in any clean checkout. Deterministic results hold for linux/amd64 with Go 1.24.7; other architectures weren't tested.

## Deterministic results (checked)

| ID | Claim | Check | Result |
|---|---|---|---|
| D1 | Missed keys on the churn workload: 0 at 1.0x and 1.1x Capacity, 0.051% at 1.15x | `go run ./cmd/tfbench -exact` | 0/99,075, 0/108,572, 58/113,040 positives missed |
| D2 | No false "seen" on the churn workload | same | 0 of 628,235 / 691,508 / 723,513 negatives |
| D3 | Static FPR 1.8e-8 at capacity | same | 18 in 1e9 never-inserted probes; exact Poisson 95% interval 1.07e-8–2.84e-8 |
| D4 | Spray can't evict repeat offenders | same; `TestSprayCannotEvictRepeatOffender` | 100 of 100 offenders keep exact counts after 20× capacity of one-off keys (test: 50 of 50) |
| D5 | Velocity error 0.081 (mean over 1, 5, 15, 30, 60s windows) | same | per window 0.2273, 0.0582, 0.0514, 0.0436, 0.0239; mean 0.0809 |
| D6 | 18.8 B per key at the default 85% load | same | 235,296 slots × 16 B / 200,000 keys |
| D7 | Per-key count limits | same (`burst` and `steady` lines) | Steady traffic, reported/exact over the last horizon of 3: within 1% up to 12 events/s at 15–60 s windows (1.000–1.006); 13/s 0.94–0.99; 20/s 0.63–0.75; 100/s 0.12–0.22. A burst in one tick, over 64 phases of the bucket cycle: reads at most 43 / 125 / 380 / 1,273 in 1 / 5 / 15 / 60 s windows, at least 14 / 66 / 192 / 770. A 10-event burst reads 5–10 in a 1 s window |
| D8 | The 1.15x misses are not forced by the layout | `go test -run TestPlacementLimit -v` | exact oracle (same lines, same 2 candidates, unbounded BFS) drops 0 of 731,739 inserts, peak slot load 0.9780; the filter evicts 4,575 times. The oracle's shortest paths needed 5 moves for 3,406 inserts; the filter's search allows 4 |
| D9 | The oracle is right | `go test -run TestOracleMatchesMaxFlow -v` | on small overloaded tables it agrees with from-scratch max bipartite b-matching on 12,941 feasible and 2,933 infeasible inserts; every drop carries a checked Hall-violation certificate |
| D10 | Load threshold for 2 choices × 4 slots is 98.04% | `go test -run TestLoadThreshold -v` | 0.98037, solved from the Cain–Sanders–Wormald / Fernholz–Ramachandran equation; matches Walzer, [arXiv 2307.00644](https://arxiv.org/abs/2307.00644) §1.1.1 (0.89701, 0.95915, 0.98037 for 2, 3, 4 slots) |
| D11 | The displaced bit halves the entries an absent lookup compares | `go test -run TestComparedEntries -v` | 3.3998 vs 6.7989 entries per lookup (1e6 probes), expected FPR 1.267e-8 vs 2.533e-8; 18.9% of keys sit in their alternate line at 85% load |
| D12 | Register prototypes (RESEARCH.md §3.2–3.3) | `go test -run TestRegisterExperiment -v` | table in `verify/expected/register-experiment.txt`: means linear 0.154, dyadic-7 0.108, Laplace 3×16 0.268 |
| D13 | Tests pass, including under the race detector | `go vet ./...`, `go test ./...`, `go test -race -short ./...` | pass |
| D14 | The two racy regression tests hold at HEAD | `verify/replay.sh stress 300` (4 runs at a time) | 0 of 300 failures each |
| D15 | Hits skip the word0 CAS within one tick, and a full register skips the word1 CAS | derived: `touch` (`if now > last && !CAS`), `bump` (`nw == old`) | code |
| D16 | 2^47 µs ≈ 4.46 years; 1.8e-8 FPR ≈ one false hit per minute at 1M absent-key lookups/s | derived: arithmetic | 1.407e14 µs = 4.46 y; 1.8e-8 × 1e6 × 60 = 1.08 |

## Replays of recorded results (replayed)

| ID | Claim | Check | Result |
|---|---|---|---|
| R1 | Every results/history.md row's deterministic columns | `verify/replay.sh history` | each row's commit re-run with its own harness: all 17 rows match on every column that harness printed |
| R2 | Four bugs found and fixed | `verify/replay.sh fixes` | each regression test fails with the bug and passes with the fix. Duplicate slots: bc2e12d fails 11–13 of 20; a8a2848 still fails ~2.5% of runs (10 of 400); e42327a 0 of 400. Lost counts under relocation races: f07b04e fails 4–5 of 40; e42327a 0 of 200. Negative windows (f9c9127) and the empty-batch panic (f07b04e): deterministic |
| R3 | Before → after numbers in results/backlog.md | `verify/replay.sh harness` (today's accuracy harness on each library commit; expected output `verify/expected/harness.txt`) | item 1: 5.016% → 0% missed at 2.5x (597ee63 harness on 8381fd2 vs 597ee63). Item 3: 1.96e-7 → 1.40e-8 at 1e9 probes (126bd87 → 4b5850b). Item 5: 41.9 → 18.8 B/key. Item 6: 0.191% → 0.066% (3e9277c → acea1cf), see X6. Item 7: 0.1288 → 0.0868. Item 9: 0.0868 → 0.0809. Item 13: 32 → 18 false positives in 1e9 (c8dbb54 → 6bf61f0) |

## Reverted experiments (patches)

Each "tried" item in results/backlog.md is a patch against the verification commit in `verify/experiments/`, applied with `git apply`. `verify/experiments.sh` applies each one and records its deterministic effect (`verify/expected/experiments.txt`); speed effects are host-dependent (`verify/ab.sh HEAD HEAD+verify/experiments/<name>.patch <bench>`).

| ID | Claim | Patch | Result | Status |
|---|---|---|---|---|
| E1 | Oldest-first eviction fails the spray test (RESEARCH §3.1) | oldest-first-eviction | `TestSprayCannotEvictRepeatOffender` fails ("offender 0: count 0 after spray"); scorecard spray 0 of 100 | checked |
| E2 | Branchless find: hot hits ~10% faster, cold lookups ~15% slower | branchless-find | `find` compiles to 4 SETEQ (0 without the patch); speed in `verify/speed.md` | checked / host |
| E3 | One CMPXCHG16B: 19 vs 15 ns for two CASes | cas2-cmpxchg16b | `verify/speed.md` | host |
| E4 | Reciprocal clock division: no change on the string path | reciprocal-clock | exactness test passes; speed in `verify/speed.md` | host |
| E5 | Raw RDTSC: 19 vs 37 ns | rdtsc-clock | benchmarks in `internal/clockbench`: `go test -run '^$' -bench . ./internal/clockbench` after applying; not in speed.md | host |
| E6 | Homing: keys in their second line 37% → 30% (steady), 33% → 24% (re-touch) | homing | 37.3% → 30.2% and 33.5% → 23.9% | checked; speed host |
| E7 | THP madvise granted, no clear gain | thp-madvise | AnonHugePages 307,200 kB with the patch, 0 without (host-dependent); speed in `verify/speed.md` | host |
| E8 | Relocation prefetch: no change | reloc-prefetch | `verify/speed.md` | host |
| E9 | Spill counts exact under a concurrent audit; absent lookups faster, inserts slower | spill-counts | audit passes; accuracy identical to the base; speed in `verify/speed.md` | checked / host |
| E10 | 46-bit register: FPR 7e-9, MAE 0.085, cap ~876 | register-46 | 7 in 1e9 (95%: 2.8e-9–1.4e-8); MAE 0.0852; FN 62 at 1.15x; 60 s burst 638–889; steady 10/s reads 12% low | checked |
| E11 | 44-bit register: no FP in 1e9, MAE 0.088, cap ~630 | register-44 | 0 in 1e9 (95% upper bound 3.7e-9); MAE 0.0879; FN 70; 60 s burst 508–633; steady 10/s reads 18–30% low | checked |
| E12 | Piecewise-linear edge estimate slightly worse | piecewise-linear | MAE 0.0834 vs 0.0809 | checked |
| E13 | 8-slot buckets: 0 misses at 98%, ~2x false positives | eight-slot | 0 missed at 1.15x; 29 vs 18 false positives in 1e9 (1.6x; "~2x" was never measured) | checked; speed host |
| E14 | Lock-free duplicate repair cut duplicates ~10x | lockfree-dup-repair-bc2e12d, lockfree-dup-repair, lockfree-dup-nolock-control | racy: mean duplicate slots over 60 runs 102 vs 1,592 without it at bc2e12d (16x); HEAD port 196 vs 1,112 (6x). See `verify/experiments/README.md` | re-run, not in expected/ |
| E15 | Pure dyadic H/32: same mean, 60 s error 0.040 vs 0.024 | dyadic-h32 | mean 0.0817 vs 0.0809; 60 s 0.0377 vs 0.0239 (the 0.040 was with the older estimator) | checked |
| E16 | Hot-key contention: 10 / 16 / 22 ns per op for 1 / 16 / 16K keys | contention-bench | `verify/speed.md` | host |

The alternate-line prefetch is commits 40f9223 (added) and bc2e12d (reverted); `verify/speed.md` compares them.

## Speed (host)

SPEED_TABLE

## Corrected claims

| ID | Was | Now | Evidence |
|---|---|---|---|
| X1 | "Two-choice, 4-slot tables saturate at ~97.7% load, so the 98% misses can't be removed with this layout" (history legend, DESIGN, thread round 5) | The threshold is 98.04%, and an exact placement exists for every insert of the 1.15x stream (peak 97.8%). The misses come from the bounded search; depth 4 / budget 1000 (9f2529f) missed 12 instead of 58 | D8, D9, D10, R3 |
| X2 | "Heavy hitters keep their counts (up to 2,617 per horizon)" (DESIGN, a test comment) | The register holds at most 1,273 events, and heavy keys read low well before that: see D7 and X16 | D7 |
| X3 | Decision card: "counts stay exact past 1,000 hits per key per horizon" with the current register | Only for a burst at a favourable phase; steady traffic is within 1% only up to ~12 events/s (720 per minute). The alternatives' caps (876, 630) were measured the same way; on both patterns they are still lower than the current register's, so the choice's ordering holds | D7, E10, E11 |
| X4 | "0.06% missed at 98%" (DESIGN) | 0.051% (58 of 113,040) at 1.15x | D1 |
| X5 | "lock-free filter", "safe for concurrent use without locks" (DESIGN, filter.go) | Hits are lock-free; a key's first touch takes a striped lock | filter.go `touch` |
| X6 | "Relocation depth 3 / budget 500: misses at 98% load 0.19% → 0.06%" (backlog item 6) | True against 3e9277c, but the commit just before it (9f2529f: depth 4 / budget 1000) missed 0.011%; the change raised misses from 12 to 75. Its insert-speed benefit was never recorded | R3 |
| X7 | "Static FPR 2e-7 → 4e-8" (backlog item 3, thread round 1) | Those were 1 and 2 events in 5M and 50M probes. At 1e9 probes: 1.96e-7 → 1.40e-8 | R3 |
| X8 | `VelocityCount` "re-buckets to now" (DESIGN) | It reads the buckets at the register's own epoch, no re-bucketing (since 24f133b) | register.go `estimate` |
| X9 | 1s error ranges in RESEARCH §3.2 (linear 0.38–0.75, dyadic 0.17–0.35) | 0.02–0.75 and 0.02–0.35 (the sparse shape was left out) | D12 |
| X10 | Laplace register "2× the linear register's error" | 1.7× (0.268 vs 0.154) | D12 |
| X11 | Truncated timestamps would allow "6 slots per line" (RESEARCH §3.4) | 15 bits freed per slot; 6 slots would need ≤ 85 bits each, a slot needs ~113 | arithmetic |
| X12 | Outdated text: 48-bit timestamps / 8.9 years, `Horizon/32` units, "255 per sub-window", "32 B per key at 50% load", "FP churn at 1.0x", "misses at zero up to ~0.95" | 47 bits / 4.46 years, `Horizon/24`, D7, D6, FP churn is at the highest load point, D1 | code, D1, D6, D7 |
| X13 | "~5% of counts lost" with 8 racing first touches (backlog item 11, thread round 3) | 8.0% in the replayed run (147,186 of 160,000 counted); duplicates 13.5% (2,695 of 20,000). The number varies per run | R2 report |
| X14 | `TestTouchRacingRelocationKeepsCounts` as the evidence that relocation races lose no counts (backlog item 16, thread round 6: "12 of 20 runs lost counts, now 0 of 20") | The test was flawed: under CPU load it failed at HEAD too (12 of 40 runs), with no race involved. Hot keys still at one event could be evicted at 98% load, and on fast runs keys passed 762 events and VelocityCount interpolated the oldest bucket. Fixed test: 3 initial touches per hot key, all touches at a bucket-aligned tick, ≤1,203 events per key. It fails 4–5 of 40 runs at f07b04e and 0 of 200 at e42327a and 0 of 300 at HEAD | R2, D14 |
| X15 | "0 duplicates across 50 runs" after the striped lock (a8a2848; thread round 3) | a8a2848 still produced 1 duplicate slot in ~2.5% of runs under CPU load (10 of 400). e42327a's install order closed it: 0 of 400 | R2 |
| X16 | Count limits stated as "exact past 1,000 per horizon" or "1,273 per burst" | Steady traffic above ~12 events/s per key reads low in every window; bursts read low in windows shorter than the horizon; even 60 s windows read 770–1,273 for a 1,273-event burst depending on phase | D7 |

## Not reproducible

| Claim | Where | Why |
|---|---|---|
| "pprof puts 71% of the time on that single line load" | thread, first prototype | profile not kept; re-profiling a5292e3 would give a new number, not this one |
| "Missed keys: 0.09% of keys were dropped when the table was full" | thread, first prototype | the test setup wasn't recorded |
| "Absent keys skip the second line read (64% of misses at 85% load)" | thread, round 4 status | how it was computed wasn't recorded |
| Asm PREFETCHT0 and "two-choice insert" variants of the alternate-line prefetch | backlog, thread round 3 | code not kept (the plain-load version is 40f9223) |
| Mechanism explanations: the clock read "stops later work from overlapping", hits on DRAM are "latency-bound because the CAS stops later lookups from overlapping", "the adjacent-line prefetcher doesn't make the second [line] free", "Go heap already benefits or TLB isn't the bottleneck" | backlog | hypotheses offered to explain measurements; never tested directly |
| Literature summaries and originality judgments | RESEARCH.md §1–2 | check them against the linked sources; the search wasn't exhaustive |
