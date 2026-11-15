# Clepsydra in Rust

The Rust port of the Go reference implementation (`go/`). It follows [the format](../docs/specification/format.md), reproduces `spec/vectors/` exactly (every hash, every operation's full `Result`, the end counters), and provides the five test tiers behind one entry point ([conformance](../docs/specification/conformance.md)). Where the specification is silent the Go code is normative, so this port translates it function by function, as [the C++ port](../cpp/README.md) does: each module is one `.go` file, each function keeps its Go name in snake_case, and every claim tag in a comment (`[A29]`, `[D29]`) points at the same code in all three.

Rust 2024 edition, declared for Rust 1.85 or later (`rust-version`; see [Portability](#portability)); the standard library only, in the library, its tests and its tools. Run with Rust 1.99.1 on x86-64 Linux and on arm64 Linux (musl, under qemu), under ThreadSanitizer with a nightly toolchain, and built with Rust 1.85; Windows (MSVC) and macOS (arm64) are checked from Linux and run only in CI (see [Portability](#portability)).

## Build and test

```
cd rust
cargo test --lib                        # every test (TF_SHORT=1: the short suite)
cargo test --lib -- TestA TestB --nocapture
cargo run --release --bin tfbench -- -exact
./test.sh smoke                         # also: sanity, regression, perf, stress, all
```

`test.sh` takes `CARGO` (default `cargo`), `NIGHTLY` (the toolchain for ThreadSanitizer, default `nightly`) and `OUT` (reports, default `./verify-report/<tier>`) from the environment. The suite reads the same switches as the Go suite: `TF_CT=1` (every filter ConstantTime), `TF_BG=1` (every filter Background), `TF_NOCAS2=1` (realign, stamp, unpoint and demotion hold word1 instead of using CMPXCHG16B), and `TF_SHORT=1` for `go test -short`. The tests share process-wide hooks, as the reference's package variables, so they run one at a time: `.cargo/config.toml` sets `RUST_TEST_THREADS=1` for `cargo test`, and `test.sh` sets it for the binaries it runs itself.

`test.sh` runs tests by their Go names (`run -- -run REGEX`), and prints the experiments' output as `go test -v` does: with `TF_LOG=<file>` the suite appends `=== RUN`, the log indented by four spaces and `--- PASS`/`--- FAIL` to the file, without the standard harness's lines between them, and the regression tier extracts the outputs from it as `go/verify/lib/checks.sh` extracts the Go tests'.

On a checkout on a Windows drive under WSL, `./cg.sh` is cargo with the target directory on a WSL-native path (builds on the Windows drive are slow) and, without a system C compiler to link with, zig's (a wrapper `zcc` running `zig cc "$@"`, as the C++ port's `zcxx`): `CARGO=./cg.sh ./test.sh sanity`. The stress tier's ThreadSanitizer build needs one thing more of that wrapper: rustc passes its TSan runtime as `-Wl,--whole-archive -Wl,<runtime>.a`, and zig's `cc` takes no archive inside `-Wl,`, so the wrapper passes such an argument's archive as a plain input (name it in `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER`, which `cg.sh` leaves alone when it is set). The instrumented suite runs with ASLR off (`setarch -R`), as the C++ port's.

## Test tiers

| Tier | What it runs here | Status |
|---|---|---|
| **smoke** | build every target; clippy with warnings denied (the Go tier's vet); `cargo fmt --check` (its gofmt); `TestSmoke` (every public operation once, three configs), `TestSmokeOptional` (Merge, a snapshot round trip, a Resizable transition), SipHash reference vectors | passes |
| **sanity** | smoke, the suite with `TF_SHORT=1` (as Go's tier), the suite under `TF_CT=1`, `TF_BG=1` and, on x86-64, `TF_NOCAS2=1`, and the conformance vectors | passes |
| **regression** | conformance vectors, `TestRegress*`, and every deterministic output of the Go regression tier against `go/verify/expected/` byte for byte: `exact` (`tfbench -exact`), `placement`, `register-experiment`, `velocity-bound`, `velocity-information`, `attacks`, `rates`, `sketch-experiments`, `flood-sizing`, `answer-digest` | passes: all ten reproduce byte for byte |
| **perf** | the twelve benchmarks of `go/test.sh`'s `PERF_BENCH` (`bench/bench.rs`, run as `testing.B` runs them and printed as `go test -bench` prints them), revision A against the working tree, alternated (`bench/ab.sh`); fails above `PERF_MAX` (1.20) | runs; `./test.sh perf go` compares with the Go reference ([below](#speed)). The Rust/Rust gate needs a committed `rust/` to compare with |
| **stress** | ThreadSanitizer on the suite, plain and in each mode (needs a nightly toolchain with `rust-src`; skipped, and the tier says so, without one); the four racy regression tests 300 times each, each run its own process (as `go/verify/replay.sh stress`); `TestStressSoak` (plain, under TSan, Background, Background with tags shared 8 ways); `TestFuzzUpper`; `TestFuzzConcurrent` (plain and with tags shared 8 ways) | passes at its defaults (nightly 1.102 for TSan): ThreadSanitizer over the whole suite, plain and in each mode, no report (25, 13, 14 and 26 min); the racy tests 0 failures in 300 runs each; soaks 60 s each (24 threads, 0.23 to 0.25 billion operations, no unflagged low answer), under TSan 20 s (4.0 million operations); the fuzzer 1.03 billion operations (872 cases); the concurrent fuzzer 2,054 and, tags shared, 2,379 cases |

### The suite

153 tests. 139 are Go's (of the reference's 150), under their Go names, with Go's constants, windows, rounds and skip conditions: every test of the library's files, the race tests and their hooks, the white-box unit tests, the experiments the regression tier diffs (`TF_EXP=1`), the soak and the two fuzzers with the tag amplifier, the timing-leak measurement (`TF_TIMING=1`), the batch hash's (`go/internal/siphash`) and the register simulation's (`go/internal/registersim`). 14 are the port's own, the C++ port's: the format's optional operations (`TestSmokeOptional`, `TestSnapshotRoundTrip`, `TestSnapshotRefusals`, `TestSnapshotRefusesWildIndices`, `TestMergeSums`, `TestTouchIfUnderNeverOverLimit`, `TestHotKeepsBurstsExact`, `TestResizeKeepsCounts`), configuration edges (`TestConfigDefaults`, `TestMaxTick`, `TestTicksNegative`, `TestExactBoundaryContains`), `TestConformanceScenarios` (the vectors' operation streams are what the reference's generators draw from their seed), and `TestGoRand` (the port of `math/rand/v2` reproduces a million of Go 1.24.7's draws).

Not ported, 11 of Go's 150, as in the C++ port: the host experiments behind `TF_EXP` and `TF_CTEXP` that the regression tier doesn't diff (`TestThroughput`, `TestHostFloor`, `TestHostGaps`, `TestMemProbe`, `TestInsertTail`, `TestPlacerInterference`, `TestSweepLatency`, `TestCTPads`, `TestCTParallel`: they measure the host and the Go runtime), and `go/internal/ecm`'s two (`TestHistogramError`, `TestSketchBounds`: the comparison sketch of `cmd/h2h`, not the filter). `go/cmd/tfbench` is ported for `-exact` (`tools/tfbench.rs`); its speed scorecard is the perf tier's job. Of `go/bench_test.go`, the twelve `PERF_BENCH` benchmarks.

The tests are `#[test]` functions in the crate (`src/tests/`), white-box as Go's are: `Filter` derefs to the reference's struct (`Core`) under `cfg(test)`. `tests/check.rs` is Go's `testing` in miniature (`T`, `errorf!`, `fatalf!`, `logf!`, `skip!`) and prints Go's formats: `%v` of a float as strconv's shortest `'g'`, a duration as `time.Duration` prints, a `Result` as `%+v`. A Go subtest (`t.Run`) is a loop whose failures name the case (`demote/cas2`).

`TestFuzzUpper` drives each case from a seeded stream on one thread, so a case is a pure function of its seed and the code: for seeds 1 to 100 (64K to 4M operations each, ConstantTime, Background, stashless, overflow-off and few-hot-entries configurations among them) every tally the case prints (operations, answers checked, Saturated, floored, evictions, folds, promotions, waits) equals the reference's.

## Architecture

`src/lib.rs` is the public API: `Config`, `Result`, `Filter`, `Resizable`, `MAX_TICK`, `AUTO_PAD`, `ClockFn`, `Error`. Names are the Go reference's exported names in snake_case (`touch_if_under`, `check_and_touch_batch`, `Config::stash`), so [the paper](../docs/paper/04-operations.md) and the specification read the same for all three implementations.

`Core` (`src/filter.rs`) is the Go `Filter` struct, field for field, with every method the reference has; the public `Filter` (`src/api.rs`) owns it behind an `Arc`, which the Background placer's thread shares while it runs. Go's package-private visibility maps to `pub(crate)`: every member is visible to the library and to the white-box tests, as Go's unexported identifiers are to `go/*_test.go`.

| Go | Rust | What it holds |
|---|---|---|
| `filter.go` | `src/filter.rs`, `src/api.rs` | construction and defaults, the clock, hashing, `ContainsAt`, counters; the public `Filter`'s methods |
| `slots.go` | `src/slots.rs` | slot words, lookups, fingerprint, tag, line arithmetic; the aligned table, the striped mutexes |
| `touch.go` | `src/touch.rs` | `touch`, `count`, `read`, `lookupStable`, the overflow read, realign and its 16-byte CAS |
| `cuckoo.go` | `src/cuckoo.rs` | `miss`, `claim`, `claimAt`, eviction, install, relocation, abandon |
| `stash.go` | `src/stash.rs` | the stash and the wait room, the Background placer |
| `ct.go` | `src/ct.rs` | ConstantTime's pads and the placement steps |
| `batch.go` | `src/batch.rs` | the batches and their prefetching |
| `decay.go` | `src/decay.rs` | the decay sweep |
| `register.go` | `src/register.rs` | the velocity register, `bump`, `unpoint`, `strayed`, `mass`, `estimate` |
| `hot.go` | `src/hot.rs` | hot entries |
| `ghost.go` | `src/ghost.rs` | the overflow table |
| `overflow.go` | `src/overflow.rs` | `spill`, the overflow table's allocation, and the count-min sketch the ghost table replaced (behind `Core::cms`, for the experiments and the answer digest) |
| `buzz.go`, `touchif.go` | `src/buzz.rs` | conditional touches and buzz entries |
| `merge.go` | `src/merge.rs` | `Merge` |
| `snapshot.go` | `src/snapshot.rs` | `WriteTo`, `ReadFrom` (version 3, reads version 2) |
| `resize.go` | `src/resize.rs` | `Resizable` |
| `internal/cpu`, `math/bits` | `src/cpu.rs` | `CAS2` (CMPXCHG16B after a CPUID check), `PrefetchLines`, `Mul64`, `Div64` (over `u128`) |
| `internal/siphash` | `src/siphash.rs` | SipHash-2-4 |
| `hash/crc32` (Castagnoli) | `src/crc32c.rs` | the snapshot's check words |
| package variables | `src/hooks.rs` | the test hooks (`testHook*`), `forceCT`, `forceBG`, `buzzOff` |
| `*_test.go` | `src/tests/*_test.rs` | tests under their Go names; `check.rs` the harness, `json.rs` reads the vectors, `gorand.rs` is Go's `math/rand/v2` over PCG bit for bit (so a seeded case replays as in Go), `gosort.rs` Go's `sort.Slice` (pdqsort, for tied ticks), `helpers.rs` holds `whereIs`, `floorLive` and the tag amplifier |

## Type mapping

| Go | Rust | Note |
|---|---|---|
| `uint64`, `int64`, `uint32` | `u64`, `i64`, `u32` | |
| `int` (counts, `Config` sizes, `Result.Count`) | `i64` | Go's `int` is 64-bit on every platform the reference runs on |
| `int` (a word index, -1 for none) | `isize` | a negative index panics, as an index out of range does in Go |
| `float64` | `f64` | Rust fuses no multiply-add it isn't asked to (`mul_add`), as Go on amd64; the operations run in the reference's order, term for term |
| `float64` → `int` | `as i64` | truncation toward zero, as Go; out of range Rust saturates where Go's result is the platform's. No value the filter computes from a valid Config is out of range; a NaN `Config::load` is ([below](#deviations-from-the-reference)) |
| `time.Duration` | `std::time::Duration` in the API, `i64` nanoseconds inside | a `Duration` is never negative: `Ticks` has no negative input to clamp (`TestRegressNegativeTicks`, `TestTicksNegative`); `AUTO_PAD` is `Duration::MAX` (Go: -1) |
| `time.Now`, `time.Since` | `std::time::Instant` | the built-in clock; `SystemTime` for the snapshot's wall time |
| `func() uint64` (`Config.Clock`) | `Option<ClockFn>`, `ClockFn = Arc<dyn Fn() -> u64 + Send + Sync>` | |
| `*[16]byte` (`Config.Key`) | `Option<[u8; 16]>` | |
| `[]uint64` (outputs of batches) | `&mut [T]` | a slice shorter than its input panics before any key is touched (Go: an index panic on the way) |
| `string` (keys) | `&str` | |
| `(T, bool)`, `(bool, int)` | tuples | |
| `error` returned | `std::result::Result<T, clepsydra::Error>` | the reference's messages, word for word |
| `panic(errTick)` | `panic!` with the same text | before any lock, as the reference |
| `sync/atomic` | `AtomicU64` and its kin, `SeqCst` everywhere | Go's atomics are sequentially consistent; a plain Go access to a word others access atomically is an atomic access here too |
| a word a lock guards (stash entries, buzz blocks, the hot table's free list) | atomics, or a `Mutex` the lock is | `StashEntry` and `BuzzEntry` are values read from and written back to their cells under the set's lock |
| `atomic.Pointer[T]`, set once | `OnceLock<T>` | replaces `skMu`; `ReadFrom` writes into the table `get_or_init` gives it (Go stores a new one) |
| `cpu.CAS2` | `core::arch::x86_64::cmpxchg16b` | over two `AtomicU64`, as the reference over two words; mixed-size atomics are outside the Rust model, inside every x86-64 CPU's |
| `sync.Mutex`, `TryLock` | `std::sync::Mutex<()>`; `PaddedMutex` is 64-byte aligned | a poisoned lock is used as it is, as Go's mutexes have no poison. The filter's own locks see no panic (a tick past MaxTick panics before any lock); `Resizable`'s mutex is held across a touch that can panic so, and its next call recovers it, as Go's deferred unlock releases it |
| `defer` | a guard's `Drop` at function scope | |
| goroutine (CoarseClock) | `std::thread` holding a `Weak` to the filter | it ends at `Close`, or when the filter is dropped. On Windows `thread::sleep` asks for a high-resolution waitable timer, as Go's runtime does (the C++ port's timer defect, below its README's table); not yet run there |
| goroutine (Background placer) | `std::thread` holding an `Arc` while it runs | started on the first wait, ends after its idle naps, as the goroutine |
| `runtime.Gosched` | `std::thread::yield_now` | |
| `bits.TrailingZeros64`, `RotateLeft64` | `u64::trailing_zeros`, `u64::rotate_left` | |
| `unsafe` aligned tables (`alignedWords`) | `AlignedWords` | 64-byte aligned, zeroed by the allocator; pages fault on first touch, as the reference's |
| `hash/maphash` (unkeyed hashing) | `std::collections::hash_map::RandomState` | a random key per filter, as `maphash.MakeSeed` |
| package variables (test hooks, `forceCT`, `buzzOff`) | `static`s in `hooks.rs` | a hook costs one relaxed load while unset |
| `Result` | `clepsydra::Result` | the reference's name; the standard library's is written `std::result::Result` in this crate |

## Porting rules

These are the rules the translation keeps; a change to any implementation should keep them too.

1. **Wrapping.** Every wrap the reference relies on is spelled `wrapping_*`. The suite runs with overflow checks on (`[profile.dev]`: optimized, overflow and debug assertions on), so an overflow the reference doesn't intend panics in the suite, the soak and the fuzzers instead of wrapping silently.
2. **Operator precedence.** Go binds `<<` and `>>` tighter than `+` and `-`; Rust doesn't. Every mixed expression is fully parenthesized (`h>>36&(1<<tagBits-1)` is `(h >> 36) & ((1 << TAG_BITS) - 1)`), and arithmetic is written as the reference writes it (`(a + b - 1) / b`, `x >= y + 1`: clippy's `manual_div_ceil` and `int_plus_one` are allowed for that).
3. **Shifts.** Go defines a shift by 64 or more as 0; Rust panics in the test profile and masks the count in release. No shift count here can reach 64.
4. **Names.** A method both `Filter` and `Core` have (`touch`, `count`, `hash`) resolves to the public one: the tests call `f.c.touch(...)` for the internal one.
5. **Same order of floating-point operations** as the reference, term for term, so `Count` agrees to the bit.
6. **Same allocation sizes** as the reference: `size_of::<StashCell>() == 24`, `size_of::<HotEntry>() == 64`, `size_of::<BuzzBlock>() == 512` (compile-time assertions), so `Bytes()` reports what the reference reports.
7. **No environment in the library.** The library reads no environment variable, as the reference's and the C++ port's don't: the suite sets `forceCT`, `forceBG` and the 16-byte CAS's switch from `TF_CT`, `TF_BG` and `TF_NOCAS2` (`tests/check.rs`).

## Speed

`./test.sh perf go` (`bench/ab.sh go .`): the twelve `PERF_BENCH` benchmarks, Go 1.24.7 against this port (the release profile, Rust 1.99.1), alternated six rounds on the Ryzen host (6 cores / 12 threads, WSL). Medians, ns/op:

| benchmark | Go | Rust | Rust/Go |
|---|---:|---:|---:|
| CheckAndTouchHit | 188.3 | 178.1 | 0.95 |
| CheckAndTouchHitHot | 59.7 | 60.2 | 1.01 |
| CheckAndTouchMissInsert | 1700.0 | 1471.6 | 0.87 |
| CheckAndTouchBatch (per key) | 89.7 | 70.0 | 0.78 |
| CheckAndTouchParallel | 23.0 | 22.2 | 0.97 |
| TouchAndCount | 198.4 | 197.6 | 1.00 |
| VelocityCount | 186.3 | 167.6 | 0.90 |
| VelocityCountHot | 93.3 | 54.5 | 0.58 |
| ContainsAbsent | 163.8 | 134.8 | 0.82 |
| InsertSteady | 469.1 | 387.3 | 0.83 |
| InsertNearFull | 4963.5 | 4213.3 | 0.85 |
| InsertSaturated | 1191.5 | 1008.6 | 0.85 |

What one response time tells about a key is compiled code's business, so the port has its own reading of it: `TF_TIMING=1 cargo test --release --lib -- --exact tests::timing_test::TestTimingLeak --nocapture` (2M keys at 85% load, 40,000 probes, the host otherwise idle). Kolmogorov-Smirnov distance between seen and unseen latencies, without ConstantTime: 0.089 (Count), 0.570 (Touch); with a 3 µs pad: 0.008 and 0.011, under the noise floor of about 0.014 at 20,000 samples per side, as the reference's [S41].

## Defects found while porting

Each was found by a review of the port against the reference, and fixed:

| Where | Defect | Found by |
|---|---|---|
| `cpu.rs` | the CPU probe read `TF_NOCAS2` from the environment (the library reads none); the suite sets the switch now, and the probe stores its finding only over "not probed yet", so the suite's setting wins | review against `internal/cpu` |
| `snapshot.rs` (`snap_scrub`) | a refused restore left the hot table's `full` (the unit in which an allocation last found no entry) at 0 instead of "none", so promotions in unit 0 skipped their search for a free entry | review against `hot.go` |

## Deviations from the reference

| What | Reference | Here | Why |
|---|---|---|---|
| Hashing without `Config::key` | `hash/maphash` under a random seed | the standard library's `RandomState` (SipHash-1-3 under a random key) | the format asks for a secret per-filter random seed; Rust has no maphash |
| `HashUint64s` | 8 keys at once with AVX-512F | one key at a time | same results; the batch hash is a later speed step |
| `ReadFrom` of a stream whose check words are right but whose words no quiescent filter writes | accepted; an out-of-range hot-entry index panics at its first use, an odd hot-entry version hangs its readers | refused (`snap_sane`), and a refused restore leaves the filter empty (`snap_scrub`), as in the C++ port | a snapshot is trusted input, but a refusal is a better failure than a panic or a hang later (`TestSnapshotRefusesWildIndices`) |
| `ReadFrom` of a stash section into a filter with `Config::stash < 0` | panics (`newStashTable` takes the address of an empty table's first entry) | refused | as above |
| Negative durations | `Ticks` clamps them to 0 | not representable | `std::time::Duration` |
| A NaN `Config::load` | passes both of `New`'s checks; the line count converts to 2^63 on amd64, and the allocation fails | passes both checks too; the conversion gives 0, so the filter has the minimum, 2 lines | invalid input, outside the format's range (at most 1); the checks are kept as the reference writes them |

## Portability

x86-64 uses CMPXCHG16B after a CPUID check, as the reference does; other architectures take the hold path (`has_cas2` false), as the reference does on arm64. `Mul64` and `Div64` are `u128` arithmetic everywhere.

| Platform | Toolchain | What has run |
|---|---|---|
| x86-64 Linux (WSL) | Rust 1.99.1, linked by zig's `cc` | every tier; the suite in each mode |
| x86-64 Linux (WSL), ThreadSanitizer | nightly 1.102 (`-Zsanitizer=thread`, `-Zbuild-std`) | the suite with `TF_SHORT=1` in each mode (plain, `TF_CT=1`, `TF_BG=1`, `TF_NOCAS2=1`): 153 pass, no report; the stress tier's TSan checks ([above](#test-tiers)) |
| arm64 Linux (musl, under qemu) | Rust 1.99.1 (`aarch64-unknown-linux-musl`), linked by zig's `cc` | the suite with `TF_SHORT=1`, plain, `TF_CT=1` and `TF_BG=1`: 151 pass in each (the two CMPXCHG16B tests are x86-64's, as Go's `cas2_amd64_test.go` is amd64's); the prefetch's `prfm` assembles |
| x86-64 Windows (MSVC); arm64 macOS | Rust 1.99.1 | clippy over every target with warnings denied, from Linux (`--target x86_64-pc-windows-msvc`, `--target aarch64-apple-darwin`); built and run only in CI (`.github/workflows/rust.yml`) |
| Rust 1.85, the declared minimum | 1.85.1 | the library, the suite and the tools build (CI's `msrv` job, run here too); clippy's `incompatible_msrv` checks the standard library's APIs against it |

ThreadSanitizer and the 16-byte CAS: the CAS is a compiler intrinsic here (`core::arch::x86_64::cmpxchg16b`, an `atomic cmpxchg` of 128 bits), not assembly as in the reference and the C++ port, so TSan instruments it, and the suite's 16-byte CAS becomes a call of the runtime's `__tsan_atomic128_compare_exchange_val`. That runtime (compiler-rt, as the nightly toolchain ships it) executes no CMPXCHG16B: it compares and stores the two words with plain loads and stores, under one global spin lock (`mutex128`) and, for a non-relaxed order, the lock of its address's sync object. An 8-byte atomic on the same words takes neither of the first and, unless it is non-relaxed and on word0 (the same address), not the second. Under TSan, then, a 16-byte CAS can overwrite an update of word1, or a relaxed update of word0, that lands between its load and its store, as the hardware's can't. A probe measured it (one thread swapping a 16-byte-aligned pair from what it read, changing one word; another adding 1 to the other word; 2 million times each, three runs per case): adds to word1 lost 1,283 to 3,934, relaxed adds to word0 216 to 3,430, SeqCst adds to word0 none; natively, none in any case. So the suite's counts under TSan are no evidence about the 16-byte CAS: the native runs are (the stress tier's racy tests, soaks and fuzzers), and under TSan `TF_NOCAS2=1` (`race-no-cas2`) is the mode whose atomic operations are the hardware's. A count wrong only under TSan with the 16-byte CAS on would be the runtime's; none has been wrong.
