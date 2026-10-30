# Source map of the Go implementation

Every file of `go/`, by category. Go builds a package from one directory and requires a package's white-box tests (which reach its unexported fields and test hooks) to sit in that same directory, so the library and its tests share `go/`'s root; everything that can stand on its own is a package of its own under `go/internal/`, and the tooling sits under `go/verify/`. The verification scripts also rely on this: `verify/replay.sh` treats the non-test `.go` files of the module's top directory and of `internal/cpu` and `internal/siphash` as "the library", and copies test files into older commits by name ([reproducing.md](reproducing.md#replays)).

## Library: `go/` (package `temporalfilter`)

| File | What it holds | Paper |
|---|---|---|
| `filter.go` | the table and slot words, placement and the cuckoo search, eviction, the stash, relocation, the wait room and Background placer, ConstantTime, the decay sweep, every public operation and counter | [§3](../paper/03-design.md), [§4](../paper/04-operations.md), [§5](../paper/05-concurrency.md) |
| `register.go` | the velocity register: packing, advance, spill, the estimate | [§3.7](../paper/03-design.md#37-the-velocity-register) |
| `hot.go` | hot entries: promotion, the lock-free add, demotion, reclaim | [§3.6](../paper/03-design.md#36-hot-entries) |
| `ghost.go` | the overflow ghost table: two buckets, floors, epochs | [§3.5](../paper/03-design.md#35-the-overflow-table) |
| `overflow.go` | the count-min overflow sketch the ghost table replaced, kept for the experiments | [record/decisions.md](../record/decisions.md#overflow) |
| `go.mod` | the module, `github.com/scooter-sensei/clepsydra/go` | — |

## Internal packages: `go/internal/`

| Package | Files | What it holds |
|---|---|---|
| `cpu` | `cas2_amd64.go`, `cas2_amd64.s`, `cas2_other.go`, `prefetch_asm.go`, `prefetch_amd64.s`, `prefetch_arm64.s`, `prefetch_other.go` | the platform primitives: `CAS2` (LOCK CMPXCHG16B, with a CPUID probe for CX16, `HasCAS2`; a stub elsewhere) for realign [A33], and `PrefetchLines` (`PREFETCHT0` on amd64, `PRFM` on arm64, plain loads elsewhere) for the batches [S48] |
| `siphash` | `siphash.go`, `siphash_test.go` | SipHash-2-4 behind `Config.Key`, and its reference vectors |
| `workload` | `churn.go` | the scorecard's churn stream, shared by `cmd/tfbench` and the placement test |
| `registersim` | `doc.go`, `registers_experiment_test.go` | the standalone simulation of the linear, dyadic and Laplace registers [D12] |
| `ecm` | `ecm.go`, `ecm_test.go` | the ECM-sketch (Papapetrou et al. 2012), the head-to-head's competitor at equal memory: exponential histograms in a count-min array, level minima from the paper's invariant 1 for every range, checked against exact counts [D20] |

## Command: `go/cmd/h2h/`

The head-to-head harness ([head-to-head.md](head-to-head.md)): `trace.go` (part 1's flood traces, their format and generator), `clep.go` (the Clepsydra arm's five passes), `timing.go`, `tsc.go`, `tsc_amd64.s`, `tsc_other.go` (RDTSCP and the log-linear latency histogram, as `bench/h2h` has them), `report.go` (scores every arm's answers, summarizes the passes), `velocity.go` (part 2: the scorecard's velocity workload with the key count as a parameter, Clepsydra and the ECM-sketch at equal memory), `main.go`. The industry arms are the Rust crate `bench/h2h/` at the repository's root (`pingora-limits` and DashMap, pinned in its `Cargo.lock`).

## Command: `go/cmd/tfbench/`

`main.go` (flags, the scorecard row, appending to [the scorecard history](../record/scorecard-history.md)), `accuracy.go` (the deterministic accuracy harness, `-exact`), `speed.go` (the speed columns) and `speed_stub.go` (built with `-tags accuracyonly`, so the harness runs on library versions without the speed code's API).

## Tests: `go/*_test.go` (package `temporalfilter`)

| Category | Files |
|---|---|
| Smoke and unit | `smoke_test.go` (every public operation once, three configs), `filter_test.go`, `register_test.go`, `disp_test.go` (the displaced bit), `batch_test.go`, `ghost_test.go`, `hot_test.go`, `background_test.go`, `ctwait_test.go` (ConstantTime's waiting keys) |
| Conformance | `conformance_test.go` (`spec/vectors/`, `-update` regenerates them) |
| Regression of fixed bugs | `regress_test.go`, `dup_test.go`, `reloc_race_test.go`, `abandon_test.go`, `read_race_test.go`, `restore_test.go`, `lost_test.go`, `movecopy_test.go`, `restorewait_test.go` (A37), `mirror_test.go` (A39), `hotdrop_test.go` (A40) |
| Concurrency races on hooks | `gaprace_test.go` (A21), `roomrace_test.go` (A20, A24), `split_race_test.go` (971dc4a, 64ebd85, A23), `sweep_race_test.go` (A26), `install_race_test.go` (A27), `tagstripe_test.go` (A28–A30), `hotspill_test.go` (A32, A38), `stash_race_test.go` (A6), `cas2_amd64_test.go`, `cas2_other_test.go` (realign both ways, A33) |
| Fuzzers and soak | `fuzz_test.go` (`TestFuzzUpper`), `fuzzconc_test.go` (`TestFuzzConcurrent`), `soak_test.go` (`TestStressSoak`), `tagamp_test.go` (the tag amplifier, `TF_CFUZZ_TAGS`, `TF_STRESS_TAGS`) |
| Attacks | `attack_test.go` |
| Deterministic experiments (`verify/expected/`) | `placement_test.go` (the placement oracle, D8–D11), `rate_test.go` (A15), `velbound_test.go` (D18), `secexp_test.go` (A9–A11), `floodexp_test.go` (A14) |
| Speed and host probes | `bench_test.go` (the benchmarks), `cycles_linux_test.go`, `cycles_other_test.go` (`cycles/op`), `throughput_test.go` (S37, S53), `inserttail_test.go` (S38, S45), `timing_test.go` (S28, S41), `ctexp_test.go` (S33, S36), `hostprobe_test.go` (S42, S43) |

## Scripts and verification: `go/test.sh`, `go/bench.sh`, `go/verify/`

| Path | What it holds |
|---|---|
| `test.sh` | the five test tiers, the entry point every implementation provides ([conformance](../specification/conformance.md#test-tiers)) |
| `bench.sh` | race tests, then a scorecard row |
| `verify/run.sh`, `isolated.sh`, `replay.sh`, `experiments.sh`, `tla.sh` | every claim check, offline isolation, replays of recorded results, reverted experiments, model checks ([reproducing.md](reproducing.md)) |
| `verify/ab.sh`, `lrab.sh`, `speed.sh` | speed A/B, placement-randomized A/B, the speed report ([measuring-speed.md](measuring-speed.md)) |
| `verify/h2h.sh` | part 1 of the head-to-head: both runners, the traces, the rotated rounds, the report ([head-to-head.md](head-to-head.md)) |
| `verify/lib/` | `lib.sh` (tree export, import rewriting) and `checks.sh` (the checks behind `run.sh` and `test.sh`), sourced by the scripts |
| `verify/expected/` | the golden outputs the deterministic checks must match byte for byte |
| `verify/experiments/` | reverted experiments as git patches ([experiments.md](experiments.md)) |
| `verify/testdata/` | older versions of `ghost_test.go` and `restore_test.go`, and `mirror_test.go` without its read check, that the bug replays need |
| `verify/tla/` | the TLA+ models of the slot protocol; `verify/tla/experiments/` holds `HotCells.tla`, the model of an experiment not adopted |
