# Clepsydra: a fixed-memory temporal filter

**Abstract.** Clepsydra is a fixed-memory filter that answers two questions about a stream of keyed events: *"was key X seen in the last W?"* and *"how many times in the last W?"*, for any window `W` up to a configured horizon, chosen per query. Each key's exact last-touch time and an age-graded event counter (the velocity register) share one 16-byte slot of a 64-byte cache line, so a hit costs one memory access. Hits are lock-free (a CAS on the slot's two words); a key's first touch takes one of 256 striped locks. Evicted keys keep their history in a stash, histories the stash drops go to an overflow table that no flood can erase within the horizon, and heavy keys move to exact 32-bit counters. Every answer says how far it can be trusted (`Upper`, `Saturated`, `Restored`, `Degraded`), so a rate limiter or abuse check can fail closed. At the default 85% load it uses 18.8 B per key, misses no key up to 1.15x its capacity, has a false-positive rate of 1.8e-8, and a velocity error of 0.078 [D1, D3, D5, D6].

**Status.** Research prototype. The Go implementation (`go/`) is the reference; Python, C++ and Java ports are planned. Every claim in these documents carries a tag that [the claims register](artifact/claims.md) maps to a check.

## How the documentation is organized

A new data structure is documented the way the field introduces one: a **paper** (what it is, why it works, what it guarantees, how well it does), a language-neutral **specification** that other implementations must reproduce, and a reproducibility **artifact** (every claim with the command that checks it). The fourth part is the **record** of how the structure got here, kept as one body of work.

| Part | Question it answers | Documents |
|---|---|---|
| [paper/](paper/) | What is it, why does it work, how well? | [1 Introduction](paper/01-introduction.md) · [2 Related work](paper/02-related-work.md) · [3 Design](paper/03-design.md) · [4 Operations and guarantees](paper/04-operations.md) · [5 Concurrency](paper/05-concurrency.md) · [6 Security](paper/06-security.md) · [7 Evaluation](paper/07-evaluation.md) · [8 Limitations and future work](paper/08-limitations.md) |
| [specification/](specification/) | What must every implementation reproduce? | [Format and parameters](specification/format.md) · [Conformance: vectors and test tiers](specification/conformance.md) |
| [artifact/](artifact/) | How is each claim checked, and how do I re-run it? | [Claims register](artifact/claims.md) · [Reproducing the results](artifact/reproducing.md) · [Source map](artifact/source-map.md) · [Measuring speed](artifact/measuring-speed.md) · [Experiments (patches)](artifact/experiments.md) · [Speed runs](artifact/speed-runs.md) · [Isolated runs](artifact/isolated-runs.md) · [Method](artifact/method.md) |
| [record/](record/) | What was changed, fixed, tried and decided? | [Changes](record/changes.md) · [Defects found and fixed](record/defects.md) · [Decisions and rejected ideas](record/decisions.md) · [Scorecard history](record/scorecard-history.md) |

Read the paper in order for the idea; go to the specification to write a port; go to the artifact to check a number; go to the record to learn why something is the way it is.

## Conventions

- **Claim tags.** `[D1]`, `[A14]`, `[S45]` and so on point to [artifact/claims.md](artifact/claims.md), which gives each claim's check, result and status. The letter is the kind of evidence: **D** deterministic results, **A** attacks, adversarial and concurrent correctness, **R** replays of recorded results from git, **E** reverted experiments (patches), **S** speed (host-dependent), **X** corrected claims.
- **Commits.** Short hashes (`964b518`) name the commit of this repository where a change landed; `go/verify/replay.sh` re-runs many of them.
- **Paths and commands.** Paths are relative to the repository root. Commands run in `go/` unless they say otherwise.
- **Targets.** The 13 numbered performance and security targets (target 1 to target 13) are defined in [paper/07-evaluation.md](paper/07-evaluation.md#73-targets).
- **Readings M1 to M9.** The placement study's readings, each with its rule written before it ran, are listed in [artifact/measuring-speed.md](artifact/measuring-speed.md#the-placement-study).

### Hosts

Speed figures are reported, not checked: they depend on the machine. Compare ratios measured on one host, not absolute numbers across hosts.

| Host | Machine | Used for |
|---|---|---|
| **Xeon host** (also "the reference host") | 4-vCPU Intel Xeon @ 2.1 GHz, 260 MB L3 (266,240 KB), Linux 6.18, Go 1.24.7 | every speed figure up to commit 0597649, including [artifact/speed-runs.md](artifact/speed-runs.md) and claims S1–S35 |
| **Ryzen host** | AMD Ryzen 5 4600H under WSL2: 6 cores / 12 threads, 4 MB L3 per CCX, 4 KB pages, Go 1.24.7. A random DRAM access costs 189 ns with 4 KB pages and 147 ns with huge pages; about 4 misses stay in flight with 4 KB pages and 7 with huge pages; the CPU is taken away ~1,070 times a second for 15 µs at the median [S42, S43] | every speed figure from commit 9be2f39 on, claims S36–S60 |

On the Ryzen host the 1M-key "cold" benchmark table (20 MB) does not fit in L3, so "hit cold" mostly misses it; on the Xeon host the same table was L3-resident. A benchmark there also moves a few percent with the code's placement in the binary, through the CPU's front end (in its worst layouts the op cache hands part of the hit to the legacy decoder), and about as much from run to run [S55, S58]: see [artifact/measuring-speed.md](artifact/measuring-speed.md).

## Repository map

```
README.md            entry point
LICENSE, NOTICE      Apache License 2.0, and the attribution notice redistributions keep
.githooks/           commit-msg and pre-push hooks that reject Co-authored-by trailers;
                     enable them once per clone: git config core.hooksPath .githooks
docs/                this documentation (paper, specification, artifact, record)
spec/vectors/        conformance vectors: language-neutral traces every implementation must reproduce
bench/h2h/           the head-to-head's industry arms (Rust: pingora-limits, DashMap), artifact/head-to-head.md
go/                  Go reference implementation
  *.go                 package temporalfilter: the library (filter, register, hot, ghost, overflow) and its white-box tests
  test.sh              the five test tiers (smoke, sanity, regression, perf, stress)
  bench.sh             a scorecard row in docs/record/scorecard-history.md
  cmd/tfbench/         the accuracy and speed scorecard
  internal/            packages of their own:
    cpu/                 16-byte CAS and line prefetches (assembly, with fallbacks)
    siphash/             SipHash-2-4 behind Config.Key
    workload/            the churn workload shared by tfbench and the tests
    registersim/         the standalone register simulation
  verify/              tools that check the claims
    *.sh                 entry points: run, isolated, replay, experiments, tla, ab, lrab, speed
    lib/                 shell helpers the entry points source
    expected/            golden outputs the deterministic checks must match byte for byte
    experiments/         reverted experiments as git patches
    testdata/            older versions of test files the replays need
    tla/                 TLA+ models of the slot protocol (tla/experiments/: models of experiments not adopted)
```

[artifact/source-map.md](artifact/source-map.md) lists every file of `go/` by category, tests included. Go requires a package's white-box tests to sit beside its code, so the library and its tests share `go/`'s root; everything that stands on its own is a package under `go/internal/`.

Each port goes in its own top-level folder (`python/`, `cpp/`, `java/`) with its own build ([specification/conformance.md](specification/conformance.md)).
